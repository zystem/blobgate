package api

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zystem/blobgate/internal/maint"
	"github.com/zystem/blobgate/internal/rq"
)

func (r *rig) dedupStore(mode string, soft int, key string) {
	r.t.Helper()
	body := map[string]any{"kind": "mirror", "default": true, "dedup": mode, "soft_delete_seconds": soft,
		"nodes": []any{map[string]any{"remote": ":local:" + r.nodes[0]}, map[string]any{"remote": ":local:" + r.nodes[1]}}}
	if key != "" {
		body["encryption"] = map[string]any{"key_id": 1, "keys": map[string]string{"1": key}}
	}
	if code, d := r.admin("PUT", "/v1/admin/stores/main", body); code != 200 {
		r.t.Fatalf("register: %d %s", code, d)
	}
}

// sweepSets is what the worker does at its next pass.
func (r *rig) sweepSets() int {
	w := r.worker("sets", nil)
	w.Lease(context.Background(), "repair")
	return w.SetPass(context.Background())
}

// refsAreTheRows is the rule of the table: for every set, `refs` is the number of objects that point to it, and no set is left that nothing points to once the worker has been.
func (r *rig) refsAreTheRows() {
	r.t.Helper()
	rows, err := r.dbc.Query(context.Background(), rq.S("SELECT s.cid, s.refs, (SELECT count(*) FROM storage_objects o WHERE o.store = s.store AND o.cid = s.cid) FROM storage_sets s"))
	if err != nil {
		r.t.Fatal(err)
	}
	for _, x := range rows {
		if rq.Int(x[1]) != rq.Int(x[2]) {
			r.t.Fatalf("the set %s has refs %d and %d objects point to it", rq.Str(x[0])[:8], rq.Int(x[1]), rq.Int(x[2]))
		}
	}
	if n := r.count("storage_objects WHERE cid != '' AND cid NOT IN (SELECT cid FROM storage_sets)"); n != 0 {
		r.t.Fatalf("%d objects point to a set that is not there", n)
	}
}

func TestSameBytesAreKeptOnce(t *testing.T) {
	r := newRig(t, nil)
	r.dedupStore("tenant", 0, "")
	body := pattern(9<<20 + 123) // two blocks
	for _, k := range []string{"img/a", "img/b", "other/c"} {
		if code, d := r.put(k, body); code != 200 {
			t.Fatalf("%s: %d %s", k, code, d)
		}
	}
	if f := files(r.nodes[0]); len(f) != 2 || len(files(r.nodes[1])) != 2 {
		t.Fatalf("three objects of the same bytes hold %v on the first node, want one set of two blocks", f)
	}
	for _, k := range []string{"img/a", "img/b", "other/c"} {
		if code, d := r.get(k, 0, len(body)-1); code != 206 || !bytes.Equal(d, body) {
			t.Fatalf("read %s: %d", k, code)
		}
	}
	_, _, ha := r.plainDo("HEAD", "/v1/stores/main/objects/img/a", nil, nil)
	_, _, hb := r.plainDo("HEAD", "/v1/stores/main/objects/img/b", nil, nil)
	if ha.Get("ETag") == "" || ha.Get("ETag") != hb.Get("ETag") {
		t.Fatalf("the same bytes have two versions: %s %s", ha.Get("ETag"), hb.Get("ETag"))
	}
	if r.count("storage_sets") != 1 || r.count("storage_objects") != 3 {
		t.Fatal("rows")
	}
	r.refsAreTheRows()
	// the usage of the tenant is what it holds, not what the store keeps
	_, d, _ := r.core("GET", "/v1/stores/main/usage", "", nil, nil)
	if !strings.Contains(string(d), fmt.Sprintf(`"bytes":%d`, 3*len(body))) {
		t.Fatalf("usage: %s", d)
	}
	// the last reference gone: the blocks go with the next pass of the worker, not before
	for _, k := range []string{"img/a", "img/b"} {
		r.core("DELETE", "/v1/stores/main/objects/"+k, "", nil, nil)
	}
	if r.sweepSets() != 0 || len(files(r.nodes[0])) != 2 {
		t.Fatal("blocks that an object still points to were removed")
	}
	if code, d := r.get("other/c", 0, 9); code != 206 || !bytes.Equal(d, body[:10]) {
		t.Fatal("the last object cannot be read")
	}
	r.core("DELETE", "/v1/stores/main/objects/other/c", "", nil, nil)
	if len(files(r.nodes[0])) != 2 {
		t.Fatal("the blocks go before the worker has been")
	}
	if n := r.sweepSets(); n != 1 {
		t.Fatalf("the worker removed %d sets", n)
	}
	if len(files(r.nodes[0])) != 0 || len(files(r.nodes[1])) != 0 || r.count("storage_sets") != 0 || r.count("storage_repair") != 0 {
		t.Fatalf("left: %v %d %d", files(r.nodes[0]), r.count("storage_sets"), r.count("storage_repair"))
	}
}

func TestReplaceAndRewrite(t *testing.T) {
	r := newRig(t, nil)
	r.dedupStore("tenant", 0, "")
	a, b := pattern(3000), pattern(3001)
	r.put("k/1", a)
	r.put("k/2", a)
	// writing the same bytes to the same key again changes nothing that is kept
	r.put("k/1", a)
	r.refsAreTheRows()
	if len(files(r.nodes[0])) != 1 {
		t.Fatalf("%v", files(r.nodes[0]))
	}
	// replacing one of two: the other still reads; replacing the other: the old set goes
	r.put("k/1", b)
	if code, d := r.get("k/2", 0, 2999); code != 206 || !bytes.Equal(d, a) {
		t.Fatal("the other object lost its bytes")
	}
	if r.sweepSets() != 0 {
		t.Fatal("a set that is still pointed to was removed")
	}
	r.put("k/2", b)
	r.refsAreTheRows()
	if r.sweepSets() != 1 || len(files(r.nodes[0])) != 1 {
		t.Fatalf("after the replace: %v", files(r.nodes[0]))
	}
	if code, d := r.get("k/2", 0, 3000); code != 206 || !bytes.Equal(d, b) {
		t.Fatal("read")
	}
	// the object of a store with no deduplication that is written over by one with it, and the other way
	r.dedupStore("", 0, "")
	r.put("k/2", a)
	r.refsAreTheRows()
	r.sweepSets()
	if code, d := r.get("k/1", 0, 3000); code != 206 || !bytes.Equal(d, b) {
		t.Fatal("an object written with deduplication cannot be read after it is switched off")
	}
	if code, d := r.get("k/2", 0, 2999); code != 206 || !bytes.Equal(d, a) {
		t.Fatal("read of the plain object")
	}
}

func TestScopeOfASet(t *testing.T) {
	for _, mode := range []string{"tenant", "store"} {
		r := newRig(t, func(c *Config) { c.ShardSecrets["other"] = "the-secret-of-the-other-tenant" })
		r.dedupStore(mode, 0, "")
		body := pattern(5000)
		r.put("k", body)
		hh := map[string]string{"Authorization": "Bearer " + authTok("the-secret-of-the-other-tenant", "other", "PUT", "/v1/stores/main/objects/k"), "X-Blobgate-Tenant": "other", "X-Content-SHA256": sha(body)}
		if code, _, _ := r.do("PUT", "/v1/stores/main/objects/k", hh, body); code != 200 {
			t.Fatal("the other tenant")
		}
		want := map[string]int{"tenant": 2, "store": 1}[mode]
		if got := len(files(r.nodes[0])); got != want {
			t.Fatalf("scope %s: %d blocks, want %d", mode, got, want)
		}
		if r.count("storage_sets") != int64(want) {
			t.Fatal("sets")
		}
		r.refsAreTheRows()
	}
}

func TestSealedBlocksAreKeptOnce(t *testing.T) {
	r := newRig(t, func(c *Config) { c.ShardSecrets["other"] = "the-secret-of-the-other-tenant" })
	r.dedupStore("tenant", 0, "a-key-that-is-long-enough-1")
	body := bytes.Repeat([]byte("secret layer "), 900)
	r.put("l/1", body)
	r.put("l/2", body)
	if len(files(r.nodes[0])) != 1 {
		t.Fatalf("sealed: %v", files(r.nodes[0]))
	}
	f := files(r.nodes[0])[0]
	raw, _ := os.ReadFile(filepath.Join(r.nodes[0], f))
	if bytes.Contains(raw, []byte("secret layer")) || len(raw) != len(body)+33 {
		t.Fatalf("the block on the node is not sealed (%d bytes)", len(raw))
	}
	if code, d := r.get("l/2", 0, len(body)-1); code != 206 || !bytes.Equal(d, body) {
		t.Fatal("read of a sealed set")
	}
	if code, d := r.get("l/2", 100, 199); code != 206 || !bytes.Equal(d, body[100:200]) {
		t.Fatal("range of a sealed set")
	}
	// another tenant's copy is sealed to other bytes (the key comes from the tenant as well), so nothing is shared and nothing can be compared
	hh := map[string]string{"Authorization": "Bearer " + authTok("the-secret-of-the-other-tenant", "other", "PUT", "/v1/stores/main/objects/l/1"), "X-Blobgate-Tenant": "other", "X-Content-SHA256": sha(body)}
	if code, _, _ := r.do("PUT", "/v1/stores/main/objects/l/1", hh, body); code != 200 {
		t.Fatal("other")
	}
	if len(files(r.nodes[0])) != 2 {
		t.Fatalf("two tenants: %v", files(r.nodes[0]))
	}
	// the same bytes, written before the store sealed anything or sealed the old way, are not mixed with these
	r.refsAreTheRows()
	// a node that lost its disk: the check finds the block of the set, the repair writes it, and a read from that node alone works
	w := r.worker("w1", nil)
	w.Lease(context.Background(), "repair")
	_ = os.RemoveAll(r.nodes[1])
	_ = os.Mkdir(r.nodes[1], 0o755)
	if made := w.CheckPass(context.Background()); made != 2 {
		t.Fatalf("the check made %d tasks for two sets", made)
	}
	w.Pass(context.Background())
	if len(files(r.nodes[1])) != 2 {
		t.Fatalf("not repaired: %v", files(r.nodes[1]))
	}
	_ = os.RemoveAll(r.nodes[0])
	_ = os.Mkdir(r.nodes[0], 0o755)
	if code, d := r.get("l/1", 0, len(body)-1); code != 206 || !bytes.Equal(d, body) {
		t.Fatal("read from the repaired node")
	}
}

func TestMultipartTakesTheSet(t *testing.T) {
	r := newRig(t, nil)
	r.dedupStore("store", 0, "")
	p1, p2 := pattern(BlockSize), pattern(700)
	if code, d := r.put("one", append(append([]byte{}, p1...), p2...)); code != 200 {
		t.Fatalf("%d %s", code, d)
	}
	_, bd, _ := r.core("POST", "/v1/stores/main/objects/two", "uploads", nil, nil)
	id := between(string(bd), `"upload_id":"`, `"`)
	var tags []string
	for i, p := range [][]byte{p1, p2} {
		_, pd, _ := r.core("PUT", "/v1/stores/main/objects/two", fmt.Sprintf("upload=%s&part=%d", id, i+1), map[string]string{"X-Content-SHA256": sha(p)}, p)
		tags = append(tags, fmt.Sprintf(`[%d,"%s"]`, i+1, between(string(pd), `"etag":"`, `"`)))
	}
	if code, d, _ := r.core("POST", "/v1/stores/main/objects/two", "upload="+id, nil, []byte(`{"parts":[`+strings.Join(tags, ",")+`]}`)); code != 200 {
		t.Fatalf("complete: %d %s", code, d)
	}
	if len(files(r.nodes[0])) != 2 || r.count("storage_sets") != 1 {
		t.Fatalf("a multipart upload of the same bytes as a plain one: %v, %d sets", files(r.nodes[0]), r.count("storage_sets"))
	}
	if code, d := r.get("two", 0, BlockSize+699); code != 206 || !bytes.Equal(d[BlockSize:], p2) {
		t.Fatal("read")
	}
	r.refsAreTheRows()
}

func TestTrashHoldsTheReference(t *testing.T) {
	r := newRig(t, nil)
	r.dedupStore("tenant", 60, "")
	body := pattern(2000)
	r.put("a", body)
	r.put("b", body)
	r.core("DELETE", "/v1/stores/main/objects/a", "", nil, nil)
	r.core("DELETE", "/v1/stores/main/objects/b", "", nil, nil)
	if r.sweepSets() != 0 || len(files(r.nodes[0])) != 1 {
		t.Fatal("blocks of objects that are in the trash were removed")
	}
	r.core("POST", "/v1/stores/main/objects/a", "restore", nil, nil)
	if code, d := r.get("a", 0, 1999); code != 206 || !bytes.Equal(d, body) {
		t.Fatal("restore")
	}
	later := r.worker("w", func(c *maint.Config) { c.Now = func() time.Time { return time.Now().Add(5 * time.Minute) } })
	later.Lease(context.Background(), "repair")
	later.Pass(context.Background()) // b's time is over; a is not in the trash
	r.refsAreTheRows()
	if r.count("storage_objects") != 1 || len(files(r.nodes[0])) != 1 {
		t.Fatalf("%d objects, %v", r.count("storage_objects"), files(r.nodes[0]))
	}
	r.core("DELETE", "/v1/stores/main/objects/a", "purge", nil, nil)
	later.Pass(context.Background())
	if len(files(r.nodes[0])) != 0 || r.count("storage_sets") != 0 {
		t.Fatalf("%v %d", files(r.nodes[0]), r.count("storage_sets"))
	}
}

// A process that dies after the transaction of a write that found its bytes in the store leaves the blocks it had just written written down for removal; one that dies before it leaves the old state.
func TestDedupAndCrash(t *testing.T) {
	c := newCrashRig(t)
	c.dedupStore("tenant", 0, "")
	body := pattern(4000)
	c.put("a", body)
	for _, point := range []string{"commit:before", "commit:after-tx"} {
		c.die(point, func() { c.put("b-"+point, body) })
	}
	w := c.worker("after", func(cfg *maint.Config) { cfg.Now = func() time.Time { return time.Now().Add(48 * time.Hour) } })
	w.Lease(context.Background(), "repair")
	w.Pass(context.Background())
	w.Pass(context.Background())
	c.refsAreTheRows()
	if n := len(files(c.nodes[0])); n != 1 {
		t.Fatalf("after the crashes %d blocks are kept: %v", n, files(c.nodes[0]))
	}
	if c.count("storage_repair") != 0 || c.count("storage_uploads") != 0 {
		t.Fatal("what is left in the queue")
	}
	if code, d := c.get("b-commit:after-tx", 0, 3999); code != 206 || !bytes.Equal(d, body) {
		t.Fatal("the object whose writer died after the transaction")
	}
	if code, _ := c.get("b-commit:before", 0, 1); code != 404 {
		t.Fatal("the object whose writer died before the transaction exists")
	}
}

// Writers and deleters of the same bytes at the same moment: the count of references is the number of rows all the time, and when everything is deleted and the worker has been, nothing is left.
func TestManyWritersOfTheSameBytes(t *testing.T) {
	r := newRig(t, nil)
	r.dedupStore("tenant", 0, "")
	body := pattern(6000)
	var wg sync.WaitGroup
	for i := 0; i < 24; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			k := fmt.Sprintf("k/%d", i%6)
			if code, d := r.put(k, body); code != 200 {
				t.Errorf("put %s: %d %s", k, code, d)
			}
			if i%3 == 0 {
				r.core("DELETE", "/v1/stores/main/objects/"+k, "", nil, nil)
			}
			if i%4 == 0 {
				r.sweepSets()
			}
		}(i)
	}
	wg.Wait()
	r.refsAreTheRows()
	for i := 0; i < 6; i++ {
		k := fmt.Sprintf("k/%d", i)
		if code, _, _ := r.plainDo("HEAD", "/v1/stores/main/objects/"+k, nil, nil); code == 200 {
			if code, d := r.get(k, 0, 5999); code != 206 || !bytes.Equal(d, body) {
				t.Fatalf("%s cannot be read after the race: %d", k, code)
			}
		}
	}
	for i := 0; i < 6; i++ {
		r.core("DELETE", "/v1/stores/main/objects/"+fmt.Sprintf("k/%d", i), "", nil, nil)
	}
	r.sweepSets()
	r.sweepSets()
	if len(files(r.nodes[0])) != 0 || len(files(r.nodes[1])) != 0 || r.count("storage_sets") != 0 || r.count("storage_repair") != 0 {
		t.Fatalf("left: %v %d %d", files(r.nodes[0]), r.count("storage_sets"), r.count("storage_repair"))
	}
}

func TestDedupSettingIsChecked(t *testing.T) {
	r := newRig(t, nil)
	if code, _ := r.admin("PUT", "/v1/admin/stores/main", map[string]any{"kind": "mirror", "dedup": "everyone",
		"nodes": []any{map[string]any{"remote": ":local:" + r.nodes[0]}}}); code != 400 {
		t.Fatal("a value that is not one of the three")
	}
	r.dedupStore("store", 0, "")
	_, d := r.admin("GET", "/v1/admin/stores", nil)
	if !strings.Contains(string(d), `"dedup":"store"`) {
		t.Fatalf("the list of stores does not say: %s", d)
	}
}
