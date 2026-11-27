package api

import (
	"bytes"
	"context"
	"io"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
)

// An abort removes the blocks of an upload that has not ended and of nothing else: an abort of an upload that was completed (a client that gave up waiting for the answer and cleans up) must leave the
// object and, with dedup, the objects that share its blocks.
func TestAbortAfterCompletionKeepsTheObject(t *testing.T) {
	for _, dedup := range []string{"", "tenant"} {
		r := newRig(t, nil)
		r.dedupStore(dedup, 0, "")
		ctx := context.Background()
		c := r.s3Client("001", secret1)
		body := pattern(100_000)
		cu, err := c.CreateMultipartUpload(ctx, &s3.CreateMultipartUploadInput{Bucket: aws.String("main"), Key: aws.String("k")})
		if err != nil {
			t.Fatal(err)
		}
		up, err := c.UploadPart(ctx, &s3.UploadPartInput{Bucket: aws.String("main"), Key: aws.String("k"), UploadId: cu.UploadId, PartNumber: aws.Int32(1), Body: bytes.NewReader(body)})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := c.CompleteMultipartUpload(ctx, &s3.CompleteMultipartUploadInput{Bucket: aws.String("main"), Key: aws.String("k"), UploadId: cu.UploadId,
			MultipartUpload: &types.CompletedMultipartUpload{Parts: []types.CompletedPart{{ETag: up.ETag, PartNumber: aws.Int32(1)}}}}); err != nil {
			t.Fatal(err)
		}
		_, _ = c.AbortMultipartUpload(ctx, &s3.AbortMultipartUploadInput{Bucket: aws.String("main"), Key: aws.String("k"), UploadId: cu.UploadId})
		got, err := c.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String("main"), Key: aws.String("k")})
		if err != nil {
			t.Fatalf("dedup %q: the object after an abort of its completed upload: %v (node 1 holds %v)", dedup, err, files(r.nodes[0]))
		}
		data, _ := io.ReadAll(got.Body)
		if !bytes.Equal(data, body) {
			t.Fatalf("dedup %q: %d bytes", dedup, len(data))
		}
	}
}

// An abort and a completion at the same moment: exactly one wins. Held back after the abort has begun to remove blocks, the completion must not publish an object whose blocks are going.
func TestAbortAndCompletionDoNotBothWin(t *testing.T) {
	r := newRig(t, nil)
	r.mirror(0)
	body := pattern(100_000)
	id := r.newUpload("acme/a")
	if code, d := r.sendPart("acme/a", id, "1", body); code != 200 {
		t.Fatalf("part: %d %s", code, d)
	}
	var completed int
	r.s.cfg.CrashAt = func(p string) {
		if p == "abort:after-purge" {
			completed, _, _ = r.core("POST", "/v1/stores/main/objects/acme/a", "upload="+id, nil, []byte(`{"parts":[[1,"`+etag(sha(body), int64(len(body)))+`"]]}`))
		}
	}
	if code, d, _ := r.core("DELETE", "/v1/stores/main/objects/acme/a", "upload="+id, nil, nil); code != 200 {
		t.Fatalf("abort: %d %s", code, d)
	}
	if completed == 200 {
		code, _ := r.get("acme/a", 0, len(body)-1)
		t.Fatalf("the completion was acknowledged while the abort removed its blocks; reading it now: %d", code)
	}
	if completed == 0 || completed == 200 || r.count("storage_objects") != 0 {
		t.Fatalf("completion %d, %d objects", completed, r.count("storage_objects"))
	}

	// the abort comes when the completion has read the parts and not yet committed: the completion is the one that loses
	id = r.newUpload("acme/b")
	if code, d := r.sendPart("acme/b", id, "1", body); code != 200 {
		t.Fatalf("part: %d %s", code, d)
	}
	r.s.cfg.CrashAt = func(p string) {
		if p == "complete:before-commit" {
			r.core("DELETE", "/v1/stores/main/objects/acme/b", "upload="+id, nil, nil)
		}
	}
	code, d, _ := r.core("POST", "/v1/stores/main/objects/acme/b", "upload="+id, nil, []byte(`{"parts":[[1,"`+etag(sha(body), int64(len(body)))+`"]]}`))
	if code != 404 || r.count("storage_objects") != 0 || len(files(r.nodes[0])) != 0 {
		t.Fatalf("a completion that lost to an abort: %d %s, %d objects, node 1 holds %v", code, d, r.count("storage_objects"), files(r.nodes[0]))
	}
}
