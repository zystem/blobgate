package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/zystem/blobgate/internal/auth"
	"github.com/zystem/blobgate/internal/rq"
	"github.com/zystem/blobgate/internal/space"
	"github.com/zystem/blobgate/internal/store"
)

type quota struct{ maxBytes, maxObjects int64 }

func (s *Server) quotaOf(ctx context.Context, st, tenant string) (quota, error) {
	rows, err := s.DB.QueryWeak(ctx, rq.S("SELECT max_bytes, max_objects FROM storage_quotas WHERE store = ? AND tenant = ?", st, tenant))
	if err != nil || len(rows) == 0 {
		return quota{}, err
	}
	return quota{rq.Int(rows[0][0]), rq.Int(rows[0][1])}, nil
}

// tenantOfKey is the tenant of a key with the tenant in front.
func tenantOfKey(full string) string {
	t, _, _ := strings.Cut(full, "/")
	return t
}

// What counts against the limits of a tenant in a store: its objects (not the trash), and the parts of its uploads that have not ended, which are room on the nodes already. A call leaves out the
// object of the key it writes (it is replaced) and what it is about to put itself.
type held struct {
	key    string // the key the call writes: its object is replaced
	upload string // the upload the call completes (part 0) or writes a part of
	part   int
}

const (
	sqlObjects = "FROM storage_objects WHERE store = ? AND key >= ? AND key < ? AND deleted_at = 0 AND key != ?"
	sqlStaged  = "(SELECT coalesce(sum(size), 0) FROM storage_parts WHERE store = ? AND key >= ? AND key < ? AND NOT (key = ? AND upload = ? AND (? = 0 OR part = ?)))"
)

func heldArgs(st, tenant string, h held) (objects, staged []any) {
	base := tenant + "/"
	return []any{st, base, upperBound(base), h.key}, []any{st, base, upperBound(base), h.key, h.upload, h.part, h.part}
}

// quotaGuard is the statement that stops the transaction it is in when the call would take the tenant over its limits: the limit is decided by the transaction that records the bytes, so writers at the
// same moment cannot pass it together. `size` is an SQL expression for the bytes the call adds, with its arguments; countObject says that the call adds an object.
func quotaGuard(st string, h held, countObject bool, size string, sizeArgs ...any) rq.Stmt {
	tenant := tenantOfKey(h.key)
	objects, staged := heldArgs(st, tenant, h)
	args := []any{st, tenant}
	over := "(q.max_bytes > 0 AND (SELECT coalesce(sum(size), 0) " + sqlObjects + ") + " + sqlStaged + " + " + size + " > q.max_bytes)"
	if countObject {
		over = "(q.max_objects > 0 AND (SELECT count(*) " + sqlObjects + ") + 1 > q.max_objects) OR " + over
		args = append(args, objects...)
	}
	args = append(append(append(args, objects...), staged...), sizeArgs...)
	// #nosec G202 -- the text is made of the constants above; the values are arguments
	return rq.S("INSERT INTO storage_guard (upload, cond, quota) SELECT 1, 1, NULL FROM storage_quotas q WHERE q.store = ? AND q.tenant = ? AND ("+over+")", args...)
}

var errQuota = errors.New("the write would take the tenant over its limit in the store (another write may have taken the room meanwhile); delete objects, or ask the operator for a larger limit")

// quotaStopped says whether a transaction was stopped by quotaGuard.
func quotaStopped(err error) bool {
	return err != nil && strings.Contains(err.Error(), "storage_guard.quota")
}

// checkQuota says beforehand, with the figures, what quotaGuard would refuse, so that bytes that cannot be kept are not taken. The decision is the guard's.
func (s *Server) checkQuota(ctx context.Context, m *store.Mirror, h held, add int64) *herr {
	tenant := tenantOfKey(h.key)
	q, err := s.quotaOf(ctx, m.Name, tenant)
	if err != nil {
		return &herr{503, "database_unavailable", s.hide("the database did not answer", err)}
	}
	if q.maxBytes == 0 && q.maxObjects == 0 {
		return nil
	}
	objects, staged := heldArgs(m.Name, tenant, h)
	args := append(append(append([]any{}, objects...), objects...), staged...)
	rows, err := s.DB.QueryWeak(ctx, rq.S("SELECT (SELECT count(*) "+sqlObjects+"), (SELECT coalesce(sum(size), 0) "+sqlObjects+"), "+sqlStaged, args...)) // #nosec G202 -- constants; the values are arguments
	if err != nil || len(rows) != 1 {
		return &herr{503, "database_unavailable", s.hide("the database did not answer", errors.Join(err, errors.New("the usage was not counted")))}
	}
	n, bytes, parts := rq.Int(rows[0][0]), rq.Int(rows[0][1]), rq.Int(rows[0][2])
	if q.maxObjects > 0 && n+1 > q.maxObjects {
		return &herr{507, "quota_exceeded", fmt.Sprintf("the tenant %s holds %d objects besides this one in the store %s and may hold %d; delete objects, or ask the operator for a larger limit", tenant, n, m.Name, q.maxObjects)}
	}
	if q.maxBytes > 0 && bytes+parts+add > q.maxBytes {
		return &herr{507, "quota_exceeded", fmt.Sprintf("the tenant %s holds %s besides this object in the store %s (%s of it in parts of uploads that have not ended) and may hold %s, and this write needs %s; delete objects, or ask the operator for a larger limit",
			tenant, space.Human(bytes+parts), m.Name, space.Human(parts), space.Human(q.maxBytes), space.Human(add))}
	}
	return nil
}

// adminQuotas is `/v1/admin/quotas`: GET lists them; `/v1/admin/quotas/{store}/{tenant}` PUT sets {max_bytes, max_objects} (0: no limit) and DELETE takes the limits away.
func (s *Server) adminQuotas(w http.ResponseWriter, r *http.Request) {
	if len(s.cfg.AdminToken) < 16 || !equal(bearer(r), s.cfg.AdminToken) {
		problem(w, 401, "unauthorized", "the admin token is wrong or none")
		return
	}
	ctx := r.Context()
	rest := strings.Trim(strings.TrimPrefix(r.URL.Path, "/v1/admin/quotas"), "/")
	if rest == "" {
		if r.Method != http.MethodGet {
			problem(w, 405, "method_not_allowed", "GET")
			return
		}
		rows, err := s.DB.QueryWeak(ctx, rq.S("SELECT store, tenant, max_bytes, max_objects FROM storage_quotas ORDER BY store, tenant"))
		if err != nil {
			s.fail(w, err)
			return
		}
		items := []map[string]any{}
		for _, row := range rows {
			items = append(items, map[string]any{"store": rq.Str(row[0]), "tenant": rq.Str(row[1]), "max_bytes": rq.Int(row[2]), "max_objects": rq.Int(row[3])})
		}
		writeJSON(w, 200, map[string]any{"quotas": items})
		return
	}
	st, tenant, ok := strings.Cut(rest, "/")
	if !ok || !auth.ValidTenant(tenant) || !store.ValidName(st) {
		problem(w, 404, "not_found", "a quota is /v1/admin/quotas/{store}/{tenant}")
		return
	}
	switch r.Method {
	case http.MethodPut:
		var body struct {
			MaxBytes   int64 `json:"max_bytes"`
			MaxObjects int64 `json:"max_objects"`
		}
		data, err := io.ReadAll(io.LimitReader(r.Body, smallBody+1))
		if err != nil || len(data) > smallBody || json.Unmarshal(data, &body) != nil || body.MaxBytes < 0 || body.MaxObjects < 0 {
			problem(w, 400, "invalid_request", `the body is {"max_bytes": n, "max_objects": n}, with 0 for no limit`)
			return
		}
		if rows, err := s.DB.QueryWeak(ctx, rq.S("SELECT count(*) FROM storage_stores WHERE name = ?", st)); err != nil || len(rows) == 0 || rq.Int(rows[0][0]) == 0 {
			problem(w, 404, "no_such_store", "no such store: "+st)
			return
		}
		if _, err := s.DB.Exec(ctx, false, rq.S("INSERT INTO storage_quotas (store, tenant, max_bytes, max_objects, updated_at) VALUES (?, ?, ?, ?, ?) "+
			"ON CONFLICT(store, tenant) DO UPDATE SET max_bytes = excluded.max_bytes, max_objects = excluded.max_objects, updated_at = excluded.updated_at", st, tenant, body.MaxBytes, body.MaxObjects, s.now())); err != nil {
			s.fail(w, err)
			return
		}
		writeJSON(w, 200, map[string]any{"store": st, "tenant": tenant, "max_bytes": body.MaxBytes, "max_objects": body.MaxObjects})
	case http.MethodDelete:
		if _, err := s.DB.Exec(ctx, false, rq.S("DELETE FROM storage_quotas WHERE store = ? AND tenant = ?", st, tenant)); err != nil {
			s.fail(w, err)
			return
		}
		writeJSON(w, 200, map[string]any{"deleted": true})
	default:
		problem(w, 405, "method_not_allowed", "PUT or DELETE")
	}
}

// adminUsage is `GET /v1/admin/stores/{name}:usage`: what every tenant holds in the store, with its limits.
func (s *Server) adminUsage(w http.ResponseWriter, r *http.Request, name string) {
	ctx := r.Context()
	rows, err := s.DB.QueryWeak(ctx, rq.S("SELECT substr(key, 1, instr(key, '/') - 1), deleted_at > 0, count(*), coalesce(sum(size), 0) FROM storage_objects WHERE store = ? GROUP BY 1, 2 ORDER BY 1", name))
	if err != nil {
		s.fail(w, err)
		return
	}
	byTenant := map[string]map[string]any{}
	order := []string{}
	for _, row := range rows {
		t := rq.Str(row[0])
		if byTenant[t] == nil {
			byTenant[t] = map[string]any{"tenant": t, "objects": int64(0), "bytes": int64(0), "trash_objects": int64(0), "trash_bytes": int64(0)}
			order = append(order, t)
		}
		if rq.Int(row[1]) == 1 {
			byTenant[t]["trash_objects"], byTenant[t]["trash_bytes"] = rq.Int(row[2]), rq.Int(row[3])
		} else {
			byTenant[t]["objects"], byTenant[t]["bytes"] = rq.Int(row[2]), rq.Int(row[3])
		}
	}
	items := []map[string]any{}
	for _, t := range order {
		q, _ := s.quotaOf(ctx, name, t)
		byTenant[t]["max_bytes"], byTenant[t]["max_objects"] = q.maxBytes, q.maxObjects
		items = append(items, byTenant[t])
	}
	out := map[string]any{"store": name, "tenants": items}
	// deduplication: what the objects hold, and what the store keeps for them
	if d, err := s.DB.QueryWeak(ctx, rq.S("SELECT (SELECT count(*) FROM storage_sets WHERE store = ?1), (SELECT coalesce(sum(size), 0) FROM storage_sets WHERE store = ?1), "+
		"(SELECT count(*) FROM storage_objects WHERE store = ?1 AND cid != ''), (SELECT coalesce(sum(size), 0) FROM storage_objects WHERE store = ?1 AND cid != '')", name)); err == nil && len(d) == 1 && rq.Int(d[0][0]) > 0 {
		out["dedup"] = map[string]any{"sets": rq.Int(d[0][0]), "stored_bytes": rq.Int(d[0][1]), "objects": rq.Int(d[0][2]), "logical_bytes": rq.Int(d[0][3])}
	}
	writeJSON(w, 200, out)
}
