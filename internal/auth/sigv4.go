package auth

import (
	"bufio"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

// The S3 door. A call is signed as AWS Signature Version 4 (the header form or the query form of a presigned address); the access key is the name of a tenant, and the secret key is made from the
// secret of that tenant, so that nothing new is kept: `blobgate s3-key <tenant>` prints the pair.

// S3Secret is the secret key of a tenant for the S3 door: it is not the secret that signs the tokens of the core, and it does not give it away.
func S3Secret(tenantSecret string) string { return mac(tenantSecret, "blobgate/s3/v1") }

// Why a signature was refused. The S3 door turns each into the code a client expects.
var (
	ErrNoAuth      = errors.New("the call is not signed")
	ErrBadRequest  = errors.New("the signature header is malformed")
	ErrUnknownKey  = errors.New("the access key is not a tenant")
	ErrBadSig      = errors.New("the signature does not match")
	ErrExpired     = errors.New("the request is too old, or from too far in the future")
	ErrBadChunk    = errors.New("a chunk of the body is not signed right")
	maxSkew        = 15 * time.Minute
	maxPresign     = 7 * 24 * time.Hour
	maxChunk       = 16 << 20
	unsignedBody   = "UNSIGNED-PAYLOAD"
	streamingSig   = "STREAMING-AWS4-HMAC-SHA256-PAYLOAD"
	streamingSigTr = "STREAMING-AWS4-HMAC-SHA256-PAYLOAD-TRAILER"
	streamingUnTr  = "STREAMING-UNSIGNED-PAYLOAD-TRAILER"
)

// S3Call is a call whose signature was found good.
type S3Call struct {
	AccessKey   string
	PayloadHash string // the x-amz-content-sha256 the client signed: a hex digest, UNSIGNED-PAYLOAD, or a STREAMING-... mode
	key         []byte // the signing key of the day, to check the chunks of a streamed body
	stamp       string
	scope       string
	seed        string // the signature of the headers, the first link of the chain of chunk signatures
}

// Streaming says whether the body is sent in aws-chunked form.
func (c *S3Call) Streaming() bool { return strings.HasPrefix(c.PayloadHash, "STREAMING-") }

// Digest is the hex SHA-256 of the body when the client signed one, "" when it did not (or the body is streamed).
func (c *S3Call) Digest() string {
	if validHex64(c.PayloadHash) {
		return strings.ToLower(c.PayloadHash)
	}
	return ""
}

func validHex64(s string) bool {
	if len(s) != 64 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F') {
			return false
		}
	}
	return true
}

func hmacRaw(key []byte, msg string) []byte {
	h := hmac.New(sha256.New, key)
	h.Write([]byte(msg))
	return h.Sum(nil)
}

func hexSHA(s string) string { h := sha256.Sum256([]byte(s)); return hex.EncodeToString(h[:]) }

// awsEscape is the URI encoding of SigV4: everything but A-Z a-z 0-9 - _ . ~ is %XX, and "/" stays when the text is a path.
func awsEscape(s string, path bool) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'A' && c <= 'Z', c >= 'a' && c <= 'z', c >= '0' && c <= '9', c == '-', c == '_', c == '.', c == '~', c == '/' && path:
			b.WriteByte(c)
		default:
			fmt.Fprintf(&b, "%%%02X", c)
		}
	}
	return b.String()
}

func canonicalQuery(q url.Values, skip string) string {
	type kv struct{ k, v string }
	var all []kv
	for k, vs := range q {
		if k == skip {
			continue
		}
		for _, v := range vs {
			all = append(all, kv{awsEscape(k, false), awsEscape(v, false)})
		}
	}
	sort.Slice(all, func(i, j int) bool {
		if all[i].k != all[j].k {
			return all[i].k < all[j].k
		}
		return all[i].v < all[j].v
	})
	parts := make([]string, len(all))
	for i, p := range all {
		parts[i] = p.k + "=" + p.v
	}
	return strings.Join(parts, "&")
}

func headerValue(r *http.Request, name string) string {
	if name == "host" {
		return r.Host
	}
	vals := r.Header.Values(name)
	for i, v := range vals {
		vals[i] = strings.Join(strings.Fields(v), " ")
	}
	return strings.Join(vals, ",")
}

// VerifySigV4 checks the signature of a call. secretOf gives the secret key for an access key (false: there is no such tenant).
func VerifySigV4(r *http.Request, secretOf func(accessKey string) (string, bool), now time.Time) (*S3Call, error) {
	q := r.URL.Query()
	var cred, signedList, sig, stamp string
	presigned := false
	var expires time.Duration
	if h := r.Header.Get("Authorization"); h != "" {
		rest, ok := strings.CutPrefix(h, "AWS4-HMAC-SHA256")
		if !ok {
			return nil, ErrBadRequest
		}
		for _, f := range strings.Split(rest, ",") {
			k, v, _ := strings.Cut(strings.TrimSpace(f), "=")
			switch k {
			case "Credential":
				cred = v
			case "SignedHeaders":
				signedList = v
			case "Signature":
				sig = v
			}
		}
		stamp = r.Header.Get("X-Amz-Date")
	} else if q.Get("X-Amz-Algorithm") != "" {
		if q.Get("X-Amz-Algorithm") != "AWS4-HMAC-SHA256" {
			return nil, ErrBadRequest
		}
		presigned = true
		cred, signedList, sig, stamp = q.Get("X-Amz-Credential"), q.Get("X-Amz-SignedHeaders"), q.Get("X-Amz-Signature"), q.Get("X-Amz-Date")
		secs, err := strconv.ParseInt(q.Get("X-Amz-Expires"), 10, 64)
		if err != nil || secs < 1 {
			return nil, ErrBadRequest
		}
		expires = time.Duration(secs) * time.Second
		if expires > maxPresign {
			return nil, ErrBadRequest
		}
	} else {
		return nil, ErrNoAuth
	}
	cp := strings.Split(cred, "/")
	if len(cp) != 5 || cp[3] != "s3" || cp[4] != "aws4_request" || cp[0] == "" || signedList == "" || sig == "" {
		return nil, ErrBadRequest
	}
	t, err := time.Parse("20060102T150405Z", stamp)
	if err != nil || stamp[:8] != cp[1] {
		return nil, ErrBadRequest
	}
	if presigned {
		if t.After(now.Add(maxSkew)) || now.After(t.Add(expires)) {
			return nil, ErrExpired
		}
	} else if d := now.Sub(t); d > maxSkew || -d > maxSkew {
		return nil, ErrExpired
	}
	secret, ok := secretOf(cp[0])
	if !ok {
		return nil, ErrUnknownKey
	}
	signed := strings.Split(signedList, ";")
	hasHost := false
	var ch strings.Builder
	for _, n := range signed {
		if n != strings.ToLower(n) || n == "" {
			return nil, ErrBadRequest
		}
		hasHost = hasHost || n == "host"
		ch.WriteString(n + ":" + headerValue(r, n) + "\n")
	}
	if !hasHost {
		return nil, ErrBadRequest
	}
	payload := r.Header.Get("X-Amz-Content-Sha256")
	if presigned {
		payload = unsignedBody
	} else if payload == "" {
		return nil, ErrBadRequest
	}
	skip := ""
	if presigned {
		skip = "X-Amz-Signature"
	}
	canon := strings.Join([]string{r.Method, awsEscape(r.URL.Path, true), canonicalQuery(q, skip), ch.String(), signedList, payload}, "\n")
	scope := strings.Join(cp[1:], "/")
	sts := "AWS4-HMAC-SHA256\n" + stamp + "\n" + scope + "\n" + hexSHA(canon)
	key := hmacRaw(hmacRaw(hmacRaw(hmacRaw([]byte("AWS4"+secret), cp[1]), cp[2]), "s3"), "aws4_request")
	want := hex.EncodeToString(hmacRaw(key, sts))
	if subtle.ConstantTimeCompare([]byte(want), []byte(strings.ToLower(sig))) != 1 {
		return nil, ErrBadSig
	}
	return &S3Call{AccessKey: cp[0], PayloadHash: payload, key: key, stamp: stamp, scope: scope, seed: want}, nil
}

// ChunkedBody decodes a body sent as aws-chunked (`hex-size;chunk-signature=...\r\n data \r\n ...`, ending with a chunk of size 0, and with a trailer when the mode says so). In the signed modes the
// signature of every chunk is checked, as a link of the chain that began with the signature of the headers, before a byte of the chunk is given; a chunk that is wrong ends the read with ErrBadChunk.
// The checksum a trailer may carry is read and not checked: the module makes its own digests of what it stores.
func (c *S3Call) ChunkedBody(r io.Reader) io.Reader {
	return &chunked{br: bufio.NewReader(r), c: c, prev: c.seed,
		signed: c.PayloadHash == streamingSig || c.PayloadHash == streamingSigTr, trailer: c.PayloadHash == streamingSigTr || c.PayloadHash == streamingUnTr}
}

type chunked struct {
	br      *bufio.Reader
	c       *S3Call
	prev    string
	signed  bool
	trailer bool
	buf     []byte
	err     error
	done    bool
}

func (d *chunked) line() (string, error) {
	var out []byte
	for {
		b, err := d.br.ReadByte()
		if err != nil {
			return "", io.ErrUnexpectedEOF
		}
		if b == '\n' {
			return strings.TrimSuffix(string(out), "\r"), nil
		}
		if out = append(out, b); len(out) > 4096 {
			return "", ErrBadChunk
		}
	}
}

func (d *chunked) next() error {
	head, err := d.line()
	if err != nil {
		return err
	}
	sizeText, ext, _ := strings.Cut(head, ";")
	size, err := strconv.ParseInt(strings.TrimSpace(sizeText), 16, 64)
	if err != nil || size < 0 || size > int64(maxChunk) {
		return ErrBadChunk
	}
	sig := ""
	if d.signed {
		var ok bool
		if sig, ok = strings.CutPrefix(ext, "chunk-signature="); !ok {
			return ErrBadChunk
		}
	}
	data := make([]byte, size)
	if _, err := io.ReadFull(d.br, data); err != nil {
		return io.ErrUnexpectedEOF
	}
	if d.signed {
		sts := "AWS4-HMAC-SHA256-PAYLOAD\n" + d.c.stamp + "\n" + d.c.scope + "\n" + d.prev + "\n" + hexSHA("") + "\n" + hex.EncodeToString(sha256Sum(data))
		want := hex.EncodeToString(hmacRaw(d.c.key, sts))
		if subtle.ConstantTimeCompare([]byte(want), []byte(strings.ToLower(sig))) != 1 {
			return ErrBadChunk
		}
		d.prev = want
	}
	if size > 0 {
		if l, err := d.line(); err != nil || l != "" {
			return ErrBadChunk
		}
		d.buf = data
		return nil
	}
	// the last chunk: a trailer (lines up to an empty one), or the empty line that ends the body
	for {
		l, err := d.line()
		if err != nil {
			return err
		}
		if l == "" || !d.trailer {
			break
		}
	}
	d.done = true
	return nil
}

func sha256Sum(b []byte) []byte { h := sha256.Sum256(b); return h[:] }

func (d *chunked) Read(p []byte) (int, error) {
	for len(d.buf) == 0 {
		if d.err != nil {
			return 0, d.err
		}
		if d.done {
			return 0, io.EOF
		}
		if err := d.next(); err != nil {
			d.err = err
			return 0, err
		}
	}
	n := copy(p, d.buf)
	d.buf = d.buf[n:]
	return n, nil
}
