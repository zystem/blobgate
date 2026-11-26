package auth

import (
	"bufio"
	"crypto/hmac"
	"crypto/sha1" // #nosec G505 -- x-amz-checksum-sha1 of S3: a checksum a sender declares, not a defence
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"hash/crc32"
	"hash/crc64"
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
	ErrBadChecksum = errors.New("the checksum the sender declared is not the checksum of the body")
	ErrUnsigned    = errors.New("a header that decides what the call does is not in the signed headers")
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
	signed      map[string]bool
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
	signedSet := make(map[string]bool, len(signed))
	for _, n := range signed {
		signedSet[n] = true
	}
	return &S3Call{AccessKey: cp[0], PayloadHash: payload, key: key, stamp: stamp, scope: scope, seed: want, signed: signedSet}, nil
}

// decides says whether a header changes what a call does (the source of a copy, how its metadata is made, the metadata, the digests the body is checked with) or what it says it is made of. S3 itself
// refuses a call with such a header that is not signed ("There were headers present in the request which were not signed"); a signature that does not cover them proves nothing about them, and
// whoever carries the request (a proxy, the holder of a presigned address) could add one.
func decides(name string) bool {
	switch name {
	case "x-amz-copy-source", "x-amz-copy-source-range", "x-amz-copy-source-if-match", "x-amz-copy-source-if-none-match", "x-amz-copy-source-if-modified-since",
		"x-amz-copy-source-if-unmodified-since", "x-amz-metadata-directive", "x-amz-tagging-directive", "x-amz-decoded-content-length", "x-amz-trailer", "x-amz-sdk-checksum-algorithm", "content-md5":
		return true
	}
	return strings.HasPrefix(name, "x-amz-meta-") || strings.HasPrefix(name, "x-amz-checksum-")
}

// CheckSigned refuses a call that carries a header which decides what it does and is not in the list of signed headers. The error names the header.
func (c *S3Call) CheckSigned(r *http.Request) error {
	for name := range r.Header {
		n := strings.ToLower(name)
		if decides(n) && !c.signed[n] {
			return fmt.Errorf("%w: %s", ErrUnsigned, n)
		}
	}
	return nil
}

// ChunkedBody decodes a body sent as aws-chunked (`hex-size;chunk-signature=...\r\n data \r\n ...`, ending with a chunk of size 0, and with a trailer when the mode says so). In the signed modes the
// signature of every chunk is checked, as a link of the chain that began with the signature of the headers, before a byte of the chunk is given; a chunk that is wrong ends the read with ErrBadChunk.
// A checksum that the trailer carries is checked by CheckedBody, which wraps the body (the signed trailer modes also check the signature of the trailer).
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
	remain  int64     // bytes of the current chunk still to be read from the wire
	h       hash.Hash // the SHA-256 of the current chunk so far (the signed modes)
	sig     string    // the signature the current chunk declared
	err     error
	done    bool
	trailed map[string]string // the lines of the trailer (lower-case name: value), when the body ended with one
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

// header reads the line that opens a chunk and sets up the reading of its bytes. A chunk is not held whole in memory: its bytes are given as they come and its signature is checked when the last of
// them has been read (the read that would give the next byte answers ErrBadChunk instead). What was given before then is not committed by anyone: a body that ends with an error is a write that is
// refused, and the module commits an object only after it has read the body to its end without one.
func (d *chunked) header() error {
	head, err := d.line()
	if err != nil {
		return err
	}
	sizeText, ext, _ := strings.Cut(head, ";")
	size, err := strconv.ParseInt(strings.TrimSpace(sizeText), 16, 64)
	if err != nil || size < 0 || size > int64(maxChunk) {
		return ErrBadChunk
	}
	d.sig = ""
	if d.signed {
		var ok bool
		if d.sig, ok = strings.CutPrefix(ext, "chunk-signature="); !ok {
			return ErrBadChunk
		}
		d.h = sha256.New()
	}
	d.remain = size
	if size == 0 {
		return d.endChunk(true)
	}
	return nil
}

// endChunk checks the signature of the chunk that has just been read, and reads what follows it: the end of the line after its bytes, or, after the last chunk, the trailer.
func (d *chunked) endChunk(last bool) error {
	if d.signed {
		sts := "AWS4-HMAC-SHA256-PAYLOAD\n" + d.c.stamp + "\n" + d.c.scope + "\n" + d.prev + "\n" + hexSHA("") + "\n" + hex.EncodeToString(d.h.Sum(nil))
		want := hex.EncodeToString(hmacRaw(d.c.key, sts))
		if subtle.ConstantTimeCompare([]byte(want), []byte(strings.ToLower(d.sig))) != 1 {
			return ErrBadChunk
		}
		d.prev = want
	}
	if !last {
		if l, err := d.line(); err != nil || l != "" {
			return ErrBadChunk
		}
		return nil
	}
	// the last chunk: a trailer (lines up to an empty one), or the empty line that ends the body
	var canon strings.Builder
	trailerSig := ""
	for {
		l, err := d.line()
		if err != nil {
			return err
		}
		if l == "" || !d.trailer {
			break
		}
		name, val, ok := strings.Cut(l, ":")
		if !ok {
			return ErrBadChunk
		}
		name = strings.ToLower(strings.TrimSpace(name))
		val = strings.TrimSpace(val)
		if name == "x-amz-trailer-signature" {
			trailerSig = val
			continue
		}
		if d.trailed == nil {
			d.trailed = map[string]string{}
		}
		d.trailed[name] = val
		canon.WriteString(name + ":" + val + "\n")
	}
	if d.trailer && d.signed { // the trailer is signed as one more link of the chain: a checksum that was changed on the way is not believed
		sts := "AWS4-HMAC-SHA256-TRAILER\n" + d.c.stamp + "\n" + d.c.scope + "\n" + d.prev + "\n" + hexSHA(canon.String())
		if subtle.ConstantTimeCompare([]byte(hex.EncodeToString(hmacRaw(d.c.key, sts))), []byte(strings.ToLower(trailerSig))) != 1 {
			return ErrBadChunk
		}
	}
	d.done = true
	return nil
}

func sha256Sum(b []byte) []byte { h := sha256.Sum256(b); return h[:] }

func (d *chunked) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	for {
		if d.err != nil {
			return 0, d.err
		}
		if d.done {
			return 0, io.EOF
		}
		if d.remain == 0 {
			if err := d.header(); err != nil {
				d.err = err
				return 0, err
			}
			continue
		}
		n := int64(len(p))
		if n > d.remain {
			n = d.remain
		}
		got, err := d.br.Read(p[:n])
		if got > 0 {
			if d.signed {
				d.h.Write(p[:got])
			}
			d.remain -= int64(got)
			if d.remain == 0 {
				if cerr := d.endChunk(false); cerr != nil { // the bytes of a chunk whose signature is wrong are not given: the error takes their place
					d.err = cerr
					return 0, cerr
				}
			}
			return got, nil
		}
		if err != nil {
			d.err = io.ErrUnexpectedEOF
			return 0, d.err
		}
	}
}

// Trailer is what the trailer of the body said, once the body has been read to its end (nil when there was none).
func (d *chunked) Trailer() map[string]string { return d.trailed }

// the checksums S3 knows and that the SDKs send (header `x-amz-checksum-<name>`, or in the trailer of a streamed body): the value is the base64 of the big-endian bytes of the result
var checksumAlgs = map[string]func() hash.Hash{
	"crc32":     func() hash.Hash { return crc32.NewIEEE() },
	"crc32c":    func() hash.Hash { return crc32.New(crc32.MakeTable(crc32.Castagnoli)) },
	"crc64nvme": func() hash.Hash { return crc64.New(crc64.MakeTable(0x9a6df6aafc7a7c15)) }, // CRC-64/NVME: the reflected form of the polynomial 0xAD93D23594C93659
	"sha1":      func() hash.Hash { return sha1.New() },                                     // #nosec G401 -- a checksum a sender declares, not a defence
	"sha256":    func() hash.Hash { return sha256.New() },
}

// CheckedBody wraps the body of a call so that the checksum the sender declared is checked when the body ends: a header `x-amz-checksum-<alg>` (known at the start), or a line of the trailer of a
// streamed body (known at the end). A body that is not the declared one ends with ErrBadChecksum instead of EOF, before anything is committed; a checksum of an algorithm this module does not know
// is refused the same way (not ignored: a sender that declares a checksum is not told that it was checked when it was not). Without a declared checksum the body is returned as it is.
func (c *S3Call) CheckedBody(r *http.Request, body io.Reader) io.Reader {
	declared := map[string]string{}
	for name, vals := range r.Header {
		n := strings.ToLower(name)
		if alg, ok := strings.CutPrefix(n, "x-amz-checksum-"); ok && alg != "type" && alg != "mode" && len(vals) > 0 {
			declared[alg] = vals[0]
		}
	}
	announced := strings.ToLower(r.Header.Get("X-Amz-Trailer")) // the names of the checksums the trailer will carry
	for _, n := range strings.Split(announced, ",") {
		if alg, ok := strings.CutPrefix(strings.TrimSpace(n), "x-amz-checksum-"); ok {
			if _, known := checksumAlgs[alg]; !known {
				return errReader{fmt.Errorf("%w: the algorithm %s is not supported", ErrBadChecksum, alg)}
			}
			declared[alg] = "" // its value comes with the trailer
		}
	}
	if len(declared) == 0 {
		return body
	}
	hs := map[string]hash.Hash{}
	for alg := range declared {
		mk, ok := checksumAlgs[alg]
		if !ok {
			return errReader{fmt.Errorf("%w: the algorithm %s is not supported", ErrBadChecksum, alg)}
		}
		hs[alg] = mk()
	}
	return &checkedBody{r: body, hs: hs, declared: declared}
}

type errReader struct{ err error }

func (e errReader) Read([]byte) (int, error) { return 0, e.err }

type checkedBody struct {
	r        io.Reader
	hs       map[string]hash.Hash
	declared map[string]string
	err      error
}

func (b *checkedBody) Read(p []byte) (int, error) {
	if b.err != nil {
		return 0, b.err
	}
	n, err := b.r.Read(p)
	for _, h := range b.hs {
		h.Write(p[:n])
	}
	if err == io.EOF {
		if t, ok := b.r.(interface{ Trailer() map[string]string }); ok {
			for name, v := range t.Trailer() {
				if alg, ok := strings.CutPrefix(name, "x-amz-checksum-"); ok {
					if _, announced := b.hs[alg]; announced {
						b.declared[alg] = v
					}
				}
			}
		}
		for alg, h := range b.hs {
			want := b.declared[alg]
			if want == "" || base64.StdEncoding.EncodeToString(h.Sum(nil)) != want {
				b.err = fmt.Errorf("%w (%s)", ErrBadChecksum, alg)
				return n, b.err
			}
		}
	}
	return n, err
}
