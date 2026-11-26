package api

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/zystem/blobgate/internal/maint"
	"github.com/zystem/blobgate/internal/metrics"
)

// A reconcile that reads the database page by page knows everything that is there, and its report is bounded while all the strays are counted and queued.
func TestReconcileInPagesAndABoundedReport(t *testing.T) {
	page, most := maint.ReconcilePage, maint.ReportStrays
	maint.ReconcilePage, maint.ReportStrays = 2, 1
	t.Cleanup(func() { maint.ReconcilePage, maint.ReportStrays = page, most })
	r := newRig(t, nil)
	r.dedupStore("tenant", 0, "")
	for i := range 5 {
		if code, data := r.put(fmt.Sprintf("acme/o%d", i), pattern(1000+i)); code != 200 { // five sets of blocks
			t.Fatalf("put: %d %s", code, data)
		}
	}
	for i := range 5 { // and five uploads that go on, each with a part on the nodes
		path := fmt.Sprintf("/v1/stores/main/objects/acme/u%d", i%2)
		_, d, _ := r.core("POST", path, "uploads", nil, nil)
		var u struct {
			ID string `json:"upload_id"`
		}
		_ = json.Unmarshal(d, &u)
		p := pattern(500)
		if code, data, _ := r.core("PUT", path, "upload="+u.ID+"&part=1", map[string]string{"X-Content-SHA256": sha(p)}, p); code != 200 {
			t.Fatalf("part: %d %s", code, data)
		}
	}
	reconcile := func(q string) (rep maint.Report) {
		code, data := r.admin("POST", "/v1/admin/stores/main:reconcile?min_age=3600"+q, nil)
		if code != 200 {
			t.Fatalf("reconcile: %d %s", code, data)
		}
		_ = json.Unmarshal(data, &rep)
		return rep
	}
	if rep := reconcile(""); rep.StraysTotal != 0 || rep.Expected != 10 {
		t.Fatalf("what the database knows was taken for stray: %+v", rep)
	}
	old := time.Now().Add(-72 * time.Hour)
	for _, name := range []string{"x~0123456789abcdef", "y~fedcba9876543210"} {
		p := filepath.Join(r.nodes[0], "001", "ghost", name, "0")
		_ = os.MkdirAll(filepath.Dir(p), 0o755)
		_ = os.WriteFile(p, []byte("stray"), 0o644)
		_ = os.Chtimes(p, old, old)
	}
	if rep := reconcile("&apply"); rep.StraysTotal != 2 || len(rep.Strays) != 1 || rep.Queued != 2 {
		t.Fatalf("the report is not bounded or not all were queued: %+v", rep)
	}

	// the parts that wait are room that is taken, and the metrics say so
	reg := prometheus.NewRegistry()
	reg.MustRegister(&metrics.DBCollector{DB: r.dbc})
	all, err := reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	staged := 0.0
	for _, f := range all {
		if f.GetName() == "blobgate_staged_bytes" {
			for _, m := range f.GetMetric() {
				staged += m.GetGauge().GetValue()
			}
		}
	}
	if staged != 5*500 {
		t.Fatalf("staged bytes: %v", staged)
	}
}
