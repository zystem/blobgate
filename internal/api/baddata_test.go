package api

import (
	"bytes"
	"context"
	"fmt"
	"github.com/zystem/blobgate/internal/maint"
	"os"
	"path/filepath"
	"testing"
)

// damage is a way a node can give back a block that is not what was written: the same size with a flipped byte, with zeros, cut short, longer, empty.
var damage = map[string]func([]byte) []byte{
	"flipped byte": func(b []byte) []byte { c := bytes.Clone(b); c[len(c)/2] ^= 0xff; return c },
	"zeros":        func(b []byte) []byte { return make([]byte, len(b)) },
	"cut short":    func(b []byte) []byte { return bytes.Clone(b[:len(b)-1]) },
	"cut in half":  func(b []byte) []byte { return bytes.Clone(b[:len(b)/2]) },
	"longer":       func(b []byte) []byte { return append(bytes.Clone(b), 'x') },
	"empty":        func([]byte) []byte { return nil },
}

// blockFiles are the files of the blocks of the object on a node, the biggest first (the first block of an object of more than one).
func blockFiles(t *testing.T, dir string) []string {
	t.Helper()
	fs := files(dir)
	if len(fs) < 2 {
		t.Fatalf("%s holds %v", dir, fs)
	}
	return fs
}

// A node that gives back damaged bytes never makes a client read them: the read goes on to the other node, whatever the damage, in a plain and in a sealed store, whichever node is damaged, and the read
// itself queues the repair that puts a good copy back (a pass of the repair worker, no deep check). When both nodes are damaged the read is refused: wrong bytes are never an answer.
func TestDamagedBlockIsNeverRead(t *testing.T) {
	body := pattern(BlockSize + 5000) // two blocks
	for _, mode := range []string{"plain", "sealed"} {
		for name, hurt := range damage {
			for victim := 0; victim < 2; victim++ {
				t.Run(fmt.Sprintf("%s/%s/node %d", mode, name, victim+1), func(t *testing.T) {
					r := newRig(t, nil)
					if mode == "sealed" {
						r.mirrorSealed(0, 1, map[string]string{"1": key1})
					} else {
						r.mirror(0)
					}
					if code, d := r.put("k/big", body); code != 200 {
						t.Fatalf("put: %d %s", code, d)
					}
					fs := blockFiles(t, r.nodes[victim])
					good := map[string][]byte{}
					for _, f := range fs {
						good[f], _ = os.ReadFile(filepath.Join(r.nodes[victim], f))
					}
					// damage every block on the victim: a read must come from the other node, block by block
					for f, b := range good {
						if err := os.WriteFile(filepath.Join(r.nodes[victim], f), hurt(b), 0o644); err != nil {
							t.Fatal(err)
						}
					}
					for _, rg := range [][2]int{{0, len(body) - 1}, {BlockSize - 10, BlockSize + 10}, {0, 0}, {len(body) - 1, len(body) - 1}} {
						code, d := r.get("k/big", rg[0], rg[1])
						if (code != 200 && code != 206) || !bytes.Equal(d, body[rg[0]:rg[1]+1]) {
							t.Fatalf("read %v with node %d damaged: %d, %d bytes", rg, victim+1, code, len(d))
						}
					}
					// A read that walked round the damaged first node queued a repair for it, one task for each of its two blocks: no check was needed. A damaged second node is never met by a read (the first
					// node answers), so only the deep check (off by default) finds it.
					w := r.worker("w1", nil)
					w.Lease(context.Background(), "repair")
					if want := map[int]int64{0: 2, 1: 0}[victim]; r.count("storage_repair") != want {
						t.Fatalf("%d repair tasks after the reads, want %d", r.count("storage_repair"), want)
					}
					if victim == 1 {
						if w.CheckPass(context.Background()) != 0 && name != "cut short" && name != "cut in half" && name != "longer" && name != "empty" {
							t.Fatal("the check by size saw damage it cannot see")
						}
						deep := r.worker("w1", func(c *maint.Config) { c.Deep = true })
						deep.Lease(context.Background(), "repair")
						if deep.CheckPass(context.Background()) == 0 {
							t.Fatalf("the deep check saw nothing of %s", name)
						}
					}
					w.Pass(context.Background())
					for f, b := range good {
						got, _ := os.ReadFile(filepath.Join(r.nodes[victim], f))
						if !bytes.Equal(got, b) {
							t.Fatalf("%s was not repaired (%d bytes, was %d)", f, len(got), len(b))
						}
					}
					if r.count("storage_repair") != 0 {
						t.Fatalf("tasks left: %d", r.count("storage_repair"))
					}

					// both nodes damaged: refused, never the wrong bytes
					for n := 0; n < 2; n++ {
						for f, b := range good {
							_ = os.WriteFile(filepath.Join(r.nodes[n], f), hurt(b), 0o644)
						}
					}
					code, d := r.get("k/big", 0, len(body)-1)
					if code == 200 || code == 206 {
						t.Fatalf("the read of a block both nodes have damaged answered %d (%d bytes, equal: %v)", code, len(d), bytes.Equal(d, body))
					}
					if code < 500 {
						t.Fatalf("the refusal is %d, want a 5xx (the data is there, not readable)", code)
					}
				})
			}
		}
	}
}
