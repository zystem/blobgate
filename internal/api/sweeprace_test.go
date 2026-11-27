package api

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"

	"github.com/zystem/blobgate/internal/maint"
	"github.com/zystem/blobgate/internal/rq"
)

// sweeper is a worker that sees every upload as older than the time limit (its clock is two hours ahead).
func (r *rig) sweeper(id string) *maint.Worker {
	return r.worker(id, func(c *maint.Config) {
		c.UploadTTL = time.Hour
		c.Now = func() time.Time { return time.Now().Add(2 * time.Hour) }
	})
}

// Finding 1 of the review of 2026-11-22: the sweep of expired uploads and the commit of an object must not both win. The sweep runs in the middle of the commit of a write in one call and of the
// completion of an upload in parts (the hook `commit:before` is the moment between the last read of the database and the transaction): the commit must be refused with `no_such_upload`, no object
// may exist, the blocks must be gone from both nodes and the queue must hold nothing that could remove what is live.
func TestSweepAndCommitDoNotBothWin(t *testing.T) {
	r := newRig(t, nil)
	r.mirror(0)
	ctx := context.Background()
	w := r.sweeper("sweep")
	if ok, _ := w.Lease(ctx, "repair"); !ok {
		t.Fatal("lease")
	}
	swept := 0
	r.s.cfg.CrashAt = func(p string) {
		if p == "commit:before" {
			swept += w.SweepPass(ctx)
		}
	}
	// a write in one call (more than one block, so that it has an upload row of its own)
	body := pattern(BlockSize + 12345)
	code, d := r.put("k/one", body)
	if code != 404 || !strings.Contains(string(d), "no_such_upload") {
		t.Fatalf("a write whose upload was swept at the commit: %d %s", code, d)
	}
	if swept != 1 {
		t.Fatalf("the sweep claimed %d uploads, want 1", swept)
	}
	if n := r.count("storage_objects"); n != 0 {
		t.Fatalf("%d objects after a commit that lost to the sweep", n)
	}
	for i := range r.nodes {
		if f := files(r.nodes[i]); len(f) != 0 {
			t.Fatalf("node %d still holds %v of a swept upload", i+1, f)
		}
	}

	// an upload in parts completed while the sweep takes it
	c := r.s3Client("001", secret1)
	cu, err := c.CreateMultipartUpload(ctx, &s3.CreateMultipartUploadInput{Bucket: aws.String("main"), Key: aws.String("k/parts")})
	if err != nil {
		t.Fatal(err)
	}
	var done []types.CompletedPart
	for n := 1; n <= 2; n++ {
		up, err := c.UploadPart(ctx, &s3.UploadPartInput{Bucket: aws.String("main"), Key: aws.String("k/parts"), UploadId: cu.UploadId, PartNumber: aws.Int32(int32(n)), Body: bytes.NewReader(pattern(BlockSize))})
		if err != nil {
			t.Fatal(err)
		}
		done = append(done, types.CompletedPart{ETag: up.ETag, PartNumber: aws.Int32(int32(n))})
	}
	swept = 0
	_, err = c.CompleteMultipartUpload(ctx, &s3.CompleteMultipartUploadInput{Bucket: aws.String("main"), Key: aws.String("k/parts"), UploadId: cu.UploadId, MultipartUpload: &types.CompletedMultipartUpload{Parts: done}})
	if err == nil || !strings.Contains(err.Error(), "NoSuchUpload") {
		t.Fatalf("a completion whose upload was swept at the commit: %v", err)
	}
	if swept != 1 || r.count("storage_objects") != 0 || r.count("storage_uploads") != 0 || r.count("storage_parts") != 0 {
		t.Fatalf("after the lost completion: swept %d, %d objects, %d uploads, %d parts", swept, r.count("storage_objects"), r.count("storage_uploads"), r.count("storage_parts"))
	}
	for i := range r.nodes {
		if f := files(r.nodes[i]); len(f) != 0 {
			t.Fatalf("node %d still holds %v of a swept upload in parts", i+1, f)
		}
	}

	// the other order: the commit wins and the sweep, which runs after, finds nothing to take; the object reads whole after every queue is worked
	r.s.cfg.CrashAt = nil
	if code, d := r.put("k/two", body); code != 200 {
		t.Fatalf("put: %d %s", code, d)
	}
	if n := w.SweepPass(ctx); n != 0 {
		t.Fatalf("the sweep took %d uploads of a write that was committed", n)
	}
	for i := 0; i < 3; i++ {
		w.Pass(ctx)
	}
	if code, got := r.get("k/two", 0, len(body)-1); code != 200 && code != 206 || !bytes.Equal(got, body) {
		t.Fatalf("the object after the maintenance: %d, %d bytes", code, len(got))
	}
}

// Finding 1, the reconcile: a directory that was a stray when the reconcile read the nodes and became an object's before the removal was done is not removed (the removal asks again who owns it).
func TestPurgepathAsksAgainWhoOwnsTheDirectory(t *testing.T) {
	r := newRig(t, nil)
	r.mirror(0)
	ctx := context.Background()
	body := pattern(70000)
	if code, d := r.put("k/live", body); code != 200 {
		t.Fatalf("put: %d %s", code, d)
	}
	var dirs []string
	for _, f := range files(r.nodes[0]) {
		if i := strings.LastIndexByte(f, '/'); i > 0 {
			dirs = append(dirs, f[:i])
		}
	}
	if len(dirs) == 0 {
		t.Fatal("no block directory on the node")
	}
	m, err := r.s.Reg.Load(ctx, "main")
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range m.Nodes {
		if _, err := r.dbc.Exec(ctx, false, rq.S("INSERT INTO storage_repair (store, key, node, op, since, attempts, next_at, last_error) VALUES ('main', ?, ?, 'purgepath', ?, 0, 0, '')", dirs[0], n.ID, time.Now().Unix())); err != nil {
			t.Fatal(err)
		}
	}
	w := r.worker("w", nil)
	if ok, _ := w.Lease(ctx, "repair"); !ok {
		t.Fatal("lease")
	}
	w.Pass(ctx)
	if r.count("storage_repair") != 0 {
		t.Fatalf("%d tasks left: the removal of a live directory should be dropped", r.count("storage_repair"))
	}
	if code, got := r.get("k/live", 0, len(body)-1); code != 200 && code != 206 || !bytes.Equal(got, body) {
		t.Fatalf("the live object after the removal of its directory was queued: %d", code)
	}
	// a directory that nothing owns is removed, as before
	stray := "gone~0123456789abcdef01234567"
	for i, n := range m.Nodes {
		if err := os.MkdirAll(filepath.Join(r.nodes[i], stray), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(r.nodes[i], stray, "0"), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := r.dbc.Exec(ctx, false, rq.S("INSERT INTO storage_repair (store, key, node, op, since, attempts, next_at, last_error) VALUES ('main', ?, ?, 'purgepath', ?, 0, 0, '')", stray, n.ID, time.Now().Unix())); err != nil {
			t.Fatal(err)
		}
	}
	w.Pass(ctx)
	for i := range r.nodes {
		for _, f := range files(r.nodes[i]) {
			if strings.HasPrefix(f, stray) {
				t.Fatalf("a stray directory stays on node %d: %s", i+1, f)
			}
		}
	}
}

// The same decision with deduplication, between two replicas of the module, and when the worker dies after it has claimed the upload.
func TestSweepWithDedupAcrossReplicasAndAWorkerThatDies(t *testing.T) {
	a := newRig(t, nil)
	a.dedupStore("tenant", 0, "")
	b := a.replica()
	ctx := context.Background()
	body := pattern(BlockSize + 321)
	if code, d := a.put("acme/first", body); code != 200 { // the set that the second write would join
		t.Fatalf("put: %d %s", code, d)
	}
	held := len(files(a.nodes[0]))

	// the write is on replica a, the sweep that takes its upload runs on replica b in the middle of the commit
	w := maint.New(maint.Config{HolderID: "b", UploadTTL: time.Hour, Now: func() time.Time { return time.Now().Add(2 * time.Hour) }}, b.dbc, b.s.Reg)
	if ok, _ := w.Lease(ctx, "repair"); !ok {
		t.Fatal("lease")
	}
	a.s.cfg.CrashAt = func(p string) {
		if p == "commit:before" {
			w.SweepPass(ctx)
		}
	}
	if code, d := a.put("acme/second", body); code != 404 {
		t.Fatalf("a write of bytes the store has, swept at its commit: %d %s", code, d)
	}
	a.s.cfg.CrashAt = nil
	if n := a.count("storage_objects"); n != 1 || len(files(a.nodes[0])) != held {
		t.Fatalf("%d objects, node 1 holds %v", n, files(a.nodes[0]))
	}
	if code, got := b.get("acme/first", 0, len(body)-1); (code != 200 && code != 206) || !bytes.Equal(got, body) {
		t.Fatalf("the object that shares the bytes: %d", code)
	}

	// the worker dies between the claim and the removal: what it left is written down, and the next worker removes it
	id := b.newUpload("acme/dead")
	if code, d := b.sendPart("acme/dead", id, "1", pattern(5000)); code != 200 {
		t.Fatalf("part: %d %s", code, d)
	}
	dying := a.worker("dying", func(c *maint.Config) {
		c.Now = func() time.Time { return time.Now().Add(2 * time.Hour) }
		c.Hook = func(p string) {
			if p == "sweep:claimed" && len(files(a.nodes[0])) > held {
				panic("the worker dies")
			}
		}
	})
	_, _ = w.DB.Exec(ctx, false, rq.S("DELETE FROM storage_lease"))
	if ok, _ := dying.Lease(ctx, "repair"); !ok {
		t.Fatal("lease")
	}
	func() {
		defer func() { _ = recover() }()
		dying.SweepPass(ctx)
	}()
	if len(files(a.nodes[0])) == held {
		t.Fatal("the worker did not die before the removal")
	}
	_, _ = w.DB.Exec(ctx, false, rq.S("DELETE FROM storage_lease"))
	next := a.worker("next", nil)
	if ok, _ := next.Lease(ctx, "repair"); !ok {
		t.Fatal("lease")
	}
	next.Pass(ctx)
	if f := files(a.nodes[0]); len(f) != held {
		t.Fatalf("blocks that nothing remembers stay after a dead worker: %v", f)
	}
}
