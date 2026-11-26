package api

// The operations of the S3 door on an object and on a part.

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
	"strings"

	"github.com/zystem/blobgate/internal/auth"
)

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
			if upload != "" || part != "" {
				s.s3CopyPart(w, r, tenant, bucket, key, upload, part)
			} else {
				s.s3Copy(w, r, tenant, bucket, key)
			}
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
			raw, ok := readSmallBody(w, r, call, body, smallBody)
			if !ok {
				return
			}
			if xml.Unmarshal(raw, &in) != nil || len(in.Parts) == 0 {
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
	// the part is held whole while it is read from the client: a place of BLOBGATE_HELD_BLOCKS, as for the blocks of a read
	if err := s.hold(r.Context(), tenant, heldWrite, true); err != nil {
		if errors.Is(err, errBusy) {
			w.Header().Set("Retry-After", "5")
			s3Fail(w, r, 503, "SlowDown", "the module holds as many blocks for clients as it may (BLOBGATE_HELD_BLOCKS); try again")
		}
		return
	}
	defer s.unhold(tenant, heldWrite) // the place is kept until the part is on the nodes: it is in memory the whole time
	data, err := io.ReadAll(io.LimitReader(body, BlockSize+1))
	if err != nil {
		if errors.Is(err, auth.ErrBadChunk) || errors.Is(err, auth.ErrBadChecksum) {
			cw.sendError(r, err)
		} else { // the client left, or was ended for being quiet: the core had not written anything yet to be told
			s3Fail(w, r, 400, "IncompleteBody", "the part did not come whole")
		}
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
	etag, ok := s.putPartBytes(r, cw, tenant, bucket, key, upload, part, data, hex.EncodeToString(sum[:]))
	if !ok {
		return
	}
	w.Header().Set("ETag", `"`+etag+`"`)
	w.WriteHeader(200)
}

// putPartBytes gives a part (held whole, at most one block) to the core and returns the ETag the core names it by; false when it answered with an error.
func (s *Server) putPartBytes(r *http.Request, cw *s3w, tenant, bucket, key, upload, part string, data []byte, digest string) (string, bool) {
	req := s.internal(r, http.MethodPut, "/v1/stores/"+bucket+"/objects/"+key, url.Values{"upload": {upload}, "part": {part}}, bytes.NewReader(data), int64(len(data)), nil)
	req.Header.Set("X-Content-SHA256", digest)
	s.object(cw, req, tenant, bucket, key)
	if cw.failed() {
		cw.sendError(r, nil)
		return "", false
	}
	var got struct {
		ETag string `json:"etag"`
	}
	_ = json.Unmarshal(cw.buf.Bytes(), &got)
	return got.ETag, true
}
