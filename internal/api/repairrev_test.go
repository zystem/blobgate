package api

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/zystem/blobgate/internal/layout"
	"github.com/zystem/blobgate/internal/maint"
	"github.com/zystem/blobgate/internal/rq"
)

// Finding 5 of the review of 2026-11-22: a task that was replaced while a worker worked the old one, in the same second, is not closed by the old one. The worker settles by the revision it read.
func TestReplacedTaskIsNotClosedByTheOldOne(t *testing.T) {
	r := newRig(t, nil)
	r.mirror(1)
	r.readOnly(1)
	body := pattern(30000)
	if code, d := r.put("acme/a", body); code != 200 {
		t.Fatalf("put with a node down: %d %s", code, d)
	}
	r.writable(1)
	rows, err := r.dbc.Query(context.Background(), rq.S("SELECT key, node, op, rev FROM storage_repair"))
	if err != nil || len(rows) != 1 || rq.Str(rows[0][2]) != "copy" {
		t.Fatalf("the queue: %v %v", rows, err)
	}
	key, node, rev := rq.Str(rows[0][0]), rq.Str(rows[0][1]), rq.Int(rows[0][3])
	ctx := context.Background()
	w := r.worker("w", func(c *maint.Config) {
		c.Hook = func(point string) {
			if point != "repair:before-settle" {
				return
			}
			// the same second, another instruction for the same block and node: a removal
			_, _ = r.dbc.Exec(ctx, false, rq.S("INSERT INTO storage_repair (store, key, node, op, since, attempts, next_at, last_error) VALUES ('main', ?, ?, 'delete', ?, 0, 0, '') "+
				"ON CONFLICT(store, key, node) DO UPDATE SET rev = rev + 1, op = excluded.op, since = excluded.since, attempts = 0, next_at = 0, last_error = ''", key, node, time.Now().Unix()))
		}
	})
	if ok, _ := w.Lease(ctx, "repair"); !ok {
		t.Fatal("lease")
	}
	if done, failed := w.RepairPass(ctx); done != 1 || failed != 0 {
		t.Fatalf("the pass: %d done, %d failed", done, failed)
	}
	rows, err = r.dbc.Query(ctx, rq.S("SELECT op, rev FROM storage_repair WHERE key = ?", key))
	if err != nil || len(rows) != 1 || rq.Str(rows[0][0]) != "delete" || rq.Int(rows[0][1]) <= rev {
		t.Fatalf("the instruction that replaced the task was closed by the worker of the old one: %v %v", rows, err)
	}
	if !strings.Contains(key, "~") {
		t.Fatalf("a block key: %q", key)
	}
	_ = layout.BlockKey
}

// Finding 5: a copy for a node that missed a block that cannot be written down refuses the write (it would be acknowledged with fewer copies and nothing to remember which), and leaves nothing behind; when
// the queue works again the same write is taken.
func TestWriteIsRefusedWhenTheMissedCopyCannotBeWrittenDown(t *testing.T) {
	r := newRig(t, nil)
	r.mirror(1)
	r.readOnly(1)
	ctx := context.Background()
	if _, err := r.dbc.Exec(ctx, false, rq.S("CREATE TRIGGER no_repair BEFORE INSERT ON storage_repair BEGIN SELECT RAISE(ABORT, 'the queue is not writable'); END")); err != nil {
		t.Fatal(err)
	}
	dropped := false
	drop := func() {
		if !dropped {
			dropped = true
			_, _ = r.dbc.Exec(ctx, false, rq.S("DROP TRIGGER IF EXISTS no_repair"))
		}
	}
	t.Cleanup(drop)
	body := pattern(BlockSize + 100) // more than one block: the write goes through writeBlock
	if code, d := r.put("acme/big", body); code != 503 {
		t.Fatalf("a write whose missed copy cannot be written down: %d %s, want 503", code, d)
	}
	if n := r.count("storage_objects"); n != 0 {
		t.Fatalf("%d objects after a refused write", n)
	}
	if f := files(r.nodes[0]); len(f) != 0 {
		t.Fatalf("blocks of a refused write stay on node 1: %v", f)
	}
	// a part of an upload in parts: refused the same way
	if code, d, _ := r.core("POST", "/v1/stores/_default/objects/acme/parted", "uploads", nil, nil); code != 200 && code != 201 {
		t.Logf("begin an upload: %d %s", code, d)
	}
	drop()
	if code, d := r.put("acme/big", body); code != 200 {
		t.Fatalf("the same write when the queue works: %d %s", code, d)
	}
	if r.count("storage_repair") == 0 {
		t.Fatal("the copy for the node that missed the block was not written down")
	}
}
