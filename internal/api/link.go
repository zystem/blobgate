package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/zystem/blobgate/internal/metrics"
	"github.com/zystem/blobgate/internal/rq"
	"github.com/zystem/blobgate/internal/store"
)

// link is `POST ...?link`: the writer names bytes by their SHA-256 and size instead of sending them, and the key is made to point to the set of blocks that holds them (a "check before the write": a copy of
// bytes that are kept already is never sent). It works in a store with `dedup: "tenant"` only, and finds sets of the same tenant only: with the scope of the whole store a digest would be a key to the bytes
// of other tenants. A set made by a multipart upload, or before 0.6.0, has no digest and is not found; send the bytes.
func (s *Server) link(w http.ResponseWriter, r *http.Request, m *store.Mirror, full string) {
	ctx := r.Context()
	if m.Dedup != "tenant" {
		problem(w, 409, "dedup_not_tenant", `a link needs a store whose setting dedup is "tenant"; send the bytes with PUT`)
		return
	}
	var body struct {
		SHA256 string `json:"sha256"`
		Size   int64  `json:"size"`
	}
	raw, err := io.ReadAll(io.LimitReader(r.Body, smallBody))
	if err != nil || json.Unmarshal(raw, &body) != nil || !validSHA(strings.ToLower(body.SHA256)) || body.Size < 1 {
		problem(w, 400, "invalid_request", `the body is {"sha256": "<hex digest of the bytes>", "size": <bytes, at least 1>}`)
		return
	}
	body.SHA256 = strings.ToLower(body.SHA256)
	om, he := readMeta(r)
	if he != nil {
		he.write(w)
		return
	}
	if s.cfg.MaxObject > 0 && body.Size > s.cfg.MaxObject {
		problem(w, 413, "too_large", fmt.Sprintf("an object is at most %d bytes", s.cfg.MaxObject))
		return
	}
	if !s.precondition(ctx, w, r, m, full) {
		return
	}
	if e := s.checkQuota(ctx, m, full, body.Size); e != nil { // the limit is of what the tenant holds as its objects, shared bytes or not
		e.write(w)
		return
	}
	set, err := s.DB.Query(ctx, rq.S("SELECT cid FROM storage_sets WHERE store = ? AND scope = ? AND digest = ? AND size = ? ORDER BY created_at LIMIT 1", m.Name, scopeOf(m, full), body.SHA256, body.Size))
	if err != nil {
		fail(w, err)
		return
	}
	noContent := func() {
		problem(w, 404, "no_such_content", "no bytes of this tenant have this digest and size (or the set is gone just now); send them with PUT")
	}
	if len(set) == 0 {
		noContent()
		return
	}
	cid := rq.Str(set[0][0])
	old, err := s.getRowAny(ctx, m.Name, full)
	if err != nil {
		fail(w, err)
		return
	}
	pj, _ := json.Marshal(om.Pairs)
	if om.Pairs == nil {
		pj = []byte("{}")
	}
	s.at("link:before")
	stmts := []rq.Stmt{s.releaseSet(m.Name, full, false)}
	stmts = append(stmts, s.purgeTasks(m, full, "")...)
	stmts = append(stmts,
		rq.S("UPDATE storage_sets SET refs = refs + 1, zero_since = 0 WHERE store = ? AND cid = ?", m.Name, cid),
		// the cid is read from the set: a set that the worker removed meanwhile gives NULL, the column refuses it, and the whole transaction is rolled back (the old object is untouched)
		rq.S("INSERT INTO storage_objects (store, key, size, written_at, generation, block_size, blocks, enc, content_type, meta, deleted_at, cid, md5) "+
			"VALUES (?, ?, ?, ?, '', 0, '[]', 0, ?, ?, 0, (SELECT cid FROM storage_sets WHERE store = ? AND cid = ?), (SELECT md5 FROM storage_sets WHERE store = ? AND cid = ?)) "+
			"ON CONFLICT(store, key) DO UPDATE SET size = excluded.size, written_at = excluded.written_at, generation = '', block_size = 0, blocks = '[]', enc = 0, "+
			"content_type = excluded.content_type, meta = excluded.meta, deleted_at = 0, cid = excluded.cid, md5 = excluded.md5",
			m.Name, full, body.Size, s.now(), om.ContentType, string(pj), m.Name, cid, m.Name, cid))
	if _, err := s.DB.Exec(ctx, true, stmts...); err != nil {
		var re *rq.Error
		if errors.As(err, &re) && strings.Contains(re.Msg, "NOT NULL") {
			noContent()
			return
		}
		fail(w, err)
		return
	}
	s.at("link:after-tx")
	if old != nil && old.cid == "" && old.generation != "" {
		s.purgeNow(ctx, m, full, old.generation)
	}
	metrics.Deduplicated(m.Name, body.Size)
	writeJSON(w, 200, map[string]any{"size": body.Size, "linked": true})
}
