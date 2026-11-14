// Package api is the HTTP side of the module: the core's calls and the operator's calls on one port, the shim's calls on another (docs/api/storage.openapi.yaml is the description).
package api

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/zystem/blobgate/internal/auth"
	"github.com/zystem/blobgate/internal/metrics"
	"github.com/zystem/blobgate/internal/rq"
	"github.com/zystem/blobgate/internal/space"
	"github.com/zystem/blobgate/internal/store"
)

//go:embed openapi.yaml
var openapiYAML []byte

const (
	// BlockSize is the most a block (an object in one call, or a part) may hold.
	BlockSize = 8 << 20
	maxParts  = 10000
	// MaxRead is the most one read may ask for (it is streamed: a guard against a mistake, not a size in memory).
	MaxRead    = 256 << 20
	smallBody  = 1 << 20
	defaultNam = "_default"
)

// Config is what the server needs.
type Config struct {
	ShardSecrets map[string]string // shard -> the secret its calls are signed with
	AdminToken   string
	Reserve      int64         // bytes kept free in a store (0: space.DefaultReserve)
	MaxObject    int64         // the most bytes an object may hold; 0: no limit besides the room
	MaxReads     int           // reads of blocks at once (0: 8): each holds one block in memory
	UploadTTL    time.Duration // an upload older than this is swept (0: 24 h)
	Now          func() time.Time
	// CrashAt, if set, is called at the named points of a write or a delete (`put:after-uploads-row`, `put:after-block`, `part:after-block`, `complete:before-commit`, `commit:before`,
	// `commit:after-tx`, `delete:after-tx`, `delete:after-trash`, `abort:after-purge`). The tests make it end the request there, as a process that dies would; nothing sets it in the program.
	CrashAt func(point string)
}

// Server answers the calls.
type Server struct {
	cfg   Config
	DB    *rq.Client
	Reg   *store.Registry
	reads chan struct{}
	seals chan struct{} // blocks being sealed: each holds one in memory
	log   *slog.Logger
}

// New makes a server.
func New(cfg Config, db *rq.Client, reg *store.Registry, log *slog.Logger) *Server {
	if cfg.Reserve == 0 {
		cfg.Reserve = space.DefaultReserve
	}
	if cfg.MaxReads <= 0 {
		cfg.MaxReads = 8
	}
	if cfg.UploadTTL == 0 {
		cfg.UploadTTL = 24 * time.Hour
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if log == nil {
		log = slog.Default()
	}
	return &Server{cfg: cfg, DB: db, Reg: reg, reads: make(chan struct{}, cfg.MaxReads), seals: make(chan struct{}, cfg.MaxReads), log: log}
}

func (s *Server) now() int64 { return s.cfg.Now().Unix() }

func (s *Server) at(point string) {
	if s.cfg.CrashAt != nil {
		s.cfg.CrashAt(point)
	}
}

type apiError struct {
	Error  string `json:"error"`
	Detail string `json:"detail"`
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func problem(w http.ResponseWriter, code int, kind, detail string) {
	if code == 503 {
		w.Header().Set("Retry-After", "5")
	}
	writeJSON(w, code, apiError{kind, detail})
}

// fail answers for an error of the database or of the nodes.
func fail(w http.ResponseWriter, err error) {
	var un *store.ErrUnavailable
	var nokey *store.ErrNoKey
	switch {
	case rq.IsDB(err):
		problem(w, 503, "database_unavailable", err.Error())
	case errors.Is(err, store.ErrNotFound):
		problem(w, 404, "no_such_object", "no such object")
	case errors.As(err, &un):
		problem(w, 503, "unavailable", un.Detail)
	case errors.Is(err, store.ErrNotConfigured):
		problem(w, 409, "not_configured", "the module has no store yet")
	case errors.Is(err, store.ErrNoSuchStore):
		problem(w, 404, "no_such_store", "no such store")
	case errors.As(err, &nokey):
		problem(w, 500, "encrypted_no_key", nokey.Error())
	case errors.Is(err, store.ErrDecrypt):
		problem(w, 500, "decrypt_failed", err.Error())
	case errors.Is(err, store.ErrNewerLayout):
		problem(w, 500, "newer_layout", "the store was written by a newer version of this program: update it (docs/compat.md)")
	case errors.Is(err, store.ErrUnreadable):
		problem(w, 500, "unreadable", "the settings of the store could not be read (the seal key is not the one they were sealed with)")
	default:
		problem(w, 502, "store_error", err.Error())
	}
}

func bearer(r *http.Request) string {
	a := r.Header.Get("Authorization")
	if len(a) > 7 && strings.EqualFold(a[:7], "bearer ") {
		return strings.TrimSpace(a[7:])
	}
	return ""
}

func validShard(s string) bool { return auth.ValidTenant(s) }

// tenantOf is the tenant a call is made for: the header X-Blobgate-Tenant, or X-Cinim-Shard (the name the first version used).
func tenantOf(r *http.Request) string {
	if t := r.Header.Get("X-Blobgate-Tenant"); t != "" {
		return t
	}
	return r.Header.Get("X-Cinim-Shard")
}

func equal(a, b string) bool { return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1 }

func newID() string {
	b := make([]byte, 12)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func validSHA(s string) bool {
	if len(s) != 64 {
		return false
	}
	for i := 0; i < len(s); i++ {
		if !(s[i] >= '0' && s[i] <= '9' || s[i] >= 'a' && s[i] <= 'f') {
			return false
		}
	}
	return true
}

// target is what the token signs: the decoded path, and the query as sent (a `?` with nothing after it is kept).
func target(r *http.Request) string {
	if i := strings.IndexByte(r.RequestURI, '?'); i >= 0 {
		return r.URL.Path + "?" + r.RequestURI[i+1:]
	}
	return r.URL.Path
}

func has(r *http.Request, name string) bool { _, ok := r.URL.Query()[name]; return ok }

type statusWriter struct {
	http.ResponseWriter
	code  int
	bytes int64
}

func (w *statusWriter) WriteHeader(c int) { w.code = c; w.ResponseWriter.WriteHeader(c) }
func (w *statusWriter) Write(b []byte) (int, error) {
	if w.code == 0 {
		w.code = 200
	}
	n, err := w.ResponseWriter.Write(b)
	w.bytes += int64(n)
	return n, err
}
func (w *statusWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// logged writes one structured line per request: method, store, key without the shard, status, duration, bytes. No token, no secret.
func (s *Server) logged(h http.Handler, door string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		sw := &statusWriter{ResponseWriter: w}
		h.ServeHTTP(sw, r)
		route := ""
		if door == "s3" {
			route = metrics.S3Route(r.Method, r.URL.Path, r.URL.Query())
		} else {
			route = metrics.Route(r.Method, r.URL.Path, r.URL.Query(), door == "shim")
		}
		metrics.Request(route, r.Method, sw.code, time.Since(start), max(r.ContentLength, 0), sw.bytes)
		if r.URL.Path == "/healthz" || r.URL.Path == "/readyz" || r.URL.Path == "/metrics" {
			return
		}
		s.log.Info("request", "method", r.Method, "path", logPath(r.URL.Path), "status", sw.code, "ms", time.Since(start).Milliseconds(), "bytes_out", sw.bytes, "bytes_in", max(r.ContentLength, 0))
	})
}

// logPath leaves out the shard of a key: it is the first segment after /objects/.
func logPath(p string) string {
	if i := strings.Index(p, "/objects/"); i >= 0 {
		rest := p[i+len("/objects/"):]
		if j := strings.IndexByte(rest, '/'); j >= 0 {
			rest = rest[j+1:]
		}
		return p[:i+len("/objects/")] + rest
	}
	return p
}

// Handler is the core's and the operator's port.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) { writeJSON(w, 200, map[string]string{"status": "ok"}) })
	mux.HandleFunc("/readyz", s.ready)
	mux.HandleFunc("/v1/openapi.yaml", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/yaml")
		_, _ = w.Write(openapiYAML)
	})
	mux.HandleFunc("/metrics", func(w http.ResponseWriter, r *http.Request) {
		if len(s.cfg.AdminToken) < 16 || !equal(bearer(r), s.cfg.AdminToken) {
			problem(w, 401, "unauthorized", "the admin token is wrong or none")
			return
		}
		metrics.Handler().ServeHTTP(w, r)
	})
	mux.HandleFunc("/v1/stores", s.listStores)
	mux.HandleFunc("/v1/stores/", s.coreObject)
	mux.HandleFunc("/v1/admin/quotas", s.adminQuotas)
	mux.HandleFunc("/v1/admin/quotas/", s.adminQuotas)
	mux.HandleFunc("/v1/admin/stores", s.admin)
	mux.HandleFunc("/v1/admin/stores/", s.admin)
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) { problem(w, 404, "not_found", "no such route") })
	return s.logged(mux, "core")
}

func (s *Server) ready(w http.ResponseWriter, r *http.Request) {
	if err := s.DB.Ready(r.Context()); err != nil {
		writeJSON(w, 503, map[string]any{"status": "not_ready", "checks": map[string]string{"database": err.Error()}})
		return
	}
	writeJSON(w, 200, map[string]any{"status": "ready", "checks": map[string]string{"database": "ok"}})
}

func (s *Server) loadStore(w http.ResponseWriter, ctx context.Context, name string) *store.Mirror {
	if name == defaultNam {
		name = ""
	}
	m, err := s.Reg.Load(ctx, name)
	if err != nil {
		fail(w, err)
		return nil
	}
	return m
}
