package api

import (
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/zystem/blobgate/internal/maint"
	"github.com/zystem/blobgate/internal/rq"
	"github.com/zystem/blobgate/internal/store"
)

type nodeBody struct {
	Remote  string            `json:"remote"` // a connection string, for example :ftp,host=h:/path (the options in it are not secret)
	Backend string            `json:"backend"`
	Options map[string]string `json:"options"`
	Path    string            `json:"path"`
	Secrets map[string]string `json:"secrets"` // options that are sealed in the database (a password): obscured for rclone when the node is opened
}

type storeBody struct {
	Kind              string     `json:"kind"`
	Endpoint          string     `json:"endpoint"`
	Region            string     `json:"region"`
	Bucket            string     `json:"bucket"`
	KeyID             string     `json:"access_key_id"`
	Secret            string     `json:"secret_access_key"`
	SpaceURL          string     `json:"space_url"`
	Nodes             []nodeBody `json:"nodes"`
	MinAcks           int        `json:"min_acks"`
	Default           bool       `json:"default"`
	SoftDeleteSeconds int        `json:"soft_delete_seconds"`
	Dedup             string     `json:"dedup"`
	// Encryption seals the blocks written from now on; keys keeps every key an object was ever sealed with (id -> text), key_id is the one new blocks use
	Encryption *struct {
		KeyID uint32            `json:"key_id"`
		Keys  map[string]string `json:"keys"`
	} `json:"encryption"`
}

func (b storeBody) parse() (store.Config, store.Secrets, error) {
	cfg := store.Config{Kind: b.Kind, MinAcks: b.MinAcks, SpaceURL: b.SpaceURL, SoftDeleteSeconds: b.SoftDeleteSeconds, Dedup: b.Dedup}
	var sec store.Secrets
	if cfg.Kind == "" {
		cfg.Kind = "s3"
	}
	if b.Encryption != nil {
		if b.Encryption.KeyID != 0 {
			cfg.Encrypt = &store.EncConfig{KeyID: b.Encryption.KeyID}
		}
		sec.EncKeys = b.Encryption.Keys
	}
	if cfg.Kind == "s3" {
		cfg.Endpoint, cfg.Region, cfg.Bucket, cfg.KeyID = b.Endpoint, b.Region, b.Bucket, b.KeyID
		if cfg.Region == "" {
			cfg.Region = "garage"
		}
		sec.S3Secret = b.Secret
		return cfg, sec, nil
	}
	for _, n := range b.Nodes {
		spec := store.NodeSpec{Backend: n.Backend, Options: n.Options, Path: n.Path}
		if n.Remote != "" {
			var err error
			if spec, err = store.ParseRemote(n.Remote); err != nil {
				return cfg, sec, err
			}
			for k, v := range n.Options {
				spec.Options[k] = v
			}
		}
		cfg.Nodes = append(cfg.Nodes, spec)
		sec.Nodes = append(sec.Nodes, n.Secrets)
	}
	return cfg, sec, nil
}

func (s *Server) admin(w http.ResponseWriter, r *http.Request) {
	if len(s.cfg.AdminToken) < 16 || !equal(bearer(r), s.cfg.AdminToken) {
		problem(w, 401, "unauthorized", "the admin token is wrong or none")
		return
	}
	ctx := r.Context()
	rest := strings.Trim(strings.TrimPrefix(r.URL.Path, "/v1/admin/stores"), "/")
	name, action := rest, ""
	if i := strings.IndexByte(rest, ':'); i >= 0 {
		name, action = rest[:i], rest[i:]
	}
	if name == "" {
		if r.Method != http.MethodGet || action != "" {
			problem(w, 405, "method_not_allowed", "GET")
			return
		}
		list, err := s.Reg.List(ctx)
		if err != nil {
			fail(w, err)
			return
		}
		items := []map[string]any{}
		for _, e := range list {
			n, _ := s.Reg.ObjectCount(ctx, e.Name)
			nodes := []string{}
			for _, sp := range e.Cfg.Specs(store.Secrets{}) {
				nodes = append(nodes, sp.ID())
			}
			acks := e.Cfg.MinAcks
			if acks <= 0 {
				acks = len(nodes)
			}
			pend := int64(0)
			if rows, err := s.DB.Query(ctx, rq.S("SELECT count(*) FROM storage_repair WHERE store = ?", e.Name)); err == nil && len(rows) == 1 {
				pend = rq.Int(rows[0][0])
			}
			var enc any
			if e.Cfg.Encrypt != nil {
				enc = map[string]any{"key_id": e.Cfg.Encrypt.KeyID}
			}
			items = append(items, map[string]any{"encryption": enc, "name": e.Name, "kind": e.Cfg.Kind, "default": e.Default, "endpoint": e.Cfg.Endpoint, "region": e.Cfg.Region,
				"bucket": e.Cfg.Bucket, "access_key_id": e.Cfg.KeyID, "nodes": nodes, "min_acks": acks, "soft_delete_seconds": e.Cfg.SoftDeleteSeconds, "dedup": e.Cfg.Dedup, "objects": n, "space_url": e.Cfg.SpaceURL, "repair_pending": pend})
		}
		writeJSON(w, 200, map[string]any{"stores": items})
		return
	}
	switch action {
	case ":usage":
		if r.Method != http.MethodGet {
			problem(w, 405, "method_not_allowed", "GET")
			return
		}
		s.adminUsage(w, r, name)
		return
	case ":check":
		if r.Method != http.MethodPost {
			problem(w, 405, "method_not_allowed", "POST")
			return
		}
		m, err := s.Reg.Load(ctx, name)
		if err != nil {
			if err == store.ErrNoSuchStore || err == store.ErrNotConfigured {
				problem(w, map[bool]int{true: 404, false: 409}[err == store.ErrNoSuchStore], err.Error(), "the store is not ready: "+err.Error())
				return
			}
			fail(w, err)
			return
		}
		if bad := m.Check(ctx); bad != "" {
			problem(w, 502, "storage_unreachable", bad)
			return
		}
		list, _ := s.Reg.List(ctx)
		kind := ""
		for _, e := range list {
			if e.Name == name {
				kind = e.Cfg.Kind
			}
		}
		writeJSON(w, 200, map[string]any{"ok": true, "name": name, "kind": kind})
		return
	case ":reconcile":
		if r.Method != http.MethodPost {
			problem(w, 405, "method_not_allowed", "POST")
			return
		}
		minAge := 24 * time.Hour
		if v := r.URL.Query().Get("min_age"); v != "" {
			n, err := strconv.Atoi(v)
			if err != nil || n < 0 {
				problem(w, 400, "invalid_request", "min_age is a number of seconds")
				return
			}
			minAge = time.Duration(n) * time.Second
		}
		rep, err := maint.New(maint.Config{Now: s.cfg.Now}, s.DB, s.Reg).Reconcile(ctx, name, r.URL.Query().Has("apply"), minAge)
		if err != nil {
			fail(w, err)
			return
		}
		writeJSON(w, 200, rep)
		return
	case ":default":
		if r.Method != http.MethodPost {
			problem(w, 405, "method_not_allowed", "POST")
			return
		}
		ok, err := s.Reg.SetDefault(ctx, name)
		if err != nil {
			fail(w, err)
			return
		}
		if !ok {
			problem(w, 404, "no_such_store", "no such store: "+name)
			return
		}
		writeJSON(w, 200, map[string]any{"name": name, "default": true})
		return
	case "":
	default:
		problem(w, 404, "not_found", "no such action")
		return
	}
	if r.Method == http.MethodDelete {
		why, err := s.Reg.Forget(ctx, name, r.URL.Query().Has("force"))
		if err != nil {
			fail(w, err)
			return
		}
		if why != "" {
			if why == "no such store" {
				problem(w, 404, "no_such_store", why)
			} else {
				problem(w, 409, "store_in_use", why)
			}
			return
		}
		writeJSON(w, 200, map[string]any{"deleted": true, "name": name})
		return
	}
	// POST does what PUT does, for the registration scripts of the chart, which have only busybox `wget` (it cannot PUT)
	if r.Method != http.MethodPut && r.Method != http.MethodPost {
		problem(w, 405, "method_not_allowed", "GET, PUT, DELETE, or POST :check / :default")
		return
	}
	data, err := io.ReadAll(io.LimitReader(r.Body, smallBody+1))
	var body storeBody
	if err != nil || len(data) > smallBody || json.Unmarshal(data, &body) != nil {
		problem(w, 400, "invalid_request", `the body is {kind: s3, endpoint, region, bucket, access_key_id, secret_access_key} or {kind: mirror, nodes: [{remote, secrets}], min_acks}`)
		return
	}
	if !store.ValidName(name) {
		problem(w, 400, "invalid_request", "a store's name is lowercase letters, digits and -, starting with a letter, at most 32")
		return
	}
	cfg, sec, perr := body.parse()
	if perr != nil {
		problem(w, 400, "invalid_request", perr.Error())
		return
	}
	if why := cfg.Check(sec); why != "" {
		problem(w, 400, "invalid_request", why)
		return
	}
	// the settings are tried before they are kept: a store that does not take a block is not one to write down
	m, perr := store.Open(ctx, name, cfg, sec)
	if perr != nil {
		problem(w, 400, "storage_unreachable", perr.Error())
		return
	}
	if bad := m.Check(ctx); bad != "" {
		problem(w, 400, "storage_unreachable", bad)
		return
	}
	why, err := s.Reg.Save(ctx, name, cfg, sec, body.Default, s.now())
	if err != nil {
		fail(w, err)
		return
	}
	if why != "" {
		problem(w, 400, "invalid_request", why)
		return
	}
	writeJSON(w, 200, map[string]any{"name": name, "checked": true})
}
