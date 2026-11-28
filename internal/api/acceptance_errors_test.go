package api

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/rclone/rclone/fs"

	"github.com/zystem/blobgate/internal/rq"
	"github.com/zystem/blobgate/internal/store"
)

// brokenNode is a node whose every call fails with the words a remote server and its library would use.
type brokenNode struct {
	fs.Fs
	err error
}

func (b brokenNode) Put(context.Context, io.Reader, fs.ObjectInfo, ...fs.OpenOption) (fs.Object, error) {
	return nil, b.err
}
func (b brokenNode) NewObject(context.Context, string) (fs.Object, error) { return nil, b.err }
func (b brokenNode) List(context.Context, string) (fs.DirEntries, error)  { return nil, b.err }
func (b brokenNode) Mkdir(context.Context, string) error                  { return b.err }

type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}
func (l *lockedBuffer) String() string { l.mu.Lock(); defer l.mu.Unlock(); return l.b.String() }

// What the inside of a failure says must reach the log and not the caller, and the log must not hold the passwords.
var insideWords = []string{"nas.internal.example", "backup-user", "hunter2", "SIG123", "/srv/blobs", "10.1.2.3"}

func remoteFailure() error {
	return errors.New(`Put "https://backup-user:hunter2@nas.internal.example:8443/srv/blobs/x?X-Amz-Signature=SIG123": dial tcp 10.1.2.3:8443: connect: connection refused`)
}

func tellsNothing(t *testing.T, what, answer string) {
	t.Helper()
	for _, w := range insideWords {
		if strings.Contains(answer, w) {
			t.Errorf("%s: the answer names %q: %s", what, w, answer)
		}
	}
}

// The acceptance for error text of the review of 2026-11-22: failures of a remote node and of the database, in the answers of the core and of the S3 door, and in the log.
func TestRemoteAndDatabaseFailuresInAnswersAndLog(t *testing.T) {
	r := newRig(t, nil)
	r.mirror(0)
	logged := &lockedBuffer{}
	r.s.log = slog.New(slog.NewTextHandler(logged, nil))
	body := pattern(2000)
	if code, d := r.put("acme/there", body); code != 200 {
		t.Fatalf("put: %d %s", code, d)
	}
	up := r.newUpload("acme/up")
	ctx := context.Background()
	c := r.s3ClientTries("001", secret1, 1)
	r.s.Reg = store.NewRegistry(r.dbc, r.s.Reg.Key) // the nodes are opened anew, with the failure in them
	r.s.Reg.OpenHook = func(m *store.Mirror) {
		for i := range m.Nodes {
			m.Nodes[i].F = brokenNode{Fs: m.Nodes[i].F, err: remoteFailure()}
		}
	}
	failed := func(what string, code int, d []byte) {
		t.Helper()
		if code < 500 {
			t.Errorf("%s with the nodes broken: %d %s", what, code, d)
		}
		tellsNothing(t, what, string(d))
	}
	code, d := r.put("acme/new", body)
	failed("a write", code, d)
	code, d = r.get("acme/there", 0, 10)
	failed("a read", code, d)
	code, d = r.sendPart("acme/up", up, "1", body)
	failed("a part", code, d)
	_, err := c.PutObject(ctx, &s3.PutObjectInput{Bucket: aws.String("main"), Key: aws.String("s3new"), Body: bytes.NewReader(body)})
	if err == nil {
		t.Error("an S3 write with the nodes broken")
	} else {
		tellsNothing(t, "an S3 write", err.Error())
	}

	good, goodReg := r.s.DB, r.s.Reg.DB
	r.s.DB = rq.New("http://backup-user:hunter2@10.1.2.3:1")
	r.s.Reg.DB = r.s.DB
	defer func() { r.s.DB, r.s.Reg.DB = good, goodReg }()
	code, d = r.put("acme/nodb", body)
	failed("a write with no database", code, d)
	_, err = c.ListObjectsV2(ctx, &s3.ListObjectsV2Input{Bucket: aws.String("main")})
	if err != nil {
		tellsNothing(t, "an S3 listing with no database", err.Error())
	} else {
		t.Error("a listing with no database")
	}

	out := logged.String()
	if !strings.Contains(out, "ref=") || !strings.Contains(out, "nas.internal.example") {
		t.Errorf("the log does not have what the operator needs: %.600s", out)
	}
	for _, secret := range []string{"hunter2", "SIG123"} {
		if strings.Contains(out, secret) {
			t.Errorf("the log holds %q", secret)
		}
	}
}

// The same with a real server that is gone: what the library says of it (its address, the port) is not told to the caller.
func TestRealRemoteFailureTellsNothing(t *testing.T) {
	srv := startSFTPGo(t, "gone")
	r := newRig(t, nil)
	if code, d := r.admin("PUT", "/v1/admin/stores/main", map[string]any{"kind": "mirror", "default": true, "nodes": []any{srv.webdav()}}); code != 200 {
		t.Fatalf("register: %d %s", code, d)
	}
	logged := &lockedBuffer{}
	r.s.log = slog.New(slog.NewTextHandler(logged, nil))
	if code, d := r.put("acme/there", pattern(2000)); code != 200 {
		t.Fatalf("put: %d %s", code, d)
	}
	srv.docker("stop")
	address := fmt.Sprintf("127.0.0.1:%d", srv.webdavP)
	code, put := r.put("acme/new", pattern(2000))
	code2, got := r.get("acme/there", 0, 10)
	for what, a := range map[string]struct {
		code int
		body []byte
	}{"a write": {code, put}, "a read": {code2, got}} {
		if a.code < 500 || !strings.Contains(string(a.body), "(ref ") {
			t.Errorf("%s with the server stopped: %d %s", what, a.code, a.body)
		}
		for _, w := range []string{address, "127.0.0.1", "userpass123", "/blobs", "webdav"} {
			if strings.Contains(string(a.body), w) {
				t.Errorf("%s: the answer names %q: %s", what, w, a.body)
			}
		}
	}
	if out := logged.String(); !strings.Contains(out, address) || strings.Contains(out, "userpass123") {
		t.Errorf("the log: %.600s", out)
	}
}
