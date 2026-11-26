package api

// What the body of a call of the S3 door is, and whether it is the one its sender signed or declared.

import (
	"crypto/md5" // #nosec G501 -- the Content-MD5 a sender declares
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"strconv"

	"github.com/zystem/blobgate/internal/auth"
)

// errBody notes the error that a body ended with, so that a chunk that was not signed right is told as that and not as a body that did not come.
type errBody struct {
	r   io.Reader
	err error
}

func (b *errBody) Read(p []byte) (int, error) {
	n, err := b.r.Read(p)
	if err != nil && err != io.EOF {
		b.err = err
	}
	return n, err
}

// readSmallBody reads the body of a call whose body is a small document (the list of keys to delete, the list of parts to complete) and checks it against what the signature says it is: the digest the
// client signed (when it signed one), a checksum it declared, the chunks of a streamed body. The answer is written here when the body is not that, and nothing is done with the bytes before then.
func readSmallBody(w http.ResponseWriter, r *http.Request, call *auth.S3Call, body io.Reader, limit int64) ([]byte, bool) {
	raw, err := io.ReadAll(io.LimitReader(body, limit+1))
	switch {
	case errors.Is(err, auth.ErrBadChunk):
		s3Fail(w, r, 403, "SignatureDoesNotMatch", "a chunk of the body is not signed right")
		return nil, false
	case errors.Is(err, auth.ErrBadChecksum):
		s3Fail(w, r, 400, "BadDigest", err.Error())
		return nil, false
	case err != nil:
		s3Fail(w, r, 400, "IncompleteBody", "the body did not come whole")
		return nil, false
	case int64(len(raw)) > limit:
		s3Fail(w, r, 400, "EntityTooLarge", "the body is larger than the service reads")
		return nil, false
	}
	if d := call.Digest(); d != "" {
		if sum := sha256.Sum256(raw); hex.EncodeToString(sum[:]) != d {
			s3Fail(w, r, 400, "XAmzContentSHA256Mismatch", "the body is not the one x-amz-content-sha256 names")
			return nil, false
		}
	}
	if want := r.Header.Get("Content-MD5"); want != "" { // S3 requires it for a delete of many keys
		if sum := md5.Sum(raw); base64.StdEncoding.EncodeToString(sum[:]) != want { // #nosec G401 -- the digest the sender declared, not a defence
			s3Fail(w, r, 400, "BadDigest", "the body is not the one Content-MD5 names")
			return nil, false
		}
	}
	return raw, true
}

// s3BodyOf is the bytes of the body and their number: as sent, or decoded from aws-chunked, and in either case checked against the checksum that the sender declared, which is told when the body ends.
func s3BodyOf(w http.ResponseWriter, r *http.Request, call *auth.S3Call) (io.Reader, int64, bool) {
	// the body: as it is, or decoded from aws-chunked
	var body io.Reader = r.Body
	clen := r.ContentLength
	if call.Streaming() {
		n, err := strconv.ParseInt(r.Header.Get("X-Amz-Decoded-Content-Length"), 10, 64)
		if err != nil || n < 0 {
			s3Fail(w, r, 411, "MissingContentLength", "x-amz-decoded-content-length is the length of the bytes, with a streamed body")
			return nil, 0, false
		}
		body, clen = call.ChunkedBody(r.Body), n
	}
	body = call.CheckedBody(r, body) // a checksum the sender declared (a header, or the trailer of a streamed body) is checked when the body ends

	return body, clen, true
}
