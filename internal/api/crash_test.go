package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/zystem/blobgate/internal/layout"
	"github.com/zystem/blobgate/internal/maint"
	"github.com/zystem/blobgate/internal/rq"
)

// crashRig is a rig whose server stops a request at a named point, as a process that is killed there would: the answer is never sent and nothing after the point runs.
type crashRig struct {
	*rig
	mu    sync.Mutex
	point string
}

func newCrashRig(t *testing.T) *crashRig {
	c := &crashRig{}
	c.rig = newRig(t, func(cfg *Config) {
		cfg.CrashAt = func(p string) {
			c.mu.Lock()
			hit := c.point == p
			c.mu.Unlock()
			if hit {
				panic(http.ErrAbortHandler)
			}
		}
	})
	c.mirror(0)
	return c
}

// die makes the next request that reaches the point end there; the call is then expected to get no answer.
func (c *crashRig) die(point string, call func()) {
	c.mu.Lock()
	c.point = point
	c.mu.Unlock()
	c.tolerate = true
	call()
	c.tolerate = false
	c.mu.Lock()
	c.point = ""
	c.mu.Unlock()
}

// settle is what a replica that comes up after the crash does: the repair pass and the sweep, twice (the sweep writes tasks that the second pass does). Then the nodes hold exactly the blocks of
// the objects the database knows, and nothing else is left in the queue, the uploads or the parts.
func (c *crashRig) settle() {
	c.t.Helper()
	w := c.worker("after-the-crash", func(cfg *maint.Config) { cfg.Now = func() time.Time { return time.Now().Add(48 * time.Hour) } })
	if ok, _ := w.Lease(context.Background(), "repair"); !ok {
		c.t.Fatal("lease")
	}
	w.Pass(context.Background())
	w.Pass(context.Background())
	rows, err := c.dbc.Query(context.Background(), rq.S("SELECT key, generation, size, block_size FROM storage_objects"))
	if err != nil {
		c.t.Fatal(err)
	}
	var want []string
	for _, r := range rows {
		full, gen, size, bs := rq.Str(r[0]), rq.Str(r[1]), rq.Int(r[2]), rq.Int(r[3])
		for i := int64(0); i*bs < size; i++ {
			want = append(want, layout.BlockPath(full, gen, int(i)))
		}
	}
	sort.Strings(want)
	for i, n := range c.nodes {
		got := files(n)
		sort.Strings(got)
		if fmt.Sprint(got) != fmt.Sprint(want) {
			c.t.Fatalf("node %d holds\n  %v\nwant\n  %v", i+1, got, want)
		}
	}
	for _, tb := range []string{"storage_repair", "storage_uploads", "storage_parts"} {
		if n := c.count(tb); n != 0 {
			c.t.Fatalf("%s still has %d rows", tb, n)
		}
	}
}

func (c *crashRig) readAll(key string, n int) (int, []byte) { return c.get(key, 0, n-1) }

func (c *crashRig) multipart(key string, parts [][]byte, point string) (id string, etags []string) {
	_, d, _ := c.core("POST", "/v1/stores/main/objects/"+key, "uploads", nil, nil)
	var u struct {
		ID string `json:"upload_id"`
	}
	_ = json.Unmarshal(d, &u)
	for i, p := range parts {
		code, data, _ := c.core("PUT", "/v1/stores/main/objects/"+key, fmt.Sprintf("upload=%s&part=%d", u.ID, i+1), map[string]string{"X-Content-SHA256": sha(p)}, p)
		var e struct{ Etag string }
		if code != 200 || json.Unmarshal(data, &e) != nil {
			c.t.Fatalf("part %d: %d %s", i+1, code, data)
		}
		etags = append(etags, e.Etag)
	}
	return u.ID, etags
}

func completeBody(etags []string) []byte {
	var parts [][]any
	for i, e := range etags {
		parts = append(parts, []any{i + 1, e})
	}
	b, _ := json.Marshal(map[string]any{"parts": parts})
	return b
}

// DAT-004: a process that dies at any step of a write or a delete leaves the object as it was or as it will be (never half of each), and the blocks that nobody refers to are found and removed
// by the passes of the next replica: the nodes then hold exactly what the database knows. (Nexus and restic both lost track of blocks across a crash between the change of the record and the
// cleanup; the cleanup is written down in the same transaction as the change.)
func TestCrashAtEveryStep(t *testing.T) {
	v1, v2 := pattern(5000), pattern(7000)
	t.Run("replace, dies after the transaction: the new one stands, the old one is removed from the queue", func(t *testing.T) {
		c := newCrashRig(t)
		c.put("k/a", v1)
		c.die("commit:after-tx", func() { c.put("k/a", v2) })
		if code, d := c.readAll("k/a", 7000); code != 206 || string(d) != string(v2) {
			t.Fatalf("the new version is not the object: %d", code)
		}
		if c.count("storage_repair") == 0 {
			t.Fatal("the removal of the old generation was not written down")
		}
		c.settle()
	})
	t.Run("replace, dies after the block and before the transaction: the old one stands", func(t *testing.T) {
		c := newCrashRig(t)
		c.put("k/a", v1)
		c.die("put:after-block", func() { c.put("k/a", v2) })
		if code, d := c.readAll("k/a", 5000); code != 206 || string(d) != string(v1) {
			t.Fatalf("the old version is not the object: %d", code)
		}
		c.settle()
	})
	t.Run("new key, dies after the block: there is no object, and the block goes", func(t *testing.T) {
		c := newCrashRig(t)
		c.die("put:after-block", func() { c.put("k/new", v1) })
		if code, _ := c.readAll("k/new", 10); code != 404 {
			t.Fatalf("an object that was not fixed exists: %d", code)
		}
		c.settle()
	})
	t.Run("new key, dies before any block", func(t *testing.T) {
		c := newCrashRig(t)
		c.die("put:after-uploads-row", func() { c.put("k/new", v1) })
		c.settle()
	})
	t.Run("delete, dies after the transaction: the object is gone, its blocks are removed later", func(t *testing.T) {
		c := newCrashRig(t)
		c.put("k/a", v1)
		c.die("delete:after-tx", func() { c.core("DELETE", "/v1/stores/main/objects/k/a", "", nil, nil) })
		if code, _ := c.readAll("k/a", 10); code != 404 {
			t.Fatalf("a deleted object is read: %d", code)
		}
		if c.count("storage_repair") == 0 {
			t.Fatal("the removal of the blocks was not written down")
		}
		c.settle()
	})
	t.Run("a part, dies after the block and before its record: the upload is swept", func(t *testing.T) {
		c := newCrashRig(t)
		_, d, _ := c.core("POST", "/v1/stores/main/objects/k/mp", "uploads", nil, nil)
		var u struct {
			ID string `json:"upload_id"`
		}
		_ = json.Unmarshal(d, &u)
		p := pattern(900)
		c.die("part:after-block", func() {
			c.core("PUT", "/v1/stores/main/objects/k/mp", "upload="+u.ID+"&part=1", map[string]string{"X-Content-SHA256": sha(p)}, p)
		})
		c.settle()
	})
	parts := [][]byte{pattern(3000), pattern(3000), pattern(1000)}
	t.Run("complete, dies before the transaction: there is no object", func(t *testing.T) {
		c := newCrashRig(t)
		id, etags := c.multipart("k/mp", parts, "")
		c.die("complete:before-commit", func() { c.core("POST", "/v1/stores/main/objects/k/mp", "upload="+id, nil, completeBody(etags)) })
		if code, _ := c.readAll("k/mp", 10); code != 404 {
			t.Fatalf("an upload that was not completed is read: %d", code)
		}
		c.settle()
	})
	t.Run("complete, dies after the transaction: the object stands", func(t *testing.T) {
		c := newCrashRig(t)
		id, etags := c.multipart("k/mp", parts, "")
		c.die("commit:after-tx", func() { c.core("POST", "/v1/stores/main/objects/k/mp", "upload="+id, nil, completeBody(etags)) })
		if code, d := c.readAll("k/mp", 7000); code != 206 || len(d) != 7000 {
			t.Fatalf("a completed upload is not the object: %d %d", code, len(d))
		}
		c.settle()
	})
	t.Run("abort, dies after the removal of the blocks", func(t *testing.T) {
		c := newCrashRig(t)
		id, _ := c.multipart("k/mp", parts, "")
		c.die("abort:after-purge", func() { c.core("DELETE", "/v1/stores/main/objects/k/mp", "upload="+id, nil, nil) })
		c.settle()
	})
}

// Writers of one key at the same moment: each replaced generation is written down by the transaction that replaces it, so after the passes one generation is left, and it is one of the written
// versions as a whole.
func TestConcurrentReplacementsLeaveOneGeneration(t *testing.T) {
	c := newCrashRig(t)
	bodies := make([][]byte, 10)
	var wg sync.WaitGroup
	for i := range bodies {
		bodies[i] = pattern(1000 + 37*i)
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if code, d := c.put("k/race", bodies[i]); code != 200 {
				t.Errorf("put %d: %d %s", i, code, d)
			}
		}(i)
	}
	wg.Wait()
	_, data, _ := c.core("GET", "/v1/stores/main/objects/k/race", "stat", nil, nil)
	var st struct{ Size int }
	_ = json.Unmarshal(data, &st)
	code, got := c.readAll("k/race", st.Size)
	ok := false
	for _, b := range bodies {
		ok = ok || (len(b) == st.Size && string(b) == string(got))
	}
	if code != 206 || !ok {
		t.Fatalf("the object is not one of the written versions: %d", code)
	}
	c.settle()
}
