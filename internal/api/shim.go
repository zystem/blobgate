package api

import (
	"net/http"
	"strings"

	"github.com/zystem/blobgate/internal/auth"
	"github.com/zystem/blobgate/internal/layout"
)

// ShimHandler is the port for the shims of steps: only the PUT of a part and the GET of a range, each under a grant the core made for one upload or one object, and nothing else.
func (s *Server) ShimHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) { writeJSON(w, 200, map[string]string{"status": "ok"}) })
	mux.HandleFunc("/s/", s.shim)
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) { problem(w, 404, "not_found", "no such route") })
	return s.logged(mux, true)
}

func (s *Server) shim(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/s/")
	slash := strings.IndexByte(rest, '/')
	if slash < 1 {
		problem(w, 404, "not_found", "no such route")
		return
	}
	st, key := rest[:slash], rest[slash+1:]
	grant := bearer(r)
	shard := auth.GrantShard(grant) // the grant says which shard it is for; the secret of that shard checks it
	secret, known := s.cfg.ShardSecrets[shard]
	if !validShard(shard) || !known || st == defaultNam || layout.Valid(key) != "" {
		problem(w, 401, "unauthorized", "the grant is wrong, expired, or not for this call")
		return
	}
	q := r.URL.Query()
	switch r.Method {
	case http.MethodPut:
		upload := q.Get("upload")
		if !auth.VerifyGrant(secret, shard, st, key, upload, "w", grant, s.now()) {
			problem(w, 401, "unauthorized", "the grant is wrong, expired, or not for this call")
			return
		}
		if upload == "" {
			problem(w, 400, "invalid_part", "a part is PUT with ?upload={id}&part=1..10000")
			return
		}
		m := s.loadStore(w, r.Context(), st)
		if m == nil {
			return
		}
		s.putPart(w, r, m, shard+"/"+key, upload, q.Get("part"))
	case http.MethodGet:
		if !auth.VerifyGrant(secret, shard, st, key, "", "r", grant, s.now()) {
			problem(w, 401, "unauthorized", "the grant is wrong, expired, or not for this call")
			return
		}
		m := s.loadStore(w, r.Context(), st)
		if m == nil {
			return
		}
		s.read(w, r, m, shard+"/"+key)
	default:
		problem(w, 405, "method_not_allowed", "PUT or GET")
	}
}
