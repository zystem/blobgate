package api

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/zystem/blobgate/internal/auth"
)

// plainDo is a call a client that knows nothing of the first version makes: the new header, a v2 token, any body (a reader of unknown length gives a chunked request).
func (r *rig) plainDo(method, path string, hdr map[string]string, body io.Reader) (int, []byte, http.Header) {
	r.t.Helper()
	req, _ := http.NewRequest(method, r.ts.URL+path, body)
	req.Header.Set("Authorization", "Bearer "+auth.SignTarget(secret1, "001", method, strings.SplitN(path, "?", 2)[0]+queryOf(path), time.Now().Unix()))
	req.Header.Set("X-Blobgate-Tenant", "001")
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		r.t.Fatal(err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, data, resp.Header
}

func queryOf(path string) string {
	if i := strings.IndexByte(path, '?'); i >= 0 {
		return path[i:]
	}
	return ""
}

func (r *rig) shaHex(b []byte) string { h := sha256.Sum256(b); return hex.EncodeToString(h[:]) }

const obj = "/v1/stores/main/objects/pkg/lib-1.0.jar"

// A client of any kind: PUT with no digest and a type and some metadata, GET of the whole, HEAD, a version tag and 304, ranges of every shape.
func TestPlainHTTPAccess(t *testing.T) {
	r := newRig(t, nil)
	r.mirror(0)
	body := pattern(300000)
	code, data, _ := r.plainDo("PUT", obj, map[string]string{"Content-Type": "application/java-archive", "X-Blobgate-Meta-Artifact": "lib", "X-Blobgate-Meta-Version": "1.0"}, bytes.NewReader(body))
	if code != 200 || !strings.Contains(string(data), `"size":300000`) {
		t.Fatalf("put: %d %s", code, data)
	}
	// the whole object, with what is known of it
	code, got, h := r.plainDo("GET", obj, nil, nil)
	if code != 200 || !bytes.Equal(got, body) || h.Get("Content-Type") != "application/java-archive" || h.Get("X-Blobgate-Meta-Artifact") != "lib" || h.Get("X-Blobgate-Meta-Version") != "1.0" ||
		h.Get("Accept-Ranges") != "bytes" || h.Get("ETag") == "" || h.Get("Last-Modified") == "" || h.Get("Content-Length") != "300000" {
		t.Fatalf("get: %d %v", code, h)
	}
	etag := h.Get("ETag")
	// HEAD says the same and sends no body
	code, got, hh := r.plainDo("HEAD", obj, nil, nil)
	if code != 200 || len(got) != 0 || hh.Get("ETag") != etag || hh.Get("Content-Length") != "300000" || hh.Get("Content-Type") != "application/java-archive" {
		t.Fatalf("head: %d %v", code, hh)
	}
	if code, _, _ := r.plainDo("HEAD", obj+"-none", nil, nil); code != 404 {
		t.Fatalf("head of nothing: %d", code)
	}
	// a version tag: 304 when it is the same, the body when it is not
	if code, got, hh := r.plainDo("GET", obj, map[string]string{"If-None-Match": etag}, nil); code != 304 || len(got) != 0 || hh.Get("ETag") != etag {
		t.Fatalf("304: %d %d", code, len(got))
	}
	if code, _, _ := r.plainDo("GET", obj, map[string]string{"If-None-Match": `"other"`}, nil); code != 200 {
		t.Fatalf("another tag: %d", code)
	}
	if code, _, _ := r.plainDo("GET", obj, map[string]string{"If-Match": `"other"`}, nil); code != 412 {
		t.Fatalf("If-Match that does not match: %d", code)
	}
	// ranges of every shape, with Content-Range
	for name, c := range map[string]struct {
		rng      string
		lo, hi   int
		wantCode int
	}{
		"both ends": {"bytes=10-19", 10, 19, 206}, "to the end": {"bytes=299990-", 299990, 299999, 206}, "the last bytes": {"bytes=-5", 299995, 299999, 206},
		"more than there is": {"bytes=299990-999999", 299990, 299999, 206}, "the last bytes of more": {"bytes=-999999", 0, 299999, 206},
	} {
		code, got, hh := r.plainDo("GET", obj, map[string]string{"Range": c.rng}, nil)
		want := fmt.Sprintf("bytes %d-%d/300000", c.lo, c.hi)
		if code != c.wantCode || !bytes.Equal(got, body[c.lo:c.hi+1]) || hh.Get("Content-Range") != want {
			t.Errorf("%s: %d, Content-Range %q (want %q), %d bytes", name, code, hh.Get("Content-Range"), want, len(got))
		}
	}
	if code, _, hh := r.plainDo("GET", obj, map[string]string{"Range": "bytes=300000-"}, nil); code != 416 || hh.Get("Content-Range") != "bytes */300000" {
		t.Fatalf("past the end: %d %v", code, hh)
	}
	// stat tells the same
	if code, d, _ := r.plainDo("GET", obj+"?stat", nil, nil); code != 200 || !strings.Contains(string(d), etag[1:len(etag)-1]) || !strings.Contains(string(d), "application/java-archive") {
		t.Fatalf("stat: %d %s", code, d)
	}
	// a replacement is another version
	r.plainDo("PUT", obj, nil, bytes.NewReader([]byte("other")))
	if _, _, h2 := r.plainDo("HEAD", obj, nil, nil); h2.Get("ETag") == etag || h2.Get("Content-Type") != "application/octet-stream" {
		t.Fatalf("the tag did not change: %v", h2)
	}
}

// A body of unknown length (chunked), one that is longer than a block, and an empty one: the module cuts it into blocks, checks the digest of the whole when it is named, and reads back every byte.
func TestPutOfAnyLength(t *testing.T) {
	for _, sealed := range []bool{false, true} {
		r := newRig(t, nil)
		if sealed {
			r.mirrorSealed(0, 1, map[string]string{"1": key1})
		} else {
			r.mirror(0)
		}
		body := pattern(2*BlockSize + 777)
		// chunked: a reader that is not a bytes.Reader has no length
		code, d, _ := r.plainDo("PUT", obj, nil, io.MultiReader(bytes.NewReader(body[:1000]), bytes.NewReader(body[1000:])))
		if code != 200 || !strings.Contains(string(d), fmt.Sprintf(`"size":%d`, len(body))) {
			t.Fatalf("sealed=%v chunked: %d %s", sealed, code, d)
		}
		if code, got, _ := r.plainDo("GET", obj, nil, nil); code != 200 || !bytes.Equal(got, body) {
			t.Fatalf("sealed=%v read back: %d %d", sealed, code, len(got))
		}
		if code, got, _ := r.plainDo("GET", obj, map[string]string{"Range": fmt.Sprintf("bytes=%d-%d", BlockSize-5, BlockSize+4)}, nil); code != 206 || !bytes.Equal(got, body[BlockSize-5:BlockSize+5]) {
			t.Fatalf("across a border: %d", code)
		}
		// the same with the length known, and the digest of the whole named
		obj2 := obj + "-2"
		if code, d, _ := r.plainDo("PUT", obj2, map[string]string{"X-Content-SHA256": r.shaHex(body)}, bytes.NewReader(body)); code != 200 {
			t.Fatalf("known length, digest named: %d %s", code, d)
		}
		obj3 := obj + "-3"
		if code, d, _ := r.plainDo("PUT", obj3, map[string]string{"X-Content-SHA256": r.shaHex([]byte("not it"))}, io.MultiReader(bytes.NewReader(body))); code != 400 || !strings.Contains(string(d), "sha256_mismatch") {
			t.Fatalf("a wrong digest of the whole: %d %s", code, d)
		}
		if code, _, _ := r.plainDo("GET", obj3, nil, nil); code != 404 {
			t.Fatal("the refused object exists")
		}
		// an empty object
		obj4 := obj + "-4"
		if code, d, _ := r.plainDo("PUT", obj4, nil, bytes.NewReader(nil)); code != 200 || !strings.Contains(string(d), `"size":0`) {
			t.Fatalf("empty: %d %s", code, d)
		}
		if code, got, h := r.plainDo("GET", obj4, nil, nil); code != 200 || len(got) != 0 || h.Get("Content-Length") != "0" {
			t.Fatalf("read of the empty one: %d %v", code, h)
		}
		if code, _, _ := r.plainDo("GET", obj4, map[string]string{"Range": "bytes=0-0"}, nil); code != 416 {
			t.Fatalf("a range of the empty one: %d", code)
		}
		// what the refusal left is nothing: only the blocks of the objects that exist
		want := 0
		for _, n := range []int{len(body), len(body)} {
			want += (n + BlockSize - 1) / BlockSize
		}
		if n := len(files(r.nodes[0])); n != want {
			t.Fatalf("sealed=%v: node holds %d files, want %d", sealed, n, want)
		}
	}
}

// A limit on the size of an object, a refusal for a version that is not the one named, and the metadata of a multipart upload.
func TestLimitsPreconditionsAndMetadata(t *testing.T) {
	r := newRig(t, func(c *Config) { c.MaxObject = 100000 })
	r.mirror(0)
	if code, d, _ := r.plainDo("PUT", obj, nil, bytes.NewReader(pattern(100001))); code != 413 {
		t.Fatalf("known length over the limit: %d %s", code, d)
	}
	if code, d, _ := r.plainDo("PUT", obj, nil, io.MultiReader(bytes.NewReader(pattern(BlockSize+5)))); code != 413 {
		t.Fatalf("unknown length over the limit: %d %s", code, d)
	}
	if n := len(files(r.nodes[0])); n != 0 {
		t.Fatalf("a refused write left %d files", n)
	}
	r.plainDo("PUT", obj, nil, bytes.NewReader([]byte("v1")))
	_, _, h := r.plainDo("HEAD", obj, nil, nil)
	etag := h.Get("ETag")
	if code, _, _ := r.plainDo("PUT", obj, map[string]string{"If-None-Match": "*"}, bytes.NewReader([]byte("again"))); code != 412 {
		t.Fatalf("create-only over an existing object: %d", code)
	}
	if code, _, _ := r.plainDo("PUT", obj+"-new", map[string]string{"If-None-Match": "*"}, bytes.NewReader([]byte("new"))); code != 200 {
		t.Fatalf("create-only of a new object: %d", code)
	}
	if code, _, _ := r.plainDo("PUT", obj, map[string]string{"If-Match": `"stale"`}, bytes.NewReader([]byte("v2"))); code != 412 {
		t.Fatalf("replace of a version that is not the current: %d", code)
	}
	if code, _, _ := r.plainDo("PUT", obj, map[string]string{"If-Match": etag}, bytes.NewReader([]byte("v2"))); code != 200 {
		t.Fatalf("replace of the current version: %d", code)
	}
	if code, _, _ := r.plainDo("DELETE", obj, map[string]string{"If-Match": etag}, nil); code != 412 {
		t.Fatalf("delete of a version that is gone: %d", code)
	}
	// metadata that is not allowed
	for name, hdr := range map[string]map[string]string{
		"a value too long":  {"X-Blobgate-Meta-Ok": strings.Repeat("x", 1025)},
		"a name with a dot": {"X-Blobgate-Meta-Bad.name": "x"},
	} {
		if code, _, _ := r.plainDo("PUT", obj+"-m", hdr, bytes.NewReader([]byte("x"))); code != 400 {
			t.Errorf("%s: %d", name, code)
		}
	}
	// the metadata given at the beginning of a multipart upload is the object's when it is completed
	_, d, _ := r.plainDo("POST", "/v1/stores/main/objects/pkg/big?uploads", map[string]string{"Content-Type": "application/x-tar", "X-Blobgate-Meta-Origin": "ci"}, nil)
	id := between(string(d), `"upload_id":"`, `"`)
	part := pattern(5000)
	_, pd, _ := r.plainDo("PUT", "/v1/stores/main/objects/pkg/big?upload="+id+"&part=1", map[string]string{"X-Content-SHA256": r.shaHex(part)}, bytes.NewReader(part))
	etagOfPart := between(string(pd), `"etag":"`, `"`)
	if code, d, _ := r.plainDo("POST", "/v1/stores/main/objects/pkg/big?upload="+id, nil, strings.NewReader(fmt.Sprintf(`{"parts":[[1,"%s"]]}`, etagOfPart))); code != 200 {
		t.Fatalf("complete: %d %s", code, d)
	}
	if _, _, hh := r.plainDo("HEAD", "/v1/stores/main/objects/pkg/big", nil, nil); hh.Get("Content-Type") != "application/x-tar" || hh.Get("X-Blobgate-Meta-Origin") != "ci" {
		t.Fatalf("the metadata of the upload is lost: %v", hh)
	}
}

func signV2(secret, tenant, method, target string) string {
	return auth.SignTarget(secret, tenant, method, target, time.Now().Unix())
}
