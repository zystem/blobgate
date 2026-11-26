package api

// What makes bytes that are on the nodes into an object: the conditions and the one transaction that records it.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/zystem/blobgate/internal/layout"
	"github.com/zystem/blobgate/internal/metrics"
	"github.com/zystem/blobgate/internal/rq"
	"github.com/zystem/blobgate/internal/store"
)

// purgeTasks are the statements that write down, for every node, that the generation the object has now must be removed from it: `INSERT ... SELECT` reads the row of the object in the same transaction
// as the statement that changes it, so every generation that is replaced or deleted is written down, whoever else writes the key at the same moment, and a process that dies after the
// transaction leaves the removal in the queue for the repair pass (write ahead: nothing is ever only in memory). `except` is the generation that stays ("" when the object goes).
func (s *Server) purgeTasks(m *store.Mirror, full, except string) []rq.Stmt {
	var out []rq.Stmt
	for _, n := range m.Nodes {
		out = append(out, rq.S("INSERT INTO storage_repair (store, key, node, op, since, attempts, next_at, last_error) "+
			"SELECT store, key || '~' || generation, ?, 'purge', ?, 0, 0, '' FROM storage_objects WHERE store = ? AND key = ? AND generation != '' AND generation != ? "+
			"ON CONFLICT(store, key, node) DO UPDATE SET rev = rev + 1, op = 'purge', since = excluded.since, attempts = 0, next_at = 0, last_error = ''", n.ID, s.now(), m.Name, full, except))
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
func (s *Server) commit(ctx context.Context, m *store.Mirror, full, gen string, size, blockSize int64, blocks, plain []string, digest, md5sum string, enc uint32, om objMeta, cond condition) error {
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
	setMD5 := md5sum // the MD5 of the whole bytes only: the set is shared by objects whose ETag is that (a `?link` takes it); the name of an upload in parts (`hex-N`) is the object's own
	if strings.Contains(setMD5, "-") {
		setMD5 = ""
	}
	cid := ""
	s.at("commit:before")
	// the upload must still be there: the sweep of expired uploads claims one by deleting its row, and from then on it may remove the blocks. Without this the transaction would publish a generation
	// whose blocks are being deleted. The statement fails (NOT NULL of the guard table) and rqlite rolls the transaction back.
	stmts := []rq.Stmt{rq.S("INSERT INTO storage_guard (upload, cond) SELECT NULL, 1 WHERE NOT EXISTS (SELECT 1 FROM storage_uploads WHERE store = ? AND key = ? AND upload = ?)", m.Name, full, gen)}
	stmts = append(stmts, cond.guard(m.Name, full)...) // and the object must still be as the If-Match or If-None-Match found it: the decision is here, in the transaction that writes
	stmts = append(stmts, quotaGuard(m.Name, held{key: full, upload: gen}, true, "?", size))
	stmts = append(stmts, s.releaseSet(m.Name, full, false))
	stmts = append(stmts, s.purgeTasks(m, full, gen)...)
	ownGen, ownBlocks, ownSize, ownEnc := gen, string(bj), blockSize, enc
	if dedup {
		cid = cidOf(scopeOf(m, full), enc, blockSize, size, plain)
		plj, _ := json.Marshal(plain)
		stmts = append(stmts, rq.S("INSERT INTO storage_sets (store, cid, owner, generation, size, block_size, blocks, plain, enc, refs, zero_since, created_at, scope, digest, md5) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, 1, 0, ?, ?, ?, ?) "+
			"ON CONFLICT(store, cid) DO UPDATE SET refs = refs + 1, zero_since = 0, scope = excluded.scope, digest = CASE WHEN digest = '' THEN excluded.digest ELSE digest END, md5 = CASE WHEN md5 = '' THEN excluded.md5 ELSE md5 END",
			m.Name, cid, full, gen, size, blockSize, string(bj), string(plj), enc, s.now(), scopeOf(m, full), digest, setMD5))
		for _, n := range m.Nodes {
			stmts = append(stmts, rq.S("INSERT INTO storage_repair (store, key, node, op, since, attempts, next_at, last_error) "+
				"SELECT store, ?, ?, 'purge', ?, 0, 0, '' FROM storage_sets WHERE store = ? AND cid = ? AND NOT (owner = ? AND generation = ?) "+
				"ON CONFLICT(store, key, node) DO UPDATE SET rev = rev + 1, op = 'purge', since = excluded.since, attempts = 0, next_at = 0, last_error = ''", layout.DirKey(full, gen), n.ID, s.now(), m.Name, cid, full, gen))
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
		switch {
		case strings.Contains(err.Error(), "storage_guard.upload"):
			return errUploadGone
		case strings.Contains(err.Error(), "storage_guard.cond"):
			return errPreconditionFailed
		case quotaStopped(err):
			return errQuota
		}
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

// condition is what a write or a delete that carried If-Match or If-None-Match was told about the object when the header was checked: that there was none, or the version it was. The check is made
// early, for the answer without a transfer of the body; the decision is the transaction's: it carries a guard that fails unless the object is still as it was here, so two writers under the same
// condition (create only, or the same version) cannot both win.
type condition struct {
	set     bool
	existed bool
	o       *objectRow
}

// precondition checks If-Match and If-None-Match of a write or a delete against the object as it is now, and returns the condition for the transaction that writes. An If-Match needs the strong
// comparison of the tags (a weak tag, `W/"..."`, never matches it); If-None-Match takes the weak one (RFC 9110, 13.1).
func (s *Server) precondition(ctx context.Context, w http.ResponseWriter, r *http.Request, m *store.Mirror, full string) (condition, bool) {
	im, inm := r.Header.Get("If-Match"), r.Header.Get("If-None-Match")
	if im == "" && inm == "" {
		return condition{}, true
	}
	o, err := s.getRow(ctx, m.Name, full)
	if err != nil {
		s.fail(w, err)
		return condition{}, false
	}
	if inm == "*" && o != nil || im != "" && im != "*" && (o == nil || !etagMatches(im, o.etag(), true)) || im == "*" && o == nil {
		problem(w, 412, "precondition_failed", "the object is not as If-Match or If-None-Match says")
		return condition{}, false
	}
	if inm != "" && inm != "*" && o != nil && etagMatches(inm, o.etag(), false) {
		problem(w, 412, "precondition_failed", "the object is the version If-None-Match names")
		return condition{}, false
	}
	return condition{set: true, existed: o != nil, o: o}, true
}

// guard is the statement that stops the transaction unless the object is as the condition found it: still absent, or still the same version (the same generation or set, the same bytes by their MD5
// and size, fixed at the same moment). Nothing when there was no condition.
func (c condition) guard(st, full string) []rq.Stmt {
	switch {
	case !c.set:
		return nil
	case !c.existed:
		return []rq.Stmt{rq.S("INSERT INTO storage_guard (upload, cond) SELECT 1, NULL WHERE EXISTS (SELECT 1 FROM storage_objects WHERE store = ? AND key = ? AND deleted_at = 0)", st, full)}
	}
	return []rq.Stmt{rq.S("INSERT INTO storage_guard (upload, cond) SELECT 1, NULL WHERE NOT EXISTS (SELECT 1 FROM storage_objects WHERE store = ? AND key = ? AND deleted_at = 0 "+
		"AND generation = ? AND cid = ? AND md5 = ? AND size = ? AND written_at = ?)", st, full, c.o.generation, c.o.cid, c.o.md5, c.o.size, c.o.written)}
}

// errPreconditionFailed is a write or a delete whose condition held when it was checked and does not hold in the transaction: another writer got there first.
var errPreconditionFailed = errors.New("the object changed while the write was going on, and is not as If-Match or If-None-Match says")

// etagMatches says whether a list of entity tags (as in If-Match) holds the tag. With strong, a weak tag in the list (`W/"..."`) does not match.
func etagMatches(list, tag string, strong bool) bool {
	for _, t := range strings.Split(list, ",") {
		t = strings.TrimSpace(t)
		if weak := strings.HasPrefix(t, "W/"); weak && strong {
			continue
		}
		if strings.TrimPrefix(t, "W/") == tag {
			return true
		}
	}
	return false
}

// errUploadGone is a commit of an upload whose row is not there any more: the sweep took it (it was older than BLOBGATE_UPLOAD_TTL_SECONDS) or it was aborted.
var errUploadGone = errors.New("the upload ended, was aborted, or was swept for being older than the time an upload may last")

// scopeOf is what two objects must share to share their blocks: the tenant, or the whole store.
func scopeOf(m *store.Mirror, full string) string {
	if m.Dedup == "store" {
		return ""
	}
	return tenantOfKey(full)
}

// surelyNotCommitted says whether the database was asked and the generation is neither the object's nor a set's: only then are its blocks removed after a commit that answered with an error. When the
// database cannot be asked the blocks stay, and the sweep (an upload row older than the time limit) or the reconcile removes them.
func (s *Server) surelyNotCommitted(ctx context.Context, m *store.Mirror, full, gen string) bool {
	ctx = context.WithoutCancel(ctx)
	rows, err := s.DB.Query(ctx, rq.S("SELECT count(*) FROM storage_objects WHERE store = ? AND key = ? AND generation = ?", m.Name, full, gen))
	if err != nil || len(rows) != 1 || rq.Int(rows[0][0]) != 0 {
		return false
	}
	rows, err = s.DB.Query(ctx, rq.S("SELECT count(*) FROM storage_sets WHERE store = ? AND owner = ? AND generation = ?", m.Name, full, gen))
	return err == nil && len(rows) == 1 && rq.Int(rows[0][0]) == 0
}
