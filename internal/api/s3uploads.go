package api

import (
	"encoding/xml"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/zystem/blobgate/internal/rq"
	"github.com/zystem/blobgate/internal/store"
)

const maxUploadsRead = 20000 // the most rows of uploads that one list reads (uploads that nobody finished are swept; a tenant with more has a tool that leaks them)

// s3ListUploads is ListMultipartUploads (`GET /bucket?uploads`): the uploads in parts of the tenant that have begun and not been completed or aborted, by key and then by upload id, with `prefix`,
// `delimiter` (folded into CommonPrefixes), `key-marker`, `upload-id-marker` and `max-uploads` (1000 at most). The writes in one call, which the table of uploads holds while they go on, are not listed.
func (s *Server) s3ListUploads(w http.ResponseWriter, r *http.Request, m *store.Mirror, tenant, bucket string, q url.Values) {
	limit := 1000
	if v := q.Get("max-uploads"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			s3Fail(w, r, 400, "InvalidArgument", "max-uploads is a number")
			return
		}
		limit = min(n, 1000)
	}
	prefix, delim, keyMarker, idMarker := q.Get("prefix"), q.Get("delimiter"), q.Get("key-marker"), q.Get("upload-id-marker")
	if idMarker != "" && keyMarker == "" {
		s3Fail(w, r, 400, "InvalidArgument", "upload-id-marker needs key-marker")
		return
	}
	own := tenant + "/"
	from := own + prefix
	if keyMarker != "" && own+keyMarker > from {
		from = own + keyMarker
	}
	sql, args := "SELECT key, upload, created_at FROM storage_uploads WHERE store = ? AND one_call = 0 AND key >= ?", []any{m.Name, from}
	if hi := upperBound(own + prefix); hi != "" {
		sql += " AND key < ?"
		args = append(args, hi)
	}
	rows, err := s.DB.QueryWeak(r.Context(), rq.S(sql+" ORDER BY key, upload LIMIT ?", append(args, maxUploadsRead)...))
	if err != nil {
		s.fail(w, err)
		return
	}
	enc := func(x string) string { return x }
	if q.Get("encoding-type") == "url" {
		enc = url.QueryEscape
	}
	type upload struct {
		Key          string
		UploadID     string `xml:"UploadId"`
		Initiator    s3Owner
		Owner        s3Owner
		StorageClass string
		Initiated    string
	}
	type cp struct{ Prefix string }
	var ups []upload
	var prefixes []cp
	var lastKey, lastID string // what the next page starts after
	count, truncated := 0, false
	lastPrefix := ""
	for _, row := range rows {
		key, id := strings.TrimPrefix(rq.Str(row[0]), own), rq.Str(row[1])
		if keyMarker != "" && (key < keyMarker || key == keyMarker && (idMarker == "" || id <= idMarker)) {
			continue
		}
		if delim != "" {
			if i := strings.Index(key[len(prefix):], delim); i >= 0 {
				folded := key[:len(prefix)+i+len(delim)]
				if folded == lastPrefix || keyMarker != "" && folded <= keyMarker {
					continue
				}
				if count == limit {
					truncated = true
					break
				}
				lastPrefix, lastKey, lastID = folded, folded, ""
				prefixes = append(prefixes, cp{enc(folded)})
				count++
				continue
			}
		}
		if count == limit {
			truncated = true
			break
		}
		lastKey, lastID = key, id
		ups = append(ups, upload{Key: enc(key), UploadID: id, Initiator: s3Owner{ID: tenant, DisplayName: tenant}, Owner: s3Owner{ID: tenant, DisplayName: tenant}, StorageClass: "STANDARD",
			Initiated: time.Unix(rq.Int(row[2]), 0).UTC().Format("2006-01-02T15:04:05.000Z")})
		count++
	}
	out := struct {
		XMLName            xml.Name `xml:"ListMultipartUploadsResult"`
		NS                 string   `xml:"xmlns,attr"`
		Bucket             string
		KeyMarker          string
		UploadIDMarker     string `xml:"UploadIdMarker"`
		NextKeyMarker      string `xml:",omitempty"`
		NextUploadIDMarker string `xml:"NextUploadIdMarker,omitempty"`
		Delimiter          string `xml:",omitempty"`
		Prefix             string
		MaxUploads         int
		EncodingType       string `xml:",omitempty"`
		IsTruncated        bool
		Upload             []upload
		CommonPrefixes     []cp
	}{NS: s3NS, Bucket: bucket, KeyMarker: enc(keyMarker), UploadIDMarker: idMarker, Delimiter: enc(delim), Prefix: enc(prefix), MaxUploads: limit, EncodingType: q.Get("encoding-type"),
		IsTruncated: truncated, Upload: ups, CommonPrefixes: prefixes}
	if truncated {
		out.NextKeyMarker, out.NextUploadIDMarker = enc(lastKey), lastID
	}
	s3Reply(w, 200, out)
}

// s3Owner is the owner and the initiator of an upload: the tenant.
type s3Owner struct {
	ID          string
	DisplayName string
}
