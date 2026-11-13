package api

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/zystem/blobgate/internal/auth"
	"github.com/zystem/blobgate/internal/maint"
)

type req struct {
	name, method, path, query string
	hdr                       map[string]string
	body                      string
	want                      int
	kind                      string // the error code that must be in the answer
}

func (r *rig) runCore(t *testing.T, cases []req) {
	t.Helper()
	for _, c := range cases {
		code, data, _ := r.core(c.method, c.path, c.query, c.hdr, []byte(c.body))
		if code != c.want || (c.kind != "" && !strings.Contains(string(data), `"`+c.kind+`"`)) {
			t.Errorf("%s: %d %s, want %d %s", c.name, code, data, c.want, c.kind)
		}
	}
}

// Every refusal of the API has its status and its code (docs: the description of the API): what a caller does wrong is told in words and not met with a crash or silence.
func TestRefusalsOfTheCoreAPI(t *testing.T) {
	r := newRig(t, nil)
	r.mirror(0)
	sha1 := map[string]string{"X-Content-SHA256": sha([]byte("x"))}
	o := "/v1/stores/main/objects/k/a"
	r.put("k/a", []byte("hello world"))
	r.runCore(t, []req{
		{"Range that is not bytes", "GET", o, "", map[string]string{"Range": "items=1-2"}, "", 400, "invalid_range"},
		{"Range backwards", "GET", o, "", map[string]string{"Range": "bytes=5-1"}, "", 400, "invalid_range"},
		{"two ranges", "GET", o, "", map[string]string{"Range": "bytes=0-1,4-5"}, "", 400, "invalid_range"},
		{"Range past the object", "GET", o, "", map[string]string{"Range": "bytes=500-600"}, "", 416, "range_not_satisfiable"},
		{"stat of nothing", "GET", "/v1/stores/main/objects/k/none", "stat", nil, "", 404, "no_such_object"},
		{"read of nothing", "GET", "/v1/stores/main/objects/k/none", "", map[string]string{"Range": "bytes=0-1"}, "", 404, "no_such_object"},
		{"parts of an upload that is not there", "GET", o, "upload=abcdef0123456789&parts", nil, "", 200, "parts"},
		{"POST with no action", "POST", o, "", nil, "", 400, "invalid_request"},
		{"complete of a bad id", "POST", o, "upload=../x", nil, "{}", 400, "invalid_request"},
		{"complete with a body that is not JSON", "POST", o, "upload=abcdef0123456789", nil, "{", 400, "invalid_request"},
		{"complete with no parts", "POST", o, "upload=abcdef0123456789", nil, `{"parts":[]}`, 400, "invalid_request"},
		{"complete with a part that is not a pair", "POST", o, "upload=abcdef0123456789", nil, `{"parts":[[1]]}`, 400, "invalid_request"},
		{"complete with a part that was not uploaded", "POST", o, "upload=abcdef0123456789", nil, `{"parts":[[1,"e"]]}`, 400, "invalid_request"},
		{"complete with a gap", "POST", o, "upload=abcdef0123456789", nil, `{"parts":[[2,"e"]]}`, 400, "invalid_request"},
		{"abort of an upload that is not there", "DELETE", o, "upload=abcdef0123456789", nil, "", 200, "aborted"},
		{"abort with a bad id", "DELETE", o, "upload=../x", nil, "", 200, "aborted"},
		{"part 0", "PUT", o, "upload=abcdef0123456789&part=0", sha1, "x", 400, "invalid_part"},
		{"part 10001", "PUT", o, "upload=abcdef0123456789&part=10001", sha1, "x", 400, "invalid_part"},
		{"part with a bad upload id", "PUT", o, "upload=NOT-HEX&part=1", sha1, "x", 400, "invalid_part"},
		{"part for an upload that is not there", "PUT", o, "upload=abcdef0123456789&part=1", sha1, "x", 404, "no_such_upload"},
		{"part with no digest", "PUT", o, "upload=abcdef0123456789&part=1", nil, "x", 400, "sha256_required"},
		{"part with no body", "PUT", o, "upload=abcdef0123456789&part=1", map[string]string{"X-Content-SHA256": sha(nil)}, "", 400, "empty_body"},
		{"a key with a backslash", "GET", "/v1/stores/main/objects/k\\a", "", nil, "", 400, "invalid_key"},
		{"a key that is too long", "PUT", "/v1/stores/main/objects/" + strings.Repeat("k", 1100), "", sha1, "x", 400, "invalid_key"},
		{"a method that is not there", "PATCH", o, "", nil, "", 405, "method_not_allowed"},
		{"a path under stores that is not an object", "GET", "/v1/stores/main/other/a", "", nil, "", 404, "not_found"},
		{"a store that is not there", "GET", "/v1/stores/nosuch/objects/k/a", "stat", nil, "", 404, "no_such_store"},
		{"the list of stores by POST", "POST", "/v1/stores", "", nil, "", 405, "method_not_allowed"},
	})
	if code, _, _ := r.do("GET", "/v1/stores", nil, nil); code != 401 {
		t.Error("the list of stores with no token")
	}
	if code, _, _ := r.do("GET", "/nowhere", nil, nil); code != 404 {
		t.Error("a path that is not a route")
	}
	if code, _, _ := r.do("GET", "/readyz", nil, nil); code != 200 {
		t.Error("readyz")
	}
}

func TestRefusalsOfTheAdminAPI(t *testing.T) {
	r := newRig(t, nil)
	node := map[string]any{"remote": ":local:" + r.nodes[0]}
	good := map[string]any{"kind": "mirror", "nodes": []any{node}}
	type a struct {
		name, method, path string
		body               any
		want               int
	}
	for _, c := range []a{
		{"list", "GET", "/v1/admin/stores", nil, 200},
		{"list by POST", "POST", "/v1/admin/stores", nil, 405},
		{"a store that is not valid JSON", "PUT", "/v1/admin/stores/x", "{", 400},
		{"a bad name", "PUT", "/v1/admin/stores/Bad_Name", good, 400},
		{"an unknown kind", "PUT", "/v1/admin/stores/x", map[string]any{"kind": "dav"}, 400},
		{"s3 with a bad endpoint", "PUT", "/v1/admin/stores/x", map[string]any{"kind": "s3", "endpoint": "nope", "bucket": "bucket-1", "access_key_id": "k", "secret_access_key": "s"}, 400},
		{"a remote that is not a connection string", "PUT", "/v1/admin/stores/x", map[string]any{"kind": "mirror", "nodes": []any{map[string]any{"remote": "plain"}}}, 400},
		{"an action that is not there", "POST", "/v1/admin/stores/x:nope", nil, 404},
		{"check by GET", "GET", "/v1/admin/stores/x:check", nil, 405},
		{"check of a store that is not there", "POST", "/v1/admin/stores/nosuch:check", nil, 404},
		{"default by GET", "GET", "/v1/admin/stores/x:default", nil, 405},
		{"default of a store that is not there", "POST", "/v1/admin/stores/nosuch:default", nil, 404},
		{"reconcile by GET", "GET", "/v1/admin/stores/x:reconcile", nil, 405},
		{"reconcile with a bad min_age", "POST", "/v1/admin/stores/x:reconcile?min_age=soon", nil, 400},
		{"reconcile of a store that is not there", "POST", "/v1/admin/stores/nosuch:reconcile", nil, 404},
		{"delete of a store that is not there", "DELETE", "/v1/admin/stores/nosuch", nil, 404},
		{"a method that is not there", "PATCH", "/v1/admin/stores/x", nil, 405},
		{"a path under admin that is not stores", "GET", "/v1/admin/other", nil, 404},
	} {
		if code, data := r.admin(c.method, c.path, c.body); code != c.want {
			t.Errorf("%s: %d %s, want %d", c.name, code, data, c.want)
		}
	}
	// the default store cannot be forgotten while there is another one
	for _, n := range []string{"one", "two"} {
		if code, d := r.admin("PUT", "/v1/admin/stores/"+n, good); code != 200 {
			t.Fatalf("%s: %d %s", n, code, d)
		}
	}
	if code, _ := r.admin("DELETE", "/v1/admin/stores/one", nil); code != 409 {
		t.Error("the default store was forgotten while there is another")
	}
	if code, _ := r.admin("POST", "/v1/admin/stores/two:default", nil); code != 200 {
		t.Error("two as the default")
	}
	if code, _ := r.admin("DELETE", "/v1/admin/stores/one", nil); code != 200 {
		t.Error("a store that is not the default, with no objects")
	}
	// the stores the core sees
	code, data, _ := r.core("GET", "/v1/stores", "", nil, nil)
	if code != 200 || !strings.Contains(string(data), `"two"`) || strings.Contains(string(data), `"one"`) {
		t.Errorf("%d %s", code, data)
	}
	// the default is read through the name `_default`
	if code, d := r.put("k/viadefault", []byte("v")); code != 200 {
		t.Errorf("%d %s", code, d)
	}
}

func (r *rig) shimDo(method, path, grant string, hdr map[string]string, body []byte) int {
	req, _ := http.NewRequest(method, r.shim.URL+path, bytes.NewReader(body))
	if grant != "" {
		req.Header.Set("Authorization", "Bearer "+grant)
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0
	}
	defer resp.Body.Close()
	return resp.StatusCode
}

func TestRefusalsOfTheShimPort(t *testing.T) {
	r := newRig(t, nil)
	r.mirror(0)
	now := time.Now().Unix()
	w := auth.SignGrant(secret1, "001", "main", "k/a", "abcdef0123456789", "w", now)
	rd := auth.SignGrant(secret1, "001", "main", "k/a", "", "r", now)
	x := map[string]string{"X-Content-SHA256": sha([]byte("x"))}
	for _, c := range []struct {
		name, method, path, grant string
		hdr                       map[string]string
		want                      int
	}{
		{"no grant", "GET", "/s/main/k/a", "", map[string]string{"Range": "bytes=0-1"}, 401},
		{"a grant of another shard", "GET", "/s/main/k/a", auth.SignGrant("another-secret-of-16-chars", "002", "main", "k/a", "", "r", now), map[string]string{"Range": "bytes=0-1"}, 401},
		{"the default store", "GET", "/s/_default/k/a", rd, map[string]string{"Range": "bytes=0-1"}, 401},
		{"a bad key", "GET", "/s/main/k\\a", rd, map[string]string{"Range": "bytes=0-1"}, 401},
		{"a path that is not /s/", "GET", "/v1/stores", rd, nil, 404},
		{"a path with no key", "GET", "/s/main", rd, nil, 404},
		{"a read grant for a write", "PUT", "/s/main/k/a?upload=abcdef0123456789&part=1", rd, x, 401},
		{"a write grant for a read", "GET", "/s/main/k/a", w, map[string]string{"Range": "bytes=0-1"}, 401},
		{"a part with no upload", "PUT", "/s/main/k/a?part=1", auth.SignGrant(secret1, "001", "main", "k/a", "", "w", now), x, 400},
		{"a method that is not there", "DELETE", "/s/main/k/a", rd, nil, 405},
		{"a read with a Range that is not one", "GET", "/s/main/k/a", rd, map[string]string{"Range": "x"}, 400},
		{"a read of a store that is not there", "GET", "/s/nosuch/k/a", auth.SignGrant(secret1, "001", "nosuch", "k/a", "", "r", now), map[string]string{"Range": "bytes=0-1"}, 404},
		{"a read of an object that is not there", "GET", "/s/main/k/a", rd, map[string]string{"Range": "bytes=0-1"}, 404},
		{"health", "GET", "/healthz", "", nil, 200},
	} {
		if code := r.shimDo(c.method, c.path, c.grant, c.hdr, []byte("x")); code != c.want {
			t.Errorf("%s: %d, want %d", c.name, code, c.want)
		}
	}
}

// The passes of the replica as a loop, until it is told to stop; it lets go of its lease. A node that cannot be cleaned by the sweep is queued for the repair pass.
func TestTheLoopAndTheSweepOfANodeThatIsAway(t *testing.T) {
	r := newRig(t, nil)
	r.mirror(0)
	_, d, _ := r.core("POST", "/v1/stores/main/objects/k/mp", "uploads", nil, nil)
	id := between(string(d), `"upload_id":"`, `"`)
	part := []byte("a part")
	if code, _, _ := r.core("PUT", "/v1/stores/main/objects/k/mp", "upload="+id+"&part=1", map[string]string{"X-Content-SHA256": sha(part)}, part); code != 200 {
		t.Fatal("part")
	}
	r.readOnly(1)
	w := r.worker("loop", func(c *maint.Config) {
		c.RepairEvery = 100 * time.Millisecond
		c.CheckEvery = 150 * time.Millisecond
		c.UploadTTL = time.Second
		c.Now = func() time.Time { return time.Now().Add(time.Hour) } // the upload is old
	})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { w.Run(ctx); close(done) }()
	waitUntil(t, "the upload is swept and the node that was away is queued", func() bool {
		return r.count("storage_uploads") == 0 && r.count("storage_repair") >= 1
	})
	cancel()
	<-done
	rows, err := r.dbc.Query(context.Background(), rqS("SELECT count(*) FROM storage_lease WHERE name = 'repair' AND until = 0"))
	if err != nil || len(rows) == 0 || fmt.Sprint(rows[0][0]) != "1" {
		t.Fatalf("the lease was not let go: %v %v", rows, err)
	}
}

// The callers of the first version still work: the header X-Cinim-Shard, the tokens v1 and the grants g1; and a tenant may be named as the operator likes.
func TestFirstVersionCallersStillWork(t *testing.T) {
	r := newRig(t, func(c *Config) { c.ShardSecrets["acme-corp"] = "the-secret-of-acme-corp-tenant" })
	r.mirror(0)
	now := time.Now().Unix()
	body := []byte("old caller")
	target := "/v1/stores/main/objects/k/old"
	// v1 token and the old header
	req, _ := http.NewRequest("PUT", r.ts.URL+target, bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+auth.SignTargetV1(secret1, "001", "PUT", target, now))
	req.Header.Set("X-Cinim-Shard", "001")
	req.Header.Set("X-Content-SHA256", sha(body))
	if resp, err := http.DefaultClient.Do(req); err != nil || resp.StatusCode != 200 {
		t.Fatalf("v1 token with the old header: %v %v", err, resp)
	}
	// v2 token with the old header, v1 token with the new header
	for name, h := range map[string]map[string]string{
		"v2, old header": {"Authorization": "Bearer " + auth.SignTarget(secret1, "001", "GET", target+"", now), "X-Cinim-Shard": "001"},
		"v1, new header": {"Authorization": "Bearer " + auth.SignTargetV1(secret1, "001", "GET", target, now), "X-Blobgate-Tenant": "001"},
	} {
		h["Range"] = "bytes=0-9"
		if code, d, _ := r.do("GET", target, h, nil); code != 206 || string(d) != "old caller" {
			t.Errorf("%s: %d %s", name, code, d)
		}
	}
	// a v1 grant on the port for shims
	grant := auth.SignGrantV1(secret1, "001", "main", "k/old", "", "r", now)
	if code := r.shimDo("GET", "/s/main/k/old", grant, map[string]string{"Range": "bytes=0-9"}, nil); code != 206 {
		t.Errorf("a grant of the first version: %d", code)
	}
	// a tenant with a name that is not digits
	t2 := "/v1/stores/main/objects/k/two"
	b2 := []byte("two")
	h := map[string]string{"Authorization": "Bearer " + auth.SignTarget("the-secret-of-acme-corp-tenant", "acme-corp", "PUT", t2, now), "X-Blobgate-Tenant": "acme-corp", "X-Content-SHA256": sha(b2)}
	if code, d, _ := r.do("PUT", t2, h, b2); code != 200 {
		t.Fatalf("tenant acme-corp: %d %s", code, d)
	}
	// and it does not see the keys of tenant 001
	h2 := map[string]string{"Authorization": "Bearer " + auth.SignTarget("the-secret-of-acme-corp-tenant", "acme-corp", "GET", target, now), "X-Blobgate-Tenant": "acme-corp", "Range": "bytes=0-1"}
	if code, _, _ := r.do("GET", target, h2, nil); code != 404 {
		t.Errorf("a tenant sees the object of another: %d", code)
	}
}
