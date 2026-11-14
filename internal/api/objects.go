package api

import (
	"bytes"
	"context"
	"crypto/md5" // #nosec G501 -- the ETag of S3 is the MD5 of the bytes; it is a name that clients compare, not a defence
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/zystem/blobgate/internal/auth"
	"github.com/zystem/blobgate/internal/layout"
	"github.com/zystem/blobgate/internal/metrics"
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
	shard := tenantOf(r)
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
	shard := tenantOf(r)
	rest := strings.TrimPrefix(r.URL.Path, "/v1/stores/")
	slash := strings.IndexByte(rest, '/')
	if slash < 1 {
		problem(w, 404, "not_found", "no such route")
		return
	}
	st, tail := rest[:slash], rest[slash+1:]
	switch {
	case tail == "objects" || tail == "objects/": // the list of the tenant's objects
		s.tenantCall(w, r, shard, st, func(m *store.Mirror) { s.list(w, r, m, shard) })
	case tail == "usage":
		s.tenantCall(w, r, shard, st, func(m *store.Mirror) { s.usage(w, r, m, shard) })
	case strings.HasPrefix(tail, "objects/"):
		s.object(w, r, shard, st, tail[len("objects/"):])
	default:
		problem(w, 404, "not_found", "no such route")
	}
}

// tenantCall runs a GET that is about the tenant and not about one object, on the store.
func (s *Server) tenantCall(w http.ResponseWriter, r *http.Request, shard, st string, f func(m *store.Mirror)) {
	if r.Method != http.MethodGet {
		problem(w, 405, "method_not_allowed", "GET")
		return
	}
	if m := s.loadStore(w, r.Context(), st); m != nil {
		f(m)
	}
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
		case has(r, "restore"):
			s.restore(w, r, m, full)
		case has(r, "link"):
			s.link(w, r, m, full)
		case has(r, "uploads"):
			s.beginUpload(w, r, m, full, q.Get("size"))
		case has(r, "upload"):
			s.complete(w, r, m, full, q.Get("upload"))
		default:
			problem(w, 400, "invalid_request", "POST takes ?uploads, ?upload={id}, ?restore or ?link")
		}
	case http.MethodHead:
		s.head(w, r, m, full)
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
		problem(w, 405, "method_not_allowed", "PUT, GET, HEAD, POST or DELETE")
	}
}

// noRoom is the 507 of a store that would be left with less than the reserve; nil when it fits or the store does not say.
func (s *Server) noRoom(ctx context.Context, m *store.Mirror, need int64) *herr {
	info := m.Free(ctx)
	if info.Known {
		metrics.Room(m.Name, info.Free, info.Total)
	}
	if why := space.Problem(info, need, s.cfg.Reserve, m.Name); why != "" {
		metrics.Refused("store_full")
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
		metrics.Refused("store_full")
		return &herr{507, "store_full", ns.Error()}
	}
	return &herr{503, "unavailable", err.Error()}
}

// written says what a block is as it lies on the nodes.
type written struct {
	plain      string // SHA-256 of what the writer sent (what the shim reported, and the etag of a part)
	stored     string // SHA-256 of the block as stored: the same as plain, or of the sealed bytes
	storedSize int64
	enc        uint32 // the id of the key the block is sealed with; 0: not sealed
	failed     []*store.Node
}

// putBlock reads a block from the body, checks its digest against the one the writer named, and writes it to the nodes (sealed, when the store seals). A block that does not match is not kept: with a
// store that seals nothing is written at all, because the body is read before; without, what the nodes took is deleted by name.
func (s *Server) putBlock(ctx context.Context, m *store.Mirror, full, gen string, n int, size int64, given string, body io.Reader) (written, *herr) {
	rel := layout.BlockPath(full, gen, n)
	if m.Sealing() {
		// a block is sealed whole (the tag is at its end): it is held in memory, one block per write going on, and the number of them is bounded
		select {
		case s.seals <- struct{}{}:
			defer func() { <-s.seals }()
		case <-ctx.Done():
			return written{}, &herr{503, "unavailable", ctx.Err().Error()}
		}
		buf := make([]byte, size)
		if _, err := io.ReadFull(body, buf); err != nil {
			return written{}, &herr{503, "unavailable", "the body did not come whole: " + err.Error()}
		}
		sum := sha256.Sum256(buf)
		plain := hex.EncodeToString(sum[:])
		if given != "" && plain != given {
			return written{}, &herr{400, "sha256_mismatch", "the body is not the one X-Content-SHA256 names"}
		}
		given = plain
		var sealed []byte
		var err error
		if m.Dedup != "" {
			sealed, err = m.Enc.SealConv(scopeOf(m, full), plain, buf)
		} else {
			sealed, err = m.Enc.Seal(full, gen, n, buf)
		}
		if err != nil {
			return written{}, &herr{500, "encryption_failed", err.Error()}
		}
		res, err := m.Put(ctx, rel, int64(len(sealed)), bytes.NewReader(sealed))
		if err != nil {
			deleteBlock(context.WithoutCancel(ctx), m, rel)
			return written{}, putFailure(err)
		}
		return written{plain: given, stored: res.SHA256, storedSize: int64(len(sealed)), enc: m.Enc.Current, failed: res.Failed}, nil
	}
	res, err := m.Put(ctx, rel, size, body)
	if err != nil {
		deleteBlock(context.WithoutCancel(ctx), m, rel) // what a node that did take it has kept
		return written{}, putFailure(err)
	}
	if given != "" && res.SHA256 != given {
		deleteBlock(context.WithoutCancel(ctx), m, rel)
		return written{}, &herr{400, "sha256_mismatch", "the body is not the one X-Content-SHA256 names"}
	}
	return written{plain: res.SHA256, stored: res.SHA256, storedSize: size, failed: res.Failed}, nil
}

// writeBlock writes the one block of an object in a single call; on a failure the whole directory of the generation is removed (what cannot be removed now is queued).
func (s *Server) writeBlock(ctx context.Context, m *store.Mirror, full, gen string, n int, size int64, given string, body io.Reader) (written, *herr) {
	w, e := s.putBlock(ctx, m, full, gen, n, size, given, body)
	if e != nil {
		s.purge(ctx, m, full, gen)
		return written{}, e
	}
	s.queue(ctx, m.Name, layout.BlockKey(full, gen, n), "copy", w.failed)
	return w, nil
}

// storedLen is the length of block i of an object as it lies on the nodes.
func (o *objectRow) storedLen(i int) int64 {
	n := min(o.blockSize, o.size-int64(i)*o.blockSize)
	if o.enc != 0 {
		n += store.EncOverhead
	}
	return n
}

type objectRow struct {
	size       int64
	generation string
	blockSize  int64
	blocks     []string // SHA-256 of the blocks as stored
	enc        uint32
	written    int64 // when it was fixed, unix seconds
	deleted    int64 // when it was put in the trash, unix seconds; 0: it is there
	meta       objMeta
	// deduplication: cid names the set of blocks the object points to ("": the object has its own, in `generation` under its own key); owner is the key of the directory of that set, plain the digests of the
	// blocks before sealing (a sealed block of a set is opened with the key made from them)
	cid   string
	owner string
	plain []string
	md5   string
}

// dir is the key whose directory holds the blocks of the object.
func (o *objectRow) dir(full string) string {
	if o.owner != "" {
		return o.owner
	}
	return full
}

// objMeta is what a writer says about an object besides its bytes: the type of its contents and some pairs of text. It is kept and given back; the module does not look at it.
type objMeta struct {
	ContentType string            `json:"content_type,omitempty"`
	Pairs       map[string]string `json:"pairs,omitempty"`
}

const (
	metaHeader  = "X-Blobgate-Meta-"
	maxMetaKeys = 32
	maxMetaSize = 8 << 10
)

// readMeta reads the Content-Type and the `X-Blobgate-Meta-<name>` headers of a write; a name is 1..64 of letters, digits, `-` and `_`, a value up to 1024 printable characters, at most 32 pairs and 8 KiB in all.
func readMeta(r *http.Request) (objMeta, *herr) {
	om := objMeta{ContentType: r.Header.Get("Content-Type")}
	if len(om.ContentType) > 255 || strings.ContainsAny(om.ContentType, "\r\n") {
		return om, &herr{400, "invalid_metadata", "Content-Type is at most 255 characters"}
	}
	total := 0
	for name, vals := range r.Header {
		rest, ok := strings.CutPrefix(http.CanonicalHeaderKey(name), http.CanonicalHeaderKey(metaHeader))
		if !ok {
			continue
		}
		key := strings.ToLower(rest)
		v := strings.Join(vals, ",")
		if len(key) < 1 || len(key) > 64 || len(v) > 1024 {
			return om, &herr{400, "invalid_metadata", "a metadata name is 1..64 characters and a value at most 1024"}
		}
		for i := 0; i < len(key); i++ {
			c := key[i]
			if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-' || c == '_') {
				return om, &herr{400, "invalid_metadata", "a metadata name is letters, digits, - and _"}
			}
		}
		for i := 0; i < len(v); i++ {
			if v[i] < ' ' || v[i] == 0x7f {
				return om, &herr{400, "invalid_metadata", "a metadata value has no control character"}
			}
		}
		if om.Pairs == nil {
			om.Pairs = map[string]string{}
		}
		om.Pairs[key] = v
		total += len(key) + len(v)
		if len(om.Pairs) > maxMetaKeys || total > maxMetaSize {
			return om, &herr{400, "invalid_metadata", "at most 32 metadata pairs and 8 KiB in all"}
		}
	}
	return om, nil
}

// etagOf names a version of an object: what a reader can send back to ask whether it changed. An object written in one call has the MD5 of its bytes, as an object of S3 has, so that the tools of S3 that
// check it are content. Any other (a multipart upload, an object of an earlier release) has a name made of the digests of the blocks as stored and the number of blocks, written as the ETag of a
// multipart object is (`hex-N`), which those tools do not check against the bytes.
func etagOf(md5sum string, blocks []string, size int64) string {
	if md5sum != "" {
		return `"` + md5sum + `"`
	}
	h := sha256.Sum256([]byte(strings.Join(blocks, "\n") + "|" + strconv.FormatInt(size, 10)))
	return `"` + hex.EncodeToString(h[:16]) + "-" + strconv.Itoa(max(len(blocks), 1)) + `"`
}

func (o *objectRow) etag() string { return etagOf(o.md5, o.blocks, o.size) }

// setHeaders puts what is known of the object in the headers of an answer.
func (o *objectRow) setHeaders(w http.ResponseWriter) {
	h := w.Header()
	h.Set("ETag", o.etag())
	h.Set("Last-Modified", time.Unix(o.written, 0).UTC().Format(http.TimeFormat))
	h.Set("Accept-Ranges", "bytes")
	ct := o.meta.ContentType
	if ct == "" {
		ct = "application/octet-stream"
	}
	h.Set("Content-Type", ct)
	for k, v := range o.meta.Pairs {
		h.Set(metaHeader+k, v)
	}
}

// getRow is the object as a reader sees it: nil when there is none, or when it is in the trash.
func (s *Server) getRow(ctx context.Context, st, full string) (*objectRow, error) {
	o, err := s.getRowAny(ctx, st, full)
	if o != nil && o.deleted > 0 {
		return nil, err
	}
	return o, err
}

// getRowAny is the row whatever its state (the writes and the cleaning see the trash too).
func (s *Server) getRowAny(ctx context.Context, st, full string) (*objectRow, error) {
	// one query: the object, and the set it points to (deduplication) when it points to one
	rows, err := s.DB.QueryWeak(ctx, rq.S("SELECT o.size, o.generation, o.block_size, o.blocks, o.enc, o.written_at, o.content_type, o.meta, o.deleted_at, o.cid, o.md5, "+
		"s.owner, s.generation, s.block_size, s.blocks, s.plain, s.enc, s.md5 FROM storage_objects o LEFT JOIN storage_sets s ON o.cid != '' AND s.store = o.store AND s.cid = o.cid WHERE o.store = ? AND o.key = ?", st, full))
	if err != nil || len(rows) == 0 {
		return nil, err
	}
	o := &objectRow{size: rq.Int(rows[0][0]), generation: rq.Str(rows[0][1]), blockSize: rq.Int(rows[0][2]), enc: keyID(rq.Int(rows[0][4])), written: rq.Int(rows[0][5]), deleted: rq.Int(rows[0][8])}
	o.meta.ContentType = rq.Str(rows[0][6])
	_ = json.Unmarshal([]byte(rq.Str(rows[0][7])), &o.meta.Pairs)
	if err := json.Unmarshal([]byte(rq.Str(rows[0][3])), &o.blocks); err != nil {
		return nil, fmt.Errorf("the row of %s is damaged: %w", full, err)
	}
	o.md5 = rq.Str(rows[0][10])
	if o.cid = rq.Str(rows[0][9]); o.cid != "" {
		if rows[0][11] == nil {
			return nil, fmt.Errorf("the set %s of %s is gone", o.cid, full)
		}
		o.owner, o.generation, o.blockSize, o.enc = rq.Str(rows[0][11]), rq.Str(rows[0][12]), rq.Int(rows[0][13]), keyID(rq.Int(rows[0][16]))
		if o.md5 == "" {
			o.md5 = rq.Str(rows[0][17])
		}
		if json.Unmarshal([]byte(rq.Str(rows[0][14])), &o.blocks) != nil || json.Unmarshal([]byte(rq.Str(rows[0][15])), &o.plain) != nil {
			return nil, fmt.Errorf("the set %s of %s is damaged", o.cid, full)
		}
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

// cidOf names a set of blocks by what is in it: the scope (who may share it), the key the blocks are sealed with, the size of a block, the size, and the digest of every block before sealing.
func cidOf(scope string, enc uint32, blockSize, size int64, plain []string) string {
	h := sha256.New()
	fmt.Fprintf(h, "blobgate/set/v1\n%s\n%d\n%d\n%d\n%s", scope, enc, blockSize, size, strings.Join(plain, "\n"))
	return hex.EncodeToString(h.Sum(nil))
}

// releaseSet is the statement that gives back the reference the object has now on its set of blocks (nothing when it has none). It reads the row of the object in the transaction that changes or removes it,
// so that the count and the rows move together whoever else writes the key at the same moment. `trash` limits it to an object that is in the trash (the worker's removal).
func (s *Server) releaseSet(st, full string, trash bool) rq.Stmt {
	cond := ""
	if trash {
		cond = " AND deleted_at > 0"
	}
	return rq.S("UPDATE storage_sets SET refs = refs - 1, zero_since = CASE WHEN refs - 1 <= 0 THEN ? ELSE 0 END WHERE store = ? AND cid = (SELECT cid FROM storage_objects WHERE store = ? AND key = ? AND cid != ''"+cond+")",
		s.now(), st, st, full)
}

// commit makes the generation the object: one transaction, which is the whole of "fixing" an object, and which also writes down the removal of the generation it replaces. The removal is then tried
// at once; what a node could not do stays in the queue. In a store with deduplication the blocks are the object's own until this transaction: it either makes them a set (the first object with these bytes)
// or takes a reference on the set that is there and writes down the removal of the blocks just written. A writer never relies on a set that may be removed: the set is found or made in the same transaction.
func (s *Server) commit(ctx context.Context, m *store.Mirror, full, gen string, size, blockSize int64, blocks, plain []string, digest, md5sum string, enc uint32, om objMeta) error {
	old, err := s.getRowAny(ctx, m.Name, full)
	if err != nil {
		return err
	}
	bj, _ := json.Marshal(blocks)
	pj, _ := json.Marshal(om.Pairs)
	if om.Pairs == nil {
		pj = []byte("{}")
	}
	dedup := m.Dedup != "" && size > 0 && len(blocks) > 0 && len(plain) == len(blocks)
	cid := ""
	s.at("commit:before")
	stmts := []rq.Stmt{s.releaseSet(m.Name, full, false)}
	stmts = append(stmts, s.purgeTasks(m, full, gen)...)
	ownGen, ownBlocks, ownSize, ownEnc := gen, string(bj), blockSize, enc
	if dedup {
		cid = cidOf(scopeOf(m, full), enc, blockSize, size, plain)
		plj, _ := json.Marshal(plain)
		stmts = append(stmts, rq.S("INSERT INTO storage_sets (store, cid, owner, generation, size, block_size, blocks, plain, enc, refs, zero_since, created_at, scope, digest, md5) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, 1, 0, ?, ?, ?, ?) "+
			"ON CONFLICT(store, cid) DO UPDATE SET refs = refs + 1, zero_since = 0, scope = excluded.scope, digest = CASE WHEN digest = '' THEN excluded.digest ELSE digest END, md5 = CASE WHEN md5 = '' THEN excluded.md5 ELSE md5 END",
			m.Name, cid, full, gen, size, blockSize, string(bj), string(plj), enc, s.now(), scopeOf(m, full), digest, md5sum))
		for _, n := range m.Nodes {
			stmts = append(stmts, rq.S("INSERT INTO storage_repair (store, key, node, op, since, attempts, next_at, last_error) "+
				"SELECT store, ?, ?, 'purge', ?, 0, 0, '' FROM storage_sets WHERE store = ? AND cid = ? AND NOT (owner = ? AND generation = ?) "+
				"ON CONFLICT(store, key, node) DO UPDATE SET op = 'purge', since = excluded.since, attempts = 0, next_at = 0, last_error = ''", layout.DirKey(full, gen), n.ID, s.now(), m.Name, cid, full, gen))
		}
		ownGen, ownBlocks, ownSize, ownEnc = "", "[]", 0, 0
	}
	stmts = append(stmts,
		rq.S("INSERT INTO storage_objects (store, key, size, written_at, generation, block_size, blocks, enc, content_type, meta, deleted_at, cid, md5) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 0, ?, ?) "+
			"ON CONFLICT(store, key) DO UPDATE SET size = excluded.size, written_at = excluded.written_at, generation = excluded.generation, block_size = excluded.block_size, blocks = excluded.blocks, enc = excluded.enc, "+
			"content_type = excluded.content_type, meta = excluded.meta, deleted_at = 0, cid = excluded.cid, md5 = excluded.md5",
			m.Name, full, size, s.now(), ownGen, ownSize, ownBlocks, ownEnc, om.ContentType, string(pj), cid, md5sum),
		rq.S("DELETE FROM storage_uploads WHERE store = ? AND key = ? AND upload = ?", m.Name, full, gen),
		rq.S("DELETE FROM storage_parts WHERE store = ? AND key = ? AND upload = ?", m.Name, full, gen))
	if _, err := s.DB.Exec(ctx, true, stmts...); err != nil {
		return err
	}
	s.at("commit:after-tx")
	if old != nil && old.cid == "" && old.generation != "" && old.generation != gen {
		s.purgeNow(ctx, m, full, old.generation)
	}
	if dedup {
		if rows, err := s.DB.Query(ctx, rq.S("SELECT owner, generation FROM storage_sets WHERE store = ? AND cid = ?", m.Name, cid)); err == nil && len(rows) == 1 && (rq.Str(rows[0][0]) != full || rq.Str(rows[0][1]) != gen) {
			s.purgeNow(ctx, m, full, gen) // the bytes were there already: the blocks just written are not needed
			metrics.Deduplicated(m.Name, size)
		}
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

// precondition checks If-Match and If-None-Match of a write or a delete against the object as it is now. It is a check made before the write, not part of it: two writers at the same moment can both pass.
func (s *Server) precondition(ctx context.Context, w http.ResponseWriter, r *http.Request, m *store.Mirror, full string) bool {
	im, inm := r.Header.Get("If-Match"), r.Header.Get("If-None-Match")
	if im == "" && inm == "" {
		return true
	}
	o, err := s.getRow(ctx, m.Name, full)
	if err != nil {
		fail(w, err)
		return false
	}
	if inm == "*" && o != nil || im != "" && im != "*" && (o == nil || !etagMatches(im, o.etag())) || im == "*" && o == nil {
		problem(w, 412, "precondition_failed", "the object is not as If-Match or If-None-Match says")
		return false
	}
	if inm != "" && inm != "*" && o != nil && etagMatches(inm, o.etag()) {
		problem(w, 412, "precondition_failed", "the object is the version If-None-Match names")
		return false
	}
	return true
}

// etagMatches says whether a list of entity tags (as in If-Match) holds the tag.
func etagMatches(list, tag string) bool {
	for _, t := range strings.Split(list, ",") {
		if strings.TrimPrefix(strings.TrimSpace(t), "W/") == tag {
			return true
		}
	}
	return false
}

// putObject writes an object. A body of known length up to one block is streamed to the nodes as it comes; a longer one, or one of unknown length (chunked), is cut into blocks of 8 MiB by the module,
// which holds one block at a time. X-Content-SHA256, when sent, is the digest of the whole body and is checked; when not, the module makes it.
func (s *Server) putObject(w http.ResponseWriter, r *http.Request, m *store.Mirror, full string) {
	ctx := r.Context()
	given := strings.ToLower(r.Header.Get("X-Content-SHA256"))
	if given != "" && !validSHA(given) {
		problem(w, 400, "invalid_sha256", "X-Content-SHA256 is the hex digest of the body")
		return
	}
	om, he := readMeta(r)
	if he != nil {
		he.write(w)
		return
	}
	size := r.ContentLength // -1: not known
	if s.cfg.MaxObject > 0 && size > s.cfg.MaxObject {
		problem(w, 413, "too_large", fmt.Sprintf("an object is at most %d bytes", s.cfg.MaxObject))
		return
	}
	if !s.precondition(ctx, w, r, m, full) {
		return
	}
	need := size
	if need < 0 {
		need = BlockSize
	}
	if e := s.noRoom(ctx, m, need); e != nil {
		e.write(w)
		return
	}
	if e := s.checkQuota(ctx, m, full, need); e != nil {
		e.write(w)
		return
	}
	gen := newID()
	pj, _ := json.Marshal(om.Pairs)
	if om.Pairs == nil {
		pj = []byte("{}")
	}
	if _, err := s.DB.Exec(ctx, false, rq.S("INSERT INTO storage_uploads (store, key, upload, created_at, content_type, meta) VALUES (?, ?, ?, ?, ?, ?)", m.Name, full, gen, s.now(), om.ContentType, string(pj))); err != nil {
		fail(w, err)
		return
	}
	s.at("put:after-uploads-row")
	fail2 := func(e *herr) {
		_, _ = s.DB.Exec(context.WithoutCancel(ctx), false, rq.S("DELETE FROM storage_uploads WHERE store = ? AND key = ? AND upload = ?", m.Name, full, gen))
		e.write(w)
	}
	var total int64
	var blocks, plain []string
	var enc uint32
	var digest, md5sum string // of the whole body, when this write has seen it all
	sum5 := md5.New()         // #nosec G401 -- see the import
	switch {
	case size == 0:
		// an empty object has no block: the row says so
	case size > 0 && size <= BlockSize:
		w0, e := s.writeBlock(ctx, m, full, gen, 0, size, given, io.TeeReader(r.Body, sum5))
		if e != nil {
			fail2(e)
			return
		}
		total, blocks, plain, enc, digest = size, []string{w0.stored}, []string{w0.plain}, w0.enc, w0.plain
		md5sum = hex.EncodeToString(sum5.Sum(nil))
	default:
		buf := make([]byte, BlockSize)
		whole := sha256.New()
		for n := 0; ; n++ {
			k, err := io.ReadFull(r.Body, buf)
			if k > 0 {
				total += int64(k)
				if s.cfg.MaxObject > 0 && total > s.cfg.MaxObject {
					s.purge(ctx, m, full, gen)
					fail2(&herr{413, "too_large", fmt.Sprintf("an object is at most %d bytes", s.cfg.MaxObject)})
					return
				}
				whole.Write(buf[:k])
				sum5.Write(buf[:k])
				sum := sha256.Sum256(buf[:k])
				w0, e := s.writeBlock(ctx, m, full, gen, n, int64(k), hex.EncodeToString(sum[:]), bytes.NewReader(buf[:k]))
				if e != nil {
					fail2(e)
					return
				}
				blocks, plain, enc = append(blocks, w0.stored), append(plain, w0.plain), w0.enc
			}
			if err == io.EOF || err == io.ErrUnexpectedEOF {
				break
			}
			if err != nil {
				s.purge(ctx, m, full, gen)
				fail2(&herr{503, "unavailable", "the body did not come whole: " + err.Error()})
				return
			}
		}
		digest, md5sum = hex.EncodeToString(whole.Sum(nil)), hex.EncodeToString(sum5.Sum(nil))
		if given != "" && digest != given {
			s.purge(ctx, m, full, gen)
			fail2(&herr{400, "sha256_mismatch", "the body is not the one X-Content-SHA256 names"})
			return
		}
	}
	if size < 0 || size > BlockSize { // what the body came to is known only now: the limit is checked again before the object is fixed
		if e := s.checkQuota(ctx, m, full, total); e != nil {
			s.purge(ctx, m, full, gen)
			fail2(e)
			return
		}
	}
	if want := r.Header.Get("Content-MD5"); want != "" && size != 0 { // the writer's own check of the body, as S3 has it
		if raw, err := base64.StdEncoding.DecodeString(want); err != nil || hex.EncodeToString(raw) != md5sum {
			s.purge(ctx, m, full, gen)
			fail2(&herr{400, "md5_mismatch", "the body is not the one Content-MD5 names"})
			return
		}
	}
	s.at("put:after-block")
	blockSize := int64(BlockSize)
	if size > 0 && size <= BlockSize {
		blockSize = size
	}
	if err := s.commit(ctx, m, full, gen, total, blockSize, blocks, plain, digest, md5sum, enc, om); err != nil {
		s.purge(ctx, m, full, gen)
		fail(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"size": total})
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
	if e := s.checkQuota(ctx, m, full, max(expected, 0)); e != nil {
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
	wr, herr := s.putBlock(ctx, m, full, upload, n-1, size, given, r.Body)
	if herr != nil {
		herr.write(w)
		return
	}
	s.queue(ctx, m.Name, layout.BlockKey(full, upload, n-1), "copy", wr.failed)
	s.at("part:after-block")
	if _, err := s.DB.Exec(ctx, false, rq.S("INSERT INTO storage_parts (store, key, upload, part, sha256, size, written_at, stored_sha256, stored_size, enc) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?) "+
		"ON CONFLICT(store, key, upload, part) DO UPDATE SET sha256 = excluded.sha256, size = excluded.size, written_at = excluded.written_at, stored_sha256 = excluded.stored_sha256, stored_size = excluded.stored_size, enc = excluded.enc",
		m.Name, full, upload, n, wr.plain, size, s.now(), wr.stored, wr.storedSize, wr.enc)); err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"etag": etag(wr.plain, size)})
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
	part       int
	sha        string // of what the writer sent
	size       int64
	stored     string // of the block as stored
	storedSize int64
	enc        uint32
}

func (s *Server) partsOf(ctx context.Context, st, full, upload string) ([]partRow, error) {
	rows, err := s.DB.Query(ctx, rq.S("SELECT part, sha256, size, stored_sha256, stored_size, enc FROM storage_parts WHERE store = ? AND key = ? AND upload = ? ORDER BY part", st, full, upload))
	if err != nil {
		return nil, err
	}
	out := make([]partRow, 0, len(rows))
	for _, r := range rows {
		p := partRow{int(rq.Int(r[0])), rq.Str(r[1]), rq.Int(r[2]), rq.Str(r[3]), rq.Int(r[4]), keyID(rq.Int(r[5]))}
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
	blocks, plain := make([]string, len(given)), make([]string, len(given))
	var enc uint32
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
		if i == 0 {
			enc = p.enc
		} else if p.enc != enc {
			problem(w, 400, "invalid_request", "the parts of the upload are not all sealed the same way (the settings of the store changed in the middle of it): upload it again")
			return
		}
		blocks[i], plain[i] = p.stored, p.sha
		total += p.size
	}
	if e := s.checkQuota(ctx, m, full, total); e != nil {
		e.write(w)
		return
	}
	s.at("complete:before-commit")
	var om objMeta
	if rows, err := s.DB.Query(ctx, rq.S("SELECT content_type, meta FROM storage_uploads WHERE store = ? AND key = ? AND upload = ?", m.Name, full, upload)); err == nil && len(rows) == 1 {
		om.ContentType = rq.Str(rows[0][0])
		_ = json.Unmarshal([]byte(rq.Str(rows[0][1])), &om.Pairs)
	}
	if err := s.commit(ctx, m, full, upload, total, blockSize, blocks, plain, "", "", enc, om); err != nil {
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

// deleteObject removes an object. With a store that has a trash (soft_delete_seconds) it is hidden and kept for that long, and can be restored; `?purge` removes it at once, from the trash too.
func (s *Server) deleteObject(w http.ResponseWriter, r *http.Request, m *store.Mirror, full string) {
	ctx := r.Context()
	if !s.precondition(ctx, w, r, m, full) {
		return
	}
	old, err := s.getRowAny(ctx, m.Name, full)
	if err != nil {
		fail(w, err)
		return
	}
	if old == nil {
		writeJSON(w, 200, map[string]any{"deleted": true})
		return
	}
	if m.SoftDelete > 0 && !has(r, "purge") {
		if old.deleted == 0 {
			// the version that was read: its set (deduplication) or its own generation
			guard, arg := "generation = ? AND cid = ''", old.generation
			if old.cid != "" {
				guard, arg = "cid = ?", old.cid
			}
			if _, err := s.DB.Exec(ctx, false, rq.S("UPDATE storage_objects SET deleted_at = ? WHERE store = ? AND key = ? AND deleted_at = 0 AND "+guard, s.now(), m.Name, full, arg)); err != nil {
				fail(w, err)
				return
			}
			s.at("delete:after-trash")
		}
		writeJSON(w, 200, map[string]any{"deleted": true, "trash": true, "restorable_until": s.now() + int64(m.SoftDelete/time.Second)})
		return
	}
	// the removal of the blocks is written down in the transaction that removes the row, so that a process that dies between the two leaves nothing that nobody knows of
	stmts := []rq.Stmt{s.releaseSet(m.Name, full, false)} // a set that nothing points to any more is removed by the worker
	stmts = append(stmts, s.purgeTasks(m, full, "")...)
	stmts = append(stmts, rq.S("DELETE FROM storage_objects WHERE store = ? AND key = ?", m.Name, full))
	if _, err := s.DB.Exec(ctx, true, stmts...); err != nil {
		fail(w, err)
		return
	}
	s.at("delete:after-tx")
	if old.cid == "" {
		s.purgeNow(ctx, m, full, old.generation)
	}
	writeJSON(w, 200, map[string]any{"deleted": true})
}

// restore takes an object out of the trash.
func (s *Server) restore(w http.ResponseWriter, r *http.Request, m *store.Mirror, full string) {
	n, err := s.DB.Exec(r.Context(), false, rq.S("UPDATE storage_objects SET deleted_at = 0 WHERE store = ? AND key = ? AND deleted_at > 0", m.Name, full))
	if err != nil {
		fail(w, err)
		return
	}
	if len(n) != 1 || n[0] == 0 {
		problem(w, 404, "no_such_object", "there is no such object in the trash")
		return
	}
	writeJSON(w, 200, map[string]any{"restored": true})
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
	writeJSON(w, 200, map[string]any{"size": o.size, "etag": o.etag(), "content_type": o.meta.ContentType, "modified": o.written, "meta": o.meta.Pairs})
}

// parseRange reads one range of a Range header: `bytes=a-b`, `bytes=a-` (to the end) and `bytes=-n` (the last n bytes). ok is false for anything else, a list of ranges included. For a suffix, a is -1 and b is n.
func parseRange(h string) (a, b int64, ok bool) {
	spec, found := strings.CutPrefix(h, "bytes=")
	if !found || strings.Contains(spec, ",") {
		return 0, 0, false
	}
	lo, hi, found := strings.Cut(spec, "-")
	if !found {
		return 0, 0, false
	}
	var err error
	switch {
	case lo == "" && hi != "": // the last n bytes
		b, err = strconv.ParseInt(hi, 10, 64)
		return -1, b, err == nil && b > 0
	case lo != "" && hi == "": // from a to the end
		a, err = strconv.ParseInt(lo, 10, 64)
		return a, -1, err == nil && a >= 0
	case lo != "" && hi != "":
		a, err = strconv.ParseInt(lo, 10, 64)
		if err != nil {
			return 0, 0, false
		}
		b, err = strconv.ParseInt(hi, 10, 64)
		return a, b, err == nil && a >= 0 && b >= a
	}
	return 0, 0, false
}

// notModified answers If-None-Match / If-Match of a read: it returns true when the answer has been given (304 or 412).
func notModified(w http.ResponseWriter, r *http.Request, o *objectRow) bool {
	if im := r.Header.Get("If-Match"); im != "" && im != "*" && !etagMatches(im, o.etag()) {
		problem(w, 412, "precondition_failed", "the object is not the version If-Match names")
		return true
	}
	if inm := r.Header.Get("If-None-Match"); inm != "" && (inm == "*" || etagMatches(inm, o.etag())) {
		o.setHeaders(w)
		w.WriteHeader(http.StatusNotModified)
		return true
	}
	return false
}

// read answers a GET: the whole object, or the range named by Range (206). The bytes are streamed, one block at a time.
func (s *Server) read(w http.ResponseWriter, r *http.Request, m *store.Mirror, full string) {
	h := r.Header.Get("Range")
	var a, b int64
	ranged := h != ""
	if ranged {
		var ok bool
		if a, b, ok = parseRange(h); !ok {
			problem(w, 400, "invalid_range", "Range: bytes=a-b, bytes=a- or bytes=-n (one range)")
			return
		}
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
	if notModified(w, r, o) {
		return
	}
	if !ranged {
		if o.size == 0 {
			o.setHeaders(w)
			w.Header().Set("Content-Length", "0")
			w.WriteHeader(200)
			return
		}
		s.streamRange(w, r, m, full, o, 0, o.size-1, false)
		return
	}
	switch {
	case a < 0: // the last b bytes
		a, b = max(o.size-b, 0), o.size-1
	case b < 0: // to the end
		b = o.size - 1
	}
	if o.size == 0 || a >= o.size {
		w.Header().Set("Content-Range", fmt.Sprintf("bytes */%d", o.size))
		problem(w, 416, "range_not_satisfiable", fmt.Sprintf("the object is %d bytes", o.size))
		return
	}
	s.streamRange(w, r, m, full, o, a, min(b, o.size-1), true)
}

// head answers a HEAD: what a GET would say, with no body.
func (s *Server) head(w http.ResponseWriter, r *http.Request, m *store.Mirror, full string) {
	o, err := s.getRow(r.Context(), m.Name, full)
	if err != nil {
		fail(w, err)
		return
	}
	if o == nil {
		w.WriteHeader(404)
		return
	}
	if notModified(w, r, o) {
		return
	}
	o.setHeaders(w)
	w.Header().Set("Content-Length", strconv.FormatInt(o.size, 10))
	w.WriteHeader(200)
}

// streamRange sends bytes a..b of the object (b is within it). A block is read whole and checked against its digest before a byte of it is sent; the first block is read before the status is written, so
// that an object that cannot be read is an error and not a broken answer. partial says whether the answer is 206 (a range) or 200 (the whole).
func (s *Server) streamRange(w http.ResponseWriter, r *http.Request, m *store.Mirror, full string, o *objectRow, a, b int64, partial bool) {
	first, last := int(a/o.blockSize), int(b/o.blockSize)
	readBlock := func(i int) ([]byte, error) {
		select {
		case s.reads <- struct{}{}:
		case <-r.Context().Done():
			return nil, r.Context().Err()
		}
		defer func() { <-s.reads }()
		data, err := m.ReadBlock(r.Context(), layout.BlockPath(o.dir(full), o.generation, i), o.blocks[i], o.storedLen(i))
		if err != nil || o.enc == 0 {
			return data, err
		}
		if m.Enc == nil {
			return nil, &store.ErrNoKey{ID: o.enc}
		}
		if o.cid != "" {
			return m.Enc.OpenConv(scopeOf(m, full), o.plain[i], data)
		}
		return m.Enc.Open(full, o.generation, i, data)
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
	o.setHeaders(w)
	w.Header().Set("Content-Length", strconv.FormatInt(b-a+1, 10))
	if partial {
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", a, b, o.size))
		w.WriteHeader(206)
	} else {
		w.WriteHeader(200)
	}
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

// keyID reads the id of a sealing key from a cell; a value that is not an id (negative, or over 32 bits) is 0, "not sealed".
func keyID(n int64) uint32 {
	if n < 0 || n > math.MaxUint32 {
		return 0
	}
	return uint32(n)
}

// scopeOf is what two objects must share to share their blocks: the tenant, or the whole store.
func scopeOf(m *store.Mirror, full string) string {
	if m.Dedup == "store" {
		return ""
	}
	return tenantOfKey(full)
}
