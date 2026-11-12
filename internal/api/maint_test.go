package api

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zystem/blobgate/internal/maint"
	"github.com/zystem/blobgate/internal/rq"
)

func (r *rig) worker(id string, mut func(*maint.Config)) *maint.Worker {
	cfg := maint.Config{HolderID: id, RetryBase: time.Second, UploadTTL: time.Hour, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	if mut != nil {
		mut(&cfg)
	}
	return maint.New(cfg, r.dbc, r.s.Reg)
}

func (r *rig) count(table string) int64 {
	rows, err := r.dbc.Query(context.Background(), rq.S("SELECT count(*) FROM "+table))
	if err != nil {
		r.t.Fatal(err)
	}
	return rq.Int(rows[0][0])
}

// chmodDirs sets the mode of a node's directory and of every directory under it.
func chmodDirs(root string, mode os.FileMode) {
	var dirs []string
	_ = filepath.Walk(root, func(p string, i os.FileInfo, err error) error {
		if err == nil && i.IsDir() {
			dirs = append(dirs, p)
		}
		return nil
	})
	if mode&0o200 != 0 { // opening up: parents first; closing: children first
		for _, d := range dirs {
			_ = os.Chmod(d, mode)
		}
		return
	}
	for i := len(dirs) - 1; i >= 0; i-- {
		_ = os.Chmod(dirs[i], mode)
	}
}

// readOnly makes node i refuse every write (all its directories are read-only); writable undoes it.
func (r *rig) readOnly(i int) {
	chmodDirs(r.nodes[i], 0o500)
	r.t.Cleanup(func() { chmodDirs(r.nodes[i], 0o755) })
}

func (r *rig) writable(i int) { chmodDirs(r.nodes[i], 0o755) }

// DAT-004: a node that missed a write is given the block by the repair pass, from the node that has it, and the task goes away.
func TestRepairGivesTheBlockBack(t *testing.T) {
	r := newRig(t, nil)
	r.mirror(1)
	r.readOnly(1)
	body := pattern(30000)
	if code, _ := r.put("acme/a", body); code != 200 {
		t.Fatal("put with a node down")
	}
	if r.count("storage_repair") != 1 || len(files(r.nodes[1])) != 0 {
		t.Fatal("the write was not missed on the node that is down")
	}
	w := r.worker("w1", nil)
	if ok, _ := w.Lease(context.Background(), "repair"); !ok {
		t.Fatal("lease")
	}
	w.Pass(context.Background()) // the node is still down: the task stays, with a pause
	if r.count("storage_repair") != 1 {
		t.Fatal("the task of a node that is down was dropped")
	}
	r.writable(1)
	time.Sleep(1100 * time.Millisecond) // the pause after a failure (RetryBase 1 s)
	w.Pass(context.Background())
	if r.count("storage_repair") != 0 {
		t.Fatalf("the task is still there")
	}
	if len(files(r.nodes[1])) != 1 {
		t.Fatalf("node 2 holds %v", files(r.nodes[1]))
	}
	// the repaired copy is a good one: read with the first node's block gone
	for _, f := range files(r.nodes[0]) {
		_ = os.Remove(filepath.Join(r.nodes[0], f))
	}
	if code, data := r.get("acme/a", 0, 29999); code != 206 || len(data) != 30000 {
		t.Fatalf("read from the repaired node: %d", code)
	}
}

// A node that lost its disk is put back in order by the check of the inventory, which needs nothing of it but the size; a deep check finds a block of the right size and the wrong bytes.
func TestCheckFindsALostDiskAndDamage(t *testing.T) {
	r := newRig(t, nil)
	r.mirror(0)
	bodies := map[string][]byte{"k/1": pattern(1000), "k/2": pattern(2000), "k/3": pattern(3000)}
	for k, b := range bodies {
		if code, _ := r.put(k, b); code != 200 {
			t.Fatal("put", k)
		}
	}
	if err := os.RemoveAll(r.nodes[1]); err != nil || os.Mkdir(r.nodes[1], 0o755) != nil {
		t.Fatal("lose the disk")
	}
	w := r.worker("w1", nil)
	w.Lease(context.Background(), "repair")
	if made := w.CheckPass(context.Background()); made != 3 {
		t.Fatalf("the check made %d tasks, want 3", made)
	}
	w.Pass(context.Background())
	if len(files(r.nodes[1])) != 3 || r.count("storage_repair") != 0 {
		t.Fatalf("after repair: %v, %d tasks", files(r.nodes[1]), r.count("storage_repair"))
	}
	// damage with the size kept: the ordinary check does not see it, the deep one does
	f := files(r.nodes[0])[0]
	info, _ := os.Stat(filepath.Join(r.nodes[0], f))
	_ = os.WriteFile(filepath.Join(r.nodes[0], f), []byte(strings.Repeat("d", int(info.Size()))), 0o644)
	for i := 0; i < 2; i++ { // a slice covers the whole inventory (three objects); the second round starts again from the first
		if w.CheckPass(context.Background()) != 0 {
			t.Fatal("the size-only check saw damage it cannot see")
		}
	}
	deep := r.worker("w1", func(c *maint.Config) { c.Deep = true })
	deep.Lease(context.Background(), "repair")
	made := deep.CheckPass(context.Background())
	if made != 1 {
		t.Fatalf("the deep check made %d tasks, want 1", made)
	}
	deep.Pass(context.Background())
	if data, _ := os.ReadFile(filepath.Join(r.nodes[0], f)); strings.Contains(string(data[:1]), "d") {
		t.Fatal("the damaged block was not repaired")
	}
}

// A copy that was missed for an object that has been deleted since is dropped, not made.
func TestRepairDropsWhatIsGone(t *testing.T) {
	r := newRig(t, nil)
	r.mirror(1)
	r.readOnly(1)
	r.put("g/one", []byte("one"))
	r.core("DELETE", "/v1/stores/main/objects/g/one", "", nil, nil)
	r.writable(1)
	w := r.worker("w1", nil)
	w.Lease(context.Background(), "repair")
	for i := 0; i < 2 && r.count("storage_repair") > 0; i++ {
		time.Sleep(1100 * time.Millisecond)
		w.Pass(context.Background())
	}
	if r.count("storage_repair") != 0 || len(files(r.nodes[1])) != 0 {
		t.Fatalf("%d tasks, node 2 holds %v", r.count("storage_repair"), files(r.nodes[1]))
	}
}

// The blocks of a replaced generation that a node could not remove are removed by the repair pass (a `purge` task); the new one is given to the node that missed it.
func TestRepairPurgesOldGeneration(t *testing.T) {
	r := newRig(t, nil)
	r.mirror(1)
	r.put("p/x", pattern(500))
	r.readOnly(1)
	r.put("p/x", pattern(600))
	if r.count("storage_repair") != 2 {
		t.Fatalf("want a copy and a purge task, have %d", r.count("storage_repair"))
	}
	r.writable(1)
	w := r.worker("w1", nil)
	w.Lease(context.Background(), "repair")
	w.Pass(context.Background())
	for _, d := range r.nodes {
		if f := files(d); len(f) != 1 {
			t.Fatalf("%s holds %v", d, f)
		}
	}
	if r.count("storage_repair") != 0 {
		t.Fatal("tasks left")
	}
}

// A failed repair is put off, and the pause doubles (DAT-004): here no node has a good copy, so nothing can be done.
func TestRepairBackoff(t *testing.T) {
	r := newRig(t, nil)
	r.mirror(1)
	r.readOnly(1)
	r.put("b/x", pattern(400))
	f := files(r.nodes[0])[0]
	_ = os.WriteFile(filepath.Join(r.nodes[0], f), []byte("garbage"), 0o644)
	r.writable(1)
	w := r.worker("w1", func(c *maint.Config) { c.RetryBase = 100 * time.Second })
	w.Lease(context.Background(), "repair")
	if _, failed := w.RepairPass(context.Background()); failed != 1 {
		t.Fatalf("failed %d", failed)
	}
	rows, _ := r.dbc.Query(context.Background(), rq.S("SELECT attempts, next_at, last_error FROM storage_repair"))
	if rq.Int(rows[0][0]) != 1 || rq.Int(rows[0][1]) < time.Now().Unix()+90 || !strings.Contains(rq.Str(rows[0][2]), "no other node") {
		t.Fatalf("%v", rows)
	}
	if done, failed := w.RepairPass(context.Background()); done+failed != 0 {
		t.Fatal("a task that is not due was tried")
	}
}

// Blocks of an upload that never ended are swept after the time-to-live, with their rows; a fresh upload is left.
func TestSweepsOrphanedUploads(t *testing.T) {
	r := newRig(t, nil)
	r.mirror(0)
	begin := func(key string) string {
		_, data, _ := r.core("POST", "/v1/stores/main/objects/"+key, "uploads", nil, nil)
		var u struct {
			ID string `json:"upload_id"`
		}
		_ = json.Unmarshal(data, &u)
		part := []byte("part of " + key)
		if code, _, _ := r.core("PUT", "/v1/stores/main/objects/"+key, "upload="+u.ID+"&part=1", map[string]string{"X-Content-SHA256": sha(part)}, part); code != 200 {
			t.Fatal("part")
		}
		return u.ID
	}
	old, fresh := begin("s/old"), begin("s/fresh")
	if _, err := r.dbc.Exec(context.Background(), false, rq.S("UPDATE storage_uploads SET created_at = 1 WHERE upload = ?", old)); err != nil {
		t.Fatal(err)
	}
	w := r.worker("w1", nil)
	w.Lease(context.Background(), "repair")
	w.Pass(context.Background())
	for _, d := range r.nodes {
		f := files(d)
		if len(f) != 1 || !strings.Contains(f[0], "s/fresh") {
			t.Fatalf("%s holds %v", d, f)
		}
	}
	if r.count("storage_uploads") != 1 || r.count("storage_parts") != 1 {
		t.Fatalf("rows: %d uploads, %d parts", r.count("storage_uploads"), r.count("storage_parts"))
	}
	_ = fresh
}

// Only one replica at a time does the passes; a lease that runs out passes to the other; a replica that stops lets go.
func TestLease(t *testing.T) {
	r := newRig(t, nil)
	ctx := context.Background()
	a, b := r.worker("a", nil), r.worker("b", nil)
	if ok, _ := a.Lease(ctx, "repair"); !ok {
		t.Fatal("a")
	}
	if ok, _ := a.Lease(ctx, "repair"); !ok {
		t.Fatal("a renews")
	}
	if ok, _ := b.Lease(ctx, "repair"); ok {
		t.Fatal("b took what a holds")
	}
	a.Release(ctx, "repair")
	if ok, _ := b.Lease(ctx, "repair"); !ok {
		t.Fatal("b after the release")
	}
	if _, err := r.dbc.Exec(ctx, false, rq.S("UPDATE storage_lease SET until = 1")); err != nil {
		t.Fatal(err)
	}
	if ok, _ := a.Lease(ctx, "repair"); !ok {
		t.Fatal("a after the lease of b ran out")
	}
}

// A check from outside: a directory of blocks that the database does not know (a node restored from an old backup) is reported, left alone when it is new or when the call is a dry run, and removed
// when it is old and `apply` is given. A file that is in no directory of blocks is reported and never removed, and the objects themselves are never touched.
func TestReconcileFindsStrays(t *testing.T) {
	r := newRig(t, nil)
	r.mirror(0)
	body := pattern(2000)
	r.put("acme/keep", body)
	old := time.Now().Add(-72 * time.Hour)
	plant := func(node int, rel string, mtime time.Time) {
		p := filepath.Join(r.nodes[node], rel)
		_ = os.MkdirAll(filepath.Dir(p), 0o755)
		_ = os.WriteFile(p, []byte("stray"), 0o644)
		_ = os.Chtimes(p, mtime, mtime)
	}
	plant(0, "001/ghost/x~0123456789abcdef/0", old)        // an old stray
	plant(1, "001/fresh/y~fedcba9876543210/0", time.Now()) // a new one: a writer may be at it
	plant(1, "001/loose-file.txt", old)                    // in no directory of blocks
	call := func(q string) map[string]any {
		code, data := r.admin("POST", "/v1/admin/stores/main:reconcile"+q, nil)
		if code != 200 {
			t.Fatalf("reconcile %s: %d %s", q, code, data)
		}
		var v map[string]any
		_ = json.Unmarshal(data, &v)
		return v
	}
	rep := call("")
	if len(rep["strays"].([]any)) != 2 || rep["unknown_files"].(float64) != 1 || rep["queued"].(float64) != 0 {
		t.Fatalf("dry run: %v", rep)
	}
	if _, err := os.Stat(filepath.Join(r.nodes[0], "001/ghost/x~0123456789abcdef/0")); err != nil {
		t.Fatal("a dry run removed something")
	}
	rep = call("?apply")
	if rep["queued"].(float64) != 1 {
		t.Fatalf("apply queued %v, want 1 (the old one; the new one is left)", rep["queued"])
	}
	w := r.worker("w", nil)
	w.Lease(context.Background(), "repair")
	w.Pass(context.Background())
	if _, err := os.Stat(filepath.Join(r.nodes[0], "001/ghost/x~0123456789abcdef/0")); err == nil {
		t.Fatal("the old stray was not removed")
	}
	for _, keep := range []string{"001/fresh/y~fedcba9876543210/0", "001/loose-file.txt"} {
		if _, err := os.Stat(filepath.Join(r.nodes[1], keep)); err != nil {
			t.Fatalf("%s was removed", keep)
		}
	}
	if code, d := r.get("acme/keep", 0, 1999); code != 206 || string(d) != string(body) {
		t.Fatal("the object was touched")
	}
	// with a min_age of 0 the new one goes too
	rep = call("?apply&min_age=0")
	w.Pass(context.Background())
	if _, err := os.Stat(filepath.Join(r.nodes[1], "001/fresh/y~fedcba9876543210/0")); err == nil {
		t.Fatalf("min_age=0 left it: %v", rep)
	}
}

// A store written with a newer layout is refused with words, not read with the old rule.
func TestNewerLayoutIsRefused(t *testing.T) {
	r := newRig(t, nil)
	r.mirror(0)
	if code, _ := r.put("k/a", []byte("a")); code != 200 {
		t.Fatal("put")
	}
	if _, err := r.dbc.Exec(context.Background(), false, rq.S(`UPDATE storage_stores SET config = json_set(config, '$.layout', 99), updated_at = updated_at + 1`)); err != nil {
		t.Fatal(err)
	}
	code, data := r.put("k/b", []byte("b"))
	if code != 500 || !strings.Contains(string(data), "newer_layout") {
		t.Fatalf("%d %s", code, data)
	}
}
