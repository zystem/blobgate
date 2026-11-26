package api

import (
	"encoding/json"
	"encoding/xml"
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

// s3 is every call of the door: who calls, what the call is, what its body is, and the operation.
func (s *Server) s3(w http.ResponseWriter, r *http.Request) {
	call, ok := s.s3Verify(w, r)
	if !ok {
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
	body, clen, ok := s3BodyOf(w, r, call)
	if !ok {
		return
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

func (s *Server) s3Buckets(w http.ResponseWriter, r *http.Request, tenant string) {
	list, err := s.Reg.List(r.Context())
	if err != nil {
		s3Fail(w, r, 503, "ServiceUnavailable", s.hide("the database did not answer", err))
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
		s.s3ListUploads(w, r, m, tenant, bucket, q)
	case r.Method == http.MethodGet:
		s.s3List(w, r, m, tenant, bucket, q)
	case r.Method == http.MethodPost && q.Has("delete"):
		s.s3Delete(w, r, call, tenant, bucket, body)
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
		KeyCount              *int   `xml:",omitempty"` // v2 only; a pointer so that 0 is written (the Java SDK reads a missing KeyCount as null and fails)
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
		n := len(out.Contents) + len(out.CommonPrefixes)
		out.KeyCount = &n
		out.ContinuationToken, out.StartAfter = q.Get("continuation-token"), enc(q.Get("start-after"))
		out.NextContinuationToken = got.Next
	} else {
		out.Marker, out.NextMarker = enc(q.Get("marker")), enc(got.Next)
	}
	s3Reply(w, 200, out)
}

func (s *Server) s3Delete(w http.ResponseWriter, r *http.Request, call *auth.S3Call, tenant, bucket string, body io.Reader) {
	var in struct {
		Quiet   bool `xml:"Quiet"`
		Objects []struct {
			Key string `xml:"Key"`
		} `xml:"Object"`
	}
	raw, ok := readSmallBody(w, r, call, body, 4<<20)
	if !ok {
		return
	}
	if xml.Unmarshal(raw, &in) != nil || len(in.Objects) == 0 || len(in.Objects) > maxDelete {
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
