package api

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/zystem/blobgate/internal/rq"
)

func (r *rig) link(key, digest string, size int) (int, string) {
	r.t.Helper()
	code, d, _ := r.core("POST", "/v1/stores/main/objects/"+key, "link", nil, []byte(fmt.Sprintf(`{"sha256":%q,"size":%d}`, digest, size)))
	return code, string(d)
}

func TestLinkTakesTheBytesThatAreThere(t *testing.T) {
	r := newRig(t, func(c *Config) { c.ShardSecrets["other"] = "the-secret-of-the-other-tenant" })
	r.dedupStore("tenant", 0, "")
	big := pattern(9<<20 + 5) // two blocks, written in chunks: the digest is of the whole body
	small := pattern(4000)
	for k, b := range map[string][]byte{"a/big": big, "a/small": small} {
		if code, d := r.put(k, b); code != 200 {
			t.Fatalf("%s %d %s", k, code, d)
		}
	}
	for _, c := range []struct {
		from string
		body []byte
	}{{"a/big", big}, {"a/small", small}} {
		if code, d := r.link("b/"+c.from, sha(c.body), len(c.body)); code != 200 || !strings.Contains(d, `"linked":true`) {
			t.Fatalf("link: %d %s", code, d)
		}
		if code, d := r.get("b/"+c.from, 0, len(c.body)-1); code != 206 || !bytes.Equal(d, c.body) {
			t.Fatalf("the linked object reads as %d", code)
		}
	}
	if len(files(r.nodes[0])) != 3 || r.count("storage_sets") != 2 || r.count("storage_objects") != 4 {
		t.Fatalf("nothing was written: %v", files(r.nodes[0]))
	}
	r.refsAreTheRows()
	// the same bytes, a size that is not theirs, another tenant: nothing to link
	if code, d := r.link("c", sha(small), 3999); code != 404 || !strings.Contains(d, "no_such_content") {
		t.Fatalf("wrong size: %d %s", code, d)
	}
	hh := map[string]string{"Authorization": "Bearer " + authTok("the-secret-of-the-other-tenant", "other", "POST", "/v1/stores/main/objects/c?link"), "X-Blobgate-Tenant": "other"}
	if code, d, _ := r.do("POST", "/v1/stores/main/objects/c?link", hh, []byte(fmt.Sprintf(`{"sha256":%q,"size":4000}`, sha(small)))); code != 404 {
		t.Fatalf("the other tenant got %d %s", code, d)
	}
	if code, _ := r.link("c", "zz", 1); code != 400 {
		t.Fatal("bad digest")
	}
	// a link replaces an object: the old blocks go, the count follows
	if code, d := r.put("b/a/small", pattern(77)); code != 200 {
		t.Fatal(d)
	}
	if code, _ := r.link("b/a/small", sha(small), len(small)); code != 200 {
		t.Fatal("link over an object")
	}
	r.refsAreTheRows()
	if code, d := r.get("b/a/small", 0, 3999); code != 206 || !bytes.Equal(d, small) {
		t.Fatal("replaced")
	}
	if n := r.count("storage_repair"); n != 0 {
		t.Fatalf("%d tasks left", n)
	}
}

func TestLinkNeedsTenantScope(t *testing.T) {
	for _, mode := range []string{"", "store"} {
		r := newRig(t, nil)
		r.dedupStore(mode, 0, "")
		b := pattern(1000)
		r.put("x", b)
		if code, d := r.link("y", sha(b), len(b)); code != 409 || !strings.Contains(d, "dedup_not_tenant") {
			t.Fatalf("mode %q: %d %s", mode, code, d)
		}
	}
}

func TestLinkAndAFreedSet(t *testing.T) {
	r := newRig(t, nil)
	r.dedupStore("tenant", 0, "")
	b := pattern(2500)
	r.put("x", b)
	r.core("DELETE", "/v1/stores/main/objects/x", "", nil, nil)
	// the set has no reference but is still there: a link takes it back
	if code, d := r.link("y", sha(b), len(b)); code != 200 {
		t.Fatalf("%d %s", code, d)
	}
	if r.sweepSets() != 0 {
		t.Fatal("a set that a link took was swept")
	}
	if code, d := r.get("y", 0, 2499); code != 206 || !bytes.Equal(d, b) {
		t.Fatal("read")
	}
	r.refsAreTheRows()
	// the set removed between the lookup and the transaction: the old object stays, the answer is 404
	r.put("z", pattern(3000))
	r.core("DELETE", "/v1/stores/main/objects/y", "", nil, nil)
	r.s.cfg.CrashAt = func(p string) {
		if p == "link:before" {
			r.sweepSets()
		}
	}
	if code, d := r.link("z", sha(b), len(b)); code != 404 {
		t.Fatalf("%d %s", code, d)
	}
	r.s.cfg.CrashAt = nil
	if code, d := r.get("z", 0, 2999); code != 206 || !bytes.Equal(d, pattern(3000)) {
		t.Fatal("the object that the failed link was to replace is gone")
	}
	r.refsAreTheRows()
	if n := r.count("storage_sets"); n != 1 {
		t.Fatalf("%d sets", n)
	}
	_ = context.Background
	_ = rq.S
}

// the version a listing names is the one a read names, for an object that points to a set too (a client that lists and then reads with If-Match, as the aws cli does in `sync`, relies on it)
func TestListNamesTheVersionOfASet(t *testing.T) {
	r := newRig(t, nil)
	r.dedupStore("tenant", 0, "")
	b := pattern(9<<20 + 1)
	r.put("e/1", b)
	r.put("e/2", b)
	_, d, _ := r.core("GET", "/v1/stores/main/objects", "prefix=e/", nil, nil)
	_, _, h1 := r.core("HEAD", "/v1/stores/main/objects/e/1", "", nil, nil)
	_, _, h2 := r.core("HEAD", "/v1/stores/main/objects/e/2", "", nil, nil)
	if h1.Get("ETag") == "" || h1.Get("ETag") != h2.Get("ETag") {
		t.Fatalf("heads: %s %s", h1.Get("ETag"), h2.Get("ETag"))
	}
	if strings.Count(string(d), strings.Trim(h1.Get("ETag"), `"`)) != 2 {
		t.Fatalf("the list names another version than the read: %s vs %s", d, h1.Get("ETag"))
	}
}
