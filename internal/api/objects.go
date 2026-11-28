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
		s.fail(w, err)
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
func (s *Server) queue(ctx context.Context, st, key, op string, nodes []*store.Node) error {
	var first error
	for _, n := range nodes {
		if _, err := s.DB.Exec(ctx, false, rq.S("INSERT INTO storage_repair (store, key, node, op, since, attempts, next_at, last_error) VALUES (?, ?, ?, ?, ?, 0, 0, '') "+
			"ON CONFLICT(store, key, node) DO UPDATE SET rev = rev + 1, op = excluded.op, since = excluded.since, attempts = 0, next_at = 0, last_error = ''", st, key, n.ID, op, s.now())); err != nil {
			metrics.QueueError()
			s.log.Warn("a repair task could not be written down", "op", op, "key", key, "error", err.Error())
			if first == nil {
				first = err
			}
		}
	}
	return first
}

// putFailure is the answer to a block that could not be written: 507 with words when a node is out of room (also for nodes that cannot say beforehand how much room they have),
// else 503, which the core retries.
func (s *Server) putFailure(err error) *herr {
	var ns *store.ErrNoSpace
	if errors.As(err, &ns) {
		metrics.Refused("store_full")
		return &herr{507, "store_full", s.hide("the store "+ns.Store+" could not take the block: it is out of space; "+space.Hint, ns)}
	}
	return &herr{503, "unavailable", s.hide("not enough nodes of the store took the block", err)}
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
func (s *Server) putBlock(ctx context.Context, m *store.Mirror, full, gen string, n int, suffix string, size int64, given string, body io.Reader) (written, *herr) {
	rel := layout.BlockPath(full, gen, n) + suffix
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
			return written{}, &herr{503, "unavailable", s.hide("the body did not come whole", err)}
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
			return written{}, &herr{500, "encryption_failed", s.hide("a block could not be sealed", err)}
		}
		res, err := m.Put(ctx, rel, int64(len(sealed)), bytes.NewReader(sealed))
		if err != nil {
			deleteBlock(context.WithoutCancel(ctx), m, rel)
			return written{}, s.putFailure(err)
		}
		return written{plain: given, stored: res.SHA256, storedSize: int64(len(sealed)), enc: m.Enc.Current, failed: res.Failed}, nil
	}
	res, err := m.Put(ctx, rel, size, body)
	if err != nil {
		deleteBlock(context.WithoutCancel(ctx), m, rel) // what a node that did take it has kept
		return written{}, s.putFailure(err)
	}
	if given != "" && res.SHA256 != given {
		deleteBlock(context.WithoutCancel(ctx), m, rel)
		return written{}, &herr{400, "sha256_mismatch", "the body is not the one X-Content-SHA256 names"}
	}
	return written{plain: res.SHA256, stored: res.SHA256, storedSize: size, failed: res.Failed}, nil
}

// writeBlock writes the one block of an object in a single call; on a failure the whole directory of the generation is removed (what cannot be removed now is queued).
func (s *Server) writeBlock(ctx context.Context, m *store.Mirror, full, gen string, n int, size int64, given string, body io.Reader) (written, *herr) {
	w, e := s.putBlock(ctx, m, full, gen, n, "", size, given, body)
	if e != nil {
		s.purge(ctx, m, full, gen)
		return written{}, e
	}
	// a block that a node missed is written down for the repair before the write goes on: when that cannot be done the object would be acknowledged with fewer copies than the store means to keep and
	// nothing would remember which, so the write is refused (the client tries again) and the blocks removed
	if err := s.queue(ctx, m.Name, layout.BlockKey(full, gen, n), "copy", w.failed); err != nil {
		s.purge(ctx, m, full, gen)
		return written{}, &herr{503, "database_unavailable", s.hide("a copy for a node that missed the block could not be written down", err)}
	}
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
	digests := make([]string, len(blocks))
	for i, b := range blocks {
		digests[i] = layout.Digest(b) // the name of an object does not depend on which attempt wrote its blocks
	}
	h := sha256.Sum256([]byte(strings.Join(digests, "\n") + "|" + strconv.FormatInt(size, 10)))
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
	cond, ok := s.precondition(ctx, w, r, m, full)
	if !ok {
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
	if e := s.checkQuota(ctx, m, held{key: full}, need); e != nil {
		e.write(w)
		return
	}
	gen := newID()
	pj, _ := json.Marshal(om.Pairs)
	if om.Pairs == nil {
		pj = []byte("{}")
	}
	if _, err := s.DB.Exec(ctx, false, rq.S("INSERT INTO storage_uploads (store, key, upload, created_at, content_type, meta, one_call) VALUES (?, ?, ?, ?, ?, ?, 1)", m.Name, full, gen, s.now(), om.ContentType, string(pj))); err != nil {
		s.fail(w, err)
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
		if err := s.hold(ctx, tenantOfKey(full), heldWrite, true); err != nil { // the buffer of a block is held for as long as the writer takes to send it
			_, _ = s.DB.Exec(context.WithoutCancel(ctx), false, rq.S("DELETE FROM storage_uploads WHERE store = ? AND key = ? AND upload = ?", m.Name, full, gen))
			if errors.Is(err, errBusy) {
				busy(w)
			}
			return // or the client has gone
		}
		defer s.unhold(tenantOfKey(full), heldWrite)
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
				fail2(&herr{503, "unavailable", s.hide("the body did not come whole", err)})
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
		if e := s.checkQuota(ctx, m, held{key: full, upload: gen}, total); e != nil {
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
	if err := s.commit(ctx, m, full, gen, total, blockSize, blocks, plain, digest, md5sum, enc, om, cond); err != nil {
		// the blocks are removed only when the generation is surely not the object's: an error of the database says nothing of whether the transaction was applied (a change of leader answers with an
		// error after the commit), and removing the blocks of a generation that is live would lose the object
		if errors.Is(err, errUploadGone) || s.surelyNotCommitted(ctx, m, full, gen) {
			s.purge(ctx, m, full, gen)
			_, _ = s.DB.Exec(context.WithoutCancel(ctx), true, rq.S("DELETE FROM storage_uploads WHERE store = ? AND key = ? AND upload = ?", m.Name, full, gen),
				rq.S("DELETE FROM storage_parts WHERE store = ? AND key = ? AND upload = ?", m.Name, full, gen))
		}
		s.fail(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"size": total})
}

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

// deleteObject removes an object. With a store that has a trash (soft_delete_seconds) it is hidden and kept for that long, and can be restored; `?purge` removes it at once, from the trash too.
func (s *Server) deleteObject(w http.ResponseWriter, r *http.Request, m *store.Mirror, full string) {
	ctx := r.Context()
	cond, ok := s.precondition(ctx, w, r, m, full)
	if !ok {
		return
	}
	old, err := s.getRowAny(ctx, m.Name, full)
	if err != nil {
		s.fail(w, err)
		return
	}
	if old == nil {
		writeJSON(w, 200, map[string]any{"deleted": true})
		return
	}
	s.at("delete:before-write")
	if m.SoftDelete > 0 && !has(r, "purge") {
		if old.deleted == 0 {
			// the version that was read: its set (deduplication) or its own generation
			guard, arg := "generation = ? AND cid = ''", old.generation
			if old.cid != "" {
				guard, arg = "cid = ?", old.cid
			}
			// with a condition the statement is also part of the guard: it changes the row only if the row is the version the condition found
			q := "UPDATE storage_objects SET deleted_at = ? WHERE store = ? AND key = ? AND deleted_at = 0 AND " + guard
			args := []any{s.now(), m.Name, full, arg}
			if cond.set && cond.existed {
				q += " AND generation = ? AND cid = ? AND md5 = ? AND size = ? AND written_at = ?"
				args = append(args, cond.o.generation, cond.o.cid, cond.o.md5, cond.o.size, cond.o.written)
			}
			n, err := s.DB.Exec(ctx, false, rq.S(q, args...))
			if err != nil {
				s.fail(w, err)
				return
			}
			if cond.set && (len(n) != 1 || n[0] == 0) {
				problem(w, 412, "precondition_failed", errPreconditionFailed.Error())
				return
			}
			s.at("delete:after-trash")
		}
		writeJSON(w, 200, map[string]any{"deleted": true, "trash": true, "restorable_until": s.now() + int64(m.SoftDelete/time.Second)})
		return
	}
	// the removal of the blocks is written down in the transaction that removes the row, so that a process that dies between the two leaves nothing that nobody knows of
	stmts := cond.guard(m.Name, full)
	stmts = append(stmts, s.releaseSet(m.Name, full, false)) // a set that nothing points to any more is removed by the worker
	stmts = append(stmts, s.purgeTasks(m, full, "")...)
	stmts = append(stmts, rq.S("DELETE FROM storage_objects WHERE store = ? AND key = ?", m.Name, full))
	if _, err := s.DB.Exec(ctx, true, stmts...); err != nil {
		if strings.Contains(err.Error(), "storage_guard.cond") {
			problem(w, 412, "precondition_failed", errPreconditionFailed.Error())
			return
		}
		s.fail(w, err)
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
	// an object that comes back counts again: only when it is in the trash does the guard ask
	guard := quotaGuard(m.Name, held{key: full}, true, "(SELECT size FROM storage_objects WHERE store = ? AND key = ?)", m.Name, full)
	guard.SQL += " AND EXISTS (SELECT 1 FROM storage_objects WHERE store = ? AND key = ? AND deleted_at > 0)"
	guard.Args = append(guard.Args, m.Name, full)
	n, err := s.DB.Exec(r.Context(), true, guard, rq.S("UPDATE storage_objects SET deleted_at = 0 WHERE store = ? AND key = ? AND deleted_at > 0", m.Name, full))
	if quotaStopped(err) {
		err = errQuota
	}
	if err != nil {
		s.fail(w, err)
		return
	}
	if len(n) != 2 || n[1] == 0 {
		problem(w, 404, "no_such_object", "there is no such object in the trash")
		return
	}
	writeJSON(w, 200, map[string]any{"restored": true})
}

// keyID reads the id of a sealing key from a cell; a value that is not an id (negative, or over 32 bits) is 0, "not sealed".
func keyID(n int64) uint32 {
	if n < 0 || n > math.MaxUint32 {
		return 0
	}
	return uint32(n)
}
