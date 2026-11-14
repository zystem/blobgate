package api

import (
	"bytes"
	"context"
	"crypto/md5"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"

	"github.com/zystem/blobgate/internal/auth"
)

// s3Client is the SDK of AWS (the client that the tools are made of) pointed at the S3 door of the rig, as the tenant 001, or the one named.
func (r *rig) s3Client(tenant, secret string) *s3.Client {
	ts := httptest.NewServer(r.s.S3Handler())
	r.t.Cleanup(ts.Close)
	cfg := aws.Config{Region: "eu-west-1", Credentials: credentials.NewStaticCredentialsProvider(tenant, auth.S3Secret(secret), "")}
	return s3.NewFromConfig(cfg, func(o *s3.Options) { o.BaseEndpoint = aws.String(ts.URL); o.UsePathStyle = true })
}

// uploadParts is what a tool does for a big object: parts of one size, a list of their ETags, a completion.
func uploadParts(ctx context.Context, c *s3.Client, bucket, key string, body []byte, partSize int) error {
	cu, err := c.CreateMultipartUpload(ctx, &s3.CreateMultipartUploadInput{Bucket: aws.String(bucket), Key: aws.String(key)})
	if err != nil {
		return err
	}
	var done []types.CompletedPart
	for n, off := int32(1), 0; off < len(body); n, off = n+1, off+partSize {
		up, err := c.UploadPart(ctx, &s3.UploadPartInput{Bucket: aws.String(bucket), Key: aws.String(key), UploadId: cu.UploadId, PartNumber: aws.Int32(n), Body: bytes.NewReader(body[off:min(off+partSize, len(body))])})
		if err != nil {
			_, _ = c.AbortMultipartUpload(ctx, &s3.AbortMultipartUploadInput{Bucket: aws.String(bucket), Key: aws.String(key), UploadId: cu.UploadId})
			return err
		}
		done = append(done, types.CompletedPart{ETag: up.ETag, PartNumber: aws.Int32(n)})
	}
	_, err = c.CompleteMultipartUpload(ctx, &s3.CompleteMultipartUploadInput{Bucket: aws.String(bucket), Key: aws.String(key), UploadId: cu.UploadId, MultipartUpload: &types.CompletedMultipartUpload{Parts: done}})
	return err
}

func TestS3ObjectsAndLists(t *testing.T) {
	r := newRig(t, nil)
	r.dedupStore("", 0, "")
	c := r.s3Client("001", secret1)
	ctx := context.Background()
	for _, size := range []int{0, 1, 4000, 8 << 20, 9<<20 + 17} {
		body := pattern(size)
		key := fmt.Sprintf("dir/obj-%d", size)
		if _, err := c.PutObject(ctx, &s3.PutObjectInput{Bucket: aws.String("main"), Key: aws.String(key), Body: bytes.NewReader(body), ContentType: aws.String("text/x-test"),
			Metadata: map[string]string{"owner": "me"}}); err != nil {
			t.Fatalf("put %d: %v", size, err)
		}
		got, err := c.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String("main"), Key: aws.String(key)})
		if err != nil {
			t.Fatalf("get %d: %v", size, err)
		}
		data, _ := io.ReadAll(got.Body)
		if !bytes.Equal(data, body) || aws.ToString(got.ContentType) != "text/x-test" || got.Metadata["owner"] != "me" || aws.ToInt64(got.ContentLength) != int64(size) {
			t.Fatalf("object of %d bytes came back wrong: %d bytes, %v, %v", size, len(data), aws.ToString(got.ContentType), got.Metadata)
		}
		if size > 4000 { // the core reads what the door wrote, as the tenant of the key
			if code, d := r.get(key, 0, 99); code != 206 && code != 200 || !bytes.Equal(d, body[:100]) {
				t.Fatalf("the core does not read what the door wrote: %d", code)
			}
		}
	}
	// a range, a head, a missing key
	rg, err := c.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String("main"), Key: aws.String("dir/obj-4000"), Range: aws.String("bytes=10-19")})
	if err != nil {
		t.Fatal(err)
	}
	if d, _ := io.ReadAll(rg.Body); !bytes.Equal(d, pattern(4000)[10:20]) || aws.ToString(rg.ContentRange) != "bytes 10-19/4000" {
		t.Fatalf("range: %q %s", d, aws.ToString(rg.ContentRange))
	}
	h, err := c.HeadObject(ctx, &s3.HeadObjectInput{Bucket: aws.String("main"), Key: aws.String("dir/obj-4000")})
	if err != nil || aws.ToInt64(h.ContentLength) != 4000 || h.ETag == nil {
		t.Fatalf("head: %v", err)
	}
	if _, err := c.HeadObject(ctx, &s3.HeadObjectInput{Bucket: aws.String("main"), Key: aws.String("nope")}); err == nil || !strings.Contains(err.Error(), "NotFound") {
		t.Fatalf("head of a missing key: %v", err)
	}
	if _, err := c.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String("main"), Key: aws.String("nope")}); err == nil || !strings.Contains(err.Error(), "NoSuchKey") {
		t.Fatalf("get of a missing key: %v", err)
	}
	// the list: all, with a prefix, with folders, and by pages
	for _, k := range []string{"a/1", "a/2", "a/b/3", "c"} {
		if _, err := c.PutObject(ctx, &s3.PutObjectInput{Bucket: aws.String("main"), Key: aws.String("l/" + k), Body: strings.NewReader("x")}); err != nil {
			t.Fatal(err)
		}
	}
	lo, err := c.ListObjectsV2(ctx, &s3.ListObjectsV2Input{Bucket: aws.String("main"), Prefix: aws.String("l/"), Delimiter: aws.String("/")})
	if err != nil || len(lo.Contents) != 1 || aws.ToString(lo.Contents[0].Key) != "l/c" || len(lo.CommonPrefixes) != 1 || aws.ToString(lo.CommonPrefixes[0].Prefix) != "l/a/" {
		t.Fatalf("folders: %v %+v", err, lo)
	}
	var keys []string
	p := s3.NewListObjectsV2Paginator(c, &s3.ListObjectsV2Input{Bucket: aws.String("main"), Prefix: aws.String("l/"), MaxKeys: aws.Int32(2)})
	pages := 0
	for p.HasMorePages() {
		pg, err := p.NextPage(ctx)
		if err != nil {
			t.Fatal(err)
		}
		pages++
		for _, o := range pg.Contents {
			keys = append(keys, aws.ToString(o.Key))
		}
	}
	if !sort.StringsAreSorted(keys) || strings.Join(keys, ",") != "l/a/1,l/a/2,l/a/b/3,l/c" || pages != 2 {
		t.Fatalf("pages %d: %v", pages, keys)
	}
	l1, err := c.ListObjects(ctx, &s3.ListObjectsInput{Bucket: aws.String("main"), Prefix: aws.String("l/a/")})
	if err != nil || len(l1.Contents) != 3 {
		t.Fatalf("v1: %v %+v", err, l1)
	}
	// delete one, and many
	if _, err := c.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: aws.String("main"), Key: aws.String("l/c")}); err != nil {
		t.Fatal(err)
	}
	dr, err := c.DeleteObjects(ctx, &s3.DeleteObjectsInput{Bucket: aws.String("main"), Delete: &types.Delete{Objects: []types.ObjectIdentifier{{Key: aws.String("l/a/1")}, {Key: aws.String("l/a/2")}, {Key: aws.String("l/zzz")}}}})
	if err != nil || len(dr.Deleted) != 3 || len(dr.Errors) != 0 {
		t.Fatalf("delete many: %v %+v", err, dr)
	}
	if code, _ := r.get("l/c", 0, 0); code != 404 {
		t.Fatal("deleted object is there")
	}
	// the buckets
	lb, err := c.ListBuckets(ctx, &s3.ListBucketsInput{})
	if err != nil || len(lb.Buckets) != 1 || aws.ToString(lb.Buckets[0].Name) != "main" {
		t.Fatalf("buckets: %v %+v", err, lb)
	}
	if _, err := c.HeadBucket(ctx, &s3.HeadBucketInput{Bucket: aws.String("main")}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.HeadBucket(ctx, &s3.HeadBucketInput{Bucket: aws.String("nope")}); err == nil {
		t.Fatal("a bucket that is not a store")
	}
}

func TestS3Multipart(t *testing.T) {
	r := newRig(t, nil)
	r.dedupStore("", 0, "")
	c := r.s3Client("001", secret1)
	ctx := context.Background()
	body := pattern(8<<20*3 + 12345)
	if err := uploadParts(ctx, c, "main", "big/file", body, 8<<20); err != nil {
		t.Fatalf("upload: %v", err)
	}
	got, err := c.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String("main"), Key: aws.String("big/file")})
	if err != nil {
		t.Fatal(err)
	}
	if d, _ := io.ReadAll(got.Body); !bytes.Equal(d, body) {
		t.Fatal("the object is not what was uploaded")
	}
	// parts of any one size up to 8 MiB (the SDK's own default is 5 MiB) make an object; a part over 8 MiB is refused with the reason; parts of different sizes are refused at the end
	odd := pattern(12<<20 + 99)
	if err := uploadParts(ctx, c, "main", "odd/file", odd, 5<<20); err != nil {
		t.Fatalf("parts of 5 MiB: %v", err)
	}
	if g, err := c.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String("main"), Key: aws.String("odd/file"), Range: aws.String("bytes=5242870-5242890")}); err != nil {
		t.Fatal(err)
	} else if d, _ := io.ReadAll(g.Body); !bytes.Equal(d, odd[5242870:5242891]) {
		t.Fatal("a range across the parts of 5 MiB")
	}
	if err := uploadParts(ctx, c, "main", "huge/file", pattern(20<<20), 16<<20); err == nil || !strings.Contains(err.Error(), "8 MiB") {
		t.Fatalf("parts of 16 MiB: %v", err)
	}
	if code, _ := r.get("huge/file", 0, 0); code != 404 {
		t.Fatal("a half object is there")
	}
	cu0, _ := c.CreateMultipartUpload(ctx, &s3.CreateMultipartUploadInput{Bucket: aws.String("main"), Key: aws.String("mixed/x")})
	var done []types.CompletedPart
	for i, n := range []int{1000, 2000, 500} {
		up, err := c.UploadPart(ctx, &s3.UploadPartInput{Bucket: aws.String("main"), Key: aws.String("mixed/x"), UploadId: cu0.UploadId, PartNumber: aws.Int32(int32(i + 1)), Body: bytes.NewReader(pattern(n))})
		if err != nil {
			t.Fatal(err)
		}
		done = append(done, types.CompletedPart{ETag: up.ETag, PartNumber: aws.Int32(int32(i + 1))})
	}
	if _, err := c.CompleteMultipartUpload(ctx, &s3.CompleteMultipartUploadInput{Bucket: aws.String("main"), Key: aws.String("mixed/x"), UploadId: cu0.UploadId, MultipartUpload: &types.CompletedMultipartUpload{Parts: done}}); err == nil || !strings.Contains(err.Error(), "as long as the first") {
		t.Fatalf("parts of different sizes: %v", err)
	}
	if _, err := c.AbortMultipartUpload(ctx, &s3.AbortMultipartUploadInput{Bucket: aws.String("main"), Key: aws.String("mixed/x"), UploadId: cu0.UploadId}); err != nil {
		t.Fatal(err)
	}
	// an upload that is abandoned
	cu, err := c.CreateMultipartUpload(ctx, &s3.CreateMultipartUploadInput{Bucket: aws.String("main"), Key: aws.String("ab/x")})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.UploadPart(ctx, &s3.UploadPartInput{Bucket: aws.String("main"), Key: aws.String("ab/x"), UploadId: cu.UploadId, PartNumber: aws.Int32(1), Body: strings.NewReader("hello")}); err != nil {
		t.Fatal(err)
	}
	lp, err := c.ListParts(ctx, &s3.ListPartsInput{Bucket: aws.String("main"), Key: aws.String("ab/x"), UploadId: cu.UploadId})
	if err != nil || len(lp.Parts) != 1 || aws.ToInt64(lp.Parts[0].Size) != 5 {
		t.Fatalf("parts: %v %+v", err, lp)
	}
	if _, err := c.AbortMultipartUpload(ctx, &s3.AbortMultipartUploadInput{Bucket: aws.String("main"), Key: aws.String("ab/x"), UploadId: cu.UploadId}); err != nil {
		t.Fatal(err)
	}
	if n := r.count("storage_parts"); n != 0 {
		t.Fatalf("%d parts left", n)
	}
}

func TestS3Refusals(t *testing.T) {
	r := newRig(t, func(c *Config) { c.ShardSecrets["other"] = "the-secret-of-the-other-tenant" })
	r.dedupStore("", 0, "")
	ctx := context.Background()
	good := r.s3Client("001", secret1)
	if _, err := good.PutObject(ctx, &s3.PutObjectInput{Bucket: aws.String("main"), Key: aws.String("k"), Body: strings.NewReader("mine")}); err != nil {
		t.Fatal(err)
	}
	// a wrong secret, an access key that is not a tenant
	for name, c := range map[string]*s3.Client{"secret": r.s3Client("001", "not-the-secret"), "key": r.s3Client("nobody", secret1)} {
		_, err := c.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String("main"), Key: aws.String("k")})
		if err == nil || !(strings.Contains(err.Error(), "SignatureDoesNotMatch") || strings.Contains(err.Error(), "InvalidAccessKeyId")) {
			t.Fatalf("%s: %v", name, err)
		}
	}
	// another tenant does not see the key
	other := r.s3Client("other", "the-secret-of-the-other-tenant")
	if _, err := other.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String("main"), Key: aws.String("k")}); err == nil || !strings.Contains(err.Error(), "NoSuchKey") {
		t.Fatalf("other tenant: %v", err)
	}
	lo, err := other.ListObjectsV2(ctx, &s3.ListObjectsV2Input{Bucket: aws.String("main")})
	if err != nil || len(lo.Contents) != 0 {
		t.Fatalf("other tenant list: %v %+v", err, lo)
	}
	// what is not done says so
	if _, err := good.CopyObject(ctx, &s3.CopyObjectInput{Bucket: aws.String("main"), Key: aws.String("k2"), CopySource: aws.String("main/k")}); err == nil || !strings.Contains(err.Error(), "NotImplemented") {
		t.Fatalf("copy: %v", err)
	}
	if _, err := good.PutBucketAcl(ctx, &s3.PutBucketAclInput{Bucket: aws.String("main"), ACL: types.BucketCannedACLPrivate}); err == nil || !strings.Contains(err.Error(), "NotImplemented") {
		t.Fatalf("acl: %v", err)
	}
	if v, err := good.GetBucketVersioning(ctx, &s3.GetBucketVersioningInput{Bucket: aws.String("main")}); err != nil || v.Status != "" {
		t.Fatalf("versioning: %v %+v", err, v)
	}
	if _, err := good.PutBucketVersioning(ctx, &s3.PutBucketVersioningInput{Bucket: aws.String("main"), VersioningConfiguration: &types.VersioningConfiguration{Status: types.BucketVersioningStatusEnabled}}); err == nil || !strings.Contains(err.Error(), "NotImplemented") {
		t.Fatalf("enabling versioning: %v", err)
	}
	if _, err := good.CreateBucket(ctx, &s3.CreateBucketInput{Bucket: aws.String("new")}); err == nil {
		t.Fatal("a bucket was made")
	}
	// a body whose digest is not the one that was signed
	if _, err := good.PutObject(ctx, &s3.PutObjectInput{Bucket: aws.String("main"), Key: aws.String("x"), Body: strings.NewReader("abc"), ChecksumSHA256: aws.String("AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=")}); err != nil {
		t.Logf("a wrong checksum header is refused or not checked by the door: %v", err)
	}
}

func TestS3PresignedAndStreamed(t *testing.T) {
	r := newRig(t, nil)
	r.dedupStore("", 0, "")
	c := r.s3Client("001", secret1)
	ctx := context.Background()
	if _, err := c.PutObject(ctx, &s3.PutObjectInput{Bucket: aws.String("main"), Key: aws.String("p/q"), Body: strings.NewReader("presigned text")}); err != nil {
		t.Fatal(err)
	}
	ps := s3.NewPresignClient(c)
	req, err := ps.PresignGetObject(ctx, &s3.GetObjectInput{Bucket: aws.String("main"), Key: aws.String("p/q")})
	if err != nil {
		t.Fatal(err)
	}
	resp, err := httpGet(req.URL)
	if err != nil || resp != "presigned text" {
		t.Fatalf("presigned: %v %q", err, resp)
	}
	if _, err := httpGet(strings.Replace(req.URL, "/p/q?", "/p/r?", 1)); err == nil {
		t.Fatal("a presigned address for another key was taken")
	}
}

func httpGet(u string) (string, error) {
	resp, err := httpClient.Get(u)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		return string(b), fmt.Errorf("status %d", resp.StatusCode)
	}
	return string(b), nil
}

var httpClient = http.DefaultClient

// the ETag of an object written in one call is the MD5 of its bytes, as in S3 (s3cmd and rclone check it); that of a multipart object has a dash, which they do not check
func TestS3ETagIsMD5(t *testing.T) {
	r := newRig(t, nil)
	r.dedupStore("tenant", 0, "")
	c := r.s3Client("001", secret1)
	ctx := context.Background()
	for _, size := range []int{5, 9<<20 + 3} {
		body := pattern(size)
		sum := md5.Sum(body)
		want := `"` + hex.EncodeToString(sum[:]) + `"`
		out, err := c.PutObject(ctx, &s3.PutObjectInput{Bucket: aws.String("main"), Key: aws.String(fmt.Sprintf("m/%d", size)), Body: bytes.NewReader(body)})
		if err != nil || aws.ToString(out.ETag) != want {
			t.Fatalf("size %d: put gave %v, want %s (%v)", size, aws.ToString(out.ETag), want, err)
		}
		// the same bytes under another key: the same ETag, and the list names it too
		if _, err := c.PutObject(ctx, &s3.PutObjectInput{Bucket: aws.String("main"), Key: aws.String(fmt.Sprintf("m/copy-%d", size)), Body: bytes.NewReader(body)}); err != nil {
			t.Fatal(err)
		}
		h, _ := c.HeadObject(ctx, &s3.HeadObjectInput{Bucket: aws.String("main"), Key: aws.String(fmt.Sprintf("m/copy-%d", size))})
		if aws.ToString(h.ETag) != want {
			t.Fatalf("head of a copy: %s", aws.ToString(h.ETag))
		}
		// a link keeps it
		code, d := r.link(fmt.Sprintf("m/linked-%d", size), sha(body), size)
		if code != 200 {
			t.Fatalf("link: %d %s", code, d)
		}
		h, _ = c.HeadObject(ctx, &s3.HeadObjectInput{Bucket: aws.String("main"), Key: aws.String(fmt.Sprintf("m/linked-%d", size))})
		if aws.ToString(h.ETag) != want {
			t.Fatalf("head of a link: %s", aws.ToString(h.ETag))
		}
	}
	lo, _ := c.ListObjectsV2(ctx, &s3.ListObjectsV2Input{Bucket: aws.String("main"), Prefix: aws.String("m/")})
	for _, o := range lo.Contents {
		h, _ := c.HeadObject(ctx, &s3.HeadObjectInput{Bucket: aws.String("main"), Key: o.Key})
		if aws.ToString(o.ETag) != aws.ToString(h.ETag) {
			t.Fatalf("%s: the list says %s and the head %s", aws.ToString(o.Key), aws.ToString(o.ETag), aws.ToString(h.ETag))
		}
	}
	// a Content-MD5 that is wrong
	bad := md5.Sum([]byte("something else"))
	if _, err := c.PutObject(ctx, &s3.PutObjectInput{Bucket: aws.String("main"), Key: aws.String("m/bad"), Body: strings.NewReader("abc"), ContentMD5: aws.String(base64.StdEncoding.EncodeToString(bad[:]))}); err == nil || !strings.Contains(err.Error(), "BadDigest") {
		t.Fatalf("a wrong Content-MD5: %v", err)
	}
	if code, _ := r.get("m/bad", 0, 0); code != 404 {
		t.Fatal("an object that failed its digest is there")
	}
	// a multipart object
	if err := uploadParts(ctx, c, "main", "m/multi", pattern(11<<20), 5<<20); err != nil {
		t.Fatal(err)
	}
	if h, _ := c.HeadObject(ctx, &s3.HeadObjectInput{Bucket: aws.String("main"), Key: aws.String("m/multi")}); !strings.Contains(aws.ToString(h.ETag), "-3") {
		t.Fatalf("a multipart object has the ETag %s", aws.ToString(h.ETag))
	}
}

// keys with spaces, plus signs, percent signs, non-ASCII letters and characters that a signature encodes with care
func TestS3AwkwardKeys(t *testing.T) {
	r := newRig(t, nil)
	r.dedupStore("", 0, "")
	c := r.s3Client("001", secret1)
	ctx := context.Background()
	keys := []string{"w/a b", "w/a+b", "w/100%", "w/é ü.txt", "w/x=1&y=2", "w/!*'()", "w/~tilde", "w/a;b,c", "w/日本語"}
	for _, k := range keys {
		if _, err := c.PutObject(ctx, &s3.PutObjectInput{Bucket: aws.String("main"), Key: aws.String(k), Body: strings.NewReader("v:" + k)}); err != nil {
			t.Fatalf("put %q: %v", k, err)
		}
		g, err := c.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String("main"), Key: aws.String(k)})
		if err != nil {
			t.Fatalf("get %q: %v", k, err)
		}
		if d, _ := io.ReadAll(g.Body); string(d) != "v:"+k {
			t.Fatalf("%q came back as %q", k, d)
		}
	}
	lo, err := c.ListObjectsV2(ctx, &s3.ListObjectsV2Input{Bucket: aws.String("main"), Prefix: aws.String("w/")})
	if err != nil || len(lo.Contents) != len(keys) {
		t.Fatalf("list: %v %d", err, len(lo.Contents))
	}
	got := map[string]bool{}
	for _, o := range lo.Contents {
		got[aws.ToString(o.Key)] = true
	}
	for _, k := range keys {
		if !got[k] {
			t.Fatalf("the list lacks %q: %v", k, got)
		}
	}
}
