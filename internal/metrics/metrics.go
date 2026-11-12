// Package metrics keeps what the module says about itself for Prometheus: requests, bytes, errors of nodes, the repair queue, the lease, the room in the stores. The labels have few values (the
// route is one of a fixed list, never a key or a store's object), so the number of series does not grow with the data.
package metrics

import (
	"context"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/zystem/blobgate/internal/rq"
)

// Registry is the set the module's metrics are in (not the global one: a test makes its own).
var Registry = prometheus.NewRegistry()

var (
	requests = prometheus.NewCounterVec(prometheus.CounterOpts{Name: "blobgate_requests_total", Help: "Requests answered, by route, method and status."}, []string{"route", "method", "status"})
	duration = prometheus.NewHistogramVec(prometheus.HistogramOpts{Name: "blobgate_request_duration_seconds", Help: "Time to answer a request, by route.",
		Buckets: []float64{.001, .005, .02, .1, .5, 2, 10, 60}}, []string{"route"})
	bytesTotal = prometheus.NewCounterVec(prometheus.CounterOpts{Name: "blobgate_bytes_total", Help: "Bytes of bodies received (in) and sent (out)."}, []string{"direction"})
	nodeErrors = prometheus.NewCounterVec(prometheus.CounterOpts{Name: "blobgate_node_errors_total", Help: "Operations that a node of a store failed, by store, node and operation."}, []string{"store", "node", "op"})
	repairs    = prometheus.NewCounterVec(prometheus.CounterOpts{Name: "blobgate_repair_tasks_total", Help: "Repair tasks worked, by result (done, failed)."}, []string{"result"})
	swept      = prometheus.NewCounter(prometheus.CounterOpts{Name: "blobgate_swept_uploads_total", Help: "Uploads that never ended whose blocks were removed."})
	checkMade  = prometheus.NewCounter(prometheus.CounterOpts{Name: "blobgate_check_tasks_total", Help: "Copy tasks made by the check of the inventory."})
	refused    = prometheus.NewCounterVec(prometheus.CounterOpts{Name: "blobgate_refused_total", Help: "Writes refused, by reason (store_full)."}, []string{"reason"})
	freeBytes  = prometheus.NewGaugeVec(prometheus.GaugeOpts{Name: "blobgate_store_free_bytes", Help: "Room the store said it had at the last write (only stores that say)."}, []string{"store"})
	totalBytes = prometheus.NewGaugeVec(prometheus.GaugeOpts{Name: "blobgate_store_total_bytes", Help: "Size the store said it had at the last write."}, []string{"store"})
	leaseHeld  = prometheus.NewGaugeVec(prometheus.GaugeOpts{Name: "blobgate_lease_held", Help: "1 while this replica holds the lease of a job (repair, backup)."}, []string{"name"})
	lastBackup = prometheus.NewGauge(prometheus.GaugeOpts{Name: "blobgate_last_backup_timestamp_seconds", Help: "When the last backup of the database by this replica was written."})
)

func init() {
	Registry.MustRegister(requests, duration, bytesTotal, nodeErrors, repairs, swept, checkMade, refused, freeBytes, totalBytes, leaseHeld, lastBackup,
		collectors.NewGoCollector(), collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}))
}

// Request records one answered request.
func Request(route, method string, status int, d time.Duration, in, out int64) {
	requests.WithLabelValues(route, method, statusClass(status)).Inc()
	duration.WithLabelValues(route).Observe(d.Seconds())
	if in > 0 {
		bytesTotal.WithLabelValues("in").Add(float64(in))
	}
	if out > 0 {
		bytesTotal.WithLabelValues("out").Add(float64(out))
	}
}

func statusClass(code int) string {
	if code == 0 {
		return "0" // no answer: the connection was cut
	}
	return string(rune('0'+code/100)) + "xx"
}

// NodeError records an operation that a node failed.
func NodeError(store, node, op string) { nodeErrors.WithLabelValues(store, node, op).Inc() }

// Repair records a repair task that was done or failed.
func Repair(done bool) {
	repairs.WithLabelValues(map[bool]string{true: "done", false: "failed"}[done]).Inc()
}

// Swept records uploads that were swept.
func Swept(n int) { swept.Add(float64(n)) }

// CheckMade records copy tasks made by a check.
func CheckMade(n int) { checkMade.Add(float64(n)) }

// Refused records a write that was refused.
func Refused(reason string) { refused.WithLabelValues(reason).Inc() }

// Room records how much room a store said it had.
func Room(store string, free, total int64) {
	freeBytes.WithLabelValues(store).Set(float64(free))
	totalBytes.WithLabelValues(store).Set(float64(total))
}

// Lease records whether this replica holds the lease of a job.
func Lease(name string, held bool) {
	v := 0.0
	if held {
		v = 1
	}
	leaseHeld.WithLabelValues(name).Set(v)
}

// BackupWritten records that a backup was written.
func BackupWritten(t time.Time) { lastBackup.Set(float64(t.Unix())) }

// DBCollector reports what only the database knows: the repair queue and the inventory by store. It reads at most every `ttl`, so that a scrape does not load the database.
type DBCollector struct {
	DB  *rq.Client
	TTL time.Duration

	mu     sync.Mutex
	at     time.Time
	repair map[string]float64
	objs   map[string]float64
	oldest float64
	up     float64
}

var (
	descRepair = prometheus.NewDesc("blobgate_repair_pending", "Repair tasks waiting, by store.", []string{"store"}, nil)
	descOldest = prometheus.NewDesc("blobgate_repair_oldest_age_seconds", "Age of the oldest task waiting in the repair queue.", nil, nil)
	descObjs   = prometheus.NewDesc("blobgate_objects", "Objects the inventory holds, by store.", []string{"store"}, nil)
	descUp     = prometheus.NewDesc("blobgate_database_up", "1 when the database answered the last time it was asked.", nil, nil)
)

// Describe implements prometheus.Collector.
func (c *DBCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- descRepair
	ch <- descOldest
	ch <- descObjs
	ch <- descUp
}

// Collect implements prometheus.Collector.
func (c *DBCollector) Collect(ch chan<- prometheus.Metric) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if time.Since(c.at) >= c.TTL {
		c.at = time.Now()
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		c.up, c.repair, c.objs, c.oldest = 0, map[string]float64{}, map[string]float64{}, 0
		if rows, err := c.DB.QueryWeak(ctx, rq.S("SELECT store, count(*), min(since) FROM storage_repair GROUP BY store")); err == nil {
			c.up = 1
			for _, r := range rows {
				c.repair[rq.Str(r[0])] = float64(rq.Int(r[1]))
				if age := float64(time.Now().Unix() - rq.Int(r[2])); age > c.oldest {
					c.oldest = age
				}
			}
		}
		if rows, err := c.DB.QueryWeak(ctx, rq.S("SELECT store, count(*) FROM storage_objects GROUP BY store")); err == nil {
			for _, r := range rows {
				c.objs[rq.Str(r[0])] = float64(rq.Int(r[1]))
			}
		}
	}
	for s, v := range c.repair {
		ch <- prometheus.MustNewConstMetric(descRepair, prometheus.GaugeValue, v, s)
	}
	for s, v := range c.objs {
		ch <- prometheus.MustNewConstMetric(descObjs, prometheus.GaugeValue, v, s)
	}
	ch <- prometheus.MustNewConstMetric(descOldest, prometheus.GaugeValue, c.oldest)
	ch <- prometheus.MustNewConstMetric(descUp, prometheus.GaugeValue, c.up)
}

// Handler serves the metrics in the text format.
func Handler() http.Handler { return promhttp.HandlerFor(Registry, promhttp.HandlerOpts{}) }

// Route names a request for the metrics: one of a fixed list, never a key. `shim` marks the port for shims.
func Route(method, path string, query map[string][]string, shim bool) string {
	has := func(k string) bool { _, ok := query[k]; return ok }
	switch {
	case path == "/healthz" || path == "/readyz":
		return "health"
	case path == "/metrics":
		return "metrics"
	case path == "/v1/openapi.yaml":
		return "openapi"
	case path == "/v1/stores":
		return "stores"
	case strings.HasPrefix(path, "/v1/admin/"):
		return "admin"
	case shim && strings.HasPrefix(path, "/s/"):
		if method == http.MethodPut {
			return "shim_part_put"
		}
		return "shim_read"
	case strings.HasPrefix(path, "/v1/stores/"):
		switch method {
		case http.MethodPut:
			if has("upload") {
				return "part_put"
			}
			return "object_put"
		case http.MethodPost:
			if has("uploads") {
				return "upload_begin"
			}
			return "upload_complete"
		case http.MethodGet:
			if has("stat") {
				return "stat"
			}
			if has("parts") {
				return "parts"
			}
			return "read"
		case http.MethodDelete:
			if has("upload") {
				return "upload_abort"
			}
			return "object_delete"
		}
	}
	return "other"
}

// RegisterDB adds the collector of what the database knows.
func RegisterDB(db *rq.Client, ttl time.Duration) {
	Registry.MustRegister(&DBCollector{DB: db, TTL: ttl})
}
