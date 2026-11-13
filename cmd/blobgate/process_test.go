package main

// Tests of the program as a process, with a real rqlite of its own (Docker): a writer that is killed with SIGKILL in the middle of a block, and two replicas of which the one that holds the lease is
// killed. BLOBGATE_DOCKER_TESTS=1 runs them.

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/zystem/blobgate/internal/auth"
	"github.com/zystem/blobgate/internal/rq"
)

const (
	secret = "the-secret-of-shard-001-process-test"
	adminT = "an-admin-token-for-the-process-test"
)

func freePort(t *testing.T) int {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

func startRqlite(t *testing.T) string {
	if os.Getenv("BLOBGATE_DOCKER_TESTS") != "1" {
		t.Skip("BLOBGATE_DOCKER_TESTS=1 runs the tests of the program as a process")
	}
	name := fmt.Sprintf("blobgate-test-proc-rq-%d", os.Getpid())
	port := freePort(t)
	if out, err := exec.Command("docker", "run", "-d", "--name", name, "-p", fmt.Sprintf("127.0.0.1:%d:4001", port), "--tmpfs", "/rqlite/file:rw,size=512m,mode=1777", "rqlite/rqlite", "-fk", "-disco-mode=").CombinedOutput(); err != nil {
		t.Skipf("docker could not start rqlite: %v %s", err, out)
	}
	t.Cleanup(func() { _ = exec.Command("docker", "rm", "-f", name).Run() })
	url := fmt.Sprintf("http://127.0.0.1:%d", port)
	for i := 0; i < 60; i++ {
		time.Sleep(500 * time.Millisecond)
		if resp, err := http.Get(url + "/readyz"); err == nil {
			resp.Body.Close()
			if resp.StatusCode == 200 {
				return url
			}
		}
	}
	t.Fatal("rqlite did not come up")
	return ""
}

var builtBinary string

func binary(t *testing.T) string {
	if builtBinary != "" {
		return builtBinary
	}
	dir, err := os.MkdirTemp("", "blobgate-bin")
	if err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(dir, "blobgate")
	cmd := exec.Command("go", "build", "-o", bin, ".")
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build: %v %s", err, out)
	}
	builtBinary = bin
	return bin
}

type proc struct {
	cmd  *exec.Cmd
	port int
	log  *bytes.Buffer
}

func startModule(t *testing.T, rqURL string, env ...string) *proc {
	t.Helper()
	p := &proc{port: freePort(t), log: &bytes.Buffer{}}
	p.cmd = exec.Command(binary(t))
	p.cmd.Env = append(os.Environ(), "BLOBGATE_RQLITE_URL="+rqURL, fmt.Sprintf("BLOBGATE_PORT=%d", p.port), "BLOBGATE_SHIM_PORT=0",
		"BLOBGATE_TENANTS=001="+secret, "BLOBGATE_ADMIN_TOKEN="+adminT, "BLOBGATE_SEAL_KEY=a-seal-key-for-the-process-test")
	p.cmd.Env = append(p.cmd.Env, env...)
	p.cmd.Stdout, p.cmd.Stderr = p.log, p.log
	if err := p.cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = p.cmd.Process.Kill(); _ = p.cmd.Wait() })
	for i := 0; i < 100; i++ {
		if resp, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d/readyz", p.port)); err == nil {
			resp.Body.Close()
			if resp.StatusCode == 200 {
				return p
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("the module did not come up: %s", p.log)
	return nil
}

// kill9 ends the process with SIGKILL: nothing after it runs, no deferred call, no release of a lease.
func (p *proc) kill9(t *testing.T) {
	t.Helper()
	if err := p.cmd.Process.Signal(syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	_ = p.cmd.Wait()
}

func (p *proc) url(path string) string { return fmt.Sprintf("http://127.0.0.1:%d%s", p.port, path) }

func (p *proc) call(method, path, query string, hdr map[string]string, body []byte) (int, []byte) {
	t := path
	if query != "" {
		t += "?" + query
	}
	req, _ := http.NewRequest(method, p.url(t), bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+auth.SignTarget(secret, "001", method, t, time.Now().Unix()))
	req.Header.Set("X-Blobgate-Tenant", "001")
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, nil
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, data
}

func sha(b []byte) string { s := sha256.Sum256(b); return hex.EncodeToString(s[:]) }

func (p *proc) register(t *testing.T, nodes ...string) {
	var ns []any
	for _, n := range nodes {
		ns = append(ns, map[string]any{"remote": ":local:" + n})
	}
	b, _ := json.Marshal(map[string]any{"kind": "mirror", "default": true, "min_acks": 1, "nodes": ns})
	req, _ := http.NewRequest("PUT", p.url("/v1/admin/stores/main"), bytes.NewReader(b))
	req.Header.Set("Authorization", "Bearer "+adminT)
	resp, err := http.DefaultClient.Do(req)
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("register: %v %v", err, resp)
	}
	resp.Body.Close()
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

// slowPut starts a PUT of `size` bytes and sends only `sent` of them, then waits: the module is in the middle of a block.
func slowPut(t *testing.T, p *proc, key string, size, sent int) net.Conn {
	t.Helper()
	body := bytes.Repeat([]byte("z"), size)
	target := "/v1/stores/main/objects/" + key
	conn, err := net.Dial("tcp", fmt.Sprintf("127.0.0.1:%d", p.port))
	if err != nil {
		t.Fatal(err)
	}
	fmt.Fprintf(conn, "PUT %s HTTP/1.1\r\nHost: x\r\nAuthorization: Bearer %s\r\nX-Blobgate-Tenant: 001\r\nX-Content-SHA256: %s\r\nContent-Length: %d\r\n\r\n",
		target, auth.SignTarget(secret, "001", "PUT", target, time.Now().Unix()), sha(body), size)
	if _, err := conn.Write(body[:sent]); err != nil {
		t.Fatal(err)
	}
	return conn
}

func waitFor(t *testing.T, what string, within time.Duration, ok func() bool) {
	t.Helper()
	end := time.Now().Add(within)
	for time.Now().Before(end) {
		if ok() {
			return
		}
		time.Sleep(300 * time.Millisecond)
	}
	t.Fatalf("not within %v: %s", within, what)
}

// DAT-004: the process is killed with SIGKILL in the middle of a block, of a new object and of the replacement of one; after the restart the object that was there is as it was, the one that was not is
// not, and the blocks that the dead writer left are found by the sweep and removed.
func TestKillNineInTheMiddleOfAWrite(t *testing.T) {
	rqURL := startRqlite(t)
	dir := t.TempDir()
	n1, n2 := filepath.Join(dir, "n1"), filepath.Join(dir, "n2")
	env := []string{"BLOBGATE_UPLOAD_TTL_SECONDS=2", "BLOBGATE_REPAIR_SECONDS=1", "BLOBGATE_REPAIR_RETRY_SECONDS=1"}
	p := startModule(t, rqURL, env...)
	p.register(t, n1, n2)
	keep := bytes.Repeat([]byte("k"), 70000)
	if code, d := p.call("PUT", "/v1/stores/main/objects/acme/keep", "", map[string]string{"X-Content-SHA256": sha(keep)}, keep); code != 200 {
		t.Fatalf("put %d %s", code, d)
	}
	before := files(n1)
	if len(before) != 1 {
		t.Fatalf("%v", before)
	}
	// two writers in the middle of a block of 8 MiB: a new key and a replacement of `keep`
	c1 := slowPut(t, p, "acme/new", 8<<20, 3<<20)
	c2 := slowPut(t, p, "acme/keep", 8<<20, 3<<20)
	defer c1.Close()
	defer c2.Close()
	waitFor(t, "the partial blocks of the writers on the nodes", 10*time.Second, func() bool { return len(files(n1)) >= 3 })
	p.kill9(t)
	if len(files(n1)) < 3 {
		t.Fatalf("the crash left nothing to clean: %v", files(n1))
	}
	// a new process, on the same database and the same nodes
	p2 := startModule(t, rqURL, env...)
	waitFor(t, "the nodes hold only the block of `keep`", 30*time.Second, func() bool {
		a, b := files(n1), files(n2)
		return len(a) == 1 && len(b) <= 1 && a[0] == before[0]
	})
	if code, d := p2.call("GET", "/v1/stores/main/objects/acme/keep", "", map[string]string{"Range": "bytes=0-69999"}, nil); code != 206 || !bytes.Equal(d, keep) {
		t.Fatalf("keep after the crash: %d", code)
	}
	if code, _ := p2.call("GET", "/v1/stores/main/objects/acme/new", "", map[string]string{"Range": "bytes=0-9"}, nil); code != 404 {
		t.Fatalf("an object that was not fixed exists: %d", code)
	}
	db := rq.New(rqURL)
	for _, tb := range []string{"storage_uploads", "storage_parts", "storage_repair"} {
		rows, _ := db.Query(context.Background(), rq.S("SELECT count(*) FROM "+tb))
		if rq.Int(rows[0][0]) != 0 {
			t.Errorf("%s has rows after the sweep", tb)
		}
	}
}

// Two replicas on one database: one holds the lease and does the repair; it is killed with SIGKILL (it lets go of nothing); the other takes the lease when it runs out and does the repair that is waiting.
func TestTheReplicaThatTakesOver(t *testing.T) {
	rqURL := startRqlite(t)
	dir := t.TempDir()
	n1, n2 := filepath.Join(dir, "n1"), filepath.Join(dir, "n2")
	env := []string{"BLOBGATE_LEASE_SECONDS=4", "BLOBGATE_REPAIR_SECONDS=1", "BLOBGATE_REPAIR_RETRY_SECONDS=1", "BLOBGATE_CHECK_SECONDS=3600"}
	a := startModule(t, rqURL, env...)
	a.register(t, n1, n2)
	db := rq.New(rqURL)
	holder := func() string {
		rows, err := db.Query(context.Background(), rq.S("SELECT holder FROM storage_lease WHERE name = 'repair' AND until > ?", time.Now().Unix()))
		if err != nil || len(rows) == 0 {
			return ""
		}
		return rq.Str(rows[0][0])
	}
	waitFor(t, "the first replica holds the lease", 15*time.Second, func() bool { return holder() != "" })
	first := holder()
	b := startModule(t, rqURL, env...)
	_ = b
	time.Sleep(3 * time.Second)
	if holder() != first {
		t.Fatal("the lease changed hands while its holder was alive")
	}
	// a write that one node misses: it is queued, and the replica that holds the lease will not be there to do it
	if err := os.Chmod(n2, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(n2, 0o755) })
	body := bytes.Repeat([]byte("r"), 5000)
	if code, d := a.call("PUT", "/v1/stores/main/objects/acme/r", "", map[string]string{"X-Content-SHA256": sha(body)}, body); code != 200 {
		t.Fatalf("put with a node down: %d %s", code, d)
	}
	a.kill9(t)
	if err := os.Chmod(n2, 0o755); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the other replica holds the lease", 30*time.Second, func() bool { h := holder(); return h != "" && h != first })
	waitFor(t, "the block is given to the node that missed it", 30*time.Second, func() bool { return len(files(n2)) == 1 })
	rows, _ := db.Query(context.Background(), rq.S("SELECT count(*) FROM storage_repair"))
	if rq.Int(rows[0][0]) != 0 {
		t.Fatal("the repair queue is not empty")
	}
	if code, d := b.call("GET", "/v1/stores/main/objects/acme/r", "", map[string]string{"Range": "bytes=0-4999"}, nil); code != 206 || !bytes.Equal(d, body) {
		t.Fatalf("read through the replica that took over: %d", code)
	}
	_ = strings.TrimSpace
}

// The secrets can be files (NAME_FILE) and the ports can be TLS: the program starts with no secret in its environment, and answers on https with a certificate that a client verifies.
func TestSecretsFromFilesAndTLS(t *testing.T) {
	rqURL := startRqlite(t)
	dir := t.TempDir()
	put := func(name, content string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(content+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "blobgate-test"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, IPAddresses: []net.IP{net.ParseIP("127.0.0.1")}}
	der, _ := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	kb, _ := x509.MarshalPKCS8PrivateKey(key)
	certFile := put("tls.crt", strings.TrimSpace(string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))))
	keyFile := put("tls.key", strings.TrimSpace(string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: kb}))))
	cert, _ := x509.ParseCertificate(der)
	pool := x509.NewCertPool()
	pool.AddCert(cert)

	p := &proc{port: freePort(t), log: &bytes.Buffer{}}
	p.cmd = exec.Command(binary(t))
	p.cmd.Env = append(os.Environ(), "BLOBGATE_RQLITE_URL="+rqURL, fmt.Sprintf("BLOBGATE_PORT=%d", p.port), "BLOBGATE_SHIM_PORT=0",
		"BLOBGATE_TENANTS=", "BLOBGATE_TENANTS_FILE="+put("shards", "001="+secret),
		"BLOBGATE_ADMIN_TOKEN=", "BLOBGATE_ADMIN_TOKEN_FILE="+put("admin", adminT),
		"BLOBGATE_SEAL_KEY=", "BLOBGATE_SEAL_KEY_FILE="+put("seal", "a-seal-key-for-the-file-test"),
		"BLOBGATE_TLS_CERT_FILE="+certFile, "BLOBGATE_TLS_KEY_FILE="+keyFile)
	p.cmd.Stdout, p.cmd.Stderr = p.log, p.log
	if err := p.cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = p.cmd.Process.Kill(); _ = p.cmd.Wait() })
	client := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool}}}
	waitFor(t, "the module answers on https", 20*time.Second, func() bool {
		resp, err := client.Get(fmt.Sprintf("https://127.0.0.1:%d/readyz", p.port))
		if err != nil {
			return false
		}
		resp.Body.Close()
		return resp.StatusCode == 200
	})
	if resp, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d/readyz", p.port)); err == nil {
		resp.Body.Close()
		if resp.StatusCode == 200 {
			t.Fatal("plain http was answered on a TLS port")
		}
	}
	if _, err := (&http.Client{}).Get(fmt.Sprintf("https://127.0.0.1:%d/readyz", p.port)); err == nil {
		t.Fatal("a client that does not trust the certificate was served")
	}
	// the admin token read from its file is the one that works
	req, _ := http.NewRequest("GET", fmt.Sprintf("https://127.0.0.1:%d/v1/admin/stores", p.port), nil)
	req.Header.Set("Authorization", "Bearer "+adminT)
	if resp, err := client.Do(req); err != nil || resp.StatusCode != 200 {
		t.Fatalf("admin with the token of the file: %v %v", err, resp)
	}
	// both the value and the file: refused at start, with words
	bad := exec.Command(binary(t))
	bad.Env = append(os.Environ(), "BLOBGATE_RQLITE_URL="+rqURL, "BLOBGATE_TENANTS=001="+secret, "BLOBGATE_SEAL_KEY=a-seal-key-for-the-file-test", "BLOBGATE_ADMIN_TOKEN=an-admin-token-for-the-test", "BLOBGATE_ADMIN_TOKEN_FILE="+put("admin2", adminT))
	if out, err := bad.CombinedOutput(); err == nil || !strings.Contains(string(out), "both set") {
		t.Fatalf("a value and a file together: %v %s", err, out)
	}
}

// A deployment with the old names of the settings (CINIM_STORAGE_*) starts and serves: nothing has to be renamed on the day of the update.
func TestOldNamesOfTheSettings(t *testing.T) {
	rqURL := startRqlite(t)
	p := &proc{port: freePort(t), log: &bytes.Buffer{}}
	p.cmd = exec.Command(binary(t))
	p.cmd.Env = append(os.Environ(), "CINIM_RQLITE_URL="+rqURL, fmt.Sprintf("CINIM_STORAGE_PORT=%d", p.port), "CINIM_STORAGE_SHIM_PORT=0",
		"CINIM_STORAGE_SHARDS=001="+secret, "CINIM_STORAGE_ADMIN_TOKEN="+adminT, "CINIM_STORAGE_SEAL_KEY=a-seal-key-for-the-old-names")
	p.cmd.Stdout, p.cmd.Stderr = p.log, p.log
	if err := p.cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = p.cmd.Process.Kill(); _ = p.cmd.Wait() })
	waitFor(t, "the module answers", 20*time.Second, func() bool {
		resp, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d/readyz", p.port))
		if err != nil {
			return false
		}
		resp.Body.Close()
		return resp.StatusCode == 200
	})
	req, _ := http.NewRequest("GET", p.url("/v1/admin/stores"), nil)
	req.Header.Set("Authorization", "Bearer "+adminT)
	if resp, err := http.DefaultClient.Do(req); err != nil || resp.StatusCode != 200 {
		t.Fatalf("admin with the old names: %v %v", err, resp)
	}
}
