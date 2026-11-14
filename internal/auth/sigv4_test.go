package auth

import (
	"bytes"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

const (
	awsKey    = "AKIAIOSFODNN7EXAMPLE"
	awsSecret = "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY"
)

var awsNow = time.Date(2013, 5, 24, 0, 0, 5, 0, time.UTC)

func awsSecretOf(k string) (string, bool) { return awsSecret, k == awsKey }

// the "GET Object" example of the documentation of AWS (Signature Version 4, header form): the signature is the one that the documentation gives
func awsGet(t *testing.T) *http.Request {
	r, _ := http.NewRequest("GET", "http://examplebucket.s3.amazonaws.com/test.txt", nil)
	r.Header.Set("Range", "bytes=0-9")
	r.Header.Set("X-Amz-Content-Sha256", "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855")
	r.Header.Set("X-Amz-Date", "20130524T000000Z")
	r.Header.Set("Authorization", "AWS4-HMAC-SHA256 Credential=AKIAIOSFODNN7EXAMPLE/20130524/us-east-1/s3/aws4_request,SignedHeaders=host;range;x-amz-content-sha256;x-amz-date,Signature=f0e8bdb87c964420e857bd35b5d6ed310bd44f0170aba48dd91039c6036bdb41")
	return r
}

func TestSigV4MatchesTheDocumentation(t *testing.T) {
	c, err := VerifySigV4(awsGet(t), awsSecretOf, awsNow)
	if err != nil || c.AccessKey != awsKey {
		t.Fatalf("the example of the documentation: %v", err)
	}
	// the presigned example of the documentation (the query form)
	r, _ := http.NewRequest("GET", "http://examplebucket.s3.amazonaws.com/test.txt?X-Amz-Algorithm=AWS4-HMAC-SHA256&X-Amz-Credential=AKIAIOSFODNN7EXAMPLE%2F20130524%2Fus-east-1%2Fs3%2Faws4_request&X-Amz-Date=20130524T000000Z&X-Amz-Expires=86400&X-Amz-SignedHeaders=host&X-Amz-Signature=aeeed9bbccd4d02ee5c0109b86d86835f995330da4c265957d157751f604d404", nil)
	if _, err := VerifySigV4(r, awsSecretOf, awsNow); err != nil {
		t.Fatalf("the presigned example: %v", err)
	}
	if _, err := VerifySigV4(r, awsSecretOf, awsNow.Add(25*time.Hour)); !errors.Is(err, ErrExpired) {
		t.Fatalf("a presigned address after its time: %v", err)
	}
}

func TestSigV4Refusals(t *testing.T) {
	cases := map[string]struct {
		edit func(*http.Request)
		now  time.Time
		want error
	}{
		"no signature":    {func(r *http.Request) { r.Header.Del("Authorization") }, awsNow, ErrNoAuth},
		"another header":  {func(r *http.Request) { r.Header.Set("Range", "bytes=0-99") }, awsNow, ErrBadSig},
		"another path":    {func(r *http.Request) { r.URL.Path = "/other.txt" }, awsNow, ErrBadSig},
		"another query":   {func(r *http.Request) { r.URL.RawQuery = "acl" }, awsNow, ErrBadSig},
		"another method":  {func(r *http.Request) { r.Method = "PUT" }, awsNow, ErrBadSig},
		"another body":    {func(r *http.Request) { r.Header.Set("X-Amz-Content-Sha256", strings.Repeat("a", 64)) }, awsNow, ErrBadSig},
		"another host":    {func(r *http.Request) { r.Host = "other.example.com" }, awsNow, ErrBadSig},
		"too old":         {func(r *http.Request) {}, awsNow.Add(time.Hour), ErrExpired},
		"from the future": {func(r *http.Request) {}, awsNow.Add(-time.Hour), ErrExpired},
		"not a tenant": {func(r *http.Request) {
			r.Header.Set("Authorization", strings.Replace(r.Header.Get("Authorization"), awsKey, "NOBODY", 1))
		}, awsNow, ErrUnknownKey},
		"another service": {func(r *http.Request) {
			r.Header.Set("Authorization", strings.Replace(r.Header.Get("Authorization"), "/s3/", "/ec2/", 1))
		}, awsNow, ErrBadRequest},
		"no host signed": {func(r *http.Request) {
			r.Header.Set("Authorization", strings.Replace(r.Header.Get("Authorization"), "host;", "", 1))
		}, awsNow, ErrBadRequest},
		"another scheme": {func(r *http.Request) { r.Header.Set("Authorization", "Bearer abc") }, awsNow, ErrBadRequest},
	}
	for name, c := range cases {
		r := awsGet(t)
		c.edit(r)
		if _, err := VerifySigV4(r, awsSecretOf, c.now); !errors.Is(err, c.want) {
			t.Errorf("%s: %v, want %v", name, err, c.want)
		}
	}
	if S3Secret("a") == S3Secret("b") || S3Secret("a") == "a" || len(S3Secret("a")) != 64 {
		t.Fatal("the secret key of the S3 door")
	}
}

// signedChunks makes a body in aws-chunked form for a call, as a client does.
func signedChunks(c *S3Call, parts [][]byte, trailer bool) []byte {
	var b bytes.Buffer
	prev := c.seed
	for _, p := range append(parts, nil) {
		sts := "AWS4-HMAC-SHA256-PAYLOAD\n" + c.stamp + "\n" + c.scope + "\n" + prev + "\n" + hexSHA("") + "\n" + hex.EncodeToString(sha256Sum(p))
		prev = hex.EncodeToString(hmacRaw(c.key, sts))
		fmt.Fprintf(&b, "%x;chunk-signature=%s\r\n", len(p), prev)
		if len(p) > 0 {
			b.Write(p)
			b.WriteString("\r\n")
		}
	}
	if trailer {
		b.WriteString("x-amz-checksum-crc32c:AAAAAA==\r\nx-amz-trailer-signature:00\r\n\r\n")
	} else {
		b.WriteString("\r\n")
	}
	return b.Bytes()
}

func TestChunkedBody(t *testing.T) {
	c := &S3Call{key: []byte("k"), stamp: "20130524T000000Z", scope: "20130524/us-east-1/s3/aws4_request", seed: strings.Repeat("ab", 32), PayloadHash: streamingSig}
	parts := [][]byte{bytes.Repeat([]byte("a"), 100), []byte("hello"), bytes.Repeat([]byte("z"), 70000)}
	want := bytes.Join(parts, nil)
	got, err := io.ReadAll(c.ChunkedBody(bytes.NewReader(signedChunks(c, parts, false))))
	if err != nil || !bytes.Equal(got, want) {
		t.Fatalf("signed: %v, %d bytes", err, len(got))
	}
	c.PayloadHash = streamingSigTr
	if got, err = io.ReadAll(c.ChunkedBody(bytes.NewReader(signedChunks(c, parts, true)))); err != nil || !bytes.Equal(got, want) {
		t.Fatalf("signed with a trailer: %v", err)
	}
	// a byte changed in a chunk, a chunk dropped, a signature taken away
	body := signedChunks(c, parts, true)
	bad := append([]byte(nil), body...)
	bad[bytes.Index(bad, []byte("hello"))] = 'j'
	if _, err := io.ReadAll(c.ChunkedBody(bytes.NewReader(bad))); !errors.Is(err, ErrBadChunk) {
		t.Fatalf("a changed chunk: %v", err)
	}
	firstEnd := bytes.Index(body, []byte("\r\n")) + 2 + len(parts[0]) + 2 // the first chunk: its line, its bytes, the end of line
	if _, err := io.ReadAll(c.ChunkedBody(bytes.NewReader(body[firstEnd:]))); !errors.Is(err, ErrBadChunk) {
		t.Fatalf("a chunk dropped: %v", err)
	}
	if _, err := io.ReadAll(c.ChunkedBody(bytes.NewReader(body[:len(body)/2]))); err == nil {
		t.Fatal("a body that stops half way was taken")
	}
	// unsigned, with a trailer (what the newer SDKs send over TLS)
	c.PayloadHash = streamingUnTr
	un := []byte("5\r\nhello\r\n6\r\n world\r\n0\r\nx-amz-checksum-crc32:AAAAAA==\r\n\r\n")
	if got, err := io.ReadAll(c.ChunkedBody(bytes.NewReader(un))); err != nil || string(got) != "hello world" {
		t.Fatalf("unsigned: %v %q", err, got)
	}
	if _, err := io.ReadAll(c.ChunkedBody(strings.NewReader("ffffffff\r\nx"))); !errors.Is(err, ErrBadChunk) {
		t.Fatalf("a chunk of 4 GiB: %v", err)
	}
}

func FuzzChunkedBody(f *testing.F) {
	c := &S3Call{key: []byte("k"), stamp: "20130524T000000Z", scope: "s", seed: "x", PayloadHash: streamingUnTr}
	f.Add([]byte("5\r\nhello\r\n0\r\n\r\n"))
	f.Add([]byte("0\r\n\r\n"))
	f.Add([]byte("zz;chunk-signature=\r\n"))
	f.Fuzz(func(t *testing.T, b []byte) {
		for _, mode := range []string{streamingUnTr, streamingSig, streamingSigTr} {
			c.PayloadHash = mode
			_, _ = io.Copy(io.Discard, c.ChunkedBody(bytes.NewReader(b))) // it must end, and not panic
		}
	})
}

func FuzzVerifySigV4(f *testing.F) {
	f.Add("AWS4-HMAC-SHA256 Credential=AKIAIOSFODNN7EXAMPLE/20130524/us-east-1/s3/aws4_request,SignedHeaders=host;x-amz-date,Signature=00", "/a/b", "x=1&y")
	f.Fuzz(func(t *testing.T, authz, path, query string) {
		r, err := http.NewRequest("GET", "http://h/", nil)
		if err != nil {
			return
		}
		r.URL.Path, r.URL.RawQuery = path, query
		r.Header.Set("Authorization", authz)
		r.Header.Set("X-Amz-Date", "20130524T000000Z")
		r.Header.Set("X-Amz-Content-Sha256", "UNSIGNED-PAYLOAD")
		_, _ = VerifySigV4(r, awsSecretOf, awsNow)
	})
}
