package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"

	"github.com/zystem/blobgate/internal/rq"
	"github.com/zystem/blobgate/internal/store"
)

const (
	defaultListLimit = 100
	maxListLimit     = 1000
)

// upperBound is the smallest string above every string that starts with p (the end of a range of keys with a prefix); "" when there is none.
func upperBound(p string) string {
	if p == "" {
		return ""
	}
	b := []byte(p)
	for i := len(b) - 1; i >= 0; i-- {
		if b[i] < 0xff {
			b[i]++
			return string(b[:i+1])
		}
	}
	return ""
}

type listed struct {
	Key         string `json:"key"`
	Size        int64  `json:"size"`
	ETag        string `json:"etag"`
	ContentType string `json:"content_type,omitempty"`
	Modified    int64  `json:"modified"`
	DeletedAt   int64  `json:"deleted_at,omitempty"`
	PurgeAt     int64  `json:"purge_at,omitempty"`
}

// list answers `GET /v1/stores/{store}/objects`: the objects of the tenant whose key starts with `prefix`, in the order of the keys, `limit` at most (100, up to 1000), after the key or cursor `after`. With
// `delimiter` the keys that have it after the prefix are folded into `prefixes`, as the folders of a listing. `trash` lists the objects that are in the trash. The answer's `next` is where the next page begins.
// The list is read from the inventory; no node is asked.
func (s *Server) list(w http.ResponseWriter, r *http.Request, m *store.Mirror, tenant string) {
	q := r.URL.Query()
	prefix, after, delim := q.Get("prefix"), q.Get("after"), q.Get("delimiter")
	limit := defaultListLimit
	if v := q.Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > maxListLimit {
			problem(w, 400, "invalid_request", "limit is 1.."+strconv.Itoa(maxListLimit))
			return
		}
		limit = n
	}
	if len(delim) > 1 || len(prefix) > 1024 || len(after) > 1100 || strings.ContainsAny(prefix+after+delim, "\x00") {
		problem(w, 400, "invalid_request", "prefix, after and delimiter are text; the delimiter is one character")
		return
	}
	trash := has(r, "trash")
	base := tenant + "/"
	lo, hi := base+prefix, upperBound(base+prefix)
	cursor := ""
	if after != "" {
		cursor = base + after
	}
	deletedCond := "deleted_at = 0"
	if trash {
		deletedCond = "deleted_at > 0"
	}
	ctx := r.Context()
	out := struct {
		Objects  []listed `json:"objects"`
		Prefixes []string `json:"prefixes"`
		Next     string   `json:"next,omitempty"`
	}{Objects: []listed{}, Prefixes: []string{}}
	count := 0
	for {
		sql := "SELECT key, size, written_at, content_type, blocks, deleted_at FROM storage_objects WHERE store = ? AND key >= ? AND " + deletedCond
		args := []any{m.Name, lo}
		if hi != "" {
			sql += " AND key < ?"
			args = append(args, hi)
		}
		if cursor != "" {
			sql += " AND key > ?"
			args = append(args, cursor)
		}
		sql += " ORDER BY key LIMIT ?"
		asked := limit - count + 1
		args = append(args, asked)
		rows, err := s.DB.QueryWeak(ctx, rq.S(sql, args...))
		if err != nil {
			fail(w, err)
			return
		}
		restart := false
		for _, row := range rows {
			if count == limit { // one more row exists beyond the page: the next page begins at the cursor
				out.Next = strings.TrimPrefix(cursor, base)
				writeJSON(w, 200, out)
				return
			}
			key := rq.Str(row[0])
			rel := key[len(base):]
			if delim != "" {
				if j := strings.Index(rel[len(prefix):], delim); j >= 0 {
					cp := rel[:len(prefix)+j+len(delim)]
					out.Prefixes = append(out.Prefixes, cp)
					count++
					cursor = upperBound(base + cp) // the next row is the first one after everything under this folder
					restart = true
					break
				}
			}
			it := listed{Key: rel, Size: rq.Int(row[1]), Modified: rq.Int(row[2]), ContentType: rq.Str(row[3]), DeletedAt: rq.Int(row[5])}
			var blocks []string
			_ = json.Unmarshal([]byte(rq.Str(row[4])), &blocks)
			h := sha256.Sum256([]byte(strings.Join(blocks, "\n") + "|" + strconv.FormatInt(it.Size, 10)))
			it.ETag = `"` + hex.EncodeToString(h[:16]) + `"`
			if trash && m.SoftDelete > 0 {
				it.PurgeAt = it.DeletedAt + int64(m.SoftDelete.Seconds())
			}
			out.Objects = append(out.Objects, it)
			count++
			cursor = key
		}
		if !restart {
			writeJSON(w, 200, out) // the rows ran out before the page did: this is the end
			return
		}
		if cursor == "" {
			writeJSON(w, 200, out)
			return
		}
	}
}

// usage answers `GET /v1/stores/{store}/usage`: what the tenant holds in the store, and its limits.
func (s *Server) usage(w http.ResponseWriter, r *http.Request, m *store.Mirror, tenant string) {
	u, err := s.tenantUsage(r.Context(), m.Name, tenant)
	if err != nil {
		fail(w, err)
		return
	}
	q, err := s.quotaOf(r.Context(), m.Name, tenant)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"tenant": tenant, "store": m.Name, "objects": u.objects, "bytes": u.bytes, "trash_objects": u.trashObjects, "trash_bytes": u.trashBytes,
		"quota": map[string]any{"max_bytes": q.maxBytes, "max_objects": q.maxObjects}})
}

type usage struct{ objects, bytes, trashObjects, trashBytes int64 }

// tenantUsage counts what a tenant holds in a store, from the inventory: the objects that are there, and those in the trash.
func (s *Server) tenantUsage(ctx context.Context, st, tenant string) (usage, error) {
	base := tenant + "/"
	var u usage
	rows, err := s.DB.QueryWeak(ctx, rq.S("SELECT deleted_at > 0, count(*), coalesce(sum(size), 0) FROM storage_objects WHERE store = ? AND key >= ? AND key < ? GROUP BY deleted_at > 0", st, base, upperBound(base)))
	if err != nil {
		return u, err
	}
	for _, r := range rows {
		if rq.Int(r[0]) == 1 {
			u.trashObjects, u.trashBytes = rq.Int(r[1]), rq.Int(r[2])
		} else {
			u.objects, u.bytes = rq.Int(r[1]), rq.Int(r[2])
		}
	}
	return u, nil
}
