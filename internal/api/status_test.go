package api

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/zystem/blobgate/internal/rq"
	"github.com/zystem/blobgate/internal/store"
)

// storeProbe is what the test of a dead node reads from the status.
type storeProbe struct{ node, store, overall string }

func TestNodeAndStoreStates(t *testing.T) {
	now := time.Unix(1_000_000, 0)
	const reserve = 256 << 20
	long := queueInfo{n: 3, oldest: now.Unix() - 600}
	short := queueInfo{n: 3, oldest: now.Unix() - 30}
	for name, c := range map[string]struct {
		p    probe
		q    queueInfo
		want string
	}{
		"not asked":            {probe{}, queueInfo{}, "unknown"},
		"answers":              {probe{seen: true, lat: time.Millisecond}, queueInfo{}, "ok"},
		"two failures are not": {probe{seen: true, fails: 2}, queueInfo{}, "ok"},
		"three failures are":   {probe{seen: true, fails: 3}, queueInfo{}, "dead"},
		"dead before lagging":  {probe{seen: true, fails: 5}, long, "dead"},
		"no room":              {probe{seen: true, spaceKnown: true, free: reserve - 1}, queueInfo{}, "full"},
		"room":                 {probe{seen: true, spaceKnown: true, free: reserve}, queueInfo{}, "ok"},
		"old copy waits":       {probe{seen: true}, long, "lagging"},
		"a new copy waits":     {probe{seen: true}, short, "ok"},
		"slow answer":          {probe{seen: true, lat: 3 * time.Second}, queueInfo{}, "slow"},
		"lagging before slow":  {probe{seen: true, lat: 3 * time.Second}, long, "lagging"},
		"full before lagging":  {probe{seen: true, spaceKnown: true, free: 0}, long, "full"},
	} {
		if got := nodeState(c.p, c.q, now, reserve); got != c.want {
			t.Errorf("%s: %s, want %s", name, got, c.want)
		}
	}
	for name, c := range map[string]struct {
		states []string
		acks   int
		want   string
	}{
		"all ok":                 {[]string{"ok", "ok"}, 2, "green"},
		"one lags, a write fits": {[]string{"ok", "lagging"}, 2, "yellow"},
		"one dead, one ack":      {[]string{"ok", "dead"}, 1, "yellow"},
		"one dead, all acks":     {[]string{"ok", "dead"}, 2, "red"},
		"all dead":               {[]string{"dead", "dead"}, 1, "red"},
		"full does not take":     {[]string{"ok", "full"}, 2, "red"},
		"none asked":             {[]string{"unknown", "unknown"}, 2, "unknown"},
		"one asked":              {[]string{"ok", "unknown"}, 1, "yellow"},
	} {
		if got := storeState(c.states, c.acks); got != c.want {
			t.Errorf("%s: %s, want %s", name, got, c.want)
		}
	}
}

func TestStatusPage(t *testing.T) {
	r := newRig(t, nil)
	if code, d := r.admin("PUT", "/v1/admin/stores/main", map[string]any{"kind": "mirror", "default": true, "min_acks": 1,
		"nodes": []any{map[string]any{"remote": ":local:" + r.nodes[0], "label": "rack-a"}, map[string]any{"remote": ":local:" + r.nodes[1]}}}); code != 200 {
		t.Fatalf("register: %d %s", code, d)
	}
	if code, _ := r.admin("PUT", "/v1/admin/stores/bad", map[string]any{"kind": "mirror", "nodes": []any{map[string]any{"remote": ":local:" + r.nodes[0], "label": "rack a<script>"}}}); code != 400 {
		t.Fatalf("a label with a space and a tag: %d", code)
	}
	ctx := context.Background()
	status := func(path, token string) (int, statusView, string) {
		t.Helper()
		h := map[string]string{}
		if token != "" {
			h["Authorization"] = "Bearer " + token
		}
		code, d, hdr := r.do("GET", path, h, nil)
		var v statusView
		_ = json.Unmarshal(d, &v)
		if code == 200 && !strings.Contains(hdr.Get("Cache-Control"), "no-store") {
			t.Errorf("%s may be kept: %q", path, hdr.Get("Cache-Control"))
		}
		return code, v, string(d)
	}

	// before the first probe nothing is known
	if code, v, _ := status("/status.json", ""); code != 200 || v.Status != "unknown" {
		t.Fatalf("before the probe: %d %s", code, v.Status)
	}
	r.s.ProbeOnce(ctx)
	code, v, body := status("/status.json", "")
	if code != 200 || v.Status != "green" || len(v.Stores) != 1 || len(v.Stores[0].Nodes) != 2 {
		t.Fatalf("both nodes answer: %d %s %s", code, v.Status, body)
	}
	if n := v.Stores[0].Nodes; n[0].Name != "rack-a" || n[1].Name != "local-2" || n[0].State != "ok" {
		t.Fatalf("the names of the nodes: %+v", n)
	}
	// the public view has nothing that tells where a node is or what the store is called
	for _, leak := range []string{r.nodes[0], r.nodes[1], "main", "local-", "\"id\"", "error", "free"} {
		if strings.Contains(strings.Replace(body, "local-2", "", 1), leak) {
			t.Errorf("the public view has %q: %s", leak, body)
		}
	}
	if v.Stores[0].Name != "store 1" {
		t.Errorf("name of the store: %s", v.Stores[0].Name)
	}

	// the details want the admin token
	if code, _, _ := status("/v1/admin/status", ""); code != 401 {
		t.Fatalf("details with no token: %d", code)
	}
	if code, _, _ := status("/v1/admin/status", "a-token-that-is-wrong-but-long"); code != 401 {
		t.Fatalf("details with a wrong token: %d", code)
	}
	code, v, _ = status("/v1/admin/status", adminT)
	if code != 200 || v.Stores[0].Name != "main" || v.Stores[0].Nodes[0].ID == "" || v.Stores[0].Nodes[0].Backend != "local" || v.Stores[0].Need != 1 {
		t.Fatalf("details: %d %+v", code, v)
	}

	// a node that does not answer (its directory is replaced by a file, which cannot be listed) is dead after three probes; its store is red when a write needs it
	one := t.TempDir() + "/gone"
	if code, d := r.admin("PUT", "/v1/admin/stores/zdead", map[string]any{"kind": "mirror",
		"nodes": []any{map[string]any{"remote": ":local:" + one, "label": "disk-gone"}}}); code != 200 {
		t.Fatalf("register: %d %s", code, d)
	}
	r.s.ProbeOnce(ctx)
	if _, v, _ = status("/status.json", ""); v.Stores[1].Nodes[0].State != "ok" || v.Stores[1].State != "green" {
		t.Fatalf("before it breaks: %+v", v.Stores[1])
	}
	if err := os.RemoveAll(one); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(one, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	dead := func() storeProbe {
		_, v, _ := status("/status.json", "")
		return storeProbe{v.Stores[1].Nodes[0].State, v.Stores[1].State, v.Status}
	}
	r.s.ProbeOnce(ctx)
	r.s.ProbeOnce(ctx)
	if got := dead(); got.node != "ok" {
		t.Fatalf("after two failed probes: %+v", got)
	}
	r.s.ProbeOnce(ctx)
	if got := dead(); got.node != "dead" || got.store != "red" || got.overall != "red" {
		t.Fatalf("after three: %+v", got)
	}
	if _, d, _ := status("/v1/admin/status", adminT); d.Stores[1].Nodes[0].Error == "" || d.Stores[1].Nodes[0].Name != "disk-gone" {
		t.Fatalf("the details do not say why: %+v", d.Stores[1].Nodes)
	}
	if code, d := r.admin("DELETE", "/v1/admin/stores/zdead?force=1", nil); code != 200 {
		t.Fatalf("remove: %d %s", code, d)
	}
	r.s.ProbeOnce(ctx)

	// a copy that waits for a long time: the node lags (the queue is read at most every 5 s: the test drops what was kept)
	if _, err := r.dbc.Exec(ctx, true, rq.S("INSERT INTO storage_repair (store, key, node, op, since, attempts, next_at, last_error) VALUES ('main', 'k', ?, 'copy', ?, 0, 0, '')",
		r.s.nodeIDs("main")[1], time.Now().Add(-10*time.Minute).Unix())); err != nil {
		t.Fatal(err)
	}
	r.s.book.mu.Lock()
	r.s.book.queue = nil
	r.s.book.mu.Unlock()
	if _, v, _ = status("/status.json", ""); v.Stores[0].Nodes[1].State != "lagging" || v.Status != "yellow" {
		t.Fatalf("a copy waits: %+v", v.Stores[0].Nodes)
	}

	// the page and its files, with the headers that keep it from being framed or kept
	for path, want := range map[string]string{"/status": "text/html", "/status.js": "text/javascript", "/status.css": "text/css"} {
		code, d, hdr := r.do("GET", path, nil, nil)
		if code != 200 || !strings.HasPrefix(hdr.Get("Content-Type"), want) || len(d) == 0 {
			t.Errorf("%s: %d %s", path, code, hdr.Get("Content-Type"))
		}
		if !strings.Contains(hdr.Get("Content-Security-Policy"), "default-src 'none'") || hdr.Get("X-Frame-Options") != "DENY" || hdr.Get("X-Content-Type-Options") != "nosniff" {
			t.Errorf("%s: headers %v", path, hdr)
		}
	}
	if code, _, _ := r.do("POST", "/status.json", nil, nil); code != http.StatusMethodNotAllowed {
		t.Errorf("POST: %d", code)
	}
	if code, d, _ := r.do("GET", "/status.js", nil, nil); code != 200 || strings.Contains(string(d), "innerHTML") {
		t.Errorf("the script puts text in the page as markup")
	}
}

// nodeIDs is the ids of the nodes of a store, in order.
func (s *Server) nodeIDs(name string) []string {
	list, _ := s.Reg.List(context.Background())
	for _, e := range list {
		if e.Name == name {
			var ids []string
			for _, sp := range e.Cfg.Specs(store.Secrets{}) {
				ids = append(ids, sp.ID())
			}
			return ids
		}
	}
	return nil
}
