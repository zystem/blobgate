package api

// Ceph RGW (the demo image of Ceph 19, "squid": one monitor, one OSD, one gateway) as an S3 node. It needs Docker, the image (about 1.3 GB, `docker pull quay.io/ceph/demo:latest-squid`) and
// BLOBGATE_DOCKER_TESTS=1, and takes a few minutes to start. The monitor of Ceph refuses to start on a disk that is more than 95 % full unless it is told otherwise.

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"

	"github.com/zystem/blobgate/internal/maint"
	"github.com/zystem/blobgate/internal/store"
)

const cephImage = "quay.io/ceph/demo:latest-squid"

// cephPort is below the range of the kernel for the ports it chooses itself (32768 and up).
const cephPort = 18481

func startCeph(t *testing.T) *minio {
	t.Helper()
	if os.Getenv("BLOBGATE_DOCKER_TESTS") != "1" {
		t.Skip("BLOBGATE_DOCKER_TESTS=1 runs the tests with real servers")
	}
	if out, err := exec.Command("docker", "image", "inspect", cephImage).CombinedOutput(); err != nil {
		t.Skipf("the image %s is not there: %v %s", cephImage, err, out)
	}
	if l, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", cephPort)); err != nil {
		t.Fatalf("the port %d is in use: %v", cephPort, err)
	} else {
		l.Close()
	}
	m := &minio{port: cephPort, user: "cephaccess01", pass: "cephsecret0123456789abcdef", bucket: "blobgate-test"}
	_ = exec.Command("docker", "rm", "-f", "blobgate-test-ceph").Run()
	out, err := exec.Command("docker", "run", "-d", "--name", "blobgate-test-ceph", "--net=host",
		"-e", "CEPH_ARGS=--mon-data-avail-crit=1 --mon-data-avail-warn=2", "-e", "MON_IP=127.0.0.1", "-e", "CEPH_PUBLIC_NETWORK=127.0.0.0/8",
		"-e", "CEPH_DEMO_UID=demo", "-e", "CEPH_DEMO_ACCESS_KEY="+m.user, "-e", "CEPH_DEMO_SECRET_KEY="+m.pass,
		"-e", fmt.Sprintf("RGW_FRONTEND_PORT=%d", cephPort), "-e", "DEMO_DAEMONS=osd,rgw", cephImage).CombinedOutput()
	if err != nil {
		t.Skipf("docker could not start ceph: %v %s", err, out)
	}
	t.Cleanup(func() { _ = exec.Command("docker", "rm", "-f", "blobgate-test-ceph").Run() })
	m.client = s3.NewFromConfig(aws.Config{Region: "us-east-1", Credentials: credentials.NewStaticCredentialsProvider(m.user, m.pass, "")},
		func(o *s3.Options) {
			o.BaseEndpoint = aws.String(fmt.Sprintf("http://127.0.0.1:%d", m.port))
			o.UsePathStyle = true
		})
	var err2 error
	for i := 0; i < 90; i++ { // the gateway answers a few minutes after the start
		time.Sleep(3 * time.Second)
		if _, err2 = m.client.CreateBucket(context.Background(), &s3.CreateBucketInput{Bucket: aws.String(m.bucket)}); err2 == nil {
			return m
		}
	}
	t.Fatalf("ceph did not take a bucket: %v", err2)
	return nil
}

// Ceph RGW as the S3 node: the door in parts and in copies, and deletes that leave no object in the bucket (the round of TestRealSeaweedS3).
func TestRealCephS3(t *testing.T) {
	m := startCeph(t)
	r := newRig(t, nil)
	if code, d := r.admin("PUT", "/v1/admin/stores/ceph", m.store(map[string]any{"dedup": "tenant", "default": true})); code != 200 {
		t.Fatalf("register the bucket: %d %s", code, d)
	}
	if code, d := r.admin("POST", "/v1/admin/stores/ceph:check", nil); code != 200 {
		t.Fatalf(":check %d %s", code, d)
	}
	c := r.s3Client("001", secret1)
	ctx := context.Background()
	s3Round(t, r, c, "ceph", pattern(20<<20+321), m.objects)
	lo, _ := c.ListObjectsV2(ctx, &s3.ListObjectsV2Input{Bucket: aws.String("ceph")})
	var ids []types.ObjectIdentifier
	for _, o := range lo.Contents {
		ids = append(ids, types.ObjectIdentifier{Key: o.Key})
	}
	if _, err := c.DeleteObjects(ctx, &s3.DeleteObjectsInput{Bucket: aws.String("ceph"), Delete: &types.Delete{Objects: ids}}); err != nil {
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

// Ceph RGW is one of two nodes of a mirror (min_acks 1) and goes silent with its connections open (docker pause) in the middle of an upload in parts: the upload goes on and ends with the other
// node, the copy for Ceph is queued; when Ceph is back the repair gives it every block, and with the other node stopped the repaired Ceph alone gives the objects.
func TestRealCephSilentAsMirrorNode(t *testing.T) {
	m := startCeph(t)
	dav := startSFTPGo(t, "dav8")
	r := newRig(t, nil)
	old, oldRe := store.NodeTimeout, store.ReopenAfter
	store.NodeTimeout = 5 * time.Second
	t.Cleanup(func() { store.NodeTimeout, store.ReopenAfter = old, oldRe })
	node := map[string]any{"remote": fmt.Sprintf(":s3,provider=Ceph,endpoint='http://127.0.0.1:%d',region=us-east-1,env_auth=false,access_key_id=%s:%s/blobs", m.port, m.user, m.bucket),
		"secrets": map[string]string{"secret_access_key": m.pass}}
	if code, d := r.admin("PUT", "/v1/admin/stores/mix", map[string]any{"kind": "mirror", "default": true, "min_acks": 1, "nodes": []any{dav.webdav(), node}}); code != 200 {
		t.Fatalf("register: %d %s", code, d)
	}
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
	if out, err := exec.Command("docker", "pause", "blobgate-test-ceph").CombinedOutput(); err != nil {
		t.Fatalf("docker pause: %v %s", err, out)
	}
	paused := true
	t.Cleanup(func() {
		if paused {
			_ = exec.Command("docker", "unpause", "blobgate-test-ceph").Run()
		}
	})
	start := time.Now()
	if err := partsOf(t, c, "mix", "big", body, 2, nparts, cu.UploadId, &done); err != nil {
		t.Fatalf("the parts while Ceph is silent (min_acks 1): %v", err)
	}
	if _, err := c.CompleteMultipartUpload(ctx, &s3.CompleteMultipartUploadInput{Bucket: aws.String("mix"), Key: aws.String("big"), UploadId: cu.UploadId, MultipartUpload: &types.CompletedMultipartUpload{Parts: done}}); err != nil {
		t.Fatalf("complete while Ceph is silent: %v", err)
	}
	if _, err := c.PutObject(ctx, &s3.PutObjectInput{Bucket: aws.String("mix"), Key: aws.String("small"), Body: bytes.NewReader(pattern(1 << 20))}); err != nil {
		t.Fatalf("a write in one call while Ceph is silent: %v", err)
	}
	if !bytes.Equal(readBack(t, c, "mix", "big"), body) {
		t.Fatal("the object is not what was sent (Ceph silent)")
	}
	t.Logf("the writes and the read with Ceph silent took %v", time.Since(start).Round(time.Second))
	if r.dbCount("storage_repair") < 1 {
		t.Fatal("no copy for Ceph was queued")
	}
	if out, err := exec.Command("docker", "unpause", "blobgate-test-ceph").CombinedOutput(); err != nil {
		t.Fatalf("docker unpause: %v %s", err, out)
	}
	paused = false
	time.Sleep(store.ReopenAfter)
	w := r.worker("w8", func(cf *maint.Config) { cf.RetryBase = time.Second })
	w.Lease(ctx, "repair")
	for i := 0; i < 30 && r.dbCount("storage_repair") > 0; i++ {
		w.Pass(ctx)
		time.Sleep(2 * time.Second)
	}
	if n := r.dbCount("storage_repair"); n != 0 {
		t.Fatalf("%d repair tasks left after Ceph is back", n)
	}
	if len(m.objects()) == 0 {
		t.Fatal("the repair put nothing on Ceph")
	}
	if out, err := exec.Command("docker", "stop", "-t", "1", dav.name).CombinedOutput(); err != nil {
		t.Fatalf("stop the other node: %v %s", err, out)
	}
	if !bytes.Equal(readBack(t, c, "mix", "big"), body) || len(readBack(t, c, "mix", "small")) != 1<<20 {
		t.Fatal("the repaired Ceph does not hold the objects")
	}
}
