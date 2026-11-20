package api

// An S3 server (SeaweedFS) that goes away while the door is writing: as the only bucket of a store, and as a node of a mirror beside an SFTPGo node. It needs Docker and runs only with
// BLOBGATE_DOCKER_TESTS=1.

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"os/exec"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"

	"github.com/zystem/blobgate/internal/maint"
	"github.com/zystem/blobgate/internal/store"
)

const seaweedName = "blobgate-test-seaweed"

// seaweedUp starts the stopped SeaweedFS again and waits until its S3 API takes a call.
func seaweedUp(t *testing.T, m *minio) {
	t.Helper()
	startAgain(t, seaweedName)
	for i := 0; i < 60; i++ {
		if _, err := m.client.ListObjectsV2(context.Background(), &s3.ListObjectsV2Input{Bucket: aws.String(m.bucket)}); err == nil {
			return
		}
		time.Sleep(time.Second)
	}
	t.Fatal("seaweedfs did not come back")
}

func seaweedDown(t *testing.T) {
	t.Helper()
	if out, err := exec.Command("docker", "stop", "-t", "1", seaweedName).CombinedOutput(); err != nil {
		t.Fatalf("docker stop: %v %s", err, out)
	}
}

// begin an upload, send the parts [0,n) of the body.
func partsOf(t *testing.T, c *s3.Client, bucket, key string, body []byte, from, to int, upload *string, done *[]types.CompletedPart) error {
	t.Helper()
	const sz = 8 << 20
	for n := from; n < to; n++ {
		t0 := time.Now()
		up, err := c.UploadPart(context.Background(), &s3.UploadPartInput{Bucket: aws.String(bucket), Key: aws.String(key), UploadId: upload, PartNumber: aws.Int32(int32(n + 1)),
			Body: bytes.NewReader(body[n*sz : min((n+1)*sz, len(body))])})
		t.Logf("part %d: %v after %v", n+1, err, time.Since(t0).Round(time.Millisecond))
		if err != nil {
			return err
		}
		*done = append(*done, types.CompletedPart{ETag: up.ETag, PartNumber: aws.Int32(int32(n + 1))})
	}
	return nil
}

func readBack(t *testing.T, c *s3.Client, bucket, key string) []byte {
	t.Helper()
	g, err := c.GetObject(context.Background(), &s3.GetObjectInput{Bucket: aws.String(bucket), Key: aws.String(key)})
	if err != nil {
		t.Fatalf("get %s/%s: %v", bucket, key, err)
	}
	d, _ := io.ReadAll(g.Body)
	g.Body.Close()
	return d
}

// The S3 server is the only place of a store and goes away between two parts, and in the middle of a write in one call: the writes are refused with words (a 5xx, never a hang, never half an
// object in the list), and when the server is back the same upload goes on and ends, and the object is what was sent.
func TestRealS3NodeAwayAsStore(t *testing.T) {
	m := startSeaweed(t)
	r := newRig(t, nil)
	registered := func(name string, body map[string]any) {
		t.Helper()
		var code int
		var d []byte
		for i := 0; i < 30; i++ { // a new SeaweedFS takes a put only when it has made its first volume
			if code, d = r.admin("PUT", "/v1/admin/stores/"+name, body); code == 200 {
				return
			}
			time.Sleep(2 * time.Second)
		}
		t.Fatalf("register the bucket: %d %s", code, d)
	}
	registered("weed", m.store(map[string]any{"default": true}))
	if code, d := r.admin("POST", "/v1/admin/stores/weed:check", nil); code != 200 {
		t.Fatalf(":check %d %s", code, d)
	}
	c := r.s3ClientTries("001", secret1, 1)
	ctx := context.Background()
	body := pattern(30<<20 + 7)

	cu, err := c.CreateMultipartUpload(ctx, &s3.CreateMultipartUploadInput{Bucket: aws.String("weed"), Key: aws.String("big")})
	if err != nil {
		t.Fatal(err)
	}
	var done []types.CompletedPart
	if err := partsOf(t, c, "weed", "big", body, 0, 2, cu.UploadId, &done); err != nil {
		t.Fatalf("parts 1-2: %v", err)
	}
	seaweedDown(t)
	start := time.Now()
	defer func() { t.Logf("refusals took %v", time.Since(start)) }()
	if err := partsOf(t, c, "weed", "big", body, 2, 3, cu.UploadId, &done); err == nil {
		t.Fatal("a part was taken while the only node is away")
	}
	if _, err := c.PutObject(ctx, &s3.PutObjectInput{Bucket: aws.String("weed"), Key: aws.String("one-call"), Body: bytes.NewReader(pattern(3 << 20))}); err == nil {
		t.Fatal("a write in one call was taken while the only node is away")
	}
	if d := time.Since(start); d > 90*time.Second {
		t.Fatalf("the refusals took %v", d)
	}
	if lo, err := c.ListObjectsV2(ctx, &s3.ListObjectsV2Input{Bucket: aws.String("weed")}); err != nil || len(lo.Contents) != 0 {
		t.Fatalf("the list while the node is away: %v %v", err, lo)
	}
	seaweedUp(t, m)
	if err := partsOf(t, c, "weed", "big", body, 2, (len(body)+8<<20-1)/(8<<20), cu.UploadId, &done); err != nil {
		t.Fatalf("the rest of the parts after the node is back: %v", err)
	}
	if _, err := c.CompleteMultipartUpload(ctx, &s3.CompleteMultipartUploadInput{Bucket: aws.String("weed"), Key: aws.String("big"), UploadId: cu.UploadId, MultipartUpload: &types.CompletedMultipartUpload{Parts: done}}); err != nil {
		t.Fatalf("complete: %v", err)
	}
	if !bytes.Equal(readBack(t, c, "weed", "big"), body) {
		t.Fatal("the object is not what was sent")
	}
	if _, err := c.PutObject(ctx, &s3.PutObjectInput{Bucket: aws.String("weed"), Key: aws.String("one-call"), Body: bytes.NewReader(pattern(3 << 20))}); err != nil {
		t.Fatalf("a write in one call after the node is back: %v", err)
	}
	// a read while the node is away is refused with words too, and works when it is back
	seaweedDown(t)
	if g, err := c.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String("weed"), Key: aws.String("big")}); err == nil {
		_, rerr := io.Copy(io.Discard, g.Body)
		g.Body.Close()
		if rerr == nil {
			t.Fatal("a read was whole while the node is away")
		}
	}
	seaweedUp(t, m)
	if !bytes.Equal(readBack(t, c, "weed", "big"), body) {
		t.Fatal("the object is not what was sent, after the second absence")
	}
	r.refsAreTheRows()
}

// The S3 server is one of two nodes of a mirror (min_acks 1) and goes away in the middle of an upload in parts: the upload goes on with the other node, a copy for the absent one is queued, and
// when it is back the repair gives it every block; then the other node is stopped and the object is read from the repaired one alone.
func TestRealS3NodeAwayAsMirrorNode(t *testing.T) {
	m := startSeaweed(t)
	dav := startSFTPGo(t, "dav7")
	r := newRig(t, nil)
	node := map[string]any{"remote": fmt.Sprintf(":s3,provider=Other,endpoint='http://127.0.0.1:%d',region=us-east-1,env_auth=false,access_key_id=%s:%s/blobs", m.port, m.user, m.bucket),
		"secrets": map[string]string{"secret_access_key": m.pass}}
	registered := func() {
		t.Helper()
		var code int
		var d []byte
		for i := 0; i < 30; i++ {
			if code, d = r.admin("PUT", "/v1/admin/stores/mix", map[string]any{"kind": "mirror", "default": true, "min_acks": 1, "nodes": []any{dav.webdav(), node}}); code == 200 {
				return
			}
			time.Sleep(2 * time.Second)
		}
		t.Fatalf("register: %d %s", code, d)
	}
	registered()
	if code, d := r.admin("POST", "/v1/admin/stores/mix:check", nil); code != 200 {
		t.Fatalf(":check %d %s", code, d)
	}
	c := r.s3ClientTries("001", secret1, 1)
	ctx := context.Background()
	body := pattern(30<<20 + 7)
	nparts := (len(body) + 8<<20 - 1) / (8 << 20)

	cu, err := c.CreateMultipartUpload(ctx, &s3.CreateMultipartUploadInput{Bucket: aws.String("mix"), Key: aws.String("big")})
	if err != nil {
		t.Fatal(err)
	}
	var done []types.CompletedPart
	if err := partsOf(t, c, "mix", "big", body, 0, 2, cu.UploadId, &done); err != nil {
		t.Fatalf("parts 1-2: %v", err)
	}
	seaweedDown(t)
	if err := partsOf(t, c, "mix", "big", body, 2, nparts, cu.UploadId, &done); err != nil {
		t.Fatalf("the parts while one node is away (min_acks 1): %v", err)
	}
	if _, err := c.CompleteMultipartUpload(ctx, &s3.CompleteMultipartUploadInput{Bucket: aws.String("mix"), Key: aws.String("big"), UploadId: cu.UploadId, MultipartUpload: &types.CompletedMultipartUpload{Parts: done}}); err != nil {
		t.Fatalf("complete while one node is away: %v", err)
	}
	if _, err := c.PutObject(ctx, &s3.PutObjectInput{Bucket: aws.String("mix"), Key: aws.String("small"), Body: bytes.NewReader(pattern(1 << 20))}); err != nil {
		t.Fatalf("a write in one call while one node is away: %v", err)
	}
	if !bytes.Equal(readBack(t, c, "mix", "big"), body) {
		t.Fatal("the object is not what was sent (one node away)")
	}
	if r.dbCount("storage_repair") < 1 {
		t.Fatal("no copy for the node that is away was queued")
	}
	seaweedUp(t, m)
	w := r.worker("w7", func(cf *maint.Config) { cf.RetryBase = time.Second })
	w.Lease(ctx, "repair")
	for i := 0; i < 15 && r.dbCount("storage_repair") > 0; i++ {
		w.Pass(ctx)
		time.Sleep(1500 * time.Millisecond)
	}
	if n := r.dbCount("storage_repair"); n != 0 {
		t.Fatalf("%d repair tasks left after the node is back", n)
	}
	if len(m.objects()) == 0 {
		t.Fatal("the repair put nothing on the node that was away")
	}
	// the repaired node alone must hold everything
	if out, err := exec.Command("docker", "stop", "-t", "1", dav.name).CombinedOutput(); err != nil {
		t.Fatalf("stop the other node: %v %s", err, out)
	}
	if !bytes.Equal(readBack(t, c, "mix", "big"), body) || len(readBack(t, c, "mix", "small")) != 1<<20 {
		t.Fatal("the repaired node does not hold the objects")
	}
}

// A node that goes silent with its connections open (docker pause: the process is frozen, nothing is answered and nothing is closed; a power cut or a cable pulled does the same): for each of the
// WebDAV, FTP and SFTP of SFTPGo, in a mirror whose first node is the silent one, a write in one call, a write in parts and a read are not held for longer than the idle timeout lets them
// (BLOBGATE_NODE_TIMEOUT_SECONDS, set to 5 s here); the other node serves them, and the copy for the silent node is queued.
func TestRealSilentNodes(t *testing.T) {
	good, silent := startSFTPGo(t, "good8"), startSFTPGo(t, "silent8")
	old := store.NodeTimeout
	store.NodeTimeout = 5 * time.Second
	oldRe := store.ReopenAfter
	store.ReopenAfter = 3 * time.Second
	t.Cleanup(func() { store.NodeTimeout, store.ReopenAfter = old, oldRe })
	r := newRig(t, nil)
	kinds := map[string]map[string]any{"webdav": silent.webdav(), "ftp": silent.ftp(), "sftp": silent.sftp()}
	for name, n := range kinds {
		if code, d := r.admin("PUT", "/v1/admin/stores/"+name, map[string]any{"kind": "mirror", "min_acks": 1, "nodes": []any{n, good.webdav()}}); code != 200 {
			t.Fatalf("register %s: %d %s", name, code, d)
		}
	}
	c := r.s3ClientTries("001", secret1, 1)
	ctx := context.Background()
	if out, err := exec.Command("docker", "pause", silent.name).CombinedOutput(); err != nil {
		t.Fatalf("docker pause: %v %s", err, out)
	}
	t.Cleanup(func() { _ = exec.Command("docker", "unpause", silent.name).Run() })
	for name := range kinds {
		t0 := time.Now()
		small := pattern(3<<20 + 5)
		if _, err := c.PutObject(ctx, &s3.PutObjectInput{Bucket: aws.String(name), Key: aws.String("small"), Body: bytes.NewReader(small)}); err != nil {
			t.Fatalf("%s: a write in one call with a silent node: %v", name, err)
		}
		t.Logf("%s: write in one call %v", name, time.Since(t0).Round(time.Second))
		t0 = time.Now()
		big := pattern(17<<20 + 5)
		if err := uploadParts(ctx, c, name, "big", big, 8<<20); err != nil {
			t.Fatalf("%s: an upload in parts with a silent node: %v", name, err)
		}
		t.Logf("%s: upload in parts %v", name, time.Since(t0).Round(time.Second))
		t0 = time.Now()
		if !bytes.Equal(readBack(t, c, name, "small"), small) || !bytes.Equal(readBack(t, c, name, "big"), big) {
			t.Fatalf("%s: a read with a silent node is not what was written", name)
		}
		t.Logf("%s: reads %v", name, time.Since(t0).Round(time.Second))
	}
	if r.dbCount("storage_repair") < 1 {
		t.Fatal("no copy for the silent node was queued")
	}
	// the node comes back: the store opens its nodes again (the SFTP one was not opened at all) and the repair gives it every block
	if out, err := exec.Command("docker", "unpause", silent.name).CombinedOutput(); err != nil {
		t.Fatalf("docker unpause: %v %s", err, out)
	}
	w := r.worker("w8", func(c *maint.Config) { c.RetryBase = time.Second })
	w.Lease(ctx, "repair")
	for i := 0; i < 30 && r.dbCount("storage_repair") > 0; i++ {
		_, _ = c.HeadObject(ctx, &s3.HeadObjectInput{Bucket: aws.String("sftp"), Key: aws.String("small")}) // a call that finds the time to open the nodes again passed
		w.Pass(ctx)
		time.Sleep(2 * time.Second)
	}
	if n := r.dbCount("storage_repair"); n != 0 {
		t.Fatalf("%d repair tasks left after the node came back", n)
	}
	if len(silent.files()) == 0 || len(silent.sftpFiles()) == 0 {
		t.Fatalf("the silent node holds nothing after the repair: %v %v", silent.files(), silent.sftpFiles())
	}
}

// slowProxy is a TCP proxy to a server that passes at most `rate` bytes a second each way (0: no limit) and, when stalled, passes nothing and closes nothing: a link that is slow, and one that
// stops in the middle of a transfer.
type slowProxy struct {
	ln      net.Listener
	rate    atomic.Int64
	stalled atomic.Bool
}

func startSlowProxy(t *testing.T, target string) *slowProxy {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	p := &slowProxy{ln: ln}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			up, err := net.Dial("tcp", target)
			if err != nil {
				c.Close()
				continue
			}
			if tc, ok := c.(*net.TCPConn); ok {
				_ = tc.SetReadBuffer(32 << 10) // a small window: the sender is held back by the link, as on a real one, and does not hand the whole block to the kernel at once
			}
			go p.pipe(c, up)
			go p.pipe(up, c)
		}
	}()
	return p
}

func (p *slowProxy) port() int { return p.ln.Addr().(*net.TCPAddr).Port }

func (p *slowProxy) pipe(dst, src net.Conn) {
	defer dst.Close()
	defer src.Close()
	buf := make([]byte, 16<<10)
	for {
		n, err := src.Read(buf)
		if n > 0 {
			for p.stalled.Load() {
				time.Sleep(100 * time.Millisecond)
			}
			if r := p.rate.Load(); r > 0 {
				time.Sleep(time.Duration(int64(n) * int64(time.Second) / r))
			}
			if _, werr := dst.Write(buf[:n]); werr != nil {
				return
			}
		}
		if err != nil {
			return
		}
	}
}

// A node on a slow link is not cut while bytes move (the idle timeout of 5 s here; an object of 24 MiB takes about 12 s at 2 MiB/s): a write that every node has to confirm (min_acks 2) ends
// with both, and a read is whole. A link that stops in the middle of a transfer is cut after the timeout: with min_acks 1 the write goes on with the other node, with min_acks 2 it is refused with
// words, in both cases within a bounded time.
func TestRealSlowAndStalledLink(t *testing.T) {
	good, far := startSFTPGo(t, "good9"), startSFTPGo(t, "far9")
	old := store.NodeTimeout
	store.NodeTimeout = 5 * time.Second
	t.Cleanup(func() { store.NodeTimeout = old })
	proxy := startSlowProxy(t, fmt.Sprintf("127.0.0.1:%d", far.webdavP))
	slow := map[string]any{"remote": fmt.Sprintf(":webdav,url='http://127.0.0.1:%d',vendor=other,user=u1:/blobs", proxy.port()), "secrets": map[string]string{"pass": "userpass123"}}
	r := newRig(t, nil)
	for name, acks := range map[string]int{"both": 2, "one": 1} {
		if code, d := r.admin("PUT", "/v1/admin/stores/"+name, map[string]any{"kind": "mirror", "min_acks": acks, "nodes": []any{good.webdav(), slow}}); code != 200 {
			t.Fatalf("register %s: %d %s", name, code, d)
		}
	}
	c := r.s3ClientTries("001", secret1, 1)
	ctx := context.Background()
	body := pattern(24<<20 + 5)

	proxy.rate.Store(2 << 20)
	t0 := time.Now()
	if _, err := c.PutObject(ctx, &s3.PutObjectInput{Bucket: aws.String("both"), Key: aws.String("slow"), Body: bytes.NewReader(body)}); err != nil {
		t.Fatalf("a write over a slow link that every node must confirm: %v (after %v)", err, time.Since(t0).Round(time.Second))
	}
	took := time.Since(t0)
	t.Logf("write over the slow link: %v", took.Round(time.Second))
	if took < 8*time.Second {
		t.Fatalf("the link was not slow (%v): the test proves nothing", took)
	}
	if n := len(far.files()); n == 0 {
		t.Fatal("the slow node holds nothing though every node had to confirm")
	}
	if !bytes.Equal(readBack(t, c, "both", "slow"), body) {
		t.Fatal("the object written over a slow link is not what was sent")
	}

	// the link stops in the middle
	proxy.rate.Store(4 << 20) // 40 MiB take 10 s; the link stops after 2
	proxy.stalled.Store(false)
	go func() { time.Sleep(2 * time.Second); proxy.stalled.Store(true) }()
	t0 = time.Now()
	_, err := c.PutObject(ctx, &s3.PutObjectInput{Bucket: aws.String("one"), Key: aws.String("stalled"), Body: bytes.NewReader(pattern(40 << 20))})
	t.Logf("write over a link that stops, min_acks 1: %v after %v", err, time.Since(t0).Round(time.Second))
	if err != nil {
		t.Fatalf("with min_acks 1 the write goes on with the other node: %v", err)
	}
	if time.Since(t0) > 90*time.Second {
		t.Fatalf("the write waited %v for a link that stopped", time.Since(t0))
	}
	t0 = time.Now()
	_, err = c.PutObject(ctx, &s3.PutObjectInput{Bucket: aws.String("both"), Key: aws.String("stalled2"), Body: bytes.NewReader(pattern(3 << 20))})
	t.Logf("write over a link that stopped, min_acks 2: %v after %v", err, time.Since(t0).Round(time.Second))
	if err == nil {
		t.Fatal("a write that every node must confirm was taken while a link is stopped")
	}
	if time.Since(t0) > 90*time.Second {
		t.Fatalf("the refusal took %v", time.Since(t0))
	}
	if lo, err := c.ListObjectsV2(ctx, &s3.ListObjectsV2Input{Bucket: aws.String("both")}); err != nil || len(lo.Contents) != 1 {
		t.Fatalf("the list after the refused write: %v %v", err, lo)
	}
}
