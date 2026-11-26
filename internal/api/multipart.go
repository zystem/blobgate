package api

// Uploads in parts: begin, part, list, completion and abort.

import (
	"context"
	"crypto/md5" // #nosec G501 -- the ETag of S3 is the MD5 of the bytes; it is a name that clients compare, not a defence
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"

	"github.com/zystem/blobgate/internal/layout"
	"github.com/zystem/blobgate/internal/rq"
	"github.com/zystem/blobgate/internal/store"
)

func validUpload(id string) bool {
	if len(id) < 8 || len(id) > 64 {
		return false
	}
	for i := 0; i < len(id); i++ {
		c := id[i]
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

func (s *Server) uploadExists(ctx context.Context, st, full, id string) (bool, error) {
	rows, err := s.DB.Query(ctx, rq.S("SELECT count(*) FROM storage_uploads WHERE store = ? AND key = ? AND upload = ?", st, full, id))
	return err == nil && len(rows) == 1 && rq.Int(rows[0][0]) > 0, err
}

func (s *Server) beginUpload(w http.ResponseWriter, r *http.Request, m *store.Mirror, full, sizeArg string) {
	ctx := r.Context()
	expected, _ := strconv.ParseInt(sizeArg, 10, 64)
	if e := s.noRoom(ctx, m, max(expected, 0)); e != nil {
		e.write(w)
		return
	}
	if e := s.checkQuota(ctx, m, held{key: full}, max(expected, 0)); e != nil {
		e.write(w)
		return
	}
	om, he := readMeta(r)
	if he != nil {
		he.write(w)
		return
	}
	pj, _ := json.Marshal(om.Pairs)
	if om.Pairs == nil {
		pj = []byte("{}")
	}
	id := newID()
	if _, err := s.DB.Exec(ctx, false, rq.S("INSERT INTO storage_uploads (store, key, upload, created_at, content_type, meta) VALUES (?, ?, ?, ?, ?, ?)", m.Name, full, id, s.now(), om.ContentType, string(pj))); err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"upload_id": id})
}

// putPart is a part of an upload, from the core (the shim's port uses it too, after its own check of the grant).
func (s *Server) putPart(w http.ResponseWriter, r *http.Request, m *store.Mirror, full, upload, partArg string) {
	ctx := r.Context()
	n, err := strconv.Atoi(partArg)
	if err != nil || n < 1 || n > maxParts || !validUpload(upload) {
		problem(w, 400, "invalid_part", fmt.Sprintf("a part is PUT with ?upload={id}&part=1..%d", maxParts))
		return
	}
	given := strings.ToLower(r.Header.Get("X-Content-SHA256"))
	if !validSHA(given) {
		problem(w, 400, "sha256_required", "X-Content-SHA256 is the hex digest of the part, sent with the request")
		return
	}
	size := r.ContentLength
	if size <= 0 {
		problem(w, 400, "empty_body", "a part is at least one byte")
		return
	}
	if size > BlockSize {
		problem(w, 413, "too_large", fmt.Sprintf("a part is 1..%d bytes", BlockSize))
		return
	}
	if e := s.noRoom(ctx, m, size); e != nil {
		e.write(w)
		return
	}
	ok, err := s.uploadExists(ctx, m.Name, full, upload)
	if err != nil {
		s.fail(w, err)
		return
	}
	if !ok {
		problem(w, 404, "no_such_upload", "no such upload (it was completed, aborted or swept)")
		return
	}
	if e := s.checkQuota(ctx, m, held{key: full, upload: upload, part: n}, size); e != nil {
		e.write(w)
		return
	}
	sum5 := md5.New() // #nosec G401 -- see the import: the part of the ETag of a multipart object
	wr, herr := s.putBlock(ctx, m, full, upload, n-1, size, given, io.TeeReader(r.Body, sum5))
	if herr != nil {
		herr.write(w)
		return
	}
	if err := s.queue(ctx, m.Name, layout.BlockKey(full, upload, n-1), "copy", wr.failed); err != nil {
		s.fail(w, err) // the part is not acknowledged: the client sends it again, which writes the block again
		return
	}
	s.at("part:after-block")
	if _, err := s.DB.Exec(ctx, true, quotaGuard(m.Name, held{key: full, upload: upload, part: n}, false, "?", size), rq.S("INSERT INTO storage_parts (store, key, upload, part, sha256, size, written_at, stored_sha256, stored_size, enc, md5) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?) "+
		"ON CONFLICT(store, key, upload, part) DO UPDATE SET sha256 = excluded.sha256, size = excluded.size, written_at = excluded.written_at, stored_sha256 = excluded.stored_sha256, stored_size = excluded.stored_size, enc = excluded.enc, md5 = excluded.md5",
		m.Name, full, upload, n, wr.plain, size, s.now(), wr.stored, wr.storedSize, wr.enc, hex.EncodeToString(sum5.Sum(nil)))); err != nil {
		if quotaStopped(err) {
			// the block of this number is on the nodes with the new bytes: a row of an earlier part with the same number would not be true of them any more
			_, _ = s.DB.Exec(context.WithoutCancel(ctx), false, rq.S("DELETE FROM storage_parts WHERE store = ? AND key = ? AND upload = ? AND part = ?", m.Name, full, upload, n))
			err = errQuota
		}
		s.fail(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"etag": etag(wr.plain, size)})
}

func etag(sha string, size int64) string { return sha + "-" + strconv.FormatInt(size, 10) }

type partRow struct {
	part       int
	sha        string // of what the writer sent
	size       int64
	stored     string // of the block as stored
	storedSize int64
	enc        uint32
	md5        string // of the part, hex; "" when the part was written before the module kept it
}

func (s *Server) partsOf(ctx context.Context, st, full, upload string) ([]partRow, error) {
	rows, err := s.DB.Query(ctx, rq.S("SELECT part, sha256, size, stored_sha256, stored_size, enc, md5 FROM storage_parts WHERE store = ? AND key = ? AND upload = ? ORDER BY part", st, full, upload))
	if err != nil {
		return nil, err
	}
	out := make([]partRow, 0, len(rows))
	for _, r := range rows {
		p := partRow{int(rq.Int(r[0])), rq.Str(r[1]), rq.Int(r[2]), rq.Str(r[3]), rq.Int(r[4]), keyID(rq.Int(r[5])), rq.Str(r[6])}
		if p.stored == "" { // a row written before the blocks could be sealed
			p.stored, p.storedSize = p.sha, p.size
		}
		out = append(out, p)
	}
	return out, nil
}

func (s *Server) listParts(w http.ResponseWriter, r *http.Request, m *store.Mirror, full, upload string) {
	parts, err := s.partsOf(r.Context(), m.Name, full, upload)
	if err != nil {
		s.fail(w, err)
		return
	}
	items := make([]map[string]any, 0, len(parts))
	for _, p := range parts {
		items = append(items, map[string]any{"part": p.part, "sha256": p.sha, "size": p.size})
	}
	writeJSON(w, 200, map[string]any{"parts": items})
}

func (s *Server) complete(w http.ResponseWriter, r *http.Request, m *store.Mirror, full, upload string) {
	ctx := r.Context()
	if !validUpload(upload) {
		problem(w, 400, "invalid_request", "no such upload")
		return
	}
	cond, ok := s.precondition(ctx, w, r, m, full) // If-None-Match: * of a completion is create-only here too, decided in the transaction of the commit
	if !ok {
		return
	}
	data, err := io.ReadAll(io.LimitReader(r.Body, smallBody+1))
	if err != nil || len(data) > smallBody {
		problem(w, 400, "invalid_request", "the body is a list of parts, at most 1 MiB")
		return
	}
	var body struct {
		Parts [][]json.RawMessage `json:"parts"`
	}
	if json.Unmarshal(data, &body) != nil {
		problem(w, 400, "invalid_request", "the body is not JSON")
		return
	}
	if len(body.Parts) == 0 || len(body.Parts) > maxParts {
		problem(w, 400, "invalid_request", `the body is {"parts": [[1, "etag"], ...]}`)
		return
	}
	given := make([]struct {
		n    int
		etag string
	}, len(body.Parts))
	for i, p := range body.Parts {
		if len(p) != 2 || json.Unmarshal(p[0], &given[i].n) != nil || json.Unmarshal(p[1], &given[i].etag) != nil {
			problem(w, 400, "invalid_request", `a part is [number, "etag"]`)
			return
		}
	}
	stored, err := s.partsOf(ctx, m.Name, full, upload)
	if err != nil {
		s.fail(w, err)
		return
	}
	byN := map[int]partRow{}
	for _, p := range stored {
		byN[p.part] = p
	}
	sort.Slice(given, func(i, j int) bool { return given[i].n < given[j].n })
	blocks, plain := make([]string, len(given)), make([]string, len(given))
	var enc uint32
	var total, blockSize int64
	partsMD5 := []byte{} // the MD5s of the parts one after another: the ETag of S3 for an upload in parts is the MD5 of this and the number of parts
	for i, g := range given {
		if g.n != i+1 {
			problem(w, 400, "invalid_request", fmt.Sprintf("the parts are 1..%d with no gap or repeat", len(given)))
			return
		}
		p, ok := byN[g.n]
		if !ok {
			problem(w, 400, "invalid_request", fmt.Sprintf("part %d was not uploaded", g.n))
			return
		}
		if g.etag != etag(p.sha, p.size) {
			problem(w, 400, "invalid_request", fmt.Sprintf("the etag of part %d is not the one the module made", g.n))
			return
		}
		if i == 0 {
			blockSize = p.size
		}
		if (i < len(given)-1 && p.size != blockSize) || p.size > blockSize {
			problem(w, 400, "invalid_request", "every part but the last is as long as the first, and the last is not longer")
			return
		}
		if i == 0 {
			enc = p.enc
		} else if p.enc != enc {
			problem(w, 400, "invalid_request", "the parts of the upload are not all sealed the same way (the settings of the store changed in the middle of it): upload it again")
			return
		}
		blocks[i], plain[i] = p.stored, p.sha
		total += p.size
		if raw, err := hex.DecodeString(p.md5); err == nil && len(raw) == md5.Size && partsMD5 != nil {
			partsMD5 = append(partsMD5, raw...)
		} else {
			partsMD5 = nil // a part with no MD5 (written before the module kept it): the ETag stays the old kind
		}
	}
	if e := s.checkQuota(ctx, m, held{key: full, upload: upload}, total); e != nil {
		e.write(w)
		return
	}
	s.at("complete:before-commit")
	var om objMeta
	if rows, err := s.DB.Query(ctx, rq.S("SELECT content_type, meta FROM storage_uploads WHERE store = ? AND key = ? AND upload = ?", m.Name, full, upload)); err == nil && len(rows) == 1 {
		om.ContentType = rq.Str(rows[0][0])
		_ = json.Unmarshal([]byte(rq.Str(rows[0][1])), &om.Pairs)
	}
	etagMD5 := ""
	if partsMD5 != nil {
		sum := md5.Sum(partsMD5) // #nosec G401 -- see the import
		etagMD5 = hex.EncodeToString(sum[:]) + "-" + strconv.Itoa(len(given))
	}
	if err := s.commit(ctx, m, full, upload, total, blockSize, blocks, plain, "", etagMD5, enc, om, cond); err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"size": total})
}

func (s *Server) abort(w http.ResponseWriter, r *http.Request, m *store.Mirror, full, upload string) {
	ctx := r.Context()
	if validUpload(upload) {
		s.purge(ctx, m, full, upload)
		s.at("abort:after-purge")
		_, err := s.DB.Exec(ctx, true, rq.S("DELETE FROM storage_uploads WHERE store = ? AND key = ? AND upload = ?", m.Name, full, upload),
			rq.S("DELETE FROM storage_parts WHERE store = ? AND key = ? AND upload = ?", m.Name, full, upload))
		if err != nil {
			s.fail(w, err)
			return
		}
	}
	writeJSON(w, 200, map[string]any{"aborted": true})
}
