package api

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/zystem/blobgate/internal/auth"
	"github.com/zystem/blobgate/internal/layout"
	"github.com/zystem/blobgate/internal/store"
)

// The S3 door (docs/s3.md): the calls that the tools of S3 make for objects, listings and multipart uploads, answered by the code of the core. The access key is a tenant, the bucket is a store, and the
// key is the key of the object under that tenant. Nothing here keeps a state; every call is turned into a call of the core on the same code, and what it answers is turned back.

const (
	s3NS      = "http://s3.amazonaws.com/doc/2006-03-01/"
	s3Time    = "2006-01-02T15:04:05.000Z"
	maxDelete = 1000
)

// S3Handler is the port of the S3 door.
func (s *Server) S3Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) { writeJSON(w, 200, map[string]string{"status": "ok"}) })
	mux.HandleFunc("/", s.s3)
	return s.logged(mux, "s3")
}

// s3SecretOf is the secret key of an access key.
func (s *Server) s3SecretOf(ak string) (string, bool) {
	secret, ok := s.cfg.ShardSecrets[ak]
	if !ok || !validShard(ak) {
		return "", false
	}
	return auth.S3Secret(secret), true
}

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
				out["X-Amz-Meta-"+rest] = v
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

// s3 is every call of the door.
func (s *Server) s3(w http.ResponseWriter, r *http.Request) {
	call, err := auth.VerifySigV4(r, s.s3SecretOf, s.cfg.Now())
	if err != nil {
		switch {
		case errors.Is(err, auth.ErrNoAuth):
			s3Fail(w, r, 403, "AccessDenied", "the call is not signed (Signature Version 4)")
		case errors.Is(err, auth.ErrUnknownKey):
			s3Fail(w, r, 403, "InvalidAccessKeyId", "the access key is not a tenant of this service")
		case errors.Is(err, auth.ErrBadSig):
			s3Fail(w, r, 403, "SignatureDoesNotMatch", "the signature does not match: check the secret key (blobgate s3-key) and the region")
		case errors.Is(err, auth.ErrExpired):
			s3Fail(w, r, 403, "RequestTimeTooSkewed", "the time of the request is not within 15 minutes of the time of the service")
		default:
			s3Fail(w, r, 400, "AuthorizationHeaderMalformed", err.Error())
		}
		return
	}
	tenant := call.AccessKey
	path := strings.TrimPrefix(r.URL.Path, "/")
	bucket, key, _ := strings.Cut(path, "/")
	q := r.URL.Query()
	if key == "" && bucket != "" && r.Method == http.MethodGet && q.Has("versioning") { // a store keeps no versions: the answer that says so, which tools ask for before a delete
		s3Reply(w, 200, struct {
			XMLName xml.Name `xml:"VersioningConfiguration"`
			NS      string   `xml:"xmlns,attr"`
		}{NS: s3NS})
		return
	}
	for name := range q {
		if s3Unsupported[name] {
			s3Fail(w, r, 501, "NotImplemented", name+" is not supported by this service")
			return
		}
	}
	// the body: as it is, or decoded from aws-chunked
	var body io.Reader = r.Body
	clen := r.ContentLength
	if call.Streaming() {
		n, err := strconv.ParseInt(r.Header.Get("X-Amz-Decoded-Content-Length"), 10, 64)
		if err != nil || n < 0 {
			s3Fail(w, r, 411, "MissingContentLength", "x-amz-decoded-content-length is the length of the bytes, with a streamed body")
			return
		}
		body, clen = call.ChunkedBody(r.Body), n
	}
	eb := &errBody{r: body}
	switch {
	case bucket == "":
		if r.Method != http.MethodGet {
			s3Fail(w, r, 405, "MethodNotAllowed", "GET")
			return
		}
		s.s3Buckets(w, r, tenant)
	case key == "":
		s.s3Bucket(w, r, call, tenant, bucket, q, eb, clen)
	default:
		s.s3Object(w, r, call, tenant, bucket, key, q, eb, clen)
	}
}

// the subresources of S3 that the door does not answer (a call that has one is refused, and is not taken for a plain read or write)
var s3Unsupported = map[string]bool{"acl": true, "tagging": true, "versioning": true, "versions": true, "versionId": true, "lifecycle": true, "policy": true, "cors": true, "website": true,
	"retention": true, "legal-hold": true, "torrent": true, "select": true, "restore": true, "attributes": true, "object-lock": true, "encryption": true, "notification": true,
	"replication": true, "logging": true, "accelerate": true, "analytics": true, "inventory": true, "metrics": true, "ownershipControls": true, "publicAccessBlock": true,
	"requestPayment": true, "intelligent-tiering": true, "policyStatus": true}

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

func (s *Server) s3Buckets(w http.ResponseWriter, r *http.Request, tenant string) {
	list, err := s.Reg.List(r.Context())
	if err != nil {
		s3Fail(w, r, 503, "ServiceUnavailable", err.Error())
		return
	}
	type bucket struct {
		Name         string
		CreationDate string
	}
	out := struct {
		XMLName xml.Name `xml:"ListAllMyBucketsResult"`
		NS      string   `xml:"xmlns,attr"`
		Owner   struct{ ID, DisplayName string }
		Buckets struct {
			Bucket []bucket
		}
	}{NS: s3NS}
	out.Owner.ID, out.Owner.DisplayName = tenant, tenant
	for _, e := range list {
		name := e.Name
		if name == "" {
			name = defaultNam
		}
		out.Buckets.Bucket = append(out.Buckets.Bucket, bucket{name, time.Unix(0, 0).UTC().Format(s3Time)})
	}
	s3Reply(w, 200, out)
}

func (s *Server) s3Bucket(w http.ResponseWriter, r *http.Request, call *auth.S3Call, tenant, bucket string, q url.Values, body io.Reader, clen int64) {
	cw := &s3w{w: w, h: http.Header{}}
	m := s.loadStore(cw, r.Context(), bucket)
	if m == nil {
		if r.Method == http.MethodPut || r.Method == http.MethodDelete {
			s3Fail(w, r, 403, "AccessDenied", "buckets are the stores that the operator made")
			return
		}
		cw.sendError(r, nil)
		return
	}
	switch {
	case r.Method == http.MethodHead:
		w.Header().Set("X-Amz-Bucket-Region", "us-east-1")
		w.WriteHeader(200)
	case r.Method == http.MethodPut:
		s3Fail(w, r, 409, "BucketAlreadyOwnedByYou", "the bucket is a store that the operator made")
	case r.Method == http.MethodDelete:
		s3Fail(w, r, 403, "AccessDenied", "buckets are removed by the operator")
	case r.Method == http.MethodGet && q.Has("location"):
		s3Reply(w, 200, struct {
			XMLName xml.Name `xml:"LocationConstraint"`
			NS      string   `xml:"xmlns,attr"`
			V       string   `xml:",chardata"`
		}{NS: s3NS})
	case r.Method == http.MethodGet && q.Has("uploads"):
		s3Fail(w, r, 501, "NotImplemented", "the list of uploads that are going on is not kept")
	case r.Method == http.MethodGet:
		s.s3List(w, r, m, tenant, bucket, q)
	case r.Method == http.MethodPost && q.Has("delete"):
		s.s3Delete(w, r, tenant, bucket, body)
	default:
		s3Fail(w, r, 405, "MethodNotAllowed", "HEAD, GET or POST ?delete on a bucket")
	}
}

type s3Content struct {
	Key          string
	LastModified string
	ETag         string
	Size         int64
	StorageClass string
}

func (s *Server) s3List(w http.ResponseWriter, r *http.Request, m *store.Mirror, tenant, bucket string, q url.Values) {
	limit := 1000
	if v := q.Get("max-keys"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			s3Fail(w, r, 400, "InvalidArgument", "max-keys is a number")
			return
		}
		limit = min(n, maxListLimit)
	}
	v2 := q.Get("list-type") == "2"
	after := q.Get("marker")
	if v2 {
		after = q.Get("continuation-token")
		if after == "" {
			after = q.Get("start-after")
		}
	}
	iq := url.Values{"prefix": {q.Get("prefix")}, "delimiter": {q.Get("delimiter")}, "limit": {strconv.Itoa(max(limit, 1))}}
	if after != "" {
		iq.Set("after", after)
	}
	cw := &s3w{w: w, h: http.Header{}}
	req := s.internal(r, http.MethodGet, "/v1/stores/"+bucket+"/objects", iq, http.NoBody, 0, nil)
	s.list(cw, req, m, tenant)
	if cw.failed() {
		cw.sendError(r, nil)
		return
	}
	var got struct {
		Objects []struct {
			Key      string `json:"key"`
			Size     int64  `json:"size"`
			ETag     string `json:"etag"`
			Modified int64  `json:"modified"`
		} `json:"objects"`
		Prefixes []string `json:"prefixes"`
		Next     string   `json:"next"`
	}
	if json.Unmarshal(cw.buf.Bytes(), &got) != nil {
		s3Fail(w, r, 500, "InternalError", "the list could not be read")
		return
	}
	enc := func(x string) string { return x }
	if q.Get("encoding-type") == "url" {
		enc = url.QueryEscape
	}
	type cp struct{ Prefix string }
	out := struct {
		XMLName               xml.Name `xml:"ListBucketResult"`
		NS                    string   `xml:"xmlns,attr"`
		Name                  string
		Prefix                string
		Delimiter             string `xml:",omitempty"`
		EncodingType          string `xml:",omitempty"`
		KeyCount              int    `xml:",omitempty"`
		MaxKeys               int
		IsTruncated           bool
		Marker                string `xml:",omitempty"`
		NextMarker            string `xml:",omitempty"`
		ContinuationToken     string `xml:",omitempty"`
		NextContinuationToken string `xml:",omitempty"`
		StartAfter            string `xml:",omitempty"`
		Contents              []s3Content
		CommonPrefixes        []cp
	}{NS: s3NS, Name: bucket, Prefix: enc(q.Get("prefix")), Delimiter: enc(q.Get("delimiter")), EncodingType: q.Get("encoding-type"), MaxKeys: limit, IsTruncated: got.Next != ""}
	if limit == 0 { // nothing is asked for
		got.Objects, got.Prefixes, out.IsTruncated = nil, nil, false
	}
	for _, o := range got.Objects {
		out.Contents = append(out.Contents, s3Content{enc(o.Key), time.Unix(o.Modified, 0).UTC().Format(s3Time), o.ETag, o.Size, "STANDARD"})
	}
	for _, p := range got.Prefixes {
		out.CommonPrefixes = append(out.CommonPrefixes, cp{enc(p)})
	}
	if v2 {
		out.KeyCount = len(out.Contents) + len(out.CommonPrefixes)
		out.ContinuationToken, out.StartAfter = q.Get("continuation-token"), enc(q.Get("start-after"))
		out.NextContinuationToken = got.Next
	} else {
		out.Marker, out.NextMarker = enc(q.Get("marker")), enc(got.Next)
	}
	s3Reply(w, 200, out)
}

func (s *Server) s3Delete(w http.ResponseWriter, r *http.Request, tenant, bucket string, body io.Reader) {
	var in struct {
		Quiet   bool `xml:"Quiet"`
		Objects []struct {
			Key string `xml:"Key"`
		} `xml:"Object"`
	}
	raw, err := io.ReadAll(io.LimitReader(body, 4<<20))
	if err != nil || xml.Unmarshal(raw, &in) != nil || len(in.Objects) == 0 || len(in.Objects) > maxDelete {
		s3Fail(w, r, 400, "MalformedXML", fmt.Sprintf("the body is <Delete> with 1..%d <Object><Key>", maxDelete))
		return
	}
	type deleted struct{ Key string }
	type failed struct{ Key, Code, Message string }
	out := struct {
		XMLName xml.Name `xml:"DeleteResult"`
		NS      string   `xml:"xmlns,attr"`
		Deleted []deleted
		Error   []failed
	}{NS: s3NS}
	for _, o := range in.Objects {
		cw := &s3w{w: w, h: http.Header{}}
		if why := layout.Valid(o.Key); why != "" {
			out.Error = append(out.Error, failed{o.Key, "InvalidArgument", why})
			continue
		}
		req := s.internal(r, http.MethodDelete, "/v1/stores/"+bucket+"/objects/"+o.Key, nil, http.NoBody, 0, nil)
		s.object(cw, req, tenant, bucket, o.Key)
		if cw.failed() {
			var e apiError
			_ = json.Unmarshal(cw.buf.Bytes(), &e)
			_, code := s3Code(cw.code, e.Error)
			out.Error = append(out.Error, failed{o.Key, code, e.Detail})
			continue
		}
		if !in.Quiet {
			out.Deleted = append(out.Deleted, deleted{o.Key})
		}
	}
	s3Reply(w, 200, out)
}

func (s *Server) s3Object(w http.ResponseWriter, r *http.Request, call *auth.S3Call, tenant, bucket, key string, q url.Values, body *errBody, clen int64) {
	base := "/v1/stores/" + bucket + "/objects/" + key
	cw := &s3w{w: w, h: http.Header{}}
	do := func(method string, iq url.Values, b io.Reader, n int64) { // one call of the core, its answer kept in cw
		s.object(cw, s.internal(r, method, base, iq, b, n, call), tenant, bucket, key)
	}
	etagOf := func() string {
		if o, err := s.getRow(r.Context(), bucket, tenant+"/"+key); err == nil && o != nil {
			return o.etag()
		}
		return ""
	}
	upload, part := q.Get("uploadId"), q.Get("partNumber")
	switch r.Method {
	case http.MethodGet, http.MethodHead:
		if upload != "" {
			do(http.MethodGet, url.Values{"parts": {""}, "upload": {upload}}, http.NoBody, 0)
			if cw.failed() {
				cw.sendError(r, nil)
				return
			}
			var got struct {
				Parts []struct {
					Part int    `json:"part"`
					SHA  string `json:"sha256"`
					Size int64  `json:"size"`
				} `json:"parts"`
			}
			_ = json.Unmarshal(cw.buf.Bytes(), &got)
			type p struct {
				PartNumber int
				ETag       string
				Size       int64
			}
			out := struct {
				XMLName  xml.Name `xml:"ListPartsResult"`
				NS       string   `xml:"xmlns,attr"`
				Bucket   string
				Key      string
				UploadID string `xml:"UploadId"`
				Part     []p
			}{NS: s3NS, Bucket: bucket, Key: key, UploadID: upload}
			for _, x := range got.Parts {
				out.Part = append(out.Part, p{x.Part, `"` + etag(x.SHA, x.Size) + `"`, x.Size})
			}
			s3Reply(w, 200, out)
			return
		}
		cw.stream = true
		do(r.Method, nil, http.NoBody, 0)
		if cw.failed() {
			cw.sendError(r, nil)
		}
	case http.MethodPut:
		if r.Header.Get("X-Amz-Copy-Source") != "" {
			s3Fail(w, r, 501, "NotImplemented", "CopyObject is not supported: read the object and write it (or use ?link of the core)")
			return
		}
		if upload != "" || part != "" {
			s.s3Part(w, r, cw, call, tenant, bucket, key, upload, part, body, clen)
			return
		}
		do(http.MethodPut, nil, body, clen)
		if cw.failed() {
			cw.sendError(r, body.err)
			return
		}
		if t := etagOf(); t != "" {
			w.Header().Set("ETag", t)
		}
		w.WriteHeader(200)
	case http.MethodDelete:
		iq := url.Values(nil)
		if upload != "" {
			iq = url.Values{"upload": {upload}}
		}
		do(http.MethodDelete, iq, http.NoBody, 0)
		if cw.failed() {
			cw.sendError(r, nil)
			return
		}
		w.WriteHeader(204)
	case http.MethodPost:
		switch {
		case q.Has("uploads"):
			do(http.MethodPost, url.Values{"uploads": {""}}, http.NoBody, 0)
			if cw.failed() {
				cw.sendError(r, nil)
				return
			}
			var got struct {
				ID string `json:"upload_id"`
			}
			_ = json.Unmarshal(cw.buf.Bytes(), &got)
			s3Reply(w, 200, struct {
				XMLName  xml.Name `xml:"InitiateMultipartUploadResult"`
				NS       string   `xml:"xmlns,attr"`
				Bucket   string
				Key      string
				UploadID string `xml:"UploadId"`
			}{NS: s3NS, Bucket: bucket, Key: key, UploadID: got.ID})
		case upload != "":
			var in struct {
				Parts []struct {
					N    int    `xml:"PartNumber"`
					ETag string `xml:"ETag"`
				} `xml:"Part"`
			}
			raw, err := io.ReadAll(io.LimitReader(body, smallBody))
			if err != nil || xml.Unmarshal(raw, &in) != nil || len(in.Parts) == 0 {
				s3Fail(w, r, 400, "MalformedXML", "the body is <CompleteMultipartUpload> with <Part><PartNumber> and <ETag>")
				return
			}
			parts := make([][]any, len(in.Parts))
			for i, p := range in.Parts {
				parts[i] = []any{p.N, strings.Trim(p.ETag, `"`)}
			}
			js, _ := json.Marshal(map[string]any{"parts": parts})
			do(http.MethodPost, url.Values{"upload": {upload}}, bytes.NewReader(js), int64(len(js)))
			if cw.failed() {
				cw.sendError(r, nil)
				return
			}
			s3Reply(w, 200, struct {
				XMLName xml.Name `xml:"CompleteMultipartUploadResult"`
				NS      string   `xml:"xmlns,attr"`
				Bucket  string
				Key     string
				ETag    string
			}{NS: s3NS, Bucket: bucket, Key: key, ETag: etagOf()})
		default:
			s3Fail(w, r, 400, "InvalidRequest", "POST on an object takes ?uploads or ?uploadId")
		}
	default:
		s3Fail(w, r, 405, "MethodNotAllowed", "GET, HEAD, PUT, POST or DELETE")
	}
}

// s3Part is UploadPart. A part is held whole (at most one block of 8 MiB; every part but the last must be as long as the first), its digest is made, and it is a part of the core.
func (s *Server) s3Part(w http.ResponseWriter, r *http.Request, cw *s3w, call *auth.S3Call, tenant, bucket, key, upload, part string, body io.Reader, clen int64) {
	if upload == "" || part == "" {
		s3Fail(w, r, 400, "InvalidArgument", "a part is PUT with partNumber and uploadId")
		return
	}
	if clen > BlockSize {
		s3Fail(w, r, 400, "EntityTooLarge", fmt.Sprintf("a part is at most %d bytes (8 MiB); set the size of the parts of the tool to 8 MiB or less (docs/s3.md)", BlockSize))
		return
	}
	data, err := io.ReadAll(io.LimitReader(body, BlockSize+1))
	if err != nil {
		cw.sendError(r, err)
		return
	}
	if len(data) > BlockSize {
		s3Fail(w, r, 400, "EntityTooLarge", fmt.Sprintf("a part is at most %d bytes (8 MiB); set the size of the parts of the tool to 8 MiB or less (docs/s3.md)", BlockSize))
		return
	}
	sum := sha256.Sum256(data)
	if d := call.Digest(); d != "" && d != hex.EncodeToString(sum[:]) {
		s3Fail(w, r, 400, "XAmzContentSHA256Mismatch", "the part is not the one x-amz-content-sha256 names")
		return
	}
	req := s.internal(r, http.MethodPut, "/v1/stores/"+bucket+"/objects/"+key, url.Values{"upload": {upload}, "part": {part}}, bytes.NewReader(data), int64(len(data)), nil)
	req.Header.Set("X-Content-SHA256", hex.EncodeToString(sum[:]))
	s.object(cw, req, tenant, bucket, key)
	if cw.failed() {
		cw.sendError(r, nil)
		return
	}
	var got struct {
		ETag string `json:"etag"`
	}
	_ = json.Unmarshal(cw.buf.Bytes(), &got)
	w.Header().Set("ETag", `"`+got.ETag+`"`)
	w.WriteHeader(200)
}
