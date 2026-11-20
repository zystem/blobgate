package api

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/zystem/blobgate/internal/rq"
	"github.com/zystem/blobgate/internal/seal"
	"github.com/zystem/blobgate/internal/store"
)

// replica is a second process of the module on the same database and the same nodes: its own registry, its own memory of the probes, its own listener.
func (r *rig) replica() *rig {
	r.t.Helper()
	s := New(Config{ShardSecrets: map[string]string{"001": secret1, "002": "the-secret-of-shard-002-go-test"}, AdminToken: adminT}, r.dbc,
		store.NewRegistry(r.dbc, seal.Key("a-seal-key-for-the-go-test")), slog.New(slog.NewTextHandler(io.Discard, nil)))
	o := &rig{t: r.t, s: s, dbc: rq.New(os.Getenv("BLOBGATE_RQLITE_URL")), nodes: r.nodes, ts: httptest.NewServer(s.Handler()), shim: httptest.NewServer(s.ShimHandler())}
	r.t.Cleanup(func() { o.ts.Close(); o.shim.Close() })
	return o
}

func (r *rig) view(path, token string) statusView {
	r.t.Helper()
	h := map[string]string{}
	if token != "" {
		h["Authorization"] = "Bearer " + token
	}
	code, d, _ := r.do("GET", path, h, nil)
	var v statusView
	if code != 200 || json.Unmarshal(d, &v) != nil {
		r.t.Fatalf("%s: %d %s", path, code, d)
	}
	return v
}

// Two replicas share the database and the nodes. What one writes the other reads, a store made through one is seen by the other, a node that one replica found dead is seen
// dead by the other after its own probes (each keeps its own view of the nodes), and the repair queue, which is in the database, is the same for both.
func TestTwoReplicasAndStatus(t *testing.T) {
	a := newRig(t, nil)
	b := a.replica()
	ctx := context.Background()
	if code, d := a.admin("PUT", "/v1/admin/stores/main", map[string]any{"kind": "mirror", "default": true, "min_acks": 1,
		"nodes": []any{map[string]any{"remote": ":local:" + a.nodes[0], "label": "rack-a"}, map[string]any{"remote": ":local:" + a.nodes[1], "label": "rack-b"}}}); code != 200 {
		t.Fatalf("register on a: %d %s", code, d)
	}

	// the store made through A is there for B at once; the bytes written through one are read through the other
	body := pattern(300 << 10)
	if code, d := a.put("one", body); code != 200 {
		t.Fatalf("put on a: %d %s", code, d)
	}
	if code, d := b.get("one", 0, len(body)-1); code != 206 && code != 200 || string(d) != string(body) {
		t.Fatalf("get on b: %d (%d bytes)", code, len(d))
	}
	if code, d := b.put("two", body); code != 200 {
		t.Fatalf("put on b: %d %s", code, d)
	}
	if code, d := a.get("two", 0, len(body)-1); code != 206 && code != 200 || string(d) != string(body) {
		t.Fatalf("get on a: %d (%d bytes)", code, len(d))
	}

	// before any probe both say "unknown"; after the probes of each, "green"
	for n, x := range map[string]*rig{"a": a, "b": b} {
		if v := x.view("/status.json", ""); v.Status != "unknown" {
			t.Fatalf("%s before its probe: %s", n, v.Status)
		}
		x.s.ProbeOnce(ctx)
		if v := x.view("/status.json", ""); v.Status != "green" {
			t.Fatalf("%s after its probe: %s", n, v.Status)
		}
	}

	// node 2 breaks. A has probed it, B has not yet: A says it is dead after three probes, B still says ok (the page is the view of one replica), then B agrees
	if err := os.RemoveAll(a.nodes[1]); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(a.nodes[1], []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	for range 3 {
		a.s.ProbeOnce(ctx)
	}
	if v := a.view("/status.json", ""); v.Stores[0].Nodes[1].State != "dead" || v.Status != "yellow" {
		t.Fatalf("a: %+v", v)
	}
	if v := b.view("/status.json", ""); v.Stores[0].Nodes[1].State != "ok" {
		t.Fatalf("b has not probed yet: %+v", v)
	}
	for range 3 {
		b.s.ProbeOnce(ctx)
	}
	if v := b.view("/status.json", ""); v.Stores[0].Nodes[1].State != "dead" || v.Status != "yellow" {
		t.Fatalf("b after its probes: %+v", v)
	}

	// a write through B while the node is away is accepted (min_acks 1) and queued in the database; A sees the queue of the node as B made it
	if code, d := b.put("three", body); code != 200 {
		t.Fatalf("put on b with a node away: %d %s", code, d)
	}
	for n, x := range map[string]*rig{"a": a, "b": b} {
		x.s.book.mu.Lock()
		x.s.book.queue = nil
		x.s.book.mu.Unlock()
		v := x.view("/v1/admin/status", adminT)
		if got := v.Stores[0].Nodes[1].Pending; got < 1 {
			t.Errorf("%s does not see the queue of the node: %+v", n, v.Stores[0].Nodes[1])
		}
	}
	if code, d := a.get("three", 0, len(body)-1); code != 206 && code != 200 || string(d) != string(body) {
		t.Fatalf("a reads what b wrote with a node away: %d", code)
	}

	// a change of the store through B (the label of a node) is in the page of A as soon as A loads the store again
	if code, d := b.admin("PUT", "/v1/admin/stores/main", map[string]any{"kind": "mirror", "default": true, "min_acks": 1,
		"nodes": []any{map[string]any{"remote": ":local:" + a.nodes[0], "label": "rack-z"}, map[string]any{"remote": ":local:" + a.nodes[1], "label": "rack-b"}}}); code != 200 {
		t.Fatalf("change on b: %d %s", code, d)
	}
	a.s.ProbeOnce(ctx)
	deadline := time.Now().Add(15 * time.Second)
	for {
		if v := a.view("/status.json", ""); v.Stores[0].Nodes[0].Name == "rack-z" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("a never saw the new label: %+v", a.view("/status.json", ""))
		}
		time.Sleep(500 * time.Millisecond)
		a.s.ProbeOnce(ctx)
	}
}
