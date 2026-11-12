package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"

	"github.com/zystem/blobgate/internal/auth"
	"github.com/zystem/blobgate/internal/layout"
	"github.com/zystem/blobgate/internal/rq"
	"github.com/zystem/blobgate/internal/space"
	"github.com/zystem/blobgate/internal/store"
)

type herr struct {
	code         int
	kind, detail string
}

func (e *herr) write(w http.ResponseWriter) { problem(w, e.code, e.kind, e.detail) }

func (s *Server) listStores(w http.ResponseWriter, r *http.Request) {
	if !s.authCore(r, "") {
		problem(w, 401, "unauthorized", "the token is wrong, expired, or not for this call")
		return
	}
	if r.Method != http.MethodGet {
		problem(w, 405, "method_not_allowed", "GET")
		return
	}
	list, err := s.Reg.List(r.Context())
	if err != nil {
		fail(w, err)
		return
	}
	def, _ := s.Reg.DefaultName(r.Context())
	items := []map[string]any{}
	for _, e := range list {
		items = append(items, map[string]any{"name": e.Name, "kind": e.Cfg.Kind, "default": e.Default})
	}
	writeJSON(w, 200, map[string]any{"stores": items, "default": def})
}

// authCore checks the v1 token of a call of the core.
func (s *Server) authCore(r *http.Request, _ string) bool {
	shard := r.Header.Get("X-Cinim-Shard")
	secret, ok := s.cfg.ShardSecrets[shard]
	if !validShard(shard) || !ok {
		return false
	}
	return auth.VerifyTarget(secret, shard, r.Method, target(r), bearer(r), s.now())
}

func (s *Server) coreObject(w http.ResponseWriter, r *http.Request) {
	if !s.authCore(r, "") {
		problem(w, 401, "unauthorized", "the token is wrong, expired, or not for this call")
		return
	}
	shard := r.Header.Get("X-Cinim-Shard")
	rest := strings.TrimPrefix(r.URL.Path, "/v1/stores/")
	slash := strings.IndexByte(rest, '/')
	if slash < 1 || !strings.HasPrefix(rest[slash+1:], "objects/") {
		problem(w, 404, "not_found", "no such route")
		return
	}
	st, key := rest[:slash], rest[slash+1+len("objects/"):]
	s.object(w, r, shard, st, key)
}

func (s *Server) object(w http.ResponseWriter, r *http.Request, shard, st, key string) {
	ctx := r.Context()
	m := s.loadStore(w, ctx, st)
	if m == nil {
		return
	}
	if why := layout.Valid(key); why != "" {
		problem(w, 400, "invalid_key", "a key is relative, with no empty, . or .. parts, at most 1024 characters: "+why)
		return
	}
	full := shard + "/" + key
	q := r.URL.Query()
	switch r.Method {
	case http.MethodPut:
		if has(r, "upload") {
			s.putPart(w, r, m, full, q.Get("upload"), q.Get("part"))
		} else {
			s.putObject(w, r, m, full)
		}
	case http.MethodPost:
		switch {
		case has(r, "uploads"):
			s.beginUpload(w, r, m, full, q.Get("size"))
		case has(r, "upload"):
			s.complete(w, r, m, full, q.Get("upload"))
		default:
			problem(w, 400, "invalid_request", "POST takes ?uploads or ?upload={id}")
		}
	case http.MethodGet:
		switch {
		case has(r, "parts") && has(r, "upload"):
			s.listParts(w, r, m, full, q.Get("upload"))
		case has(r, "stat"):
			s.stat(w, r, m, full)
		default:
			s.read(w, r, m, full)
		}
	case http.MethodDelete:
		if has(r, "upload") {
			s.abort(w, r, m, full, q.Get("upload"))
		} else {
			s.deleteObject(w, r, m, full)
		}
	default:
		problem(w, 405, "method_not_allowed", "PUT, GET, POST or DELETE")
	}
}

// noRoom is the 507 of a store that would be left with less than the reserve; nil when it fits or the store does not say.
func (s *Server) noRoom(ctx context.Context, m *store.Mirror, need int64) *herr {
	if why := space.Problem(m.Free(ctx), need, s.cfg.Reserve, m.Name); why != "" {
		return &herr{507, "store_full", why}
	}
	return nil
}

// queue puts what a node missed into the repair queue; `key` is a layout.BlockKey or layout.DirKey (the last operation on a key and a node is the one that stands).
func (s *Server) queue(ctx context.Context, st, key, op string, nodes []*store.Node) {
	for _, n := range nodes {
		_, _ = s.DB.Exec(ctx, false, rq.S("INSERT INTO storage_repair (store, key, node, op, since, attempts, next_at, last_error) VALUES (?, ?, ?, ?, ?, 0, 0, '') "+
			"ON CONFLICT(store, key, node) DO UPDATE SET op = excluded.op, since = excluded.since, attempts = 0, next_at = 0, last_error = ''", st, key, n.ID, op, s.now()))
	}
}

// putFailure is the answer to a block that could not be written: 507 with words when a node is out of room (also for nodes that cannot say beforehand how much room they have),
// else 503, which the core retries.
func putFailure(err error) *herr {
	var ns *store.ErrNoSpace
	if errors.As(err, &ns) {
		return &herr{507, "store_full", ns.Error()}
	}
	return &herr{503, "unavailable", err.Error()}
}

// writeBlock writes block n of a generation to the nodes and checks the digest of what arrived; on a bad digest the block is taken back.
func (s *Server) writeBlock(ctx context.Context, m *store.Mirror, full, gen string, n int, size int64, given string, body io.Reader) (string, *herr) {
	rel := layout.BlockPath(full, gen, n)
	res, err := m.Put(ctx, rel, size, body)
	if err != nil {
		s.purge(ctx, m, full, gen) // what a node that did take it has kept; a node that cannot be cleaned now is queued
		return "", putFailure(err)
	}
	if res.SHA256 != given {
		s.purge(ctx, m, full, gen)
		return "", &herr{400, "sha256_mismatch", "the body is not the one X-Content-SHA256 names"}
	}
	s.queue(ctx, m.Name, layout.BlockKey(full, gen, n), "copy", res.Failed)
	return res.SHA256, nil
}

type objectRow struct {
	size       int64
	generation string
	blockSize  int64
	blocks     []string
}

func (s *Server) getRow(ctx context.Context, st, full string) (*objectRow, error) {
	rows, err := s.DB.Query(ctx, rq.S("SELECT size, generation, block_size, blocks FROM storage_objects WHERE store = ? AND key = ?", st, full))
	if err != nil || len(rows) == 0 {
		return nil, err
	}
	o := &objectRow{size: rq.Int(rows[0][0]), generation: rq.Str(rows[0][1]), blockSize: rq.Int(rows[0][2])}
	if err := json.Unmarshal([]byte(rq.Str(rows[0][3])), &o.blocks); err != nil {
		return nil, fmt.Errorf("the row of %s is damaged: %w", full, err)
	}
	return o, nil
}

// purgeTasks are the statements that write down, for every node, that the generation the object has now must be removed from it: `INSERT ... SELECT` reads the row of the object in the same transaction
// as the statement that changes it, so every generation that is replaced or deleted is written down, whoever else writes the key at the same moment, and a process that dies after the
// transaction leaves the removal in the queue for the repair pass (write ahead: nothing is ever only in memory). `except` is the generation that stays ("" when the object goes).
func (s *Server) purgeTasks(m *store.Mirror, full, except string) []rq.Stmt {
	var out []rq.Stmt
	for _, n := range m.Nodes {
		out = append(out, rq.S("INSERT INTO storage_repair (store, key, node, op, since, attempts, next_at, last_error) "+
			"SELECT store, key || '~' || generation, ?, 'purge', ?, 0, 0, '' FROM storage_objects WHERE store = ? AND key = ? AND generation != '' AND generation != ? "+
			"ON CONFLICT(store, key, node) DO UPDATE SET op = 'purge', since = excluded.since, attempts = 0, next_at = 0, last_error = ''", n.ID, s.now(), m.Name, full, except))
	}
	return out
}

// commit makes the generation the object: one transaction, which is the whole of "fixing" an object, and which also writes down the removal of the generation it replaces. The removal is then tried
// at once; what a node could not do stays in the queue.
func (s *Server) commit(ctx context.Context, m *store.Mirror, full, gen string, size, blockSize int64, blocks []string) error {
	old, err := s.getRow(ctx, m.Name, full)
	if err != nil {
		return err
	}
	bj, _ := json.Marshal(blocks)
	s.at("commit:before")
	stmts := s.purgeTasks(m, full, gen)
	stmts = append(stmts,
		rq.S("INSERT INTO storage_objects (store, key, size, written_at, generation, block_size, blocks) VALUES (?, ?, ?, ?, ?, ?, ?) "+
			"ON CONFLICT(store, key) DO UPDATE SET size = excluded.size, written_at = excluded.written_at, generation = excluded.generation, block_size = excluded.block_size, blocks = excluded.blocks",
			m.Name, full, size, s.now(), gen, blockSize, string(bj)),
		rq.S("DELETE FROM storage_uploads WHERE store = ? AND key = ? AND upload = ?", m.Name, full, gen),
		rq.S("DELETE FROM storage_parts WHERE store = ? AND key = ? AND upload = ?", m.Name, full, gen))
	if _, err := s.DB.Exec(ctx, true, stmts...); err != nil {
		return err
	}
	s.at("commit:after-tx")
	if old != nil && old.generation != "" && old.generation != gen {
		s.purgeNow(ctx, m, full, old.generation)
	}
	return nil
}

// purgeNow removes a generation whose removal is already in the queue, and takes the task of every node that did it out of the queue.
func (s *Server) purgeNow(ctx context.Context, m *store.Mirror, full, gen string) {
	ctx = context.WithoutCancel(ctx)
	failed := map[string]bool{}
	for _, n := range m.DeleteDir(ctx, layout.BlockDir(full, gen)) {
		failed[n.ID] = true
	}
	for _, n := range m.Nodes {
		if !failed[n.ID] {
			_, _ = s.DB.Exec(ctx, false, rq.S("DELETE FROM storage_repair WHERE store = ? AND key = ? AND node = ? AND op = 'purge'", m.Name, layout.DirKey(full, gen), n.ID))
		}
	}
}

// purge removes the blocks of a generation; the nodes that could not are put in the repair queue.
func (s *Server) purge(ctx context.Context, m *store.Mirror, full, gen string) {
	ctx = context.WithoutCancel(ctx)
	s.queue(ctx, m.Name, layout.DirKey(full, gen), "purge", m.DeleteDir(ctx, layout.BlockDir(full, gen)))
}

func (s *Server) putObject(w http.ResponseWriter, r *http.Request, m *store.Mirror, full string) {
	ctx := r.Context()
	given := strings.ToLower(r.Header.Get("X-Content-SHA256"))
	if !validSHA(given) {
		problem(w, 400, "sha256_required", "X-Content-SHA256 is the hex digest of the body, sent with the request")
		return
	}
	size := r.ContentLength
	if size > BlockSize {
		problem(w, 413, "too_large", fmt.Sprintf("an object in one call, or a part, is at most %d bytes; use a multipart upload", BlockSize))
		return
	}
	if size <= 0 {
		problem(w, 400, "empty_body", "an object is at least one byte")
		return
	}
	if e := s.noRoom(ctx, m, size); e != nil {
		e.write(w)
		return
	}
	gen := newID()
	if _, err := s.DB.Exec(ctx, false, rq.S("INSERT INTO storage_uploads (store, key, upload, created_at) VALUES (?, ?, ?, ?)", m.Name, full, gen, s.now())); err != nil {
		fail(w, err)
		return
	}
	s.at("put:after-uploads-row")
	sha, e := s.writeBlock(ctx, m, full, gen, 0, size, given, r.Body)
	if e != nil {
		_, _ = s.DB.Exec(context.WithoutCancel(ctx), false, rq.S("DELETE FROM storage_uploads WHERE store = ? AND key = ? AND upload = ?", m.Name, full, gen))
		e.write(w)
		return
	}
	s.at("put:after-block")
	if err := s.commit(ctx, m, full, gen, size, size, []string{sha}); err != nil {
		s.purge(ctx, m, full, gen)
		fail(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"size": size})
}

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
	id := newID()
	if _, err := s.DB.Exec(ctx, false, rq.S("INSERT INTO storage_uploads (store, key, upload, created_at) VALUES (?, ?, ?, ?)", m.Name, full, id, s.now())); err != nil {
		fail(w, err)
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
		fail(w, err)
		return
	}
	if !ok {
		problem(w, 404, "no_such_upload", "no such upload (it was completed, aborted or swept)")
		return
	}
	rel := layout.BlockPath(full, upload, n-1)
	res, perr := m.Put(ctx, rel, size, r.Body)
	if perr != nil {
		deleteBlock(context.WithoutCancel(ctx), m, rel) // what a node that did take it has kept; one that cannot be cleaned now is overwritten by the retry, or swept with the upload
		putFailure(perr).write(w)
		return
	}
	if res.SHA256 != given {
		// the block that was just written is deleted by name, not its directory: other parts of the upload are in it
		deleteBlock(context.WithoutCancel(ctx), m, rel)
		problem(w, 400, "sha256_mismatch", "the body is not the one X-Content-SHA256 names")
		return
	}
	s.queue(ctx, m.Name, layout.BlockKey(full, upload, n-1), "copy", res.Failed)
	s.at("part:after-block")
	if _, err := s.DB.Exec(ctx, false, rq.S("INSERT INTO storage_parts (store, key, upload, part, sha256, size, written_at) VALUES (?, ?, ?, ?, ?, ?, ?) "+
		"ON CONFLICT(store, key, upload, part) DO UPDATE SET sha256 = excluded.sha256, size = excluded.size, written_at = excluded.written_at",
		m.Name, full, upload, n, res.SHA256, size, s.now())); err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"etag": etag(res.SHA256, size)})
}

func etag(sha string, size int64) string { return sha + "-" + strconv.FormatInt(size, 10) }

func deleteBlock(ctx context.Context, m *store.Mirror, rel string) []*store.Node {
	var failed []*store.Node
	for _, n := range m.Nodes {
		if o, err := n.F.NewObject(ctx, rel); err == nil {
			if err := o.Remove(ctx); err != nil {
				failed = append(failed, n)
			}
		}
	}
	return failed
}

type partRow struct {
	part int
	sha  string
	size int64
}

func (s *Server) partsOf(ctx context.Context, st, full, upload string) ([]partRow, error) {
	rows, err := s.DB.Query(ctx, rq.S("SELECT part, sha256, size FROM storage_parts WHERE store = ? AND key = ? AND upload = ? ORDER BY part", st, full, upload))
	if err != nil {
		return nil, err
	}
	out := make([]partRow, 0, len(rows))
	for _, r := range rows {
		out = append(out, partRow{int(rq.Int(r[0])), rq.Str(r[1]), rq.Int(r[2])})
	}
	return out, nil
}

func (s *Server) listParts(w http.ResponseWriter, r *http.Request, m *store.Mirror, full, upload string) {
	parts, err := s.partsOf(r.Context(), m.Name, full, upload)
	if err != nil {
		fail(w, err)
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
		fail(w, err)
		return
	}
	byN := map[int]partRow{}
	for _, p := range stored {
		byN[p.part] = p
	}
	sort.Slice(given, func(i, j int) bool { return given[i].n < given[j].n })
	blocks := make([]string, len(given))
	var total, blockSize int64
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
		blocks[i] = p.sha
		total += p.size
	}
	s.at("complete:before-commit")
	if err := s.commit(ctx, m, full, upload, total, blockSize, blocks); err != nil {
		fail(w, err)
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
			fail(w, err)
			return
		}
	}
	writeJSON(w, 200, map[string]any{"aborted": true})
}

func (s *Server) deleteObject(w http.ResponseWriter, r *http.Request, m *store.Mirror, full string) {
	ctx := r.Context()
	old, err := s.getRow(ctx, m.Name, full)
	if err != nil {
		fail(w, err)
		return
	}
	if old != nil {
		// the removal of the blocks is written down in the transaction that removes the row, so that a process that dies between the two leaves nothing that nobody knows of
		stmts := s.purgeTasks(m, full, "")
		stmts = append(stmts, rq.S("DELETE FROM storage_objects WHERE store = ? AND key = ?", m.Name, full))
		if _, err := s.DB.Exec(ctx, true, stmts...); err != nil {
			fail(w, err)
			return
		}
		s.at("delete:after-tx")
		s.purgeNow(ctx, m, full, old.generation)
	}
	writeJSON(w, 200, map[string]any{"deleted": true})
}

func (s *Server) stat(w http.ResponseWriter, r *http.Request, m *store.Mirror, full string) {
	o, err := s.getRow(r.Context(), m.Name, full)
	if err != nil {
		fail(w, err)
		return
	}
	if o == nil {
		problem(w, 404, "no_such_object", "no such object")
		return
	}
	writeJSON(w, 200, map[string]any{"size": o.size})
}

// parseRange reads `bytes=a-b` with both ends.
func parseRange(h string) (a, b int64, ok bool) {
	if !strings.HasPrefix(h, "bytes=") {
		return 0, 0, false
	}
	ends := strings.Split(h[6:], "-")
	if len(ends) != 2 {
		return 0, 0, false
	}
	var err1, err2 error
	a, err1 = strconv.ParseInt(ends[0], 10, 64)
	b, err2 = strconv.ParseInt(ends[1], 10, 64)
	return a, b, err1 == nil && err2 == nil
}

func (s *Server) read(w http.ResponseWriter, r *http.Request, m *store.Mirror, full string) {
	h := r.Header.Get("Range")
	if !strings.HasPrefix(h, "bytes=") {
		problem(w, 400, "range_required", "a read names a range: Range: bytes=a-b")
		return
	}
	a, b, ok := parseRange(h)
	if !ok {
		problem(w, 400, "invalid_range", "Range: bytes=a-b, with both ends")
		return
	}
	if a < 0 || b < a || b-a+1 > MaxRead {
		problem(w, 400, "invalid_range", fmt.Sprintf("a read is 1 byte to %d MiB", MaxRead>>20))
		return
	}
	o, err := s.getRow(r.Context(), m.Name, full)
	if err != nil {
		fail(w, err)
		return
	}
	if o == nil {
		problem(w, 404, "no_such_object", "no such object")
		return
	}
	s.streamRange(w, r, m, full, o, a, b)
}

// streamRange sends bytes a..b of the object (b is cut at its end). A block is read whole and checked against its digest before a byte of it is sent; the first block is read before the
// status is written, so that an object that cannot be read is an error and not a broken answer.
func (s *Server) streamRange(w http.ResponseWriter, r *http.Request, m *store.Mirror, full string, o *objectRow, a, b int64) {
	if a >= o.size {
		problem(w, 400, "invalid_range", fmt.Sprintf("the object is %d bytes", o.size))
		return
	}
	b = min(b, o.size-1)
	first, last := int(a/o.blockSize), int(b/o.blockSize)
	readBlock := func(i int) ([]byte, error) {
		select {
		case s.reads <- struct{}{}:
		case <-r.Context().Done():
			return nil, r.Context().Err()
		}
		defer func() { <-s.reads }()
		bl := min(o.blockSize, o.size-int64(i)*o.blockSize)
		return m.ReadBlock(r.Context(), layout.BlockPath(full, o.generation, i), o.blocks[i], bl)
	}
	slice := func(i int, data []byte) []byte {
		start := int64(i) * o.blockSize
		lo, hi := max(a-start, 0), min(b-start+1, int64(len(data)))
		return data[lo:hi]
	}
	data, err := readBlock(first)
	if err != nil {
		fail(w, err)
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Length", strconv.FormatInt(b-a+1, 10))
	w.WriteHeader(206)
	for i := first; ; i++ {
		if _, err := w.Write(slice(i, data)); err != nil || i == last {
			return
		}
		if data, err = readBlock(i + 1); err != nil {
			// after the first piece an error cannot be a status: the connection is cut, and the reader, who gets less than it asked for, knows
			panic(http.ErrAbortHandler)
		}
	}
}
