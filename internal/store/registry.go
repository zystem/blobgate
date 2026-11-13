package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/zystem/blobgate/internal/rq"
	"github.com/zystem/blobgate/internal/seal"
)

// Errors of Load, which the API turns into answers.
var (
	ErrNotConfigured = errors.New("not_configured") // the module has no store yet
	ErrNoSuchStore   = errors.New("no_such_store")
	ErrUnreadable    = errors.New("unreadable")   // the seal key is not the one the settings were sealed with
	ErrNewerLayout   = errors.New("newer_layout") // the store was written by a newer program
)

// NodeWrites is how many writes one node takes at the same time.
const NodeWrites = 8

// Entry is a store as the admin API lists it.
type Entry struct {
	Name    string
	Cfg     Config
	Default bool
}

// Registry reads and writes the stores in rqlite and keeps the opened nodes of each (a node of FTP or SFTP costs a connection to open).
type Registry struct {
	DB  *rq.Client
	Key seal.Keys

	// OpenHook, if set, is called with every store that Load opens (the tests wrap the nodes of a store with it to make them fail in ways a directory cannot)
	OpenHook func(*Mirror)

	mu    sync.Mutex
	cache map[string]cached
	opens map[string]*sync.Mutex
	rows  map[string]rowCache // the settings of a store as last read, for RowTTL
	def   defCache
}

// RowTTL is how long the settings of a store (and the name of the default one) are kept before they are read again. A change made through this replica shows at once; one made through another
// replica shows within this time. A request costs no query of the database for them in between.
const RowTTL = 2 * time.Second

type rowCache struct {
	config, secret string
	updated        int64
	at             time.Time
}

type defCache struct {
	name string
	at   time.Time
	ok   bool
}

// Refresh drops what is kept of the settings, so that the next request reads them (the tests, which change the database behind the registry's back).
func (r *Registry) Refresh() { r.forget() }

func (r *Registry) forget() {
	r.mu.Lock()
	r.rows = nil
	r.def = defCache{}
	r.mu.Unlock()
}

func (r *Registry) openLock(name string) *sync.Mutex {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.opens == nil {
		r.opens = map[string]*sync.Mutex{}
	}
	if r.opens[name] == nil {
		r.opens[name] = &sync.Mutex{}
	}
	return r.opens[name]
}

type cached struct {
	updated int64
	m       *Mirror
}

// NewRegistry makes a registry over the database with the key the secrets are sealed under.
func NewRegistry(db *rq.Client, key seal.Keys) *Registry {
	return &Registry{DB: db, Key: key, cache: map[string]cached{}}
}

// List returns the stores by name.
func (r *Registry) List(ctx context.Context) ([]Entry, error) {
	rows, err := r.DB.Query(ctx, rq.S("SELECT name, config, is_default FROM storage_stores ORDER BY name"))
	if err != nil {
		return nil, err
	}
	var out []Entry
	for _, row := range rows {
		var c Config
		_ = json.Unmarshal([]byte(rq.Str(row[1])), &c)
		out = append(out, Entry{Name: rq.Str(row[0]), Cfg: c, Default: rq.Bool(row[2])})
	}
	return out, nil
}

// DefaultName is the default store, or "".
func (r *Registry) DefaultName(ctx context.Context) (string, error) {
	rows, err := r.DB.Query(ctx, rq.S("SELECT name FROM storage_stores WHERE is_default = 1"))
	if err != nil || len(rows) == 0 {
		return "", err
	}
	return rq.Str(rows[0][0]), nil
}

// SetDefault makes a store the default; false if there is no such store.
func (r *Registry) SetDefault(ctx context.Context, name string) (bool, error) {
	rows, err := r.DB.Query(ctx, rq.S("SELECT count(*) FROM storage_stores WHERE name = ?", name))
	if err != nil || len(rows) == 0 || rq.Int(rows[0][0]) == 0 {
		return false, err
	}
	_, err = r.DB.Exec(ctx, false, rq.S("UPDATE storage_stores SET is_default = CASE WHEN name = ? THEN 1 ELSE 0 END", name))
	r.forget()
	return err == nil, err
}

// ObjectCount is the number of objects the inventory holds for the store.
func (r *Registry) ObjectCount(ctx context.Context, store string) (int64, error) {
	rows, err := r.DB.Query(ctx, rq.S("SELECT count(*) FROM storage_objects WHERE store = ?", store))
	if err != nil || len(rows) == 0 {
		return 0, err
	}
	return rq.Int(rows[0][0]), nil
}

// Save keeps (or replaces) a store; the first one is the default, and so is one that asks for it. It returns "" or why not.
func (r *Registry) Save(ctx context.Context, name string, cfg Config, sec Secrets, makeDefault bool, now int64) (string, error) {
	if !ValidName(name) {
		return "a store's name is lowercase letters, digits and -, starting with a letter, at most 32", nil
	}
	if why := cfg.Check(sec); why != "" {
		return why, nil
	}
	cfg.Layout = CurrentLayout
	cj, _ := json.Marshal(cfg)
	sj, _ := json.Marshal(sec)
	sealed, err := seal.Seal(r.Key, name, string(sj))
	if err != nil {
		return "", err
	}
	if _, err := r.DB.Exec(ctx, false, rq.S("INSERT INTO storage_stores (name, kind, config, secret, is_default, created_at, updated_at) VALUES (?, ?, ?, ?, 0, ?, ?) "+
		"ON CONFLICT(name) DO UPDATE SET kind = excluded.kind, config = excluded.config, secret = excluded.secret, updated_at = max(excluded.updated_at, storage_stores.updated_at + 1)", // it must change at every save: the opened nodes are kept by it
		name, cfg.Kind, string(cj), sealed, now, now)); err != nil {
		return "", err
	}
	r.forget()
	// "no default" rather than "no store" is the test, so that a registration cut off between the row and the mark is put right by the next one
	if d, err := r.DefaultName(ctx); makeDefault || (err == nil && d == "") {
		_, err = r.SetDefault(ctx, name)
		return "", err
	}
	return "", nil
}

// Forget removes a store. Refused while the inventory holds objects of it (force goes on: the objects stay where they are), and for the default one while there is another.
func (r *Registry) Forget(ctx context.Context, name string, force bool) (string, error) {
	rows, err := r.DB.Query(ctx, rq.S("SELECT is_default FROM storage_stores WHERE name = ?", name))
	if err != nil {
		return "", err
	}
	if len(rows) == 0 {
		return "no such store", nil
	}
	held, err := r.ObjectCount(ctx, name)
	if err != nil {
		return "", err
	}
	if held > 0 && !force {
		return fmt.Sprintf("the store holds %d object(s); forget it with force=1 to leave them where they are", held), nil
	}
	if rq.Bool(rows[0][0]) {
		o, err := r.DB.Query(ctx, rq.S("SELECT count(*) FROM storage_stores WHERE name != ?", name))
		if err != nil {
			return "", err
		}
		if len(o) > 0 && rq.Int(o[0][0]) > 0 {
			return "this is the default store: make another one the default first", nil
		}
	}
	_, err = r.DB.Exec(ctx, true, rq.S("DELETE FROM storage_stores WHERE name = ?", name), rq.S("DELETE FROM storage_objects WHERE store = ?", name),
		rq.S("DELETE FROM storage_repair WHERE store = ?", name), rq.S("DELETE FROM storage_parts WHERE store = ?", name), rq.S("DELETE FROM storage_uploads WHERE store = ?", name))
	r.mu.Lock()
	delete(r.cache, name)
	r.mu.Unlock()
	r.forget()
	return "", err
}

// Load returns the store `name` ("" is the default one), its nodes opened. The nodes are kept until the settings change.
func (r *Registry) Load(ctx context.Context, name string) (*Mirror, error) {
	want := name
	if want == "" {
		d, err := r.defaultCached(ctx)
		if err != nil {
			return nil, err
		}
		if d == "" {
			return nil, ErrNotConfigured
		}
		want = d
	}
	row, err := r.rowCached(ctx, want)
	if err != nil {
		return nil, err
	}
	if row == nil {
		return nil, ErrNoSuchStore
	}
	rows := [][]any{{row.config, row.secret, float64(row.updated)}}
	updated := rq.Int(rows[0][2])
	r.mu.Lock()
	c, ok := r.cache[want]
	r.mu.Unlock()
	if ok && c.updated == updated {
		return c.m, nil
	}
	// one request opens the nodes and the others wait for it: twenty at once would make twenty connections to a server that takes a few handshakes at a time
	lock := r.openLock(want)
	lock.Lock()
	defer lock.Unlock()
	r.mu.Lock()
	c, ok = r.cache[want]
	r.mu.Unlock()
	if ok && c.updated == updated {
		return c.m, nil
	}
	plain, err := seal.Open(r.Key, want, rq.Str(rows[0][1]))
	if err != nil {
		return nil, ErrUnreadable
	}
	var cfg Config
	var sec Secrets
	if json.Unmarshal([]byte(rq.Str(rows[0][0])), &cfg) != nil || json.Unmarshal([]byte(plain), &sec) != nil {
		return nil, ErrUnreadable
	}
	if cfg.Layout > CurrentLayout {
		return nil, ErrNewerLayout
	}
	m, err := Open(ctx, want, cfg, sec)
	if err != nil {
		return nil, err
	}
	if r.OpenHook != nil {
		r.OpenHook(m)
	}
	r.mu.Lock()
	r.cache[want] = cached{updated: updated, m: m}
	r.mu.Unlock()
	return m, nil
}

// Open makes the Mirror of settings (the admin call tries them before it keeps them).
func Open(ctx context.Context, name string, cfg Config, sec Secrets) (*Mirror, error) {
	specs := cfg.Specs(sec)
	m := &Mirror{Name: name, MinAcks: cfg.MinAcks, SpaceURL: cfg.SpaceURL, SoftDelete: time.Duration(cfg.SoftDeleteSeconds) * time.Second}
	if cfg.Encrypt != nil || len(sec.EncKeys) > 0 {
		var cur uint32
		if cfg.Encrypt != nil {
			cur = cfg.Encrypt.KeyID
		}
		c, err := NewCipher(cur, sec.EncKeys)
		if err != nil {
			return nil, err
		}
		m.Enc = c
	}
	if m.MinAcks <= 0 || m.MinAcks > len(specs) {
		m.MinAcks = len(specs)
	}
	for _, s := range specs {
		f, err := s.Open(ctx)
		if err != nil {
			return nil, fmt.Errorf("node %s: %w", s.ID(), err)
		}
		m.Nodes = append(m.Nodes, NewNode(s, f))
	}
	return m, nil
}

func (r *Registry) defaultCached(ctx context.Context) (string, error) {
	r.mu.Lock()
	d := r.def
	r.mu.Unlock()
	if d.ok && time.Since(d.at) < RowTTL {
		return d.name, nil
	}
	rows, err := r.DB.QueryWeak(ctx, rq.S("SELECT name FROM storage_stores WHERE is_default = 1"))
	if err != nil {
		return "", err
	}
	name := ""
	if len(rows) > 0 {
		name = rq.Str(rows[0][0])
	}
	r.mu.Lock()
	r.def = defCache{name: name, at: time.Now(), ok: true}
	r.mu.Unlock()
	return name, nil
}

// rowCached is the settings of a store, or nil when there is none.
func (r *Registry) rowCached(ctx context.Context, name string) (*rowCache, error) {
	r.mu.Lock()
	c, ok := r.rows[name]
	r.mu.Unlock()
	if ok && time.Since(c.at) < RowTTL {
		return &c, nil
	}
	rows, err := r.DB.QueryWeak(ctx, rq.S("SELECT config, secret, updated_at FROM storage_stores WHERE name = ?", name))
	if err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		r.mu.Lock()
		delete(r.rows, name)
		r.mu.Unlock()
		return nil, nil
	}
	c = rowCache{config: rq.Str(rows[0][0]), secret: rq.Str(rows[0][1]), updated: rq.Int(rows[0][2]), at: time.Now()}
	r.mu.Lock()
	if r.rows == nil {
		r.rows = map[string]rowCache{}
	}
	r.rows[name] = c
	r.mu.Unlock()
	return &c, nil
}
