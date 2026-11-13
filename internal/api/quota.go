package api

import (
	"context"
	"encoding/json"
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

// checkQuota says, as a refusal, whether writing `add` bytes under `full` would take the tenant over its limits. An object that is replaced counts for what it is now, not twice. The usage is counted from
// the inventory when the call is made, so a few writers at the same moment can pass together (a limit is kept to within what they write at once).
func (s *Server) checkQuota(ctx context.Context, m *store.Mirror, full string, add int64) *herr {
	tenant := tenantOfKey(full)
	q, err := s.quotaOf(ctx, m.Name, tenant)
	if err != nil {
		return &herr{503, "database_unavailable", err.Error()}
	}
	if q.maxBytes == 0 && q.maxObjects == 0 {
		return nil
	}
	u, err := s.tenantUsage(ctx, m.Name, tenant)
	if err != nil {
		return &herr{503, "database_unavailable", err.Error()}
	}
	objects, bytes := u.objects+1, u.bytes+add
	if old, _ := s.getRow(ctx, m.Name, full); old != nil { // a replacement: the old object goes
		objects, bytes = u.objects, u.bytes-old.size+add
	}
	if q.maxObjects > 0 && objects > q.maxObjects {
		return &herr{507, "quota_exceeded", fmt.Sprintf("the tenant %s holds %d objects in the store %s and may hold %d; delete objects, or ask the operator for a larger limit", tenant, u.objects, m.Name, q.maxObjects)}
	}
	if q.maxBytes > 0 && bytes > q.maxBytes {
		return &herr{507, "quota_exceeded", fmt.Sprintf("the tenant %s holds %s in the store %s and may hold %s, and this write needs %s; delete objects, or ask the operator for a larger limit",
			tenant, space.Human(u.bytes), m.Name, space.Human(q.maxBytes), space.Human(add))}
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
			fail(w, err)
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
			fail(w, err)
			return
		}
		writeJSON(w, 200, map[string]any{"store": st, "tenant": tenant, "max_bytes": body.MaxBytes, "max_objects": body.MaxObjects})
	case http.MethodDelete:
		if _, err := s.DB.Exec(ctx, false, rq.S("DELETE FROM storage_quotas WHERE store = ? AND tenant = ?", st, tenant)); err != nil {
			fail(w, err)
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
		fail(w, err)
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
	writeJSON(w, 200, map[string]any{"store": name, "tenants": items})
}
