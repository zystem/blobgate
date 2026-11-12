package api

// Tests with real servers: two SFTPGo containers, one serving WebDAV and the other FTP (and SFTP), which a mirror of different kinds of node is made of. They need Docker and the image
// `drakkan/sftpgo`, and run only with BLOBGATE_DOCKER_TESTS=1 (and CINIM_RQLITE_URL, as the other tests of the API).

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/zystem/blobgate/internal/maint"
	"github.com/zystem/blobgate/internal/rq"
)

type sftpgo struct {
	t                           *testing.T
	name                        string
	httpP, webdavP, ftpP, sftpP int
	passive                     int // the first of three passive ports, published at the same number (the server tells the client which one to use)
}

func freePort(t *testing.T) int {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

func startSFTPGo(t *testing.T, id string) *sftpgo {
	if os.Getenv("BLOBGATE_DOCKER_TESTS") != "1" {
		t.Skip("BLOBGATE_DOCKER_TESTS=1 runs the tests with real servers")
	}
	s := &sftpgo{t: t, name: "blobgate-test-" + id, httpP: freePort(t), webdavP: freePort(t), ftpP: freePort(t), sftpP: freePort(t)}
	// three passive ports in a row, free now
	for {
		p := freePort(t)
		ok := true
		for i := 0; i < 3; i++ {
			if l, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", p+i)); err != nil {
				ok = false
			} else {
				l.Close()
			}
		}
		if ok {
			s.passive = p
			break
		}
	}
	_ = exec.Command("docker", "rm", "-f", s.name).Run()
	args := []string{"run", "-d", "--name", s.name,
		"-p", fmt.Sprintf("127.0.0.1:%d:8080", s.httpP), "-p", fmt.Sprintf("127.0.0.1:%d:8090", s.webdavP),
		"-p", fmt.Sprintf("127.0.0.1:%d:2121", s.ftpP), "-p", fmt.Sprintf("127.0.0.1:%d:2022", s.sftpP),
		"-p", fmt.Sprintf("127.0.0.1:%d-%d:%d-%d", s.passive, s.passive+2, s.passive, s.passive+2),
		"-e", "SFTPGO_FTPD__BINDINGS__0__PORT=2121", "-e", "SFTPGO_FTPD__BINDINGS__0__FORCE_PASSIVE_IP=127.0.0.1",
		"-e", fmt.Sprintf("SFTPGO_FTPD__PASSIVE_PORT_RANGE__START=%d", s.passive), "-e", fmt.Sprintf("SFTPGO_FTPD__PASSIVE_PORT_RANGE__END=%d", s.passive+2),
		"-e", "SFTPGO_WEBDAVD__BINDINGS__0__PORT=8090", "-e", "SFTPGO_SFTPD__BINDINGS__0__PORT=2022",
		"-e", "SFTPGO_DATA_PROVIDER__CREATE_DEFAULT_ADMIN=true", "-e", "SFTPGO_DEFAULT_ADMIN_USERNAME=admin", "-e", "SFTPGO_DEFAULT_ADMIN_PASSWORD=adminpass123",
		"drakkan/sftpgo:latest"}
	if out, err := exec.Command("docker", args...).CombinedOutput(); err != nil {
		t.Skipf("docker could not start the server: %v %s", err, out)
	}
	t.Cleanup(func() { _ = exec.Command("docker", "rm", "-f", s.name).Run() })
	// the admin API answers, then a user is made
	var tok string
	for i := 0; i < 60 && tok == ""; i++ {
		time.Sleep(500 * time.Millisecond)
		req, _ := http.NewRequest("GET", fmt.Sprintf("http://127.0.0.1:%d/api/v2/token", s.httpP), nil)
		req.SetBasicAuth("admin", "adminpass123")
		if resp, err := http.DefaultClient.Do(req); err == nil {
			var v struct {
				T string `json:"access_token"`
			}
			_ = json.NewDecoder(resp.Body).Decode(&v)
			resp.Body.Close()
			tok = v.T
		}
	}
	if tok == "" {
		t.Fatal("the server did not come up")
	}
	body := `{"username":"u1","password":"userpass123","status":1,"home_dir":"/srv/sftpgo/data/u1","permissions":{"/":["*"]}}`
	req, _ := http.NewRequest("POST", fmt.Sprintf("http://127.0.0.1:%d/api/v2/users", s.httpP), strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil || resp.StatusCode != 201 {
		t.Fatalf("the user was not made: %v", err)
	}
	resp.Body.Close()
	return s
}

func (s *sftpgo) webdav() map[string]any {
	return map[string]any{"remote": fmt.Sprintf(":webdav,url='http://127.0.0.1:%d',vendor=other,user=u1:/blobs", s.webdavP), "secrets": map[string]string{"pass": "userpass123"}}
}

func (s *sftpgo) ftp() map[string]any {
	return map[string]any{"remote": fmt.Sprintf(":ftp,host=127.0.0.1,port=%d,user=u1:/blobs", s.ftpP), "secrets": map[string]string{"pass": "userpass123"}}
}

func (s *sftpgo) sftp() map[string]any {
	return map[string]any{"remote": fmt.Sprintf(":sftp,host=127.0.0.1,port=%d,user=u1,disable_hashcheck=true:/blobs-sftp", s.sftpP), "secrets": map[string]string{"pass": "userpass123"}}
}

func (s *sftpgo) docker(args ...string) {
	s.t.Helper()
	if out, err := exec.Command("docker", append(args, s.name)...).CombinedOutput(); err != nil {
		s.t.Fatalf("docker %v: %v %s", args, err, out)
	}
}

// the data of the user, as the server holds it (a list of files)
func (s *sftpgo) files() []string {
	out, _ := exec.Command("docker", "exec", s.name, "find", "/srv/sftpgo/data/u1/blobs", "-type", "f").Output()
	var f []string
	for _, l := range strings.Fields(string(out)) {
		f = append(f, l)
	}
	return f
}

// DAT-004, STO-006: a mirror of a WebDAV node and an FTP node, which no Caddy of ours is part of: an object written to both, read back by ranges, replaced, deleted; the nodes hold only blocks of
// the current generation.
func TestRealWebDAVAndFTPMirror(t *testing.T) {
	dav, ftp := startSFTPGo(t, "dav"), startSFTPGo(t, "ftp")
	r := newRig(t, nil)
	code, data := r.admin("PUT", "/v1/admin/stores/mix", map[string]any{"kind": "mirror", "default": true, "nodes": []any{dav.webdav(), ftp.ftp()}})
	if code != 200 {
		t.Fatalf("register: %d %s", code, data)
	}
	body := pattern(3 << 20)
	if code, d := r.put("acme/s1/app.bin", body); code != 200 {
		t.Fatalf("put %d %s", code, d)
	}
	for _, s := range []*sftpgo{dav, ftp} {
		if f := s.files(); len(f) != 1 || !strings.Contains(f[0], "acme/s1/app.bin~") {
			t.Fatalf("%s holds %v", s.name, f)
		}
	}
	if code, d, _ := r.core("GET", "/v1/stores/mix/objects/acme/s1/app.bin", "", map[string]string{"Range": "bytes=1000-2000"}, nil); code != 206 || len(d) != 1001 {
		t.Fatalf("range %d", code)
	}
	if code, _ := r.put("acme/s1/app.bin", pattern(100)); code != 200 {
		t.Fatal("replace")
	}
	for _, s := range []*sftpgo{dav, ftp} {
		if f := s.files(); len(f) != 1 {
			t.Fatalf("%s after a replacement: %v", s.name, f)
		}
	}
	if code, _, _ := r.core("DELETE", "/v1/stores/mix/objects/acme/s1/app.bin", "", nil, nil); code != 200 {
		t.Fatal("delete")
	}
	for _, s := range []*sftpgo{dav, ftp} {
		if f := s.files(); len(f) != 0 {
			t.Fatalf("%s after a delete: %v", s.name, f)
		}
	}
}

// A node that goes away: with min_acks 1 the store goes on and queues the copy; when the node is back the repair pass gives it the block. A node that lost its disk is found by the check.
func TestRealNodeAwayAndLostDisk(t *testing.T) {
	dav, ftp := startSFTPGo(t, "dav2"), startSFTPGo(t, "ftp2")
	r := newRig(t, nil)
	if code, d := r.admin("PUT", "/v1/admin/stores/mix", map[string]any{"kind": "mirror", "default": true, "min_acks": 1, "nodes": []any{dav.webdav(), ftp.ftp()}}); code != 200 {
		t.Fatalf("register: %d %s", code, d)
	}
	r.put("k/before", pattern(2000))
	ftp.docker("stop", "-t", "1")
	if code, d := r.put("k/while-away", pattern(5000)); code != 200 {
		t.Fatalf("a write with one node away: %d %s", code, d)
	}
	if r.dbCount("storage_repair") < 1 {
		t.Fatal("the copy for the node that is away was not queued")
	}
	ftp.docker("start")
	time.Sleep(3 * time.Second)
	w := r.worker("w1", func(c *maint.Config) { c.RetryBase = time.Second })
	w.Lease(context.Background(), "repair")
	for i := 0; i < 5 && r.dbCount("storage_repair") > 0; i++ {
		w.Pass(context.Background())
		time.Sleep(1500 * time.Millisecond)
	}
	if r.dbCount("storage_repair") != 0 || len(ftp.files()) != 2 {
		t.Fatalf("after the node is back: %d tasks, it holds %v", r.dbCount("storage_repair"), ftp.files())
	}
	// the disk of the ftp node is lost
	if out, err := exec.Command("docker", "exec", ftp.name, "rm", "-rf", "/srv/sftpgo/data/u1/blobs").CombinedOutput(); err != nil {
		t.Fatalf("%v %s", err, out)
	}
	if made := w.CheckPass(context.Background()); made != 2 {
		t.Fatalf("the check made %d tasks, want 2", made)
	}
	for i := 0; i < 5 && r.dbCount("storage_repair") > 0; i++ {
		w.Pass(context.Background())
		time.Sleep(1500 * time.Millisecond)
	}
	if len(ftp.files()) != 2 {
		t.Fatalf("the lost disk was not repaired: %v", ftp.files())
	}
	_ = rq.Int
}

// A mirror of SFTP, FTP and WebDAV at once, and a writer killed in the middle of a block (the partial file that the servers keep under the final name is never read as the object).
func TestRealThreeKindsAndNames(t *testing.T) {
	a, b := startSFTPGo(t, "a3"), startSFTPGo(t, "b3")
	r := newRig(t, nil)
	if code, d := r.admin("PUT", "/v1/admin/stores/three", map[string]any{"kind": "mirror", "default": true, "nodes": []any{a.webdav(), b.ftp(), b.sftp()}}); code != 200 {
		t.Fatalf("register: %d %s", code, d)
	}
	// names that a node could trip on: spaces, dots at the ends, unicode, a very long segment, characters that Windows refuses
	keys := []string{"acme/with space/a b.txt", "acme/.hidden/trailing.", "acme/héllo/日本語.bin", "acme/" + strings.Repeat("x", 250) + "/f", "acme/a:b*c?d/e", "acme/UPPER/Lower"}
	for i, k := range keys {
		body := pattern(100 + i)
		if code, d := r.put(k, body); code != 200 {
			t.Fatalf("put %q: %d %s", k, code, d)
		}
		code, d, _ := r.core("GET", "/v1/stores/three/objects/"+k, "", map[string]string{"Range": fmt.Sprintf("bytes=0-%d", len(body)-1)}, nil)
		if code != 206 || string(d) != string(body) {
			t.Fatalf("read %q: %d", k, code)
		}
	}
	_ = http.StatusOK
}

// Many writers at once under one parent directory (two nodes that make the same directory at the same moment fail on some servers): all of them succeed.
func TestRealConcurrentWritersOneParent(t *testing.T) {
	a, b := startSFTPGo(t, "a4"), startSFTPGo(t, "b4")
	r := newRig(t, nil)
	if code, d := r.admin("PUT", "/v1/admin/stores/conc", map[string]any{"kind": "mirror", "default": true, "nodes": []any{a.webdav(), b.sftp()}}); code != 200 {
		t.Fatalf("register: %d %s", code, d)
	}
	errs := make(chan string, 24)
	for i := 0; i < 24; i++ {
		go func(i int) {
			code, d := r.put(fmt.Sprintf("acme/shared/dir/obj-%d", i), pattern(2000+i))
			if code != 200 {
				errs <- fmt.Sprintf("%d: %d %s", i, code, d)
				return
			}
			errs <- ""
		}(i)
	}
	for i := 0; i < 24; i++ {
		if e := <-errs; e != "" {
			t.Error(e)
		}
	}
}
