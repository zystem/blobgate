package api

// Tests with real servers: two SFTPGo containers, one serving WebDAV and the other FTP (and SFTP), which a mirror of different kinds of node is made of. They need Docker and the image
// `drakkan/sftpgo`, and run only with BLOBGATE_DOCKER_TESTS=1 (and BLOBGATE_RQLITE_URL, as the other tests of the API).

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math/rand/v2"
	"net"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"

	"github.com/zystem/blobgate/internal/maint"
	"github.com/zystem/blobgate/internal/rq"
)

type sftpgo struct {
	t                           *testing.T
	name                        string
	httpP, webdavP, ftpP, sftpP int
	passive                     int // the first of three passive ports, published at the same number (the server tells the client which one to use)
}

// A port for a server of a test: free now, and below the range the kernel takes the ports of outgoing connections from (32768 and up), so that a connection of another test cannot take it before it is
// used. The packages of tests run side by side and each has its own stretch: here 23000..25999.
var (
	portMu sync.Mutex
	portAt = rand.IntN(3000)
)

func freePort(t *testing.T) int {
	t.Helper()
	portMu.Lock()
	defer portMu.Unlock()
	for range 3000 {
		portAt++
		p := 23000 + portAt%3000
		if l, err := net.Listen("tcp", fmt.Sprintf(":%d", p)); err == nil {
			l.Close()
			return p
		}
	}
	t.Fatal("no free port")
	return 0
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
	return strings.Fields(string(out))
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

// The S3 door, deduplication, the copies and the status page on real servers: a mirror of a WebDAV node and an FTP node (SFTPGo) and a store that is a Garage bucket. Objects of 20 MiB in parts, a
// copy that stores nothing, a copy by ranges, a copy between the two kinds of store, deletes that leave nothing on the nodes, and a node that goes away (dead on the page, lagging when a copy waits,
// green again when the repair has run).
func TestRealS3DoorDedupCopyAndStatus(t *testing.T) {
	dav, ftp, g := startSFTPGo(t, "dav5"), startSFTPGo(t, "ftp5"), startGarage(t)
	r := newRig(t, nil)
	mixNodes := []any{dav.webdav(), ftp.ftp()}
	mixNodes[0].(map[string]any)["label"], mixNodes[1].(map[string]any)["label"] = "dav-box", "ftp-box"
	if code, d := r.admin("PUT", "/v1/admin/stores/mix", map[string]any{"kind": "mirror", "default": true, "min_acks": 1, "dedup": "tenant", "nodes": mixNodes}); code != 200 {
		t.Fatalf("register mix: %d %s", code, d)
	}
	if code, d := r.admin("PUT", "/v1/admin/stores/gar", g.store(map[string]any{"default": false, "dedup": "tenant"})); code != 200 {
		t.Fatalf("register gar: %d %s", code, d)
	}
	c := r.s3Client("001", secret1)
	ctx := context.Background()
	body := pattern(20<<20 + 321)
	read := func(bucket, key string) []byte {
		t.Helper()
		got, err := c.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String(bucket), Key: aws.String(key)})
		if err != nil {
			t.Fatalf("get %s/%s: %v", bucket, key, err)
		}
		d, _ := io.ReadAll(got.Body)
		return d
	}

	for _, bucket := range []string{"mix", "gar"} {
		s3Round(t, r, c, bucket, body, dav.files)
	}
	// between the two kinds of store, by bytes, and back
	if _, err := c.CopyObject(ctx, &s3.CopyObjectInput{Bucket: aws.String("gar"), Key: aws.String("from-mix"), CopySource: aws.String("mix/orig")}); err != nil {
		t.Fatalf("copy from the mirror to the bucket: %v", err)
	}
	if _, err := c.CopyObject(ctx, &s3.CopyObjectInput{Bucket: aws.String("mix"), Key: aws.String("from-gar"), CopySource: aws.String("gar/from-mix")}); err != nil {
		t.Fatalf("copy back: %v", err)
	}
	if !bytes.Equal(read("gar", "from-mix"), body) || !bytes.Equal(read("mix", "from-gar"), body) {
		t.Fatal("the copies between the stores are not the source")
	}
	r.refsAreTheRows()

	// a node that goes away: dead on the page after three probes, the store goes on (min_acks 1) and queues the copy, which lags when it has waited; back, the repair gives it the block, green again
	status := func() statusView {
		t.Helper()
		code, d, _ := r.do("GET", "/status.json", nil, nil)
		var v statusView
		if code != 200 || json.Unmarshal(d, &v) != nil {
			t.Fatalf("status: %d %s", code, d)
		}
		return v
	}
	nodeOf := func(v statusView, name string) string {
		for _, s := range v.Stores {
			for _, n := range s.Nodes {
				if n.Name == name {
					return n.State
				}
			}
		}
		return "missing"
	}
	r.s.ProbeOnce(ctx)
	if v := status(); nodeOf(v, "dav-box") != "ok" || nodeOf(v, "ftp-box") != "ok" {
		t.Fatalf("both nodes answer: %+v", v)
	}
	ftp.docker("stop", "-t", "1")
	for i := 0; i < 3; i++ {
		r.s.ProbeOnce(ctx)
	}
	if v := status(); nodeOf(v, "ftp-box") != "dead" || nodeOf(v, "dav-box") != "ok" {
		t.Fatalf("the FTP node is away: %+v", v)
	}
	if _, err := c.PutObject(ctx, &s3.PutObjectInput{Bucket: aws.String("mix"), Key: aws.String("while-away"), Body: bytes.NewReader(pattern(300000))}); err != nil {
		t.Fatalf("a write with one node away: %v", err)
	}
	if r.dbCount("storage_repair") < 1 {
		t.Fatal("the copy for the node that is away was not queued")
	}
	startAgain(t, ftp.name)
	time.Sleep(3 * time.Second)
	r.s.ProbeOnce(ctx)
	if _, err := r.dbc.Exec(ctx, true, rq.S("UPDATE storage_repair SET since = ?", time.Now().Add(-10*time.Minute).Unix())); err != nil {
		t.Fatal(err)
	}
	r.s.book.mu.Lock()
	r.s.book.queue = nil
	r.s.book.mu.Unlock()
	if v := status(); nodeOf(v, "ftp-box") != "lagging" {
		t.Fatalf("the node is back and a copy has waited long: %+v", v)
	}
	w := r.worker("w5", func(c *maint.Config) { c.RetryBase = time.Second })
	w.Lease(ctx, "repair")
	for i := 0; i < 8 && r.dbCount("storage_repair") > 0; i++ {
		w.Pass(ctx)
		time.Sleep(1500 * time.Millisecond)
	}
	r.s.book.mu.Lock()
	r.s.book.queue = nil
	r.s.book.mu.Unlock()
	if v := status(); nodeOf(v, "ftp-box") != "ok" || nodeOf(v, "dav-box") != "ok" {
		t.Fatalf("after the repair: %+v", v)
	}

	// deletes leave nothing on the nodes once the worker has swept the sets
	for _, bucket := range []string{"mix", "gar"} {
		lo, _ := c.ListObjectsV2(ctx, &s3.ListObjectsV2Input{Bucket: aws.String(bucket)})
		var ids []types.ObjectIdentifier
		for _, o := range lo.Contents {
			ids = append(ids, types.ObjectIdentifier{Key: o.Key})
		}
		if _, err := c.DeleteObjects(ctx, &s3.DeleteObjectsInput{Bucket: aws.String(bucket), Delete: &types.Delete{Objects: ids}}); err != nil {
			t.Fatalf("%s: delete: %v", bucket, err)
		}
	}
	for i := 0; i < 8 && (r.dbCount("storage_sets") > 0 || r.dbCount("storage_repair") > 0 || len(dav.files()) > 0 || len(ftp.files()) > 0); i++ {
		r.sweepSets()
		w.Pass(ctx)
		time.Sleep(1500 * time.Millisecond)
	}
	if r.dbCount("storage_sets") != 0 || len(dav.files()) != 0 || len(ftp.files()) != 0 {
		t.Fatalf("what the deletes leave: %d sets, %v on the WebDAV node, %v on the FTP node", r.dbCount("storage_sets"), dav.files(), ftp.files())
	}
}

// s3Round is what a tool does with a bucket on the door, with real nodes behind it: an object of 20 MiB written in parts and read back, a copy in the store that stores nothing (no set and no file on
// `node` is added), a copy by ranges, a listing; and the counts of references are the rows.
func s3Round(t *testing.T, r *rig, c *s3.Client, bucket string, body []byte, files func() []string) {
	t.Helper()
	ctx := context.Background()
	read := func(bucket, key string) []byte {
		t.Helper()
		got, err := c.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String(bucket), Key: aws.String(key)})
		if err != nil {
			t.Fatalf("get %s/%s: %v", bucket, key, err)
		}
		d, _ := io.ReadAll(got.Body)
		return d
	}
	if err := uploadParts(ctx, c, bucket, "orig", body, 8<<20); err != nil {
		t.Fatalf("%s: upload in parts: %v", bucket, err)
	}
	if !bytes.Equal(read(bucket, "orig"), body) {
		t.Fatalf("%s: the object written in parts is not what was sent", bucket)
	}
	sets, filesBefore := r.dbCount("storage_sets"), len(files())
	// a copy in the store stores nothing
	if _, err := c.CopyObject(ctx, &s3.CopyObjectInput{Bucket: aws.String(bucket), Key: aws.String("copy"), CopySource: aws.String(bucket + "/orig")}); err != nil {
		t.Fatalf("%s: copy: %v", bucket, err)
	}
	if r.dbCount("storage_sets") != sets || len(files()) != filesBefore {
		t.Fatalf("%s: a copy in the store stored something: %d sets (were %d), %d files on the node (were %d)", bucket, r.dbCount("storage_sets"), sets, len(files()), filesBefore)
	}
	// a copy by ranges (what the AWS CLI does)
	cu, err := c.CreateMultipartUpload(ctx, &s3.CreateMultipartUploadInput{Bucket: aws.String(bucket), Key: aws.String("ranges")})
	if err != nil {
		t.Fatal(err)
	}
	var done []types.CompletedPart
	for n, off := int32(1), 0; off < len(body); n, off = n+1, off+8<<20 {
		end := min(off+8<<20, len(body)) - 1
		up, err := c.UploadPartCopy(ctx, &s3.UploadPartCopyInput{Bucket: aws.String(bucket), Key: aws.String("ranges"), UploadId: cu.UploadId, PartNumber: aws.Int32(n),
			CopySource: aws.String(bucket + "/orig"), CopySourceRange: aws.String(fmt.Sprintf("bytes=%d-%d", off, end))})
		if err != nil {
			t.Fatalf("%s: part %d: %v", bucket, n, err)
		}
		done = append(done, types.CompletedPart{ETag: up.CopyPartResult.ETag, PartNumber: aws.Int32(n)})
	}
	if _, err := c.CompleteMultipartUpload(ctx, &s3.CompleteMultipartUploadInput{Bucket: aws.String(bucket), Key: aws.String("ranges"), UploadId: cu.UploadId, MultipartUpload: &types.CompletedMultipartUpload{Parts: done}}); err != nil {
		t.Fatalf("%s: complete: %v", bucket, err)
	}
	if !bytes.Equal(read(bucket, "copy"), body) || !bytes.Equal(read(bucket, "ranges"), body) {
		t.Fatalf("%s: a copy is not the source", bucket)
	}
	lo, err := c.ListObjectsV2(ctx, &s3.ListObjectsV2Input{Bucket: aws.String(bucket)})
	if err != nil || len(lo.Contents) != 3 {
		t.Fatalf("%s: list: %v %d", bucket, err, len(lo.Contents))
	}
	r.refsAreTheRows()
}

// startAgain starts a container that was stopped. The port of the container was free for a moment and another program on the machine may have taken it: it asks again a few times.
func startAgain(t *testing.T, name string) {
	t.Helper()
	for i := 0; ; i++ {
		out, err := exec.Command("docker", "start", name).CombinedOutput()
		if err == nil {
			return
		}
		if i == 5 {
			t.Fatalf("docker start %s: %v %s", name, err, out)
		}
		time.Sleep(2 * time.Second)
	}
}

// the files of the user on the SFTP path of the server
func (s *sftpgo) sftpFiles() []string {
	out, _ := exec.Command("docker", "exec", s.name, "find", "/srv/sftpgo/data/u1/blobs-sftp", "-type", "f").Output()
	return strings.Fields(string(out))
}

const minioImage = "minio/minio:RELEASE.2024-06-06T09-36-42Z"

// An S3 server with one bucket (MinIO, SeaweedFS): an S3 node of another make than Garage.
type minio struct {
	port               int
	user, pass, bucket string
	client             *s3.Client
}

// startMinIO is nil when the image is not there (the test goes on without MinIO).
func startMinIO(t *testing.T) *minio {
	if os.Getenv("BLOBGATE_DOCKER_TESTS") != "1" {
		t.Skip("BLOBGATE_DOCKER_TESTS=1 runs the tests with real servers")
	}
	m := &minio{port: freePort(t), user: "minioadmin", pass: "minioadmin-secret-123", bucket: "blobgate-test"}
	image := minioImage
	if v := os.Getenv("BLOBGATE_TEST_MINIO_IMAGE"); v != "" { // another image, or one that is not there (the round then goes on without MinIO)
		image = v
	}
	_ = exec.Command("docker", "rm", "-f", "blobgate-test-minio").Run()
	out, err := exec.Command("docker", "run", "-d", "--name", "blobgate-test-minio", "-p", fmt.Sprintf("127.0.0.1:%d:9000", m.port), "-e", "MINIO_ROOT_USER="+m.user, "-e", "MINIO_ROOT_PASSWORD="+m.pass,
		image, "server", "/data").CombinedOutput()
	if err != nil { // MinIO no longer publishes its images where they can be pulled: with no local image the round goes on without it
		t.Logf("no MinIO in this run: %v %s", err, out)
		return nil
	}
	t.Cleanup(func() { _ = exec.Command("docker", "rm", "-f", "blobgate-test-minio").Run() })
	up := false
	for i := 0; i < 60 && !up; i++ {
		time.Sleep(500 * time.Millisecond)
		if resp, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d/minio/health/live", m.port)); err == nil {
			resp.Body.Close()
			up = resp.StatusCode == 200
		}
	}
	if !up {
		t.Fatal("minio did not come up")
	}
	m.client = s3.NewFromConfig(aws.Config{Region: "us-east-1", Credentials: credentials.NewStaticCredentialsProvider(m.user, m.pass, "")},
		func(o *s3.Options) {
			o.BaseEndpoint = aws.String(fmt.Sprintf("http://127.0.0.1:%d", m.port))
			o.UsePathStyle = true
		})
	if _, err := m.client.CreateBucket(context.Background(), &s3.CreateBucketInput{Bucket: aws.String(m.bucket)}); err != nil {
		t.Fatalf("make the bucket: %v", err)
	}
	return m
}

// objects is what the bucket holds.
func (m *minio) objects() []string {
	var keys []string
	for tok := (*string)(nil); ; {
		lo, err := m.client.ListObjectsV2(context.Background(), &s3.ListObjectsV2Input{Bucket: aws.String(m.bucket), ContinuationToken: tok})
		if err != nil {
			return keys
		}
		for _, o := range lo.Contents {
			keys = append(keys, aws.ToString(o.Key))
		}
		if tok = lo.NextContinuationToken; tok == nil {
			return keys
		}
	}
}

// startSeaweed starts SeaweedFS with its S3 API (no identities: it takes any key) and makes a bucket.
func startSeaweed(t *testing.T) *minio {
	if os.Getenv("BLOBGATE_DOCKER_TESTS") != "1" {
		t.Skip("BLOBGATE_DOCKER_TESTS=1 runs the tests with real servers")
	}
	m := &minio{port: freePort(t), user: "anything", pass: "anything-secret-123", bucket: "blobgate-test"}
	_ = exec.Command("docker", "rm", "-f", "blobgate-test-seaweed").Run()
	out, err := exec.Command("docker", "run", "-d", "--name", "blobgate-test-seaweed", "-p", fmt.Sprintf("127.0.0.1:%d:8333", m.port), "chrislusf/seaweedfs:3.85", "server", "-s3", "-dir=/data", "-s3.port=8333").CombinedOutput()
	if err != nil {
		t.Skipf("docker could not start seaweedfs: %v %s", err, out)
	}
	t.Cleanup(func() { _ = exec.Command("docker", "rm", "-f", "blobgate-test-seaweed").Run() })
	m.client = s3.NewFromConfig(aws.Config{Region: "us-east-1", Credentials: credentials.NewStaticCredentialsProvider(m.user, m.pass, "")},
		func(o *s3.Options) {
			o.BaseEndpoint = aws.String(fmt.Sprintf("http://127.0.0.1:%d", m.port))
			o.UsePathStyle = true
		})
	var err2 error
	for i := 0; i < 60; i++ {
		time.Sleep(time.Second)
		if _, err2 = m.client.CreateBucket(context.Background(), &s3.CreateBucketInput{Bucket: aws.String(m.bucket)}); err2 == nil {
			return m
		}
	}
	t.Fatalf("seaweedfs did not take a bucket: %v", err2)
	return nil
}

// SeaweedFS as the S3 node: the door in parts and in copies, and deletes that leave no object in the bucket.
func TestRealSeaweedS3(t *testing.T) {
	m := startSeaweed(t)
	r := newRig(t, nil)
	if code, d := r.admin("PUT", "/v1/admin/stores/weed", m.store(map[string]any{"dedup": "tenant", "default": true})); code != 200 {
		t.Fatalf("register the bucket: %d %s", code, d)
	}
	if code, d := r.admin("POST", "/v1/admin/stores/weed:check", nil); code != 200 {
		t.Fatalf(":check %d %s", code, d)
	}
	c := r.s3Client("001", secret1)
	ctx := context.Background()
	s3Round(t, r, c, "weed", pattern(20<<20+321), m.objects)
	lo, _ := c.ListObjectsV2(ctx, &s3.ListObjectsV2Input{Bucket: aws.String("weed")})
	var ids []types.ObjectIdentifier
	for _, o := range lo.Contents {
		ids = append(ids, types.ObjectIdentifier{Key: o.Key})
	}
	if _, err := c.DeleteObjects(ctx, &s3.DeleteObjectsInput{Bucket: aws.String("weed"), Delete: &types.Delete{Objects: ids}}); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 8 && (r.dbCount("storage_sets") > 0 || len(m.objects()) > 0); i++ {
		r.sweepSets()
		time.Sleep(1500 * time.Millisecond)
	}
	if r.dbCount("storage_sets") != 0 || len(m.objects()) != 0 {
		t.Fatalf("what the deletes leave: %d sets, %v in the bucket", r.dbCount("storage_sets"), m.objects())
	}
}

func (m *minio) store(extra map[string]any) map[string]any {
	b := map[string]any{"kind": "s3", "endpoint": fmt.Sprintf("http://127.0.0.1:%d", m.port), "region": "us-east-1", "bucket": m.bucket, "access_key_id": m.user, "secret_access_key": m.pass}
	for k, v := range extra {
		b[k] = v
	}
	return b
}

// The same round on an SFTP node (SFTPGo's, with a WebDAV node beside it), and, when the image is there, on an S3 node that is not Garage (MinIO): the door in parts and in copies, a copy between the two, a node that goes away
// (the SFTP one: dead on the page, back, ok), and deletes that leave nothing in the bucket nor on the SFTP node.
func TestRealMinIOAndSFTP(t *testing.T) {
	a, b, m := startSFTPGo(t, "dav6"), startSFTPGo(t, "sftp6"), startMinIO(t)
	r := newRig(t, nil)
	nodes := []any{a.webdav(), b.sftp()}
	nodes[0].(map[string]any)["label"], nodes[1].(map[string]any)["label"] = "dav-box", "sftp-box"
	if code, d := r.admin("PUT", "/v1/admin/stores/sftp", map[string]any{"kind": "mirror", "default": true, "min_acks": 1, "dedup": "tenant", "nodes": nodes}); code != 200 {
		t.Fatalf("register the mirror: %d %s", code, d)
	}
	if m != nil {
		if code, d := r.admin("PUT", "/v1/admin/stores/minio", m.store(map[string]any{"dedup": "tenant"})); code != 200 {
			t.Fatalf("register the bucket: %d %s", code, d)
		}
		if code, d := r.admin("POST", "/v1/admin/stores/minio:check", nil); code != 200 {
			t.Fatalf(":check %d %s", code, d)
		}
	}
	c := r.s3Client("001", secret1)
	ctx := context.Background()
	body := pattern(20<<20 + 321)
	s3Round(t, r, c, "sftp", body, b.sftpFiles)
	pairs := [][2]string{}
	if m != nil {
		s3Round(t, r, c, "minio", body, m.objects)
		pairs = [][2]string{{"minio", "sftp"}, {"sftp", "minio"}}
	}
	for _, p := range pairs {
		if _, err := c.CopyObject(ctx, &s3.CopyObjectInput{Bucket: aws.String(p[1]), Key: aws.String("from-" + p[0]), CopySource: aws.String(p[0] + "/orig")}); err != nil {
			t.Fatalf("copy %s to %s: %v", p[0], p[1], err)
		}
		g, err := c.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String(p[1]), Key: aws.String("from-" + p[0])})
		if err != nil {
			t.Fatal(err)
		}
		if d, _ := io.ReadAll(g.Body); !bytes.Equal(d, body) {
			t.Fatalf("the copy from %s to %s is not the source", p[0], p[1])
		}
	}
	r.refsAreTheRows()

	// the SFTP node goes away
	state := func(name string) string {
		code, d, _ := r.do("GET", "/status.json", nil, nil)
		var v statusView
		if code != 200 || json.Unmarshal(d, &v) != nil {
			t.Fatalf("status: %d %s", code, d)
		}
		for _, s := range v.Stores {
			for _, n := range s.Nodes {
				if n.Name == name {
					return n.State
				}
			}
		}
		return "missing"
	}
	r.s.ProbeOnce(ctx)
	if state("sftp-box") != "ok" || state("dav-box") != "ok" {
		t.Fatalf("both nodes answer: %s %s", state("sftp-box"), state("dav-box"))
	}
	b.docker("stop", "-t", "1")
	for i := 0; i < 3; i++ {
		r.s.ProbeOnce(ctx)
	}
	if state("sftp-box") != "dead" {
		t.Fatalf("the SFTP node is away: %s", state("sftp-box"))
	}
	startAgain(t, b.name)
	time.Sleep(3 * time.Second)
	r.s.ProbeOnce(ctx)
	if state("sftp-box") != "ok" {
		t.Fatalf("the SFTP node is back: %s", state("sftp-box"))
	}

	// deletes leave nothing
	buckets := []string{"sftp"}
	if m != nil {
		buckets = append(buckets, "minio")
	}
	for _, bucket := range buckets {
		lo, _ := c.ListObjectsV2(ctx, &s3.ListObjectsV2Input{Bucket: aws.String(bucket)})
		var ids []types.ObjectIdentifier
		for _, o := range lo.Contents {
			ids = append(ids, types.ObjectIdentifier{Key: o.Key})
		}
		if _, err := c.DeleteObjects(ctx, &s3.DeleteObjectsInput{Bucket: aws.String(bucket), Delete: &types.Delete{Objects: ids}}); err != nil {
			t.Fatalf("%s: delete: %v", bucket, err)
		}
	}
	w := r.worker("w6", nil)
	w.Lease(ctx, "repair")
	left := func() int {
		if m == nil {
			return 0
		}
		return len(m.objects())
	}
	for i := 0; i < 8 && (r.dbCount("storage_sets") > 0 || r.dbCount("storage_repair") > 0 || len(b.sftpFiles()) > 0 || left() > 0); i++ {
		r.sweepSets()
		w.Pass(ctx)
		time.Sleep(1500 * time.Millisecond)
	}
	if r.dbCount("storage_sets") != 0 || len(b.sftpFiles()) != 0 || left() != 0 {
		t.Fatalf("what the deletes leave: %d sets, %v on the SFTP node, %d objects in the MinIO bucket", r.dbCount("storage_sets"), b.sftpFiles(), left())
	}
}
