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
	"time"

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

// CopyObject: inside a store of sets it takes a reference on the set (nothing is stored again, whatever the size, an upload in parts included), between stores or with no sets it is a copy of the bytes;
// the metadata is kept or replaced, a copy onto itself is refused unless it replaces the metadata.
func TestS3CopyObject(t *testing.T) {
	r := newRig(t, nil)
	r.dedupStore("tenant", 0, "")
	other := t.TempDir()
	if code, d := r.admin("PUT", "/v1/admin/stores/plain", map[string]any{"kind": "mirror",
		"nodes": []any{map[string]any{"remote": ":local:" + other + "/a"}, map[string]any{"remote": ":local:" + other + "/b"}}}); code != 200 {
		t.Fatalf("register: %d %s", code, d)
	}
	c := r.s3Client("001", secret1)
	ctx := context.Background()
	body := pattern(9<<20 + 17)
	put, err := c.PutObject(ctx, &s3.PutObjectInput{Bucket: aws.String("main"), Key: aws.String("dir/a b"), Body: bytes.NewReader(body), ContentType: aws.String("text/x-one"), Metadata: map[string]string{"owner": "me"}})
	if err != nil {
		t.Fatal(err)
	}
	read := func(bucket, key string) ([]byte, *s3.GetObjectOutput) {
		t.Helper()
		g, err := c.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String(bucket), Key: aws.String(key)})
		if err != nil {
			t.Fatalf("get %s/%s: %v", bucket, key, err)
		}
		d, _ := io.ReadAll(g.Body)
		return d, g
	}
	sets := r.dbCount("storage_sets")

	// in the store: a reference, the metadata kept
	cp, err := c.CopyObject(ctx, &s3.CopyObjectInput{Bucket: aws.String("main"), Key: aws.String("copy/b"), CopySource: aws.String("main/dir/a%20b")})
	if err != nil {
		t.Fatalf("copy in the store: %v", err)
	}
	if aws.ToString(cp.CopyObjectResult.ETag) != aws.ToString(put.ETag) {
		t.Fatalf("the ETag of the copy is %s, of the source %s", aws.ToString(cp.CopyObjectResult.ETag), aws.ToString(put.ETag))
	}
	d, g := read("main", "copy/b")
	if !bytes.Equal(d, body) || aws.ToString(g.ContentType) != "text/x-one" || g.Metadata["owner"] != "me" {
		t.Fatalf("the copy is not the source: %d bytes, %s, %v", len(d), aws.ToString(g.ContentType), g.Metadata)
	}
	if got := r.dbCount("storage_sets"); got != sets {
		t.Fatalf("a copy in the store made a set: %d sets, were %d", got, sets)
	}
	r.refsAreTheRows()

	// the metadata replaced
	if _, err := c.CopyObject(ctx, &s3.CopyObjectInput{Bucket: aws.String("main"), Key: aws.String("copy/c"), CopySource: aws.String("/main/dir/a%20b"), MetadataDirective: types.MetadataDirectiveReplace,
		ContentType: aws.String("text/x-two"), Metadata: map[string]string{"owner": "you"}}); err != nil {
		t.Fatal(err)
	}
	if _, g = read("main", "copy/c"); aws.ToString(g.ContentType) != "text/x-two" || g.Metadata["owner"] != "you" {
		t.Fatalf("the metadata was not replaced: %s %v", aws.ToString(g.ContentType), g.Metadata)
	}
	// onto itself: refused, unless it replaces the metadata
	_, err = c.CopyObject(ctx, &s3.CopyObjectInput{Bucket: aws.String("main"), Key: aws.String("copy/b"), CopySource: aws.String("main/copy/b")})
	if err == nil || !strings.Contains(err.Error(), "InvalidRequest") {
		t.Fatalf("a copy onto itself: %v", err)
	}
	if _, err = c.CopyObject(ctx, &s3.CopyObjectInput{Bucket: aws.String("main"), Key: aws.String("copy/b"), CopySource: aws.String("main/copy/b"), MetadataDirective: types.MetadataDirectiveReplace,
		Metadata: map[string]string{"owner": "again"}}); err != nil {
		t.Fatalf("a copy onto itself that replaces the metadata: %v", err)
	}
	if d, g = read("main", "copy/b"); !bytes.Equal(d, body) || g.Metadata["owner"] != "again" {
		t.Fatalf("the object that replaced its metadata: %d bytes %v", len(d), g.Metadata)
	}
	r.refsAreTheRows()

	// the source goes, the copies stay
	if _, err := c.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: aws.String("main"), Key: aws.String("dir/a b")}); err != nil {
		t.Fatal(err)
	}
	if d, _ = read("main", "copy/c"); !bytes.Equal(d, body) {
		t.Fatal("the copy does not outlive the source")
	}

	// between stores: the bytes, and back (the sets of the first store give a reference to the bytes written again)
	if _, err := c.CopyObject(ctx, &s3.CopyObjectInput{Bucket: aws.String("plain"), Key: aws.String("p/one"), CopySource: aws.String("main/copy/c")}); err != nil {
		t.Fatalf("copy to another store: %v", err)
	}
	d, g = read("plain", "p/one")
	if !bytes.Equal(d, body) || aws.ToString(g.ContentType) != "text/x-two" || g.Metadata["owner"] != "you" {
		t.Fatalf("the copy in the other store: %d bytes %s %v", len(d), aws.ToString(g.ContentType), g.Metadata)
	}
	if _, err := c.CopyObject(ctx, &s3.CopyObjectInput{Bucket: aws.String("main"), Key: aws.String("back"), CopySource: aws.String("plain/p/one")}); err != nil {
		t.Fatalf("copy back: %v", err)
	}
	if d, _ = read("main", "back"); !bytes.Equal(d, body) {
		t.Fatal("the copy that came back is not the source")
	}
	if got := r.dbCount("storage_sets"); got != sets {
		t.Fatalf("the bytes written again did not find their set: %d sets, were %d", got, sets)
	}
	r.refsAreTheRows()

	// what it refuses and what is not there
	for what, in := range map[string]*s3.CopyObjectInput{
		"NoSuchKey":       {Bucket: aws.String("main"), Key: aws.String("x"), CopySource: aws.String("main/not-there")},
		"NoSuchBucket":    {Bucket: aws.String("main"), Key: aws.String("x"), CopySource: aws.String("nostore/k")},
		"NotImplemented":  {Bucket: aws.String("main"), Key: aws.String("x"), CopySource: aws.String("main/copy/c?versionId=1")},
		"InvalidArgument": {Bucket: aws.String("main"), Key: aws.String("x"), CopySource: aws.String("main")},
	} {
		if _, err := c.CopyObject(ctx, in); err == nil || !strings.Contains(err.Error(), what) {
			t.Errorf("%s: %v", what, err)
		}
	}
	if _, err := c.HeadObject(ctx, &s3.HeadObjectInput{Bucket: aws.String("main"), Key: aws.String("x")}); err == nil {
		t.Error("a refused copy left an object")
	}

	// the set removed between the read of the source and the transaction: the answer is NoSuchKey and the old destination is untouched
	zbody := pattern(5000)
	if _, err := c.PutObject(ctx, &s3.PutObjectInput{Bucket: aws.String("main"), Key: aws.String("z"), Body: bytes.NewReader(zbody)}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.PutObject(ctx, &s3.PutObjectInput{Bucket: aws.String("main"), Key: aws.String("src"), Body: bytes.NewReader(pattern(7000))}); err != nil {
		t.Fatal(err)
	}
	r.s.cfg.CrashAt = func(p string) {
		if p == "copy:before" {
			_, _ = c.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: aws.String("main"), Key: aws.String("src")})
			r.sweepSets()
		}
	}
	if _, err := c.CopyObject(ctx, &s3.CopyObjectInput{Bucket: aws.String("main"), Key: aws.String("z"), CopySource: aws.String("main/src")}); err == nil || !strings.Contains(err.Error(), "NoSuchKey") {
		t.Fatalf("a copy of a set that went: %v", err)
	}
	r.s.cfg.CrashAt = nil
	if d, _ = read("main", "z"); !bytes.Equal(d, zbody) {
		t.Fatal("the destination that the failed copy was to replace is gone")
	}
	r.refsAreTheRows()
}

// UploadPartCopy: an object is made of ranges of another (what the AWS CLI does above its multipart threshold), with the conditions on the source that the tools send.
func TestS3UploadPartCopy(t *testing.T) {
	r := newRig(t, nil)
	r.dedupStore("tenant", 0, "")
	c := r.s3Client("001", secret1)
	ctx := context.Background()
	body := pattern(20<<20 + 5)
	put, err := c.PutObject(ctx, &s3.PutObjectInput{Bucket: aws.String("main"), Key: aws.String("src"), Body: bytes.NewReader(body)})
	if err != nil {
		t.Fatal(err)
	}
	const part = 8 << 20
	cu, err := c.CreateMultipartUpload(ctx, &s3.CreateMultipartUploadInput{Bucket: aws.String("main"), Key: aws.String("dst")})
	if err != nil {
		t.Fatal(err)
	}
	var done []types.CompletedPart
	for n, off := int32(1), 0; off < len(body); n, off = n+1, off+part {
		end := min(off+part, len(body)) - 1
		up, err := c.UploadPartCopy(ctx, &s3.UploadPartCopyInput{Bucket: aws.String("main"), Key: aws.String("dst"), UploadId: cu.UploadId, PartNumber: aws.Int32(n),
			CopySource: aws.String("main/src"), CopySourceRange: aws.String(fmt.Sprintf("bytes=%d-%d", off, end)), CopySourceIfMatch: put.ETag})
		if err != nil {
			t.Fatalf("part %d: %v", n, err)
		}
		done = append(done, types.CompletedPart{ETag: up.CopyPartResult.ETag, PartNumber: aws.Int32(n)})
	}
	if _, err := c.CompleteMultipartUpload(ctx, &s3.CompleteMultipartUploadInput{Bucket: aws.String("main"), Key: aws.String("dst"), UploadId: cu.UploadId, MultipartUpload: &types.CompletedMultipartUpload{Parts: done}}); err != nil {
		t.Fatal(err)
	}
	g, err := c.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String("main"), Key: aws.String("dst")})
	if err != nil {
		t.Fatal(err)
	}
	if d, _ := io.ReadAll(g.Body); !bytes.Equal(d, body) {
		t.Fatalf("the object made of ranges is not the source: %d bytes", len(d))
	}
	r.refsAreTheRows()

	// the conditions, on a copy of the whole object and on a part
	for name, in := range map[string]*s3.CopyObjectInput{
		"if-match that is not the tag":         {CopySourceIfMatch: aws.String(`"not-the-tag"`)},
		"if-none-match that is the tag":        {CopySourceIfNoneMatch: put.ETag},
		"if-unmodified-since before the write": {CopySourceIfUnmodifiedSince: aws.Time(time.Unix(1_000_000_000, 0))},
	} {
		in.Bucket, in.Key, in.CopySource = aws.String("main"), aws.String("c-"+strings.ReplaceAll(name, " ", "-")), aws.String("main/src")
		if _, err := c.CopyObject(ctx, in); err == nil || !strings.Contains(err.Error(), "PreconditionFailed") {
			t.Errorf("%s: %v", name, err)
		}
	}
	if _, err := c.CopyObject(ctx, &s3.CopyObjectInput{Bucket: aws.String("main"), Key: aws.String("c-ok"), CopySource: aws.String("main/src"), CopySourceIfMatch: put.ETag,
		CopySourceIfModifiedSince: aws.Time(time.Unix(1_000_000_000, 0))}); err != nil {
		t.Errorf("conditions that hold: %v", err)
	}

	// what is refused: a range past the end, a part that is longer than a block, a range that is not first-last, an upload that is not there
	for what, in := range map[string]*s3.UploadPartCopyInput{
		"InvalidRange":    {CopySourceRange: aws.String(fmt.Sprintf("bytes=0-%d", len(body)))},
		"EntityTooLarge":  {},
		"InvalidArgument": {CopySourceRange: aws.String("bytes=5-")},
		"NoSuchUpload":    {CopySourceRange: aws.String("bytes=0-9"), UploadId: aws.String("0123456789abcdef0123456789abcdef")},
	} {
		in.Bucket, in.Key, in.CopySource, in.PartNumber = aws.String("main"), aws.String("dst2"), aws.String("main/src"), aws.Int32(1)
		if in.UploadId == nil {
			cu2, err := c.CreateMultipartUpload(ctx, &s3.CreateMultipartUploadInput{Bucket: aws.String("main"), Key: aws.String("dst2")})
			if err != nil {
				t.Fatal(err)
			}
			in.UploadId = cu2.UploadId
		}
		if _, err := c.UploadPartCopy(ctx, in); err == nil || !strings.Contains(err.Error(), what) {
			t.Errorf("%s: %v", what, err)
		}
	}
}

// The names of the metadata go out in lower case, as S3 sends them (a client that reads them as they come, boto3, gives back what was written).
func TestS3MetadataNamesAreLowerCaseOnTheWire(t *testing.T) {
	rec := httptest.NewRecorder()
	cw := &s3w{w: rec, h: http.Header{}, stream: true}
	cw.Header().Set("X-Blobgate-Meta-Owner", "me")
	cw.Header().Set("ETag", `"x"`)
	cw.WriteHeader(200)
	if v := rec.Header()["x-amz-meta-owner"]; len(v) != 1 || v[0] != "me" {
		t.Fatalf("the metadata went out as %v", rec.Header())
	}
	if _, ok := rec.Header()["X-Amz-Meta-Owner"]; ok {
		t.Fatal("the name with capitals went out too")
	}
}

// A listing of nothing says KeyCount 0 (v2), as S3 does: the Java SDK reads a missing one as null and fails; a v1 listing has no KeyCount.
func TestS3EmptyListingSaysKeyCountZero(t *testing.T) {
	r := newRig(t, nil)
	r.dedupStore("", 0, "")
	cl := r.s3Client("001", secret1)
	lo, err := cl.ListObjectsV2(context.Background(), &s3.ListObjectsV2Input{Bucket: aws.String("main")})
	if err != nil || lo.KeyCount == nil || *lo.KeyCount != 0 {
		t.Fatalf("an empty listing: %v %+v", err, lo)
	}
	l1, err := cl.ListObjects(context.Background(), &s3.ListObjectsInput{Bucket: aws.String("main")})
	if err != nil || len(l1.Contents) != 0 {
		t.Fatalf("v1: %v %+v", err, l1)
	}
}
