package api

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

// send makes the request of a presigned address, with the headers that were signed and with what `edit` adds: what whoever holds the address, or carries the request, can do.
func send(t *testing.T, method, url string, signed http.Header, body []byte, add ...[2]string) (int, string) {
	t.Helper()
	req, _ := http.NewRequest(method, url, bytes.NewReader(body))
	for k, v := range signed {
		if k != "Host" {
			req.Header[k] = v
		}
	}
	for _, h := range add {
		req.Header.Set(h[0], h[1])
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	d, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(d)
}

// Finding 2 of the review of 2026-11-22, the acceptance for presigned addresses and for the copy of a part: a header that decides what a call does, added to a presigned PUT or part, or not covered by
// the signature of a part copy, is refused, and no object, part or repair task changes. The same calls without the addition, and a part copy whose headers are signed, are done.
func TestPresignedAndPartCopyDoNotBelieveWhatIsNotSigned(t *testing.T) {
	r := newRig(t, nil)
	r.mirror(0)
	ctx := context.Background()
	c := r.s3Client("001", secret1)
	pre := s3.NewPresignClient(c)
	if _, err := c.PutObject(ctx, &s3.PutObjectInput{Bucket: aws.String("main"), Key: aws.String("secret"), Body: bytes.NewReader(pattern(9000))}); err != nil {
		t.Fatal(err)
	}
	cu, err := c.CreateMultipartUpload(ctx, &s3.CreateMultipartUploadInput{Bucket: aws.String("main"), Key: aws.String("big")})
	if err != nil {
		t.Fatal(err)
	}
	put, err := pre.PresignPutObject(ctx, &s3.PutObjectInput{Bucket: aws.String("main"), Key: aws.String("given")})
	if err != nil {
		t.Fatal(err)
	}
	part, err := pre.PresignUploadPart(ctx, &s3.UploadPartInput{Bucket: aws.String("main"), Key: aws.String("big"), UploadId: cu.UploadId, PartNumber: aws.Int32(1)})
	if err != nil {
		t.Fatal(err)
	}
	counts := func() [3]int64 {
		return [3]int64{r.count("storage_objects"), r.count("storage_parts"), r.count("storage_repair")}
	}
	before := counts()
	added := [][2]string{{"X-Amz-Copy-Source", "main/secret"}, {"X-Amz-Copy-Source-Range", "bytes=0-3"}, {"X-Amz-Metadata-Directive", "REPLACE"}, {"X-Amz-Meta-Owner", "someone"},
		{"X-Amz-Checksum-Crc32", "AAAAAA=="}, {"Content-MD5", "AAAAAAAAAAAAAAAAAAAAAA=="}}
	for what, a := range map[string]struct {
		url    string
		signed http.Header
	}{"PutObject": {put.URL, put.SignedHeader}, "UploadPart": {part.URL, part.SignedHeader}} {
		for _, h := range added {
			if code, d := send(t, "PUT", a.url, a.signed, []byte("from whoever holds the address"), h); code != 403 || !strings.Contains(d, "AccessDenied") {
				t.Errorf("presigned %s with %s added: %d %s", what, h[0], code, d)
			}
		}
	}
	// a part copy with signed headers, where the copy source (or its range) is added after the signature
	partPath := "/main/big?partNumber=2&uploadId=" + aws.ToString(cu.UploadId)
	sc := r.signedCalls()
	for _, h := range added[:2] {
		code, d := sc.do("PUT", partPath, nil, []byte{}, []byte{}, func(q *http.Request) { q.Header.Set(h[0], h[1]) })
		if code != 403 || !strings.Contains(d, "AccessDenied") {
			t.Errorf("a part with %s added after the signature: %d %s", h[0], code, d)
		}
	}
	if code, d := sc.do("PUT", partPath, map[string]string{"X-Amz-Copy-Source": "main/secret"}, []byte{}, []byte{}, func(q *http.Request) { q.Header.Set("X-Amz-Copy-Source-Range", "bytes=0-3") }); code != 403 {
		t.Errorf("a range added to a signed part copy: %d %s", code, d)
	}
	if got := counts(); got != before {
		t.Errorf("refused calls changed the store: objects, parts, repairs %v, were %v", got, before)
	}
	// the positive cases
	if code, d := sc.do("PUT", partPath, map[string]string{"X-Amz-Copy-Source": "main/secret", "X-Amz-Copy-Source-Range": "bytes=0-3"}, []byte{}, []byte{}, nil); code != 200 {
		t.Errorf("a signed part copy: %d %s", code, d)
	}
	if code, d := send(t, "PUT", put.URL, put.SignedHeader, []byte("given")); code != 200 {
		t.Errorf("the presigned PUT as it is: %d %s", code, d)
	}
	if code, d := send(t, "PUT", part.URL, part.SignedHeader, []byte("a part")); code != 200 {
		t.Errorf("the presigned part as it is: %d %s", code, d)
	}
}
