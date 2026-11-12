package api

import (
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zystem/blobgate/internal/metrics"
)

var registerOnce sync.Once

func (r *rig) scrape() string {
	r.t.Helper()
	code, data, _ := r.do("GET", "/metrics", map[string]string{"Authorization": "Bearer " + adminT}, nil)
	if code != 200 {
		r.t.Fatalf("/metrics: %d %s", code, data)
	}
	return string(data)
}

// The module says what it does: requests by route and status, bytes, the failures of nodes, the refusals for room, the repair queue.
func TestMetrics(t *testing.T) {
	r := newRig(t, func(c *Config) { c.Reserve = 0 })
	registerOnce.Do(func() { metrics.RegisterDB(r.dbc, time.Millisecond) })
	if code, _, _ := r.do("GET", "/metrics", nil, nil); code != 401 {
		t.Fatal("metrics without the admin token")
	}
	r.mirror(1)
	r.readOnly(1)
	if code, _ := r.put("k/a", pattern(1000)); code != 200 {
		t.Fatal("put")
	}
	r.get("k/a", 0, 99)
	r.get("k/missing", 0, 9)
	out := r.scrape()
	for _, want := range []string{
		`blobgate_requests_total{method="PUT",route="object_put",status="2xx"}`,
		`blobgate_requests_total{method="GET",route="read",status="2xx"}`,
		`blobgate_requests_total{method="GET",route="read",status="4xx"}`,
		`blobgate_bytes_total{direction="in"}`,
		`blobgate_bytes_total{direction="out"}`,
		`blobgate_request_duration_seconds_bucket{route="object_put"`,
		`blobgate_node_errors_total{node="local-`,
		`blobgate_repair_pending{store="main"} 1`,
		`blobgate_objects{store="main"} 1`,
		`blobgate_database_up 1`,
		`process_resident_memory_bytes`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("no %q in the metrics", want)
		}
	}
	// no key of an object in a label
	if strings.Contains(out, "k/a") || strings.Contains(out, "k/missing") {
		t.Error("a key is in the metrics")
	}
	// a refusal for room
	r2 := newRig(t, func(c *Config) { c.Reserve = 1 << 60 })
	r2.mirror(0)
	r2.put("k/b", []byte("x"))
	if out := r2.scrape(); !strings.Contains(out, `blobgate_refused_total{reason="store_full"}`) || !strings.Contains(out, `blobgate_store_free_bytes{store="main"}`) {
		t.Error("the refusal for room is not in the metrics")
	}
}
