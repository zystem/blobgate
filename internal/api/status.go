package api

import (
	"context"
	"embed"
	"errors"
	"io"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/rclone/rclone/fs"

	"github.com/zystem/blobgate/internal/rq"
	"github.com/zystem/blobgate/internal/space"
	"github.com/zystem/blobgate/internal/store"
)

//go:embed statuspage/index.html statuspage/status.js statuspage/status.css
var statusFiles embed.FS

// What decides the state of a node (docs/status-page.md).
const (
	probeFails   = 3               // probes in a row that failed: the node is dead
	lagAfter     = 5 * time.Minute // a copy that has waited this long in the repair queue: the node lags
	slowAfter    = 2 * time.Second // an answer of the probe that takes this long: the node is slow
	probeTimeout = 3 * time.Second // a probe that takes longer has failed
	queueTTL     = 5 * time.Second // the queue is read from the database at most this often
	maxErrText   = 300             // what is kept of the error of a node
)

// probe is what is known of a node from the last probes.
type probe struct {
	seen       bool
	fails      int
	lat        time.Duration
	err        string
	at         time.Time
	spaceKnown bool
	free       int64
	total      int64
}

type queueInfo struct {
	n      int64
	oldest int64 // unix seconds when the oldest copy was queued
}

// statusBook is what the probes found, and the queue as the database said it a moment ago.
type statusBook struct {
	mu      sync.Mutex
	nodes   map[string]*probe // "store|node id"
	queue   map[string]queueInfo
	queueAt time.Time
}

// nodeState is the word for a node, from the first rule that holds: unknown (not asked yet), dead, full, lagging, slow, ok.
func nodeState(p probe, q queueInfo, now time.Time, reserve int64) string {
	switch {
	case !p.seen:
		return "unknown"
	case p.fails >= probeFails:
		return "dead"
	case p.spaceKnown && p.free < reserve:
		return "full"
	case q.n > 0 && now.Unix()-q.oldest >= int64(lagAfter/time.Second):
		return "lagging"
	case p.lat > slowAfter:
		return "slow"
	}
	return "ok"
}

// storeState is green when every node is ok, red when fewer nodes than a write needs can take one (dead, full and not yet asked do not), unknown when no node was asked yet, and yellow otherwise.
func storeState(states []string, minAcks int) string {
	writable, unknown := 0, 0
	allOK := true
	for _, s := range states {
		switch s {
		case "unknown":
			unknown++
			allOK = false
		case "dead", "full":
			allOK = false
		default:
			writable++
			if s != "ok" {
				allOK = false
			}
		}
	}
	switch {
	case len(states) > 0 && unknown == len(states):
		return "unknown"
	case writable < minAcks:
		return "red"
	case allOK:
		return "green"
	}
	return "yellow"
}

func worse(a, b string) bool {
	rank := map[string]int{"green": 0, "unknown": 1, "yellow": 2, "red": 3}
	return rank[b] > rank[a]
}

// ProbeOnce asks every node of every store whether it answers (a listing of its root, with a timeout) and how much room it says it has, at the same time, and keeps the answers.
func (s *Server) ProbeOnce(ctx context.Context) {
	list, err := s.Reg.List(ctx)
	if err != nil {
		return
	}
	s.book.mu.Lock()
	old := s.book.nodes
	s.book.mu.Unlock()
	next := map[string]*probe{}
	var mu sync.Mutex
	var wg sync.WaitGroup
	for _, e := range list {
		m, err := s.Reg.Load(ctx, e.Name)
		if err != nil {
			continue
		}
		for _, n := range m.Nodes {
			key := e.Name + "|" + n.ID
			wg.Add(1)
			go func(n *store.Node, prev *probe) {
				defer wg.Done()
				p := probeNode(ctx, n, prev)
				mu.Lock()
				next[key] = p
				mu.Unlock()
			}(n, old[key])
		}
	}
	wg.Wait()
	s.book.mu.Lock()
	s.book.nodes = next
	s.book.mu.Unlock()
}

func probeNode(ctx context.Context, n *store.Node, prev *probe) *probe {
	cctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	start := time.Now()
	_, err := n.F.List(cctx, "")
	if err == nil {
		err = localRootReadable(n.F) // the local backend of rclone logs an unreadable directory and lists it as empty
	}
	p := &probe{seen: true, lat: time.Since(start), at: start}
	if err != nil && !errors.Is(err, fs.ErrorDirNotFound) { // a root that is not made yet is a node that answers
		p.fails = 1
		if prev != nil {
			p.fails = prev.fails + 1
		}
		p.err = err.Error()
		if len(p.err) > maxErrText {
			p.err = p.err[:maxErrText]
		}
		if prev != nil { // what was known of the room stays until the node answers again
			p.spaceKnown, p.free, p.total = prev.spaceKnown, prev.free, prev.total
		}
		return p
	}
	if ab := n.F.Features().About; ab != nil {
		if u, err := ab(cctx); err == nil && u != nil && u.Free != nil && u.Total != nil {
			p.spaceKnown, p.free, p.total = true, *u.Free, *u.Total
		}
	}
	return p
}

// localRootReadable says whether the directory of a node of the local backend can be read. rclone's local backend answers an empty listing for a directory it cannot read (mode 000, a disk that has
// gone read-only is another case), so List alone calls the node well. A root that does not exist yet is fine (it is made by the first write). Other backends: nil, List is trusted.
func localRootReadable(f fs.Fs) error {
	if n := f.Name(); n != "local" && n != ":local" { // a connection string ':local:' names it ":local", a configured remote "local"
		return nil
	}
	d, err := os.Open(f.Root())
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer d.Close()
	if _, err := d.Readdirnames(1); err != nil && !errors.Is(err, io.EOF) {
		return err
	}
	return nil
}

// RunProbes probes every `every` until ctx ends; 0 or less: never (every node stays "unknown").
func (s *Server) RunProbes(ctx context.Context, every time.Duration) {
	if every <= 0 {
		return
	}
	s.ProbeOnce(ctx)
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.ProbeOnce(ctx)
		}
	}
}

// queued reads the repair queue by node, not more often than queueTTL.
func (s *Server) queued(ctx context.Context) map[string]queueInfo {
	s.book.mu.Lock()
	defer s.book.mu.Unlock()
	if s.book.queue != nil && time.Since(s.book.queueAt) < queueTTL {
		return s.book.queue
	}
	q := map[string]queueInfo{}
	if rows, err := s.DB.QueryWeak(ctx, rq.S("SELECT store, node, count(*), min(since) FROM storage_repair WHERE op = 'copy' GROUP BY store, node")); err == nil {
		for _, r := range rows {
			q[rq.Str(r[0])+"|"+rq.Str(r[1])] = queueInfo{n: rq.Int(r[2]), oldest: rq.Int(r[3])}
		}
	} else if s.book.queue != nil {
		return s.book.queue // the database did not answer: the last figures, not an empty queue
	}
	s.book.queue, s.book.queueAt = q, time.Now()
	return q
}

type nodeView struct {
	Name      string `json:"name"`
	State     string `json:"state"`
	ID        string `json:"id,omitempty"`
	Backend   string `json:"backend,omitempty"`
	LatencyMS int64  `json:"latency_ms,omitempty"`
	Error     string `json:"error,omitempty"`
	Pending   int64  `json:"pending,omitempty"`
	OldestAge int64  `json:"oldest_age_seconds,omitempty"`
	Free      *int64 `json:"free,omitempty"`
	Total     *int64 `json:"total,omitempty"`
	CheckedAt int64  `json:"checked_at,omitempty"`
}

type storeView struct {
	Name  string     `json:"name"`
	State string     `json:"state"`
	Need  int        `json:"nodes_needed_for_a_write,omitempty"`
	Nodes []nodeView `json:"nodes"`
}

type statusView struct {
	Status string      `json:"status"`
	Stores []storeView `json:"stores"`
}

// statusSnapshot is the state of every node. A public view has no store names (`store 1`...), no ids, no errors, no figures: only the name of a node (its label) and its state.
func (s *Server) statusSnapshot(ctx context.Context, detail bool) (statusView, error) {
	list, err := s.Reg.List(ctx)
	if err != nil {
		return statusView{}, err
	}
	sort.Slice(list, func(i, j int) bool { return list[i].Name < list[j].Name })
	q := s.queued(ctx)
	reserve := s.cfg.Reserve
	if reserve <= 0 {
		reserve = space.DefaultReserve
	}
	now := s.cfg.Now()
	s.book.mu.Lock()
	probes := s.book.nodes
	s.book.mu.Unlock()
	out := statusView{Status: "green", Stores: []storeView{}}
	for i, e := range list {
		specs := e.Cfg.Specs(store.Secrets{})
		sv := storeView{Name: e.Name, Nodes: []nodeView{}}
		if !detail {
			sv.Name = "store " + strconv.Itoa(i+1)
		}
		var states []string
		for pos, sp := range specs {
			id := sp.ID()
			var p probe
			if pp := probes[e.Name+"|"+id]; pp != nil {
				p = *pp
			}
			qi := q[e.Name+"|"+id]
			st := nodeState(p, qi, now, reserve)
			states = append(states, st)
			nv := nodeView{Name: sp.PublicName(pos), State: st}
			if detail {
				nv.ID, nv.Backend, nv.LatencyMS, nv.Error, nv.Pending = id, sp.Backend, p.lat.Milliseconds(), p.err, qi.n
				if qi.n > 0 {
					nv.OldestAge = max(now.Unix()-qi.oldest, 0)
				}
				if p.spaceKnown {
					f, t := p.free, p.total
					nv.Free, nv.Total = &f, &t
				}
				if p.seen {
					nv.CheckedAt = p.at.Unix()
				}
			}
			sv.Nodes = append(sv.Nodes, nv)
		}
		need := e.Cfg.MinAcks
		if need <= 0 || need > len(specs) {
			need = len(specs)
		}
		sv.State = storeState(states, need)
		if detail {
			sv.Need = need
		}
		if worse(out.Status, sv.State) {
			out.Status = sv.State
		}
		out.Stores = append(out.Stores, sv)
	}
	if len(out.Stores) == 0 {
		out.Status = "unknown"
	}
	return out, nil
}

// noStore is what a page that tells the state of the service sends so that nothing keeps it and nothing embeds it.
func noStore(w http.ResponseWriter) {
	h := w.Header()
	h.Set("Cache-Control", "no-store")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("X-Frame-Options", "DENY")
	h.Set("Referrer-Policy", "no-referrer")
	h.Set("Content-Security-Policy", "default-src 'none'; script-src 'self'; style-src 'self'; connect-src 'self'; base-uri 'none'; form-action 'none'; frame-ancestors 'none'")
}

// statusPage serves the page and its two files (`/status`, `/status.js`, `/status.css`): static, the same for everybody; what it shows it fetches.
func (s *Server) statusPage(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		problem(w, 405, "method_not_allowed", "GET")
		return
	}
	name, ctype := "statuspage/index.html", "text/html; charset=utf-8"
	switch r.URL.Path {
	case "/status.js":
		name, ctype = "statuspage/status.js", "text/javascript; charset=utf-8"
	case "/status.css":
		name, ctype = "statuspage/status.css", "text/css; charset=utf-8"
	}
	data, err := statusFiles.ReadFile(name)
	if err != nil {
		problem(w, 404, "not_found", "no such file")
		return
	}
	noStore(w)
	w.Header().Set("Content-Type", ctype)
	if r.Method == http.MethodGet {
		_, _ = w.Write(data)
	}
}

// statusPublic is `GET /status.json`: the state of every node and of every store, with no way to tell where a node is.
func (s *Server) statusPublic(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		problem(w, 405, "method_not_allowed", "GET")
		return
	}
	v, err := s.statusSnapshot(r.Context(), false)
	if err != nil {
		fail(w, err)
		return
	}
	noStore(w)
	writeJSON(w, 200, v)
}

// statusAdmin is `GET /v1/admin/status`: the same with the details, for the admin token.
func (s *Server) statusAdmin(w http.ResponseWriter, r *http.Request) {
	if len(s.cfg.AdminToken) < 16 || !equal(bearer(r), s.cfg.AdminToken) {
		problem(w, 401, "unauthorized", "the admin token is wrong or none")
		return
	}
	if r.Method != http.MethodGet {
		problem(w, 405, "method_not_allowed", "GET")
		return
	}
	v, err := s.statusSnapshot(r.Context(), true)
	if err != nil {
		fail(w, err)
		return
	}
	noStore(w)
	writeJSON(w, 200, v)
}

func isStatusPath(p string) bool {
	return p == "/status" || p == "/status.json" || p == "/status.js" || p == "/status.css" || strings.HasPrefix(p, "/status/")
}
