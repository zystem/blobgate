package api

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	v4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"

	"github.com/zystem/blobgate/internal/auth"
)

// signedCall makes a call to the door the way a client signs it, and lets the test change the request after the signature (what a proxy or the holder of a presigned address can do).
type signedCall struct {
	t   *testing.T
	url string
}

func (r *rig) signedCalls() *signedCall {
	ts := httptest.NewServer(r.s.S3Handler())
	r.t.Cleanup(ts.Close)
	return &signedCall{t: r.t, url: ts.URL}
}

// do signs a request with `signedBody` as the payload digest and sends `sentBody`; edit may change the request after the signature.
func (c *signedCall) do(method, path string, headers map[string]string, signedBody, sentBody []byte, edit func(*http.Request)) (int, string) {
	c.t.Helper()
	req, _ := http.NewRequest(method, c.url+path, bytes.NewReader(signedBody))
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	sum := sha256.Sum256(signedBody)
	payload := hex.EncodeToString(sum[:])
	req.Header.Set("X-Amz-Content-Sha256", payload)
	creds := aws.Credentials{AccessKeyID: "001", SecretAccessKey: auth.S3Secret(secret1)}
	if err := v4.NewSigner().SignHTTP(context.Background(), creds, req, payload, "s3", "us-east-1", time.Now()); err != nil {
		c.t.Fatal(err)
	}
	req.Body, req.ContentLength = io.NopCloser(bytes.NewReader(sentBody)), int64(len(sentBody))
	if edit != nil {
		edit(req)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		c.t.Fatal(err)
	}
	defer resp.Body.Close()
	d, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(d)
}

// Finding 2 of the review of 2026-11-22: what the signature does not cover is not believed. A header that decides what a call does (the source of a copy, the directive of the metadata, the metadata,
// the digests) that was added after the signature is refused; the body of a delete of many keys and of the completion of an upload is checked against the digest the client signed, and against a
// checksum it declared, before anything is done with it. Nothing may change in the store when a call is refused.
func TestWhatTheSignatureDoesNotCoverIsRefused(t *testing.T) {
	r := newRig(t, nil)
	r.mirror(0)
	r.dedupStore("", 0, "")
	ctx := context.Background()
	c := r.s3Client("001", secret1)
	for _, k := range []string{"secret", "victim"} {
		if _, err := c.PutObject(ctx, &s3.PutObjectInput{Bucket: aws.String("main"), Key: aws.String(k), Body: bytes.NewReader([]byte("body of " + k))}); err != nil {
			t.Fatal(err)
		}
	}
	sc := r.signedCalls()
	state := func() string {
		lo, err := c.ListObjectsV2(ctx, &s3.ListObjectsV2Input{Bucket: aws.String("main")})
		if err != nil {
			t.Fatal(err)
		}
		var keys []string
		for _, o := range lo.Contents {
			keys = append(keys, aws.ToString(o.Key)+":"+aws.ToString(o.ETag))
		}
		return strings.Join(keys, ",") + " repair:" + string(rune('0'+r.count("storage_repair"))) + " objects:" + string(rune('0'+r.count("storage_objects")))
	}
	before := state()

	// a PUT whose signature does not cover the header that makes it a copy
	code, d := sc.do("PUT", "/main/copied", nil, []byte{}, []byte{}, func(q *http.Request) { q.Header.Set("X-Amz-Copy-Source", "main/secret") })
	if code != 403 || !strings.Contains(d, "AccessDenied") || !strings.Contains(d, "x-amz-copy-source") {
		t.Errorf("a copy source added after the signature: %d %s", code, d)
	}
	// the same with the headers of the metadata and of a part copy
	for _, h := range [][2]string{{"X-Amz-Metadata-Directive", "REPLACE"}, {"X-Amz-Meta-Owner", "someone"}, {"X-Amz-Copy-Source-Range", "bytes=0-3"}, {"X-Amz-Checksum-Crc32", "AAAAAA=="}} {
		code, d = sc.do("PUT", "/main/x", nil, []byte("x"), []byte("x"), func(q *http.Request) { q.Header.Set(h[0], h[1]) })
		if code != 403 || !strings.Contains(d, "AccessDenied") {
			t.Errorf("%s added after the signature: %d %s", h[0], code, d)
		}
	}
	// signed, the same copy works (the positive case: the headers are covered)
	if code, d = sc.do("PUT", "/main/copied", map[string]string{"X-Amz-Copy-Source": "main/secret"}, []byte{}, []byte{}, nil); code != 200 {
		t.Errorf("a signed copy: %d %s", code, d)
	}
	if _, err := c.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: aws.String("main"), Key: aws.String("copied")}); err != nil {
		t.Fatal(err)
	}

	// a delete of many keys: signed for the delete of "other", sent for the delete of "secret"
	del := func(k string) []byte {
		return []byte(`<Delete><Object><Key>` + k + `</Key></Object></Delete>`)
	}
	code, d = sc.do("POST", "/main?delete", nil, del("victim"), del("secret"), nil)
	if code != 400 || !strings.Contains(d, "XAmzContentSHA256Mismatch") {
		t.Errorf("a delete whose body is not the signed one: %d %s", code, d)
	}
	// the Content-MD5 of the body, wrong
	code, d = sc.do("POST", "/main?delete", map[string]string{"Content-MD5": "AAAAAAAAAAAAAAAAAAAAAA=="}, del("victim"), del("victim"), nil)
	if code != 400 || !strings.Contains(d, "BadDigest") {
		t.Errorf("a delete with a wrong Content-MD5: %d %s", code, d)
	}
	if got := state(); got != before {
		t.Errorf("the store changed by calls that were refused:\n before %s\n after  %s", before, got)
	}
	// and the right body is done
	if code, d = sc.do("POST", "/main?delete", nil, del("victim"), del("victim"), nil); code != 200 {
		t.Errorf("a delete with the signed body: %d %s", code, d)
	}

	// the completion of an upload: the parts of the body are not the signed ones
	cu, err := c.CreateMultipartUpload(ctx, &s3.CreateMultipartUploadInput{Bucket: aws.String("main"), Key: aws.String("big")})
	if err != nil {
		t.Fatal(err)
	}
	up, err := c.UploadPart(ctx, &s3.UploadPartInput{Bucket: aws.String("main"), Key: aws.String("big"), UploadId: cu.UploadId, PartNumber: aws.Int32(1), Body: bytes.NewReader(pattern(3000))})
	if err != nil {
		t.Fatal(err)
	}
	complete := func(etag string) []byte {
		return []byte(`<CompleteMultipartUpload><Part><PartNumber>1</PartNumber><ETag>` + etag + `</ETag></Part></CompleteMultipartUpload>`)
	}
	good := aws.ToString(up.ETag)
	wrong := good[:len(good)-2] + "00" // the same length: the signature covers the length of the body too, and a body of another length is refused before its digest is looked at
	if wrong == good {
		wrong = good[:len(good)-2] + "11"
	}
	code, d = sc.do("POST", "/main/big?uploadId="+aws.ToString(cu.UploadId), nil, complete(good), complete(wrong), nil)
	if code != 400 || !strings.Contains(d, "XAmzContentSHA256Mismatch") {
		t.Errorf("a completion whose body is not the signed one: %d %s", code, d)
	}
	if _, err := c.ListParts(ctx, &s3.ListPartsInput{Bucket: aws.String("main"), Key: aws.String("big"), UploadId: cu.UploadId}); err != nil {
		t.Errorf("the upload was touched by a refused completion: %v", err)
	}
	if _, err := c.CompleteMultipartUpload(ctx, &s3.CompleteMultipartUploadInput{Bucket: aws.String("main"), Key: aws.String("big"), UploadId: cu.UploadId,
		MultipartUpload: &types.CompletedMultipartUpload{Parts: []types.CompletedPart{{ETag: up.ETag, PartNumber: aws.Int32(1)}}}}); err != nil {
		t.Errorf("the right completion: %v", err)
	}
	// a PUT with a declared checksum that is not the body's leaves no object
	code, d = sc.do("PUT", "/main/checked", map[string]string{"X-Amz-Checksum-Crc32": "AAAAAA=="}, []byte("some bytes"), []byte("some bytes"), nil)
	if code != 400 || !strings.Contains(d, "BadDigest") {
		t.Errorf("a PUT with a wrong crc32: %d %s", code, d)
	}
	if _, err := c.HeadObject(ctx, &s3.HeadObjectInput{Bucket: aws.String("main"), Key: aws.String("checked")}); err == nil {
		t.Error("an object was made of a body that failed its declared checksum")
	}
}

var _ = credentials.NewStaticCredentialsProvider
