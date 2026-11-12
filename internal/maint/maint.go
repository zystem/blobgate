// Package maint keeps the nodes of a mirror alike and the nodes clean (docs/storage-module.md): the repair queue, the check of the inventory against the nodes, and the sweep of the blocks of
// uploads that never ended. One replica of the module does it at a time: the one that holds the lease (a row of `storage_lease`, renewed while it works).
package maint

import (
	"context"
	"encoding/json"
	"log/slog"
	"time"

	"github.com/zystem/blobgate/internal/layout"
	"github.com/zystem/blobgate/internal/rq"
	"github.com/zystem/blobgate/internal/store"
)

// Config says how often and how hard.
type Config struct {
	HolderID    string        // who holds the lease: a name that differs between replicas
	RepairEvery time.Duration // between repair passes (60 s)
	CheckEvery  time.Duration // between checks of a slice of the inventory (10 min)
	RetryBase   time.Duration // the first pause after a repair that failed; it doubles with each failure, up to an hour (30 s)
	UploadTTL   time.Duration // an upload older than this is swept (24 h)
	CheckSlice  int           // objects in a slice (200)
	Deep        bool          // the check reads the blocks and compares their SHA-256, not only their size
	DeepBytesPS int64         // the most the deep check reads in a second (0: no limit)
	Now         func() time.Time
	Log         *slog.Logger
}

// LeaseSeconds is how long a hold lasts without being renewed.
const LeaseSeconds = 120

const maxPause = time.Hour

// Worker does the passes.
type Worker struct {
	cfg       Config
	DB        *rq.Client
	Reg       *store.Registry
	cursor    map[string]string // per store: the last key of the slice the check looked at (kept in memory: the position is no loss when it is gone)
	renewedAt time.Time
}

// New makes a worker; zero settings get their defaults.
func New(cfg Config, db *rq.Client, reg *store.Registry) *Worker {
	if cfg.HolderID == "" {
		cfg.HolderID = "storage"
	}
	if cfg.RepairEvery <= 0 {
		cfg.RepairEvery = 60 * time.Second
	}
	if cfg.CheckEvery <= 0 {
		cfg.CheckEvery = 10 * time.Minute
	}
	if cfg.RetryBase <= 0 {
		cfg.RetryBase = 30 * time.Second
	}
	if cfg.UploadTTL <= 0 {
		cfg.UploadTTL = 24 * time.Hour
	}
	if cfg.CheckSlice <= 0 {
		cfg.CheckSlice = 200
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Log == nil {
		cfg.Log = slog.Default()
	}
	return &Worker{cfg: cfg, DB: db, Reg: reg, cursor: map[string]string{}}
}

func (w *Worker) now() int64 { return w.cfg.Now().Unix() }

// Lease takes or renews the hold of the named job; false when another replica holds it.
func (w *Worker) Lease(ctx context.Context, name string) (bool, error) {
	n, err := w.DB.Exec(ctx, false, rq.S("INSERT INTO storage_lease (name, holder, until) VALUES (?, ?, ?) "+
		"ON CONFLICT(name) DO UPDATE SET holder = excluded.holder, until = excluded.until WHERE storage_lease.holder = excluded.holder OR storage_lease.until < ?",
		name, w.cfg.HolderID, w.now()+LeaseSeconds, w.now()))
	if err != nil {
		return false, err
	}
	w.renewedAt = w.cfg.Now()
	return len(n) == 1 && n[0] > 0, nil
}

// Release lets go of the hold (the replica is stopping), so that another one need not wait for it to run out.
func (w *Worker) Release(ctx context.Context, name string) {
	_, _ = w.DB.Exec(ctx, false, rq.S("UPDATE storage_lease SET until = 0 WHERE name = ? AND holder = ?", name, w.cfg.HolderID))
}

// stillMine renews the hold when it is half spent, so that a long pass keeps it; false when it was lost.
func (w *Worker) stillMine(ctx context.Context) bool {
	if w.cfg.Now().Sub(w.renewedAt) < LeaseSeconds*time.Second/2 {
		return true
	}
	ok, err := w.Lease(ctx, "repair")
	return err == nil && ok
}

// Run works until ctx ends: every RepairEvery the due tasks and the sweep, every CheckEvery a slice of the inventory.
func (w *Worker) Run(ctx context.Context) {
	t := time.NewTicker(w.cfg.RepairEvery)
	defer t.Stop()
	var lastCheck time.Time
	for {
		select {
		case <-ctx.Done():
			w.Release(context.Background(), "repair")
			return
		case <-t.C:
		}
		if ok, err := w.Lease(ctx, "repair"); err != nil || !ok {
			continue
		}
		w.Pass(ctx)
		if w.cfg.Now().Sub(lastCheck) >= w.cfg.CheckEvery {
			w.CheckPass(ctx)
			lastCheck = w.cfg.Now()
		}
	}
}

// Pass does the due repairs and the sweep once.
func (w *Worker) Pass(ctx context.Context) {
	done, failed := w.RepairPass(ctx)
	swept := w.SweepPass(ctx)
	if done+failed+swept > 0 {
		w.cfg.Log.Info("maintenance", "repaired", done, "failed", failed, "swept_uploads", swept)
	}
}

type task struct {
	store, key, node, op string
	since, attempts      int64
}

// RepairPass works the due tasks of the queue; it returns how many were done and how many failed (to be tried again later).
func (w *Worker) RepairPass(ctx context.Context) (done, failed int) {
	rows, err := w.DB.Query(ctx, rq.S("SELECT store, key, node, op, since, attempts FROM storage_repair WHERE next_at <= ? ORDER BY since LIMIT 100", w.now()))
	if err != nil {
		w.cfg.Log.Warn("repair: the queue could not be read", "error", err.Error())
		return
	}
	for _, r := range rows {
		if ctx.Err() != nil || !w.stillMine(ctx) {
			return
		}
		t := task{rq.Str(r[0]), rq.Str(r[1]), rq.Str(r[2]), rq.Str(r[3]), rq.Int(r[4]), rq.Int(r[5])}
		switch err := w.do(ctx, t); err {
		case nil:
			w.settle(ctx, t, true, "")
			done++
		default:
			w.settle(ctx, t, false, err.Error())
			failed++
		}
	}
	return
}

// settle ends a task that was done (or dropped), or puts off one that failed. The row is touched only if it is the one that was read (the last operation on a key and a node wins).
func (w *Worker) settle(ctx context.Context, t task, ok bool, why string) {
	if ok {
		_, _ = w.DB.Exec(ctx, false, rq.S("DELETE FROM storage_repair WHERE store = ? AND key = ? AND node = ? AND since = ?", t.store, t.key, t.node, t.since))
		return
	}
	pause := w.cfg.RetryBase << min(t.attempts, 20)
	if pause <= 0 || pause > maxPause {
		pause = maxPause
	}
	if len(why) > 300 {
		why = why[:300]
	}
	_, _ = w.DB.Exec(ctx, false, rq.S("UPDATE storage_repair SET attempts = attempts + 1, next_at = ?, last_error = ? WHERE store = ? AND key = ? AND node = ? AND since = ?",
		w.now()+int64(pause/time.Second), why, t.store, t.key, t.node, t.since))
}

// do runs one task; nil also for a task that has nothing left to do (the store, the node, the object or the upload is gone).
func (w *Worker) do(ctx context.Context, t task) error {
	m, err := w.Reg.Load(ctx, t.store)
	if err == store.ErrNoSuchStore {
		return nil
	}
	if err != nil {
		return err
	}
	node := m.NodeByID(t.node)
	full, gen, n, isBlock, ok := layout.ParseKey(t.key)
	if node == nil || !ok {
		return nil
	}
	switch t.op {
	case "purge":
		return node.RemoveDir(ctx, layout.BlockDir(full, gen))
	case "delete":
		if !isBlock {
			return node.RemoveDir(ctx, layout.BlockDir(full, gen))
		}
		return node.RemoveBlock(ctx, layout.BlockPath(full, gen, n))
	case "copy":
		if !isBlock {
			return nil
		}
		sha, size, found, err := w.expected(ctx, t.store, full, gen, n)
		if err != nil {
			return err
		}
		if !found {
			return nil // the object was replaced or deleted meanwhile, or the upload ended: there is nothing to give
		}
		rel := layout.BlockPath(full, gen, n)
		if node.Verify(ctx, rel, size, sha, true) == nil {
			return nil
		}
		return m.CopyTo(ctx, node, rel, sha, size)
	}
	return nil
}

// expected is the digest and the size block n of the generation must have: from the object's row if the generation is the current one, else from the parts of the upload it belongs to.
func (w *Worker) expected(ctx context.Context, st, full, gen string, n int) (sha string, size int64, found bool, err error) {
	rows, err := w.DB.Query(ctx, rq.S("SELECT generation, size, block_size, blocks FROM storage_objects WHERE store = ? AND key = ?", st, full))
	if err != nil {
		return "", 0, false, err
	}
	if len(rows) == 1 && rq.Str(rows[0][0]) == gen {
		var blocks []string
		if json.Unmarshal([]byte(rq.Str(rows[0][3])), &blocks) == nil && n < len(blocks) {
			total, bs := rq.Int(rows[0][1]), rq.Int(rows[0][2])
			return blocks[n], min(bs, total-int64(n)*bs), true, nil
		}
		return "", 0, false, nil
	}
	rows, err = w.DB.Query(ctx, rq.S("SELECT sha256, size FROM storage_parts WHERE store = ? AND key = ? AND upload = ? AND part = ?", st, full, gen, n+1))
	if err != nil || len(rows) == 0 {
		return "", 0, false, err
	}
	return rq.Str(rows[0][0]), rq.Int(rows[0][1]), true, nil
}

// SweepPass removes the blocks of uploads that began more than UploadTTL ago and neither ended nor were aborted (a writer that was killed): the directory of the generation on every node, then the rows.
func (w *Worker) SweepPass(ctx context.Context) int {
	rows, err := w.DB.Query(ctx, rq.S("SELECT store, key, upload FROM storage_uploads WHERE created_at < ? LIMIT 100", w.now()-int64(w.cfg.UploadTTL/time.Second)))
	if err != nil {
		return 0
	}
	swept := 0
	for _, r := range rows {
		if ctx.Err() != nil || !w.stillMine(ctx) {
			return swept
		}
		st, full, up := rq.Str(r[0]), rq.Str(r[1]), rq.Str(r[2])
		if m, err := w.Reg.Load(ctx, st); err == nil {
			if failed := m.DeleteDir(ctx, layout.BlockDir(full, up)); len(failed) > 0 {
				for _, n := range failed {
					w.queuePurge(ctx, st, layout.DirKey(full, up), n.ID)
				}
			}
		} else if err != store.ErrNoSuchStore {
			continue // the store could not be read now: tried again at the next pass
		}
		if _, err := w.DB.Exec(ctx, true, rq.S("DELETE FROM storage_uploads WHERE store = ? AND key = ? AND upload = ?", st, full, up),
			rq.S("DELETE FROM storage_parts WHERE store = ? AND key = ? AND upload = ?", st, full, up)); err == nil {
			swept++
		}
	}
	return swept
}

func (w *Worker) queuePurge(ctx context.Context, st, key, node string) {
	_, _ = w.DB.Exec(ctx, false, rq.S("INSERT INTO storage_repair (store, key, node, op, since, attempts, next_at, last_error) VALUES (?, ?, ?, 'purge', ?, 0, 0, '') "+
		"ON CONFLICT(store, key, node) DO UPDATE SET op = 'purge', since = excluded.since, attempts = 0, next_at = 0, last_error = ''", st, key, node, w.now()))
}

// queueCopy makes a `copy` task of a block a node lacks; a task that is already there is left alone, so that its pause is not lost.
func (w *Worker) queueCopy(ctx context.Context, st, key, node string) {
	_, _ = w.DB.Exec(ctx, false, rq.S("INSERT INTO storage_repair (store, key, node, op, since, attempts, next_at, last_error) VALUES (?, ?, ?, 'copy', ?, 0, 0, '') "+
		"ON CONFLICT(store, key, node) DO UPDATE SET op = 'copy', since = excluded.since, attempts = 0, next_at = 0, last_error = '' WHERE storage_repair.op != 'copy'", st, key, node, w.now()))
}

// CheckPass takes a slice of the inventory of every store with more than one node, asks every node for the size of every block (or, in a deep check, reads it and compares its digest), and makes
// a `copy` task of what is missing or differs. It is what puts a node whose disk was lost back in order. It returns the number of tasks it made.
func (w *Worker) CheckPass(ctx context.Context) int {
	list, err := w.Reg.List(ctx)
	if err != nil {
		return 0
	}
	made := 0
	for _, e := range list {
		if ctx.Err() != nil || !w.stillMine(ctx) {
			return made
		}
		m, err := w.Reg.Load(ctx, e.Name)
		if err != nil || len(m.Nodes) < 2 {
			continue
		}
		rows, err := w.DB.Query(ctx, rq.S("SELECT key, size, generation, block_size, blocks FROM storage_objects WHERE store = ? AND key > ? ORDER BY key LIMIT ?", e.Name, w.cursor[e.Name], w.cfg.CheckSlice))
		if err != nil {
			continue
		}
		if len(rows) == 0 {
			w.cursor[e.Name] = "" // the end: the next slice starts again from the first
			continue
		}
		w.cursor[e.Name] = rq.Str(rows[len(rows)-1][0])
		for _, r := range rows {
			full, total, gen, bs := rq.Str(r[0]), rq.Int(r[1]), rq.Str(r[2]), rq.Int(r[3])
			var blocks []string
			if gen == "" || json.Unmarshal([]byte(rq.Str(r[4])), &blocks) != nil {
				continue
			}
			for i, sha := range blocks {
				size := min(bs, total-int64(i)*bs)
				rel := layout.BlockPath(full, gen, i)
				for _, n := range m.Nodes {
					if n.Verify(ctx, rel, size, sha, w.cfg.Deep) == nil {
						continue
					}
					w.queueCopy(ctx, e.Name, layout.BlockKey(full, gen, i), n.ID)
					made++
				}
				if w.cfg.Deep && w.cfg.DeepBytesPS > 0 {
					time.Sleep(time.Duration(float64(size) * float64(len(m.Nodes)) / float64(w.cfg.DeepBytesPS) * float64(time.Second)))
				}
			}
		}
	}
	return made
}
