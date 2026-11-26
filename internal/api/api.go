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
	"io"
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
	ShardSecrets  map[string]string // shard -> the secret its calls are signed with
	AdminToken    string
	Reserve       int64         // bytes kept free in a store (0: space.DefaultReserve)
	MaxObject     int64         // the most bytes an object may hold; 0: no limit besides the room
	MaxReads      int           // reads of blocks at once (0: 4): each holds one block in memory
	MaxHeld       int           // blocks held in memory for clients at once, by reads being sent and by writes of more than one block or of no known size (0: 32)
	MaxHeldTenant int           // the most of those places one tenant may take; 0: three quarters of them when there is more than one tenant, else no limit of its own
	ClientIdle    time.Duration // the longest a client may send or take no byte before its call is ended (0: 120 s)
	UploadTTL     time.Duration // an upload older than this is swept (0: 24 h)
	Now           func() time.Time
	// CrashAt, if set, is called at the named points of a write or a delete (`put:after-uploads-row`, `put:after-block`, `part:after-block`, `complete:before-commit`, `commit:before`,
	// `commit:after-tx`, `delete:after-tx`, `delete:after-trash`, `abort:after-purge`). The tests make it end the request there, as a process that dies would; nothing sets it in the program.
	CrashAt func(point string)
}

// Server answers the calls.
type Server struct {
	cfg      Config
	DB       *rq.Client
	Reg      *store.Registry
	reads    chan struct{}
	seals    chan struct{}    // blocks being sealed: each holds one in memory
	held     chan struct{}    // blocks held in memory for a client (a read being sent, a write of many blocks)
	heldKind [2]chan struct{} // the same by kind (heldRead, heldWrite): each may take three quarters of the places
	// the places a tenant may take, when it has a limit of its own: made at start for the tenants of the settings, never written to after
	heldTenant map[string]chan struct{}
	log        *slog.Logger
	book       statusBook
}

// New makes a server.
func New(cfg Config, db *rq.Client, reg *store.Registry, log *slog.Logger) *Server {
	if cfg.Reserve == 0 {
		cfg.Reserve = space.DefaultReserve
	}
	if cfg.MaxReads <= 0 {
		cfg.MaxReads = 4
	}
	if cfg.MaxHeld <= 0 {
		cfg.MaxHeld = 32
	}
	if cfg.ClientIdle <= 0 {
		cfg.ClientIdle = 120 * time.Second
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
	kindMax := max(cfg.MaxHeld-cfg.MaxHeld/4, 1)
	// with more than one tenant, one of them cannot take every place: by default the last quarter is for the others
	perTenant := cfg.MaxHeldTenant
	if perTenant <= 0 && len(cfg.ShardSecrets) > 1 {
		perTenant = kindMax
	}
	heldTenant := map[string]chan struct{}{}
	if perTenant > 0 && perTenant < cfg.MaxHeld {
		for t := range cfg.ShardSecrets {
			heldTenant[t] = make(chan struct{}, perTenant)
		}
	}
	return &Server{cfg: cfg, DB: db, Reg: reg, reads: make(chan struct{}, cfg.MaxReads), seals: make(chan struct{}, cfg.MaxReads), held: make(chan struct{}, cfg.MaxHeld),
		heldKind: [2]chan struct{}{make(chan struct{}, kindMax), make(chan struct{}, kindMax)}, heldTenant: heldTenant, log: log}
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
func (s *Server) fail(w http.ResponseWriter, err error) {
	var un *store.ErrUnavailable
	var nokey *store.ErrNoKey
	switch {
	case errors.Is(err, errPreconditionFailed):
		problem(w, 412, "precondition_failed", errPreconditionFailed.Error())
	case errors.Is(err, errQuota):
		problem(w, 507, "quota_exceeded", errQuota.Error())
	case errors.Is(err, errUploadGone):
		problem(w, 404, "no_such_upload", errUploadGone.Error())
	case rq.IsDB(err):
		problem(w, 503, "database_unavailable", s.hide("the database did not answer", err))
	case errors.Is(err, store.ErrNotFound):
		problem(w, 404, "no_such_object", "no such object")
	case errors.As(err, &un):
		problem(w, 503, "unavailable", s.hide("not enough nodes of the store answered", err))
	case errors.Is(err, store.ErrNotConfigured):
		problem(w, 409, "not_configured", "the module has no store yet")
	case errors.Is(err, store.ErrNoSuchStore):
		problem(w, 404, "no_such_store", "no such store")
	case errors.As(err, &nokey):
		problem(w, 500, "encrypted_no_key", nokey.Error())
	case errors.Is(err, store.ErrDecrypt):
		problem(w, 500, "decrypt_failed", s.hide(store.ErrDecrypt.Error(), err))
	case errors.Is(err, store.ErrNewerLayout):
		problem(w, 500, "newer_layout", "the store was written by a newer version of this program: update it (docs/compat.md)")
	case errors.Is(err, store.ErrUnreadable):
		problem(w, 500, "unreadable", "the settings of the store could not be read (the seal key is not the one they were sealed with)")
	default:
		problem(w, 502, "store_error", s.hide("the store failed", err))
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
	rc    *http.ResponseController
	idle  time.Duration
}

// idleBody is the body of a request that ends the call when no byte comes for `idle`: a client that has gone quiet holds nothing for long. The deadline moves at most once a second.
type idleBody struct {
	io.ReadCloser
	rc   *http.ResponseController
	idle time.Duration
	last time.Time
}

func (b *idleBody) Read(p []byte) (int, error) {
	if now := time.Now(); now.Sub(b.last) > time.Second {
		_ = b.rc.SetReadDeadline(now.Add(b.idle)) // a writer that cannot do it (a test's recorder) has no deadline
		b.last = now
	}
	return b.ReadCloser.Read(p)
}

// writePiece is the most one write to a client is, so that its deadline is for the next bytes and not for a whole block.
const writePiece = 256 << 10

func (w *statusWriter) WriteHeader(c int) { w.code = c; w.ResponseWriter.WriteHeader(c) }
func (w *statusWriter) Write(b []byte) (int, error) {
	if w.code == 0 {
		w.code = 200
	}
	var done int
	for len(b) > 0 {
		if w.rc != nil {
			_ = w.rc.SetWriteDeadline(time.Now().Add(w.idle))
		}
		piece := b[:min(len(b), writePiece)]
		n, err := w.ResponseWriter.Write(piece)
		done += n
		w.bytes += int64(n)
		if err != nil {
			return done, err
		}
		b = b[len(piece):]
	}
	return done, nil
}

// Unwrap lets http.ResponseController reach the connection.
func (w *statusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }
func (w *statusWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// logged writes one structured line per request: method, store, key without the shard, status, duration, bytes. No token, no secret.
func (s *Server) logged(h http.Handler, door string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rc := http.NewResponseController(w)
		sw := &statusWriter{ResponseWriter: w, rc: rc, idle: s.cfg.ClientIdle}
		if r.Body != nil && r.Body != http.NoBody {
			r.Body = &idleBody{ReadCloser: r.Body, rc: rc, idle: s.cfg.ClientIdle}
		}
		defer func() { _ = rc.SetReadDeadline(time.Time{}); _ = rc.SetWriteDeadline(time.Time{}) }()
		h.ServeHTTP(sw, r)
		route := ""
		if door == "s3" {
			route = metrics.S3Route(r.Method, r.URL.Path, r.URL.Query())
		} else {
			route = metrics.Route(r.Method, r.URL.Path, r.URL.Query(), door == "shim")
		}
		metrics.Request(route, r.Method, sw.code, time.Since(start), max(r.ContentLength, 0), sw.bytes)
		if r.URL.Path == "/healthz" || r.URL.Path == "/readyz" || r.URL.Path == "/metrics" || isStatusPath(r.URL.Path) {
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
	mux.HandleFunc("/status", s.statusPage)
	mux.HandleFunc("/status.js", s.statusPage)
	mux.HandleFunc("/status.css", s.statusPage)
	mux.HandleFunc("/status.json", s.statusPublic)
	mux.HandleFunc("/v1/admin/status", s.statusAdmin)
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
		s.log.Warn("not ready", "error", redact(err.Error()))
		writeJSON(w, 503, map[string]any{"status": "not_ready", "checks": map[string]string{"database": "unreachable"}})
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
		s.fail(w, err)
		return nil
	}
	return m
}
