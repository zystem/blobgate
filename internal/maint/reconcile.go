package maint

import (
	"context"
	"path"
	"strings"
	"time"

	"github.com/rclone/rclone/fs"

	"github.com/zystem/blobgate/internal/layout"
	"github.com/zystem/blobgate/internal/rq"
)

// Stray is a directory of blocks on a node that no object and no upload of the database refers to.
type Stray struct {
	Node   string    `json:"node"`
	Dir    string    `json:"dir"`
	Files  int       `json:"files"`
	Bytes  int64     `json:"bytes"`
	Newest time.Time `json:"newest"`
	Recent bool      `json:"recent"` // newer than min_age: left alone (a writer may be at it)
}

// Report is what Reconcile found.
type Report struct {
	Store    string  `json:"store"`
	Nodes    int     `json:"nodes"`
	Files    int     `json:"files"`    // files seen on the nodes
	Expected int     `json:"expected"` // directories of blocks the database knows
	Strays   []Stray `json:"strays"`
	Unknown  int     `json:"unknown_files"` // files that are in no directory of blocks: reported, never removed
	Queued   int     `json:"queued"`        // removals written down (apply)
}

type dirInfo struct {
	files  int
	bytes  int64
	newest time.Time
}

func walk(ctx context.Context, f fs.Fs, dir string, fn func(o fs.Object)) error {
	entries, err := f.List(ctx, dir)
	if err == fs.ErrorDirNotFound {
		return nil
	}
	if err != nil {
		return err
	}
	for _, e := range entries {
		switch x := e.(type) {
		case fs.Object:
			fn(x)
		case fs.Directory:
			if err := walk(ctx, f, x.Remote(), fn); err != nil {
				return err
			}
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
	}
	return nil
}

// isBlockDir says whether a directory is named like the directory of the blocks of a generation: `<escaped segment>~<hex generation>`.
func isBlockDir(d string) bool {
	b := path.Base(d)
	i := strings.LastIndexByte(b, '~')
	if i < 1 || len(b)-i-1 < 8 {
		return false
	}
	for _, c := range b[i+1:] {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

// Reconcile lists every node of the store and compares what is there with what the database knows, the way Nexus has a "reconcile" task for the blob store that fell out of step with its records.
// The passes of the module never list a node; this is the check from outside, for when something went wrong that the module does not know of (a node restored from an old backup, a database
// restored from one). It removes nothing unless `apply`; with it, the directories of blocks that no object and no upload refers to, and that are older than `minAge`, are written down for removal
// (the repair pass removes them). It never touches a file that is not in a directory of blocks.
func (w *Worker) Reconcile(ctx context.Context, name string, apply bool, minAge time.Duration) (Report, error) {
	m, err := w.Reg.Load(ctx, name)
	if err != nil {
		return Report{}, err
	}
	rep := Report{Store: m.Name, Nodes: len(m.Nodes), Strays: []Stray{}}
	// the nodes are listed first and the database read after: a writer puts its upload row in the database before its first block on a node, so a block seen in the listing has its row
	listed := make([]map[string]*dirInfo, len(m.Nodes))
	for i, n := range m.Nodes {
		listed[i] = map[string]*dirInfo{}
		err := walk(ctx, n.F, "", func(o fs.Object) {
			rep.Files++
			dir := path.Dir(o.Remote())
			if strings.HasPrefix(o.Remote(), "_check/") || strings.HasPrefix(o.Remote(), "_validate/") {
				return
			}
			if !isBlockDir(dir) {
				rep.Unknown++
				return
			}
			d := listed[i][dir]
			if d == nil {
				d = &dirInfo{}
				listed[i][dir] = d
			}
			d.files++
			d.bytes += o.Size()
			if t := o.ModTime(ctx); t.After(d.newest) {
				d.newest = t
			}
		})
		if err != nil {
			return rep, err
		}
	}
	known := map[string]bool{}
	cursor := ""
	for {
		rows, err := w.DB.Query(ctx, rq.S("SELECT key, generation FROM storage_objects WHERE store = ? AND key > ? ORDER BY key LIMIT 5000", m.Name, cursor))
		if err != nil {
			return rep, err
		}
		if len(rows) == 0 {
			break
		}
		for _, r := range rows {
			if g := rq.Str(r[1]); g != "" {
				known[layout.BlockDir(rq.Str(r[0]), g)] = true
			}
		}
		cursor = rq.Str(rows[len(rows)-1][0])
	}
	ups, err := w.DB.Query(ctx, rq.S("SELECT key, upload FROM storage_uploads WHERE store = ?", m.Name))
	if err != nil {
		return rep, err
	}
	for _, r := range ups {
		known[layout.BlockDir(rq.Str(r[0]), rq.Str(r[1]))] = true
	}
	rep.Expected = len(known)
	for i, n := range m.Nodes {
		for dir, d := range listed[i] {
			if known[dir] {
				continue
			}
			s := Stray{Node: n.ID, Dir: dir, Files: d.files, Bytes: d.bytes, Newest: d.newest, Recent: w.cfg.Now().Sub(d.newest) < minAge}
			rep.Strays = append(rep.Strays, s)
			if apply && !s.Recent {
				if _, err := w.DB.Exec(ctx, false, rq.S("INSERT INTO storage_repair (store, key, node, op, since, attempts, next_at, last_error) VALUES (?, ?, ?, 'purgepath', ?, 0, 0, '') "+
					"ON CONFLICT(store, key, node) DO UPDATE SET op = 'purgepath', since = excluded.since, attempts = 0, next_at = 0, last_error = ''", m.Name, dir, n.ID, w.now())); err == nil {
					rep.Queued++
				}
			}
		}
	}
	return rep, nil
}
