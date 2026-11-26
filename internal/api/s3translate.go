package api

// The calls of the S3 door in the words of the core, and the answers of the core in the words of S3.

import (
	"bytes"
	"encoding/json"
	"encoding/xml"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/zystem/blobgate/internal/auth"
)

type s3Error struct {
	XMLName   xml.Name `xml:"Error"`
	Code      string   `xml:"Code"`
	Message   string   `xml:"Message"`
	Resource  string   `xml:"Resource,omitempty"`
	RequestID string   `xml:"RequestId"`
}

func s3Reply(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/xml")
	w.WriteHeader(code)
	_, _ = io.WriteString(w, xml.Header)
	_ = xml.NewEncoder(w).Encode(v)
}

func s3Fail(w http.ResponseWriter, r *http.Request, status int, code, msg string) {
	if r.Method == http.MethodHead {
		w.WriteHeader(status)
		return
	}
	s3Reply(w, status, s3Error{Code: code, Message: msg, Resource: r.URL.Path, RequestID: newID()})
}

// s3Code is the status and the code of S3 for an error of the core.
func s3Code(status int, kind string) (int, string) {
	switch kind {
	case "no_such_object":
		return 404, "NoSuchKey"
	case "no_such_store":
		return 404, "NoSuchBucket"
	case "no_such_upload":
		return 404, "NoSuchUpload"
	case "precondition_failed":
		return 412, "PreconditionFailed"
	case "sha256_mismatch":
		return 400, "XAmzContentSHA256Mismatch"
	case "md5_mismatch":
		return 400, "BadDigest"
	case "too_large":
		return 400, "EntityTooLarge"
	case "invalid_key":
		return 400, "InvalidArgument"
	case "quota_exceeded", "storage_full":
		return 507, "InsufficientStorage"
	case "unauthorized":
		return 403, "AccessDenied"
	}
	switch status {
	case 404:
		return 404, "NoSuchKey"
	case 412:
		return 412, "PreconditionFailed"
	case 416:
		return 416, "InvalidRange"
	case 507:
		return 507, "InsufficientStorage"
	case 503:
		return 503, "ServiceUnavailable"
	case 409:
		return 409, "OperationAborted"
	}
	if status >= 500 {
		return status, "InternalError"
	}
	return status, "InvalidRequest"
}

// s3w takes what a handler of the core writes: kept in memory (a small answer that is turned into XML), or, for the read of an object, passed on as it comes, with the names of the headers turned.
type s3w struct {
	w      http.ResponseWriter
	h      http.Header
	code   int
	buf    bytes.Buffer
	stream bool
}

func (x *s3w) Header() http.Header { return x.h }

func (x *s3w) WriteHeader(c int) {
	if x.code != 0 {
		return
	}
	x.code = c
	if x.stream && c < 400 {
		out := x.w.Header()
		for k, v := range x.h {
			if rest, ok := strings.CutPrefix(k, "X-Blobgate-Meta-"); ok {
				out["x-amz-meta-"+strings.ToLower(rest)] = v // lower case on the wire, as S3 sends it: a client that reads the names as they come (boto3) gets the names that were written
			} else {
				out[k] = v
			}
		}
		x.w.WriteHeader(c)
	}
}

func (x *s3w) Write(b []byte) (int, error) {
	if x.code == 0 {
		x.WriteHeader(200)
	}
	if x.stream && x.code < 400 {
		return x.w.Write(b)
	}
	return x.buf.Write(b)
}

func (x *s3w) Flush() {
	if f, ok := x.w.(http.Flusher); ok && x.stream && x.code < 400 {
		f.Flush()
	}
}

func (x *s3w) failed() bool { return x.code >= 400 }

// sendError answers with the error of the core that the handler wrote, in the words of S3.
func (x *s3w) sendError(r *http.Request, bad error) {
	if errors.Is(bad, auth.ErrBadChunk) {
		s3Fail(x.w, r, 403, "SignatureDoesNotMatch", "a chunk of the body is not signed right")
		return
	}
	if errors.Is(bad, auth.ErrBadChecksum) {
		s3Fail(x.w, r, 400, "BadDigest", bad.Error())
		return
	}
	var e apiError
	_ = json.Unmarshal(x.buf.Bytes(), &e)
	st, code := s3Code(x.code, e.Error)
	msg := e.Detail
	if msg == "" {
		msg = e.Error
	}
	if code == "InsufficientStorage" || st == 503 {
		x.w.Header().Set("Retry-After", "5")
	}
	s3Fail(x.w, r, st, code, msg)
}

// internal makes the call of the core that does what the call of S3 asks.
func (s *Server) internal(r *http.Request, method, path string, q url.Values, body io.Reader, clen int64, call *auth.S3Call) *http.Request {
	req := r.Clone(r.Context())
	req.Method = method
	req.URL = &url.URL{Path: path, RawQuery: q.Encode()}
	req.RequestURI = ""
	req.Header = http.Header{}
	for _, n := range []string{"Content-MD5", "Range", "If-Match", "If-None-Match", "If-Modified-Since", "If-Unmodified-Since", "Content-Type"} {
		if v := r.Header.Values(n); len(v) > 0 {
			req.Header[http.CanonicalHeaderKey(n)] = v
		}
	}
	for k, v := range r.Header {
		if rest, ok := strings.CutPrefix(strings.ToLower(k), "x-amz-meta-"); ok {
			req.Header["X-Blobgate-Meta-"+rest] = v
		}
	}
	if call != nil && method == http.MethodPut && !call.Streaming() {
		if d := call.Digest(); d != "" {
			req.Header.Set("X-Content-SHA256", d)
		}
	}
	req.Body, req.ContentLength = io.NopCloser(body), clen
	return req
}
