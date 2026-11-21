package api

import (
	"context"
	"os"
	"testing"
)

// The local backend of rclone logs a directory it cannot read and lists it as empty, so a probe that only lists called such a node well. The probe now reads the directory of a local node
// itself: a node whose directory cannot be read is dead after three probes, and ok again when it can be read.
func TestProbeFindsAnUnreadableLocalNode(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("root reads any directory")
	}
	r := newRig(t, nil)
	ctx := context.Background()
	if code, d := r.admin("PUT", "/v1/admin/stores/main", map[string]any{"kind": "mirror", "default": true, "min_acks": 1,
		"nodes": []any{map[string]any{"remote": ":local:" + r.nodes[0]}, map[string]any{"remote": ":local:" + r.nodes[1]}}}); code != 200 {
		t.Fatalf("register: %d %s", code, d)
	}
	for range 3 {
		r.s.ProbeOnce(ctx)
	}
	if v := r.view("/status.json", ""); v.Status != "green" {
		t.Fatalf("both nodes well: %+v", v)
	}
	if err := os.Chmod(r.nodes[1], 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(r.nodes[1], 0o755) })
	for range 3 {
		r.s.ProbeOnce(ctx)
	}
	if v := r.view("/status.json", ""); v.Stores[0].Nodes[1].State != "dead" || v.Stores[0].Nodes[0].State != "ok" || v.Status != "yellow" {
		t.Fatalf("an unreadable directory: %+v", v)
	}
	if err := os.Chmod(r.nodes[1], 0o755); err != nil {
		t.Fatal(err)
	}
	r.s.ProbeOnce(ctx)
	if v := r.view("/status.json", ""); v.Stores[0].Nodes[1].State != "ok" || v.Status != "green" {
		t.Fatalf("readable again: %+v", v)
	}
	// a root that is not made yet is a node that answers
	if err := os.RemoveAll(r.nodes[0]); err != nil {
		t.Fatal(err)
	}
	r.s.ProbeOnce(ctx)
	if v := r.view("/status.json", ""); v.Stores[0].Nodes[0].State != "ok" {
		t.Fatalf("a root not made yet: %+v", v)
	}
}
