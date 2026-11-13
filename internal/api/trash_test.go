package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/zystem/blobgate/internal/maint"
)

func (r *rig) mirrorSoft(seconds int) {
	r.t.Helper()
	code, data := r.admin("PUT", "/v1/admin/stores/main", map[string]any{"kind": "mirror", "default": true, "soft_delete_seconds": seconds,
		"nodes": []any{map[string]any{"remote": ":local:" + r.nodes[0]}, map[string]any{"remote": ":local:" + r.nodes[1]}}})
	if code != 200 {
		r.t.Fatalf("register: %d %s", code, data)
	}
}

func listKeys(t *testing.T, d []byte) (keys, prefixes []string, next string) {
	t.Helper()
	var v struct {
		Objects  []struct{ Key string } `json:"objects"`
		Prefixes []string               `json:"prefixes"`
		Next     string                 `json:"next"`
	}
	if err := json.Unmarshal(d, &v); err != nil {
		t.Fatalf("%v: %s", err, d)
	}
	for _, o := range v.Objects {
		keys = append(keys, o.Key)
	}
	return keys, v.Prefixes, v.Next
}

// The list of a tenant's objects, by prefix, in the order of the keys, in pages, with folders; one tenant never sees the keys of another.
func TestListByPrefix(t *testing.T) {
	r := newRig(t, func(c *Config) { c.ShardSecrets["other"] = "the-secret-of-the-other-tenant" })
	r.mirror(0)
	for _, k := range []string{"a/1", "a/2", "b/x/1", "b/x/2", "b/y/1", "c", "a-b"} {
		if code, d := r.put(k, []byte("v-"+k)); code != 200 {
			t.Fatalf("%s: %d %s", k, code, d)
		}
	}
	// the other tenant has a key of the same name
	h := func(method, path string) map[string]string {
		return map[string]string{"Authorization": "Bearer " + authTok("the-secret-of-the-other-tenant", "other", method, path), "X-Blobgate-Tenant": "other"}
	}
	hh := h("PUT", "/v1/stores/main/objects/a/1")
	hh["X-Content-SHA256"] = sha([]byte("x"))
	if code, _, _ := r.do("PUT", "/v1/stores/main/objects/a/1", hh, []byte("x")); code != 200 {
		t.Fatal("the other tenant")
	}
	ls := func(q string) (keys, prefixes []string, next string) {
		code, d, _ := r.core("GET", "/v1/stores/main/objects", q, nil, nil)
		if code != 200 {
			t.Fatalf("list %q: %d %s", q, code, d)
		}
		return listKeys(t, d)
	}
	if k, _, n := ls(""); strings.Join(k, ",") != "a-b,a/1,a/2,b/x/1,b/x/2,b/y/1,c" || n != "" {
		t.Fatalf("all: %v %q", k, n)
	}
	if k, _, _ := ls("prefix=a/"); strings.Join(k, ",") != "a/1,a/2" {
		t.Fatalf("prefix a/: %v", k)
	}
	if k, _, _ := ls("prefix=b"); strings.Join(k, ",") != "b/x/1,b/x/2,b/y/1" {
		t.Fatalf("prefix b: %v", k)
	}
	// pages of two, to the end, with no repeat and no gap
	var all []string
	after := ""
	for i := 0; i < 10; i++ {
		q := "limit=2"
		if after != "" {
			q += "&after=" + after
		}
		k, _, n := ls(q)
		all = append(all, k...)
		if n == "" {
			break
		}
		after = n
	}
	if strings.Join(all, ",") != "a-b,a/1,a/2,b/x/1,b/x/2,b/y/1,c" {
		t.Fatalf("pages: %v", all)
	}
	// folders
	k, p, n := ls("delimiter=/")
	if strings.Join(k, ",") != "a-b,c" || strings.Join(p, ",") != "a/,b/" || n != "" {
		t.Fatalf("folders: %v %v %q", k, p, n)
	}
	if k, p, _ := ls("delimiter=/&prefix=b/"); len(k) != 0 || strings.Join(p, ",") != "b/x/,b/y/" {
		t.Fatalf("folders of b/: %v %v", k, p)
	}
	// folders in pages of two
	var seen []string
	after = ""
	for i := 0; i < 10; i++ {
		q := "delimiter=/&limit=2"
		if after != "" {
			q += "&after=" + after
		}
		k, p, n := ls(q)
		seen = append(append(seen, k...), p...)
		if n == "" {
			break
		}
		after = n
	}
	if strings.Join(seen, ",") != "a-b,a/,c,b/" {
		t.Fatalf("folders in pages: %v", seen)
	}
	// what the list says about an object is what HEAD says
	_, d, _ := r.core("GET", "/v1/stores/main/objects", "prefix=c", nil, nil)
	_, _, hd := r.plainDo("HEAD", "/v1/stores/main/objects/c", nil, nil)
	if !strings.Contains(string(d), hd.Get("ETag")[1:len(hd.Get("ETag"))-1]) || !strings.Contains(string(d), `"size":3`) {
		t.Fatalf("%s vs %v", d, hd)
	}
	for _, bad := range []string{"limit=0", "limit=1001", "limit=x", "delimiter=ab"} {
		if code, _, _ := r.core("GET", "/v1/stores/main/objects", bad, nil, nil); code != 400 {
			t.Errorf("%s: %d", bad, code)
		}
	}
	if code, _, _ := r.core("POST", "/v1/stores/main/objects", "", nil, nil); code != 405 {
		t.Error("a POST on the list")
	}
}

func authTok(secret, tenant, method, target string) string {
	return signV2(secret, tenant, method, target)
}

// Soft delete: a deleted object is hidden and kept, can be restored, is replaced by a new write, is removed at once with ?purge, and is removed by the worker when its time is over.
func TestSoftDeleteAndRestore(t *testing.T) {
	r := newRig(t, nil)
	r.mirrorSoft(3600)
	body := pattern(4000)
	r.put("k/a", body)
	_, _, h0 := r.plainDo("HEAD", "/v1/stores/main/objects/k/a", nil, nil)
	code, d, _ := r.core("DELETE", "/v1/stores/main/objects/k/a", "", nil, nil)
	if code != 200 || !strings.Contains(string(d), `"trash":true`) || !strings.Contains(string(d), "restorable_until") {
		t.Fatalf("delete: %d %s", code, d)
	}
	for name, c := range map[string]func() int{
		"read": func() int { c, _ := r.get("k/a", 0, 9); return c },
		"head": func() int { c, _, _ := r.plainDo("HEAD", "/v1/stores/main/objects/k/a", nil, nil); return c },
		"stat": func() int { c, _, _ := r.core("GET", "/v1/stores/main/objects/k/a", "stat", nil, nil); return c },
	} {
		if got := c(); got != 404 {
			t.Errorf("%s of a deleted object: %d", name, got)
		}
	}
	if code, d, _ := r.core("GET", "/v1/stores/main/objects", "", nil, nil); code != 200 || strings.Contains(string(d), "k/a") {
		t.Fatalf("the list shows what is in the trash: %s", d)
	}
	_, td, _ := r.core("GET", "/v1/stores/main/objects", "trash=1", nil, nil)
	if !strings.Contains(string(td), `"key":"k/a"`) || !strings.Contains(string(td), `"purge_at"`) {
		t.Fatalf("the trash: %s", td)
	}
	if len(files(r.nodes[0])) != 1 {
		t.Fatal("the blocks of an object in the trash are removed")
	}
	// deleting again is fine; restoring brings back the same version
	if code, _, _ := r.core("DELETE", "/v1/stores/main/objects/k/a", "", nil, nil); code != 200 {
		t.Fatal("a second delete")
	}
	if code, _, _ := r.core("POST", "/v1/stores/main/objects/k/a", "restore", nil, nil); code != 200 {
		t.Fatal("restore")
	}
	code, got, h1 := r.plainDo("GET", "/v1/stores/main/objects/k/a", nil, nil)
	if code != 200 || !bytes.Equal(got, body) || h1.Get("ETag") != h0.Get("ETag") {
		t.Fatalf("after the restore: %d, tag %s vs %s", code, h1.Get("ETag"), h0.Get("ETag"))
	}
	if code, _, _ := r.core("POST", "/v1/stores/main/objects/k/a", "restore", nil, nil); code != 404 {
		t.Fatal("restore of what is not in the trash")
	}
	// a new write over an object in the trash replaces it, and the old blocks go
	r.core("DELETE", "/v1/stores/main/objects/k/a", "", nil, nil)
	r.put("k/a", []byte("new"))
	if c, d := r.get("k/a", 0, 2); c != 206 || string(d) != "new" {
		t.Fatal("the new write")
	}
	if len(files(r.nodes[0])) != 1 {
		t.Fatalf("the old blocks are left: %v", files(r.nodes[0]))
	}
	// ?purge removes at once, also what is in the trash
	r.core("DELETE", "/v1/stores/main/objects/k/a", "", nil, nil)
	if code, _, _ := r.core("DELETE", "/v1/stores/main/objects/k/a", "purge", nil, nil); code != 200 {
		t.Fatal("purge")
	}
	if len(files(r.nodes[0])) != 0 || r.count("storage_objects") != 0 {
		t.Fatalf("purge left %v, %d rows", files(r.nodes[0]), r.count("storage_objects"))
	}
}

func TestTrashIsEmptiedByTheWorker(t *testing.T) {
	r := newRig(t, nil)
	r.mirrorSoft(60)
	r.put("k/old", pattern(500))
	r.put("k/new", pattern(600))
	r.core("DELETE", "/v1/stores/main/objects/k/old", "", nil, nil)
	later := func(d time.Duration) *maint.Worker {
		w := r.worker("w", func(c *maint.Config) { c.Now = func() time.Time { return time.Now().Add(d) } })
		w.Lease(context.Background(), "repair")
		return w
	}
	later(30 * time.Second).Pass(context.Background()) // not yet
	if r.count("storage_objects") != 2 {
		t.Fatal("an object was removed before its time")
	}
	r.core("DELETE", "/v1/stores/main/objects/k/new", "", nil, nil)
	later(2 * time.Minute).Pass(context.Background()) // k/old's time is over; k/new was deleted just now, but this worker's clock is two minutes on: both go
	if r.count("storage_objects") != 0 || len(files(r.nodes[0])) != 0 || r.count("storage_repair") != 0 {
		t.Fatalf("%d rows, %v, %d tasks", r.count("storage_objects"), files(r.nodes[0]), r.count("storage_repair"))
	}
}

// A process that dies right after the object was put in the trash leaves it in the trash, restorable; and one restored while the worker is at it is not removed.
func TestTrashAndCrash(t *testing.T) {
	c := newCrashRig(t)
	c.mirrorSoft(60)
	c.put("k/a", pattern(800))
	c.die("delete:after-trash", func() { c.core("DELETE", "/v1/stores/main/objects/k/a", "", nil, nil) })
	if code, _ := c.get("k/a", 0, 1); code != 404 {
		t.Fatal("it is not in the trash")
	}
	if code, _, _ := c.core("POST", "/v1/stores/main/objects/k/a", "restore", nil, nil); code != 200 {
		t.Fatal("restore after the crash")
	}
	if code, d := c.get("k/a", 0, 799); code != 206 || !bytes.Equal(d, pattern(800)) {
		t.Fatal("the restored object")
	}
	c.core("DELETE", "/v1/stores/main/objects/k/a", "", nil, nil)
	// the worker's clock is far on, but the object is restored before the worker looks at it
	c.core("POST", "/v1/stores/main/objects/k/a", "restore", nil, nil)
	w := c.worker("w", func(cfg *maint.Config) { cfg.Now = func() time.Time { return time.Now().Add(time.Hour) } })
	w.Lease(context.Background(), "repair")
	w.Pass(context.Background())
	if code, _ := c.get("k/a", 0, 1); code != 206 {
		t.Fatal("a restored object was removed by the worker")
	}
}

// The limits of a tenant: objects and bytes, a replacement counted once, bodies of unknown length, multipart, the trash not counted, one tenant not held by the limit of another.
func TestQuotas(t *testing.T) {
	r := newRig(t, func(c *Config) { c.ShardSecrets["free"] = "the-secret-of-the-free-tenant" })
	r.mirrorSoft(3600)
	if code, d := r.admin("PUT", "/v1/admin/quotas/main/001", map[string]any{"max_bytes": 10000, "max_objects": 3}); code != 200 {
		t.Fatalf("set: %d %s", code, d)
	}
	if code, _ := r.admin("PUT", "/v1/admin/quotas/nosuch/001", map[string]any{"max_bytes": 1}); code != 404 {
		t.Fatal("a quota in a store that is not there")
	}
	if code, _ := r.admin("PUT", "/v1/admin/quotas/main/001", "x"); code != 400 {
		t.Fatal("a body that is not JSON")
	}
	if code, d := r.put("a", pattern(4000)); code != 200 {
		t.Fatalf("%d %s", code, d)
	}
	r.put("b", pattern(4000))
	code, d := r.put("c", pattern(4000))
	if code != 507 || !strings.Contains(string(d), "quota_exceeded") || !strings.Contains(string(d), "may hold") {
		t.Fatalf("over the bytes: %d %s", code, d)
	}
	// a replacement counts once: 4000 -> 5000 is within 10000 although 4000 + 4000 + 5000 is not
	if code, d := r.put("a", pattern(5000)); code != 200 {
		t.Fatalf("a replacement: %d %s", code, d)
	}
	if code, _ := r.put("a", pattern(6001)); code != 507 {
		t.Fatalf("a replacement over the limit: %d", code)
	}
	r.put("s1", pattern(100))
	if code, d := r.put("s2", pattern(100)); code != 507 || !strings.Contains(string(d), "objects") {
		t.Fatalf("over the number of objects: %d %s", code, d)
	}
	// a body of unknown length that comes to more than is left is refused when it has come, and leaves nothing
	before := len(files(r.nodes[0]))
	if code, _, _ := r.plainDo("PUT", "/v1/stores/main/objects/s1", nil, io.MultiReader(bytes.NewReader(pattern(BlockSize+10)))); code != 507 {
		t.Fatalf("a chunked body over the limit: %d", code)
	}
	if len(files(r.nodes[0])) != before {
		t.Fatalf("a refused write left blocks: %d vs %d", len(files(r.nodes[0])), before)
	}
	// the usage, for the tenant and for the operator
	code, d, _ = r.core("GET", "/v1/stores/main/usage", "", nil, nil)
	var u struct {
		Objects, Bytes int64
		Quota          struct {
			MaxBytes   int64 `json:"max_bytes"`
			MaxObjects int64 `json:"max_objects"`
		}
	}
	_ = json.Unmarshal(d, &u)
	if code != 200 || u.Objects != 3 || u.Bytes != 5000+4000+100 || u.Quota.MaxBytes != 10000 || u.Quota.MaxObjects != 3 {
		t.Fatalf("usage: %d %s", code, d)
	}
	if code, d := r.admin("GET", "/v1/admin/stores/main:usage", nil); code != 200 || !strings.Contains(string(d), `"tenant":"001"`) || !strings.Contains(string(d), `"max_objects":3`) {
		t.Fatalf("admin usage: %d %s", code, d)
	}
	// the trash is not counted: deleting frees the room, and the trash is shown apart
	r.core("DELETE", "/v1/stores/main/objects/b", "", nil, nil)
	_, d, _ = r.core("GET", "/v1/stores/main/usage", "", nil, nil)
	if !strings.Contains(string(d), `"objects":2`) || !strings.Contains(string(d), `"trash_objects":1`) {
		t.Fatalf("usage after a delete: %s", d)
	}
	if code, _ := r.put("c", pattern(3000)); code != 200 {
		t.Fatal("room freed by a delete")
	}
	// multipart: the size when it is completed (room for one more object, not for the bytes)
	r.admin("PUT", "/v1/admin/quotas/main/001", map[string]any{"max_bytes": 10000, "max_objects": 10})
	_, bd, _ := r.core("POST", "/v1/stores/main/objects/big", "uploads", nil, nil)
	id := between(string(bd), `"upload_id":"`, `"`)
	p1 := pattern(9000)
	_, pd, _ := r.core("PUT", "/v1/stores/main/objects/big", "upload="+id+"&part=1", map[string]string{"X-Content-SHA256": sha(p1)}, p1)
	if code, d, _ := r.core("POST", "/v1/stores/main/objects/big", "upload="+id, nil, []byte(fmt.Sprintf(`{"parts":[[1,"%s"]]}`, between(string(pd), `"etag":"`, `"`)))); code != 507 {
		t.Fatalf("complete over the limit: %d %s", code, d)
	}
	// another tenant has no limit, and the limits can be taken away
	h := map[string]string{"Authorization": "Bearer " + authTok("the-secret-of-the-free-tenant", "free", "PUT", "/v1/stores/main/objects/big"), "X-Blobgate-Tenant": "free", "X-Content-SHA256": sha(p1)}
	if code, _, _ := r.do("PUT", "/v1/stores/main/objects/big", h, p1); code != 200 {
		t.Fatalf("a tenant with no limit: %d", code)
	}
	if code, d := r.admin("GET", "/v1/admin/quotas", nil); code != 200 || !strings.Contains(string(d), `"tenant":"001"`) {
		t.Fatalf("list of quotas: %d %s", code, d)
	}
	if code, _ := r.admin("DELETE", "/v1/admin/quotas/main/001", nil); code != 200 {
		t.Fatal("take the limit away")
	}
	if code, _ := r.put("more", pattern(20000)); code != 200 {
		t.Fatal("after the limit is gone")
	}
	_ = http.StatusOK
}
