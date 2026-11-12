package api

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/rclone/rclone/fs"
	"gopkg.in/yaml.v3"

	"github.com/zystem/blobgate/internal/auth"
	"github.com/zystem/blobgate/internal/db"
	"github.com/zystem/blobgate/internal/rq"
	"github.com/zystem/blobgate/internal/seal"
	"github.com/zystem/blobgate/internal/store"
)

const (
	secret1 = "the-secret-of-shard-001-go-test"
	adminT  = "an-admin-token-for-the-go-test"
)

type rig struct {
	tolerate bool // a call may end with no answer (a simulated crash)
	t        *testing.T
	s        *Server
	ts       *httptest.Server
	shim     *httptest.Server
	nodes    [2]string
	dbc      *rq.Client
}

func newRig(t *testing.T, mut func(*Config)) *rig {
	url := os.Getenv("CINIM_RQLITE_URL")
	if url == "" {
		t.Skip("CINIM_RQLITE_URL is not set")
	}
	c := rq.New(url)
	ctx := context.Background()
	for _, tb := range []string{"storage_stores", "storage_objects", "storage_repair", "storage_parts", "storage_uploads", "storage_lease"} {
		if _, err := c.Exec(ctx, false, rq.S("DROP TABLE IF EXISTS "+tb)); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.Migrate(ctx, c); err != nil {
		t.Fatal(err)
	}
	cfg := Config{ShardSecrets: map[string]string{"001": secret1, "002": "the-secret-of-shard-002-go-test"}, AdminToken: adminT}
	if mut != nil {
		mut(&cfg)
	}
	s := New(cfg, c, store.NewRegistry(c, seal.Key("a-seal-key-for-the-go-test")), slog.New(slog.NewTextHandler(io.Discard, nil)))
	r := &rig{t: t, s: s, dbc: c, ts: httptest.NewServer(s.Handler()), shim: httptest.NewServer(s.ShimHandler())}
	t.Cleanup(func() { r.ts.Close(); r.shim.Close() })
	dir := t.TempDir()
	r.nodes = [2]string{filepath.Join(dir, "n1"), filepath.Join(dir, "n2")}
	return r
}

func (r *rig) do(method, path string, hdr map[string]string, body []byte) (int, []byte, http.Header) {
	r.t.Helper()
	req, _ := http.NewRequest(method, r.ts.URL+path, bytes.NewReader(body))
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		if r.tolerate { // the request was ended by a simulated crash of the process
			return 0, nil, nil
		}
		r.t.Fatal(err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, data, resp.Header
}

func (r *rig) admin(method, path string, body any) (int, []byte) {
	b, _ := json.Marshal(body)
	code, data, _ := r.do(method, path, map[string]string{"Authorization": "Bearer " + adminT}, b)
	return code, data
}

// core makes a signed call of the core for shard 001.
func (r *rig) core(method, path, query string, hdr map[string]string, body []byte) (int, []byte, http.Header) {
	t := path
	if query != "" {
		t += "?" + query
	}
	h := map[string]string{"Authorization": "Bearer " + auth.SignTarget(secret1, "001", method, t, time.Now().Unix()), "X-Cinim-Shard": "001"}
	for k, v := range hdr {
		h[k] = v
	}
	return r.do(method, t, h, body)
}

func (r *rig) mirror(minAcks int) {
	r.t.Helper()
	code, data := r.admin("PUT", "/v1/admin/stores/main", map[string]any{"kind": "mirror", "min_acks": minAcks, "default": true,
		"nodes": []any{map[string]any{"remote": ":local:" + r.nodes[0]}, map[string]any{"remote": ":local:" + r.nodes[1]}}})
	if code != 200 {
		r.t.Fatalf("register the store: %d %s", code, data)
	}
}

func sha(b []byte) string { s := sha256.Sum256(b); return hex.EncodeToString(s[:]) }

func (r *rig) put(key string, body []byte) (int, []byte) {
	code, data, _ := r.core("PUT", "/v1/stores/_default/objects/"+key, "", map[string]string{"X-Content-SHA256": sha(body)}, body)
	return code, data
}

func (r *rig) get(key string, a, b int) (int, []byte) { return r.getFrom("main", key, a, b) }

func (r *rig) getFrom(store, key string, a, b int) (int, []byte) {
	code, data, _ := r.core("GET", "/v1/stores/"+store+"/objects/"+key, "", map[string]string{"Range": fmt.Sprintf("bytes=%d-%d", a, b)}, nil)
	return code, data
}

func pattern(n int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(i*7 + i/251)
	}
	return b
}

func files(dir string) []string {
	var out []string
	_ = filepath.Walk(dir, func(p string, i os.FileInfo, err error) error {
		if err == nil && !i.IsDir() {
			rel, _ := filepath.Rel(dir, p)
			out = append(out, rel)
		}
		return nil
	})
	return out
}

// STO-006 (D-60): an object goes to both nodes, under the path of a generation, and reads back by ranges; the shard is in the key.
func TestPutGetStatDelete(t *testing.T) {
	r := newRig(t, nil)
	if code, _, _ := r.core("GET", "/v1/stores", "", nil, nil); code != 200 {
		t.Fatal("stores", code)
	}
	if code, _ := r.put("a/b.txt", []byte("x")); code != 409 {
		t.Fatalf("no store yet: %d", code)
	}
	r.mirror(0)
	body := pattern(100000)
	if code, data := r.put("acme/s1/app.bin", body); code != 200 {
		t.Fatalf("put %d %s", code, data)
	}
	for _, n := range r.nodes {
		f := files(n)
		if len(f) != 1 || !strings.HasPrefix(f[0], "001/acme/s1/app.bin~") || !strings.HasSuffix(f[0], "/0") {
			t.Fatalf("node %s holds %v", n, f)
		}
	}
	if code, data := r.get("acme/s1/app.bin", 10, 19); code != 206 || !bytes.Equal(data, body[10:20]) {
		t.Fatalf("range %d %v", code, data)
	}
	if code, data := r.get("acme/s1/app.bin", 99990, 200000); code != 206 || !bytes.Equal(data, body[99990:]) {
		t.Fatalf("past the end is cut: %d %d", code, len(data))
	}
	code, data, _ := r.core("GET", "/v1/stores/main/objects/acme/s1/app.bin", "stat", nil, nil)
	if code != 200 || !strings.Contains(string(data), `"size":100000`) {
		t.Fatalf("stat %d %s", code, data)
	}
	// a replacement is a new generation; the old one is removed
	body2 := pattern(5000)
	if code, _ := r.put("acme/s1/app.bin", body2); code != 200 {
		t.Fatal("replace")
	}
	if f := files(r.nodes[0]); len(f) != 1 {
		t.Fatalf("after a replacement: %v", f)
	}
	if code, data := r.get("acme/s1/app.bin", 0, 4999); code != 206 || !bytes.Equal(data, body2) {
		t.Fatal("read the new one")
	}
	if code, _, _ := r.core("DELETE", "/v1/stores/main/objects/acme/s1/app.bin", "", nil, nil); code != 200 {
		t.Fatal("delete")
	}
	if code, _ := r.get("acme/s1/app.bin", 0, 1); code != 404 {
		t.Fatalf("after delete: %d", code)
	}
	if f := files(r.nodes[0]); len(f) != 0 {
		t.Fatalf("blocks left: %v", f)
	}
	if code, _, _ := r.core("DELETE", "/v1/stores/main/objects/never/was", "", nil, nil); code != 200 {
		t.Fatal("deleting what is not there is fine")
	}
}

// DAT-004: a body that is not the one named is refused and leaves nothing behind.
func TestWrongDigestLeavesNothing(t *testing.T) {
	r := newRig(t, nil)
	r.mirror(0)
	code, data, _ := r.core("PUT", "/v1/stores/main/objects/a/b", "", map[string]string{"X-Content-SHA256": sha([]byte("other"))}, []byte("body"))
	if code != 400 || !strings.Contains(string(data), "sha256_mismatch") {
		t.Fatalf("%d %s", code, data)
	}
	if code, _, _ := r.core("PUT", "/v1/stores/main/objects/a/b", "", nil, []byte("body")); code != 400 {
		t.Fatal("the digest is required")
	}
	if code, _, _ := r.core("PUT", "/v1/stores/main/objects/a/b", "", map[string]string{"X-Content-SHA256": sha(nil)}, nil); code != 400 {
		t.Fatal("empty body")
	}
	big := make([]byte, BlockSize+1)
	if code, _, _ := r.core("PUT", "/v1/stores/main/objects/a/b", "", map[string]string{"X-Content-SHA256": sha(big)}, big); code != 413 {
		t.Fatal("too large")
	}
	for _, n := range r.nodes {
		if f := files(n); len(f) != 0 {
			t.Fatalf("left behind: %v", f)
		}
	}
	if code, _ := r.get("a/b", 0, 1); code != 404 {
		t.Fatal("the object must not exist")
	}
}

// STO-006: a token for one call opens no other; a shard sees only its own keys.
func TestAuthAndShards(t *testing.T) {
	r := newRig(t, nil)
	r.mirror(0)
	body := []byte("shard one")
	if code, _ := r.put("k", body); code != 200 {
		t.Fatal("put")
	}
	tok := auth.SignTarget(secret1, "001", "GET", "/v1/stores/main/objects/k", time.Now().Unix())
	for name, h := range map[string]map[string]string{
		"no token":        {"X-Cinim-Shard": "001", "Range": "bytes=0-1"},
		"another shard":   {"Authorization": "Bearer " + tok, "X-Cinim-Shard": "002", "Range": "bytes=0-1"},
		"no shard header": {"Authorization": "Bearer " + tok, "Range": "bytes=0-1"},
	} {
		if code, _, _ := r.do("GET", "/v1/stores/main/objects/k", h, nil); code != 401 {
			t.Errorf("%s: %d", name, code)
		}
	}
	// a token for a read is no good for a delete
	if code, _, _ := r.do("DELETE", "/v1/stores/main/objects/k", map[string]string{"Authorization": "Bearer " + tok, "X-Cinim-Shard": "001"}, nil); code != 401 {
		t.Error("a read token deleted")
	}
	// the same key of shard 002 is another object
	t2 := "/v1/stores/main/objects/k"
	h := map[string]string{"Authorization": "Bearer " + auth.SignTarget("the-secret-of-shard-002-go-test", "002", "GET", t2, time.Now().Unix()), "X-Cinim-Shard": "002", "Range": "bytes=0-1"}
	if code, _, _ := r.do("GET", t2, h, nil); code != 404 {
		t.Errorf("shard 002 sees shard 001's object: %d", code)
	}
	if code, _ := r.admin("GET", "/v1/admin/stores", nil); code != 200 {
		t.Error("admin")
	}
	if code, _, _ := r.do("GET", "/v1/admin/stores", map[string]string{"Authorization": "Bearer wrong"}, nil); code != 401 {
		t.Error("admin with a wrong token")
	}
	if code, _, _ := r.core("PUT", "/v1/stores/main/objects/../x", "", map[string]string{"X-Content-SHA256": sha(body)}, body); code != 400 && code != 404 && code != 401 {
		t.Errorf("a key with .. : %d", code)
	}
}

// DAT-003 (D-61): a multipart upload by parts, with the digests the module made; blocks of a generation; read across the border of two blocks.
func TestMultipartAndShim(t *testing.T) {
	r := newRig(t, nil)
	r.mirror(0)
	code, data, _ := r.core("POST", "/v1/stores/main/objects/acme/big", "uploads&size=250000", nil, nil)
	var up struct {
		ID string `json:"upload_id"`
	}
	if code != 200 || json.Unmarshal(data, &up) != nil || up.ID == "" {
		t.Fatalf("begin %d %s", code, data)
	}
	whole := pattern(250000)
	parts := [][]byte{whole[:100000], whole[100000:200000], whole[200000:]}
	var etags [3]string
	for i, p := range parts {
		var code int
		var data []byte
		if i == 1 {
			// a part through the shim's port, under a grant
			grant := auth.SignGrant(secret1, "001", "main", "acme/big", up.ID, "w", time.Now().Unix())
			req, _ := http.NewRequest("PUT", fmt.Sprintf("%s/s/main/acme/big?upload=%s&part=2", r.shim.URL, up.ID), bytes.NewReader(p))
			req.Header.Set("Authorization", "Bearer "+grant)
			req.Header.Set("X-Content-SHA256", sha(p))
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			data, _ = io.ReadAll(resp.Body)
			resp.Body.Close()
			code = resp.StatusCode
		} else {
			code, data, _ = r.core("PUT", "/v1/stores/main/objects/acme/big", fmt.Sprintf("upload=%s&part=%d", up.ID, i+1), map[string]string{"X-Content-SHA256": sha(p)}, p)
		}
		var e struct{ Etag string }
		if code != 200 || json.Unmarshal(data, &e) != nil {
			t.Fatalf("part %d: %d %s", i+1, code, data)
		}
		etags[i] = e.Etag
	}
	code, data, _ = r.core("GET", "/v1/stores/main/objects/acme/big", "upload="+up.ID+"&parts", nil, nil)
	if code != 200 || !strings.Contains(string(data), sha(parts[1])) {
		t.Fatalf("parts %d %s", code, data)
	}
	// complete with a wrong etag, then right
	bad, _ := json.Marshal(map[string]any{"parts": [][]any{{1, etags[0]}, {2, "x"}, {3, etags[2]}}})
	if code, _, _ := r.core("POST", "/v1/stores/main/objects/acme/big", "upload="+up.ID, nil, bad); code != 400 {
		t.Fatalf("a wrong etag was accepted: %d", code)
	}
	ok, _ := json.Marshal(map[string]any{"parts": [][]any{{1, etags[0]}, {2, etags[1]}, {3, etags[2]}}})
	if code, data, _ := r.core("POST", "/v1/stores/main/objects/acme/big", "upload="+up.ID, nil, ok); code != 200 || !strings.Contains(string(data), "250000") {
		t.Fatalf("complete %d %s", code, data)
	}
	if code, data := r.get("acme/big", 99990, 200010); code != 206 || !bytes.Equal(data, whole[99990:200011]) {
		t.Fatalf("across two borders: %d %d", code, len(data))
	}
	// the same read through the shim's port, under a read grant
	grant := auth.SignGrant(secret1, "001", "main", "acme/big", "", "r", time.Now().Unix())
	req, _ := http.NewRequest("GET", r.shim.URL+"/s/main/acme/big", nil)
	req.Header.Set("Authorization", "Bearer "+grant)
	req.Header.Set("Range", "bytes=0-99")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 206 || !bytes.Equal(got, whole[:100]) {
		t.Fatalf("shim read %d", resp.StatusCode)
	}
	// a write grant does not read, and nothing else is open on that port
	req, _ = http.NewRequest("DELETE", r.shim.URL+"/s/main/acme/big", nil)
	req.Header.Set("Authorization", "Bearer "+grant)
	if resp, _ := http.DefaultClient.Do(req); resp.StatusCode == 200 {
		t.Fatal("the shim port deleted")
	}
	// an aborted upload leaves nothing
	_, data, _ = r.core("POST", "/v1/stores/main/objects/acme/gone", "uploads", nil, nil)
	_ = json.Unmarshal(data, &up)
	r.core("PUT", "/v1/stores/main/objects/acme/gone", "upload="+up.ID+"&part=1", map[string]string{"X-Content-SHA256": sha(parts[0])}, parts[0])
	if code, _, _ := r.core("DELETE", "/v1/stores/main/objects/acme/gone", "upload="+up.ID, nil, nil); code != 200 {
		t.Fatal("abort")
	}
	for _, n := range r.nodes {
		for _, f := range files(n) {
			if strings.Contains(f, "acme/gone") {
				t.Fatalf("the aborted upload left %s", f)
			}
		}
	}
}

// A block that does not match its digest on one node is read from the other (the read checks what it reads); with both bad it is an error and not wrong bytes.
func TestReadChecksBlocks(t *testing.T) {
	r := newRig(t, nil)
	r.mirror(0)
	body := pattern(4000)
	r.put("x/y", body)
	f := files(r.nodes[0])[0]
	if err := os.WriteFile(filepath.Join(r.nodes[0], f), []byte("damaged"), 0o644); err != nil {
		t.Fatal(err)
	}
	if code, data := r.get("x/y", 0, 3999); code != 206 || !bytes.Equal(data, body) {
		t.Fatalf("the good node was not used: %d", code)
	}
	if err := os.WriteFile(filepath.Join(r.nodes[1], f), bytes.Repeat([]byte("z"), 4000), 0o644); err != nil {
		t.Fatal(err)
	}
	if code, data := r.get("x/y", 0, 3999); code == 206 {
		t.Fatalf("wrong bytes were served: %d %q", code, data[:10])
	}
}

// the node that is away misses a write: the store still takes it with min_acks 1, and refuses with all required (DAT-004)
func TestMinAcks(t *testing.T) {
	down := func(r *rig) {
		// a node that refuses writes (a read-only directory)
		if err := os.Chmod(r.nodes[1], 0o500); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(r.nodes[1], 0o755) })
	}
	r := newRig(t, nil)
	r.mirror(1)
	down(r)
	if code, data := r.put("k/one", []byte("one")); code != 200 {
		t.Fatalf("min_acks 1: %d %s", code, data)
	}
	rows, _ := r.dbc.Query(context.Background(), rq.S("SELECT count(*) FROM storage_repair"))
	if rq.Int(rows[0][0]) != 1 {
		t.Fatalf("the missed write is not in the repair queue: %v", rows)
	}
	if code, data := r.get("k/one", 0, 2); code != 206 || string(data) != "one" {
		t.Fatalf("read with a node down: %d", code)
	}
	r2 := newRig(t, nil)
	r2.mirror(0)
	down(r2)
	if code, _ := r2.put("k/one", []byte("one")); code != 503 {
		t.Fatalf("all nodes required, one down: %d", code)
	}
	if f := files(r2.nodes[0]); len(f) != 0 {
		t.Fatalf("a write that was refused left blocks: %v", f)
	}
}

// STO-006: a full store is refused before a byte is written, with words (the incident of 2026-11-11)
func TestStoreFull(t *testing.T) {
	r := newRig(t, func(c *Config) { c.Reserve = 1 << 60 })
	r.mirror(0)
	code, data := r.put("k/a", []byte("x"))
	if code != 507 || !strings.Contains(string(data), "store_full") || !strings.Contains(string(data), "nearly full") {
		t.Fatalf("%d %s", code, data)
	}
	for _, n := range r.nodes {
		if f := files(n); len(f) != 0 {
			t.Fatalf("something was written: %v", f)
		}
	}
	if code, _, _ := r.core("POST", "/v1/stores/main/objects/k/b", "uploads&size=10", nil, nil); code != 507 {
		t.Fatal("begin of an upload in a full store")
	}
}

func TestAdmin(t *testing.T) {
	r := newRig(t, nil)
	code, data := r.admin("PUT", "/v1/admin/stores/bad", map[string]any{"kind": "mirror", "nodes": []any{map[string]any{"remote": ":nosuch:/x"}}})
	if code != 400 {
		t.Fatalf("an unknown backend: %d %s", code, data)
	}
	code, data = r.admin("PUT", "/v1/admin/stores/bad", map[string]any{"kind": "mirror", "nodes": []any{map[string]any{"remote": ":local:/proc/nowhere/at/all"}}})
	if code != 400 || !strings.Contains(string(data), "storage_unreachable") {
		t.Fatalf("a node that cannot be written: %d %s", code, data)
	}
	r.mirror(0)
	if code, data := r.admin("POST", "/v1/admin/stores/main:check", nil); code != 200 {
		t.Fatalf("check %d %s", code, data)
	}
	if code, _ := r.admin("POST", "/v1/admin/stores/nope:default", nil); code != 404 {
		t.Fatal("default of an unknown store")
	}
	code, data = r.admin("GET", "/v1/admin/stores", nil)
	if code != 200 || strings.Contains(string(data), "secret") && strings.Contains(string(data), "pass") {
		t.Fatalf("list %d %s", code, data)
	}
	r.put("k/one", []byte("one"))
	if code, _ := r.admin("DELETE", "/v1/admin/stores/main", nil); code != 409 {
		t.Fatal("a store that holds objects was forgotten")
	}
	if code, _ := r.admin("DELETE", "/v1/admin/stores/main?force=1", nil); code != 200 {
		t.Fatal("force")
	}
	if code, _, _ := r.do("GET", "/v1/openapi.yaml", nil, nil); code != 200 {
		t.Fatal("openapi")
	}
}

// fullFs is a node whose every write fails as a full disk (or a full FTP allocation) does; the others are real.
type fullFs struct {
	fs.Fs
	err error
}

func (f fullFs) Put(ctx context.Context, in io.Reader, src fs.ObjectInfo, opts ...fs.OpenOption) (fs.Object, error) {
	return nil, f.err
}

func (r *rig) nodeFull(i int, err error) {
	r.s.Reg.OpenHook = func(m *store.Mirror) { m.Nodes[i].F = fullFs{Fs: m.Nodes[i].F, err: err} }
}

// STO-006: when a node cannot say how much room it has, a block that does not fit is still refused with words and 507, never with a bare store error (the incident of 2026-11-11).
func TestNodeOutOfSpaceIsAnInformativeRefusal(t *testing.T) {
	for name, nodeErr := range map[string]error{
		"disk full":  &os.PathError{Op: "write", Path: "/data/x", Err: syscall.ENOSPC},
		"ftp 552":    errors.New("update stor: 552 Requested file action aborted. Exceeded storage allocation"),
		"webdav 507": errors.New("Put: 507 Insufficient Storage"),
	} {
		r := newRig(t, nil)
		r.mirror(0)
		r.nodeFull(1, nodeErr)
		code, data := r.put("acme/big", pattern(1000))
		if code != 507 || !strings.Contains(string(data), "store_full") || !strings.Contains(string(data), "out of space") || !strings.Contains(string(data), "local:") ||
			!strings.Contains(string(data), "cannot say beforehand") {
			t.Fatalf("%s: %d %s", name, code, data)
		}
		for _, n := range r.nodes {
			if f := files(n); len(f) != 0 {
				t.Fatalf("%s: a refused write left %v", name, f)
			}
		}
		if code, _ := r.get("acme/big", 0, 1); code != 404 {
			t.Fatalf("%s: the refused object exists", name)
		}
		// a part of a multipart upload says the same, also through the shim's port
		_, d, _ := r.core("POST", "/v1/stores/main/objects/acme/mp", "uploads", nil, nil)
		var u struct {
			ID string `json:"upload_id"`
		}
		_ = json.Unmarshal(d, &u)
		p := pattern(500)
		code, data, _ = r.core("PUT", "/v1/stores/main/objects/acme/mp", "upload="+u.ID+"&part=1", map[string]string{"X-Content-SHA256": sha(p)}, p)
		if code != 507 || !strings.Contains(string(data), "out of space") {
			t.Fatalf("%s: a part: %d %s", name, code, data)
		}
	}
	// a node that is full does not stop a store whose other nodes are enough (min_acks 1): the write is taken and the copy is queued
	r := newRig(t, nil)
	r.mirror(1)
	r.nodeFull(1, errors.New("552 Exceeded storage allocation"))
	if code, data := r.put("acme/ok", pattern(300)); code != 200 {
		t.Fatalf("min_acks 1: %d %s", code, data)
	}
	if r.dbCount("storage_repair") != 1 {
		t.Fatal("the missed copy is not queued")
	}
}

func (r *rig) dbCount(table string) int64 {
	rows, err := r.dbc.Query(context.Background(), rq.S("SELECT count(*) FROM "+table))
	if err != nil {
		r.t.Fatal(err)
	}
	return rq.Int(rows[0][0])
}

// The description of the API (openapi.yaml, served at /v1/openapi.yaml) and the routes of the server are the same set: an operation in one and not in the other fails here.
func TestOpenAPIMatchesRoutes(t *testing.T) {
	var spec struct {
		Paths map[string]map[string]any `yaml:"paths"`
	}
	if err := yaml.Unmarshal(openapiYAML, &spec); err != nil {
		t.Fatal(err)
	}
	inSpec := map[string]bool{}
	for p, ops := range spec.Paths {
		for m := range ops {
			if m == "parameters" {
				continue
			}
			inSpec[strings.ToUpper(m)+" "+p] = true
		}
	}
	// what the server does answer, as (method, template of the path): the concrete requests below are made from the templates
	impl := []string{
		"GET /healthz", "GET /readyz", "GET /v1/openapi.yaml", "GET /v1/stores",
		"PUT /v1/stores/{store}/objects/{key}", "GET /v1/stores/{store}/objects/{key}", "POST /v1/stores/{store}/objects/{key}", "DELETE /v1/stores/{store}/objects/{key}",
		"GET /v1/admin/stores", "PUT /v1/admin/stores/{name}", "DELETE /v1/admin/stores/{name}", "POST /v1/admin/stores/{name}:check", "POST /v1/admin/stores/{name}:default", "POST /v1/admin/stores/{name}:reconcile",
	}
	for _, o := range impl {
		if !inSpec[o] {
			t.Errorf("the server has %s and the description has not", o)
		}
		delete(inSpec, o)
	}
	for o := range inSpec {
		t.Errorf("the description has %s and the server has not", o)
	}
	// and every one of them is answered by a route (not "no such route", not "method not allowed")
	r := newRig(t, nil)
	r.mirror(0)
	for _, o := range impl {
		m, p, _ := strings.Cut(o, " ")
		p = strings.NewReplacer("{store}", "main", "{key}", "k/x", "{name}", "main").Replace(p)
		var code int
		var data []byte
		switch {
		case strings.HasPrefix(p, "/v1/admin/"):
			code, data = r.admin(m, p, map[string]any{"kind": "mirror", "nodes": []any{map[string]any{"remote": ":local:" + r.nodes[0]}}})
		case strings.HasPrefix(p, "/v1/stores"):
			code, data, _ = r.core(m, p, "", map[string]string{"Range": "bytes=0-1", "X-Content-SHA256": sha([]byte("x"))}, []byte("x"))
		default:
			code, data, _ = r.do(m, p, nil, nil)
		}
		if strings.Contains(string(data), `"not_found"`) || strings.Contains(string(data), `"method_not_allowed"`) {
			t.Errorf("%s: %d %s", o, code, data)
		}
	}
}
