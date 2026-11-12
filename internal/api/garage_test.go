package api

// A test with a real Garage (an S3 store) through rclone: the same code path as the nodes of a mirror, with the room taken from Garage's metrics. It needs Docker and the image
// `dxflrs/garage`, and runs only with BLOBGATE_DOCKER_TESTS=1.

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

type garage struct {
	t                     *testing.T
	name                  string
	s3P, adminP           int
	keyID, secret, bucket string
}

func (g *garage) cli(args ...string) string {
	g.t.Helper()
	out, err := exec.Command("docker", append([]string{"exec", g.name, "/garage"}, args...)...).CombinedOutput()
	if err != nil {
		g.t.Fatalf("garage %v: %v %s", args, err, out)
	}
	return string(out)
}

func startGarage(t *testing.T) *garage {
	if os.Getenv("BLOBGATE_DOCKER_TESTS") != "1" {
		t.Skip("BLOBGATE_DOCKER_TESTS=1 runs the tests with real servers")
	}
	g := &garage{t: t, name: "blobgate-test-garage", s3P: freePort(t), adminP: freePort(t), bucket: "blobgate-test"}
	dir := t.TempDir()
	for _, d := range []string{"meta", "data"} {
		if err := os.Mkdir(filepath.Join(dir, d), 0o777); err != nil {
			t.Fatal(err)
		}
		_ = os.Chmod(filepath.Join(dir, d), 0o777)
	}
	conf := filepath.Join(dir, "garage.toml")
	toml := `metadata_dir = "/meta"
data_dir = "/data"
db_engine = "sqlite"
replication_factor = 1
rpc_bind_addr = "127.0.0.1:3901"
rpc_secret = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
[s3_api]
s3_region = "garage"
api_bind_addr = "0.0.0.0:3900"
[admin]
api_bind_addr = "0.0.0.0:3903"
`
	if err := os.WriteFile(conf, []byte(toml), 0o644); err != nil {
		t.Fatal(err)
	}
	_ = exec.Command("docker", "rm", "-f", g.name).Run()
	out, err := exec.Command("docker", "run", "-d", "--name", g.name, "--user", fmt.Sprintf("%d:%d", os.Getuid(), os.Getgid()), "-p", fmt.Sprintf("127.0.0.1:%d:3900", g.s3P), "-p", fmt.Sprintf("127.0.0.1:%d:3903", g.adminP),
		"-v", conf+":/etc/garage.toml", "-v", filepath.Join(dir, "meta")+":/meta", "-v", filepath.Join(dir, "data")+":/data", "dxflrs/garage:v2.4.1").CombinedOutput()
	if err != nil {
		t.Skipf("docker could not start garage: %v %s", err, out)
	}
	t.Cleanup(func() { _ = exec.Command("docker", "rm", "-f", g.name).Run() })
	var id string
	for i := 0; i < 40 && id == ""; i++ {
		time.Sleep(500 * time.Millisecond)
		if o, err := exec.Command("docker", "exec", g.name, "/garage", "node", "id").CombinedOutput(); err == nil {
			id = regexp.MustCompile(`(?m)^[0-9a-f]{64}`).FindString(string(o))
		}
	}
	if id == "" {
		t.Fatal("garage did not come up")
	}
	g.cli("layout", "assign", "-z", "dc1", "-c", "1G", id)
	g.cli("layout", "apply", "--version", "1")
	time.Sleep(2 * time.Second)
	k := g.cli("key", "create", "k1")
	g.keyID = regexp.MustCompile(`Key ID:\s+(\S+)`).FindStringSubmatch(k)[1]
	g.secret = regexp.MustCompile(`Secret key:\s+(\S+)`).FindStringSubmatch(k)[1]
	g.cli("bucket", "create", g.bucket)
	g.cli("bucket", "allow", "--read", "--write", "--owner", g.bucket, "--key", "k1")
	return g
}

func (g *garage) store(extra map[string]any) map[string]any {
	m := map[string]any{"kind": "s3", "endpoint": fmt.Sprintf("http://127.0.0.1:%d", g.s3P), "region": "garage", "bucket": g.bucket,
		"access_key_id": g.keyID, "secret_access_key": g.secret, "space_url": fmt.Sprintf("http://127.0.0.1:%d/metrics", g.adminP), "default": true}
	for k, v := range extra {
		m[k] = v
	}
	return m
}

// STO-006, DAT-003: Garage through rclone: an object, a replacement, a multipart upload with ranges across the blocks, a delete; and the room that Garage's metrics say is what a refusal is made from.
func TestRealGarage(t *testing.T) {
	g := startGarage(t)
	r := newRig(t, nil)
	if code, d := r.admin("PUT", "/v1/admin/stores/garage-main", g.store(nil)); code != 200 {
		t.Fatalf("register: %d %s", code, d)
	}
	if code, d := r.admin("POST", "/v1/admin/stores/garage-main:check", nil); code != 200 {
		t.Fatalf(":check %d %s", code, d)
	}
	body := pattern(3<<20 + 17)
	if code, d := r.put("acme/s1/app.bin", body); code != 200 {
		t.Fatalf("put %d %s", code, d)
	}
	if code, d := r.getFrom("garage-main", "acme/s1/app.bin", 1<<20, 1<<20+99); code != 206 || string(d) != string(body[1<<20:1<<20+100]) {
		t.Fatalf("range %d", code)
	}
	if code, _ := r.put("acme/s1/app.bin", pattern(10)); code != 200 {
		t.Fatal("replace")
	}
	if code, d := r.getFrom("garage-main", "acme/s1/app.bin", 0, 9); code != 206 || string(d) != string(pattern(10)) {
		t.Fatalf("read the replacement %d", code)
	}
	// a multipart upload of three blocks, read across the borders
	_, d, _ := r.core("POST", "/v1/stores/garage-main/objects/acme/s1/big", "uploads", nil, nil)
	var up struct {
		ID string `json:"upload_id"`
	}
	_ = json.Unmarshal(d, &up)
	whole := pattern(2*BlockSize + 12345)
	var parts [][]any
	for i, off := 0, 0; off < len(whole); i, off = i+1, off+BlockSize {
		p := whole[off:min(off+BlockSize, len(whole))]
		code, data, _ := r.core("PUT", "/v1/stores/garage-main/objects/acme/s1/big", fmt.Sprintf("upload=%s&part=%d", up.ID, i+1), map[string]string{"X-Content-SHA256": sha(p)}, p)
		var e struct{ Etag string }
		if code != 200 || json.Unmarshal(data, &e) != nil {
			t.Fatalf("part %d: %d %s", i+1, code, data)
		}
		parts = append(parts, []any{i + 1, e.Etag})
	}
	cb, _ := json.Marshal(map[string]any{"parts": parts})
	if code, d, _ := r.core("POST", "/v1/stores/garage-main/objects/acme/s1/big", "upload="+up.ID, nil, cb); code != 200 {
		t.Fatalf("complete %d %s", code, d)
	}
	if code, d := r.getFrom("garage-main", "acme/s1/big", BlockSize-50, BlockSize+49); code != 206 || string(d) != string(whole[BlockSize-50:BlockSize+50]) {
		t.Fatalf("across a border %d", code)
	}
	if code, _, _ := r.core("DELETE", "/v1/stores/garage-main/objects/acme/s1/big", "", nil, nil); code != 200 {
		t.Fatal("delete")
	}
	if code, _ := r.getFrom("garage-main", "acme/s1/big", 0, 1); code != 404 {
		t.Fatal("deleted, and read")
	}
	if o := g.cli("bucket", "info", g.bucket); !strings.Contains(o, "Objects:") {
		t.Fatalf("bucket info: %s", o)
	}
}

// The room comes from Garage's metrics (space_url): with a reserve larger than the volume an upload is refused with 507 and the figures, before a byte is written.
func TestRealGarageRoom(t *testing.T) {
	g := startGarage(t)
	r := newRig(t, func(c *Config) { c.Reserve = 1 << 60 })
	if code, d := r.admin("PUT", "/v1/admin/stores/garage-main", g.store(nil)); code != 200 {
		// the registration check writes a block: the refusal is the room check, so it is registered through a module with a normal reserve first
		_ = d
	}
	r2 := newRig(t, nil)
	if code, d := r2.admin("PUT", "/v1/admin/stores/garage-main", g.store(nil)); code != 200 {
		t.Fatalf("register: %d %s", code, d)
	}
	r.s.Reg = r2.s.Reg
	code, data := r.put("acme/k", []byte("x"))
	if code != 507 || !strings.Contains(string(data), "nearly full") || !strings.Contains(string(data), "GiB free of") {
		t.Fatalf("%d %s", code, data)
	}
}
