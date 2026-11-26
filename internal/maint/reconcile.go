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
	Store       string  `json:"store"`
	Nodes       int     `json:"nodes"`
	Files       int     `json:"files"`         // files seen on the nodes
	Expected    int     `json:"expected"`      // directories of blocks the database knows
	Strays      []Stray `json:"strays"`        // at most ReportStrays of them
	StraysTotal int     `json:"strays_total"`  // all that were found
	Unknown     int     `json:"unknown_files"` // files that are in no directory of blocks: reported, never removed
	Queued      int     `json:"queued"`        // removals written down (apply)
}

// ReportStrays is the most strays a report lists; the others are counted (and, with apply, removed like the listed ones).
var ReportStrays = 1000

// ReconcilePage is how many directories of blocks Reconcile asks the database about at once: what it keeps in memory is that many, and the entries of one directory of a node, whatever the size of the store.
var ReconcilePage = 500

type dirInfo struct {
	dir    string
	files  int
	bytes  int64
	newest time.Time
}

// walk calls fn for every directory under dir, with the files in it. It holds the listing of one directory at a time at each depth.
func walk(ctx context.Context, f fs.Fs, dir string, fn func(dir string, files []fs.Object) error) error {
	entries, err := f.List(ctx, dir)
	if err == fs.ErrorDirNotFound {
		return nil
	}
	if err != nil {
		return err
	}
	var files []fs.Object
	var subs []string
	for _, e := range entries {
		switch x := e.(type) {
		case fs.Object:
			files = append(files, x)
		case fs.Directory:
			subs = append(subs, x.Remote())
		}
	}
	if len(files) > 0 {
		if err := fn(dir, files); err != nil {
			return err
		}
	}
	for _, d := range subs {
		if err := walk(ctx, f, d, fn); err != nil {
			return err
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
	}
	return nil
}

// generationOf is the generation in the name of a directory of blocks (`<escaped segment>~<hex generation>`), or "" when the directory is not named so.
func generationOf(d string) string {
	b := path.Base(d)
	i := strings.LastIndexByte(b, '~')
	if i < 1 || len(b)-i-1 < 8 {
		return ""
	}
	for _, c := range b[i+1:] {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return ""
		}
	}
	return b[i+1:]
}

// knownDirs says which directories of blocks of these generations the database knows. It is one statement, so one view of the three tables: what an upload becomes at that moment is in it as the one or
// the other.
func (w *Worker) knownDirs(ctx context.Context, st string, generations []string) (map[string]bool, error) {
	in := strings.TrimSuffix(strings.Repeat("?,", len(generations)), ",")
	args := make([]any, 0, 3*(len(generations)+1))
	for range 3 {
		args = append(args, st)
		for _, g := range generations {
			args = append(args, g)
		}
	}
	// #nosec G202 -- only question marks are put into the text; the values are arguments
	rows, err := w.DB.Query(ctx, rq.S("SELECT key, upload FROM storage_uploads WHERE store = ? AND upload IN ("+in+") "+
		"UNION ALL SELECT owner, generation FROM storage_sets WHERE store = ? AND generation IN ("+in+") "+
		"UNION ALL SELECT key, generation FROM storage_objects WHERE store = ? AND generation IN ("+in+")", args...))
	if err != nil {
		return nil, err
	}
	known := make(map[string]bool, len(rows))
	for _, r := range rows {
		known[layout.BlockDir(rq.Str(r[0]), rq.Str(r[1]))] = true
	}
	return known, nil
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
	count, err := w.DB.Query(ctx, rq.S("SELECT count(*) FROM (SELECT key, generation FROM storage_objects WHERE store = ?1 AND generation != '' "+
		"UNION SELECT owner, generation FROM storage_sets WHERE store = ?1 UNION SELECT key, upload FROM storage_uploads WHERE store = ?1)", m.Name))
	if err != nil {
		return rep, err
	}
	if len(count) == 1 {
		rep.Expected = int(rq.Int(count[0][0]))
	}
	for _, n := range m.Nodes {
		var batch []dirInfo
		// a directory is listed on the node first and asked of the database after: a writer puts its upload row in the database before its first block on a node, so a block seen in the listing has its row
		check := func() error {
			if len(batch) == 0 {
				return nil
			}
			gens := make([]string, len(batch))
			for i, d := range batch {
				gens[i] = generationOf(d.dir)
			}
			known, err := w.knownDirs(ctx, m.Name, gens)
			if err != nil {
				return err
			}
			for _, d := range batch {
				if known[d.dir] {
					continue
				}
				s := Stray{Node: n.ID, Dir: d.dir, Files: d.files, Bytes: d.bytes, Newest: d.newest, Recent: w.cfg.Now().Sub(d.newest) < minAge}
				rep.StraysTotal++
				if len(rep.Strays) < ReportStrays {
					rep.Strays = append(rep.Strays, s)
				}
				if apply && !s.Recent {
					if _, err := w.DB.Exec(ctx, false, rq.S("INSERT INTO storage_repair (store, key, node, op, since, attempts, next_at, last_error) VALUES (?, ?, ?, 'purgepath', ?, 0, 0, '') "+
						"ON CONFLICT(store, key, node) DO UPDATE SET rev = rev + 1, op = 'purgepath', since = excluded.since, attempts = 0, next_at = 0, last_error = ''", m.Name, d.dir, n.ID, w.now())); err == nil {
						rep.Queued++
					}
				}
			}
			batch = batch[:0]
			return nil
		}
		err := walk(ctx, n.F, "", func(dir string, files []fs.Object) error {
			rep.Files += len(files)
			if dir == "_check" || dir == "_validate" || strings.HasPrefix(dir, "_check/") || strings.HasPrefix(dir, "_validate/") {
				return nil
			}
			if generationOf(dir) == "" {
				rep.Unknown += len(files)
				return nil
			}
			d := dirInfo{dir: dir, files: len(files)}
			for _, o := range files {
				d.bytes += o.Size()
				if t := o.ModTime(ctx); t.After(d.newest) {
					d.newest = t
				}
			}
			if batch = append(batch, d); len(batch) >= ReconcilePage {
				return check()
			}
			return nil
		})
		if err == nil {
			err = check()
		}
		if err != nil {
			return rep, err
		}
	}
	return rep, nil
}
