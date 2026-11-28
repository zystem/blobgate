package api

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zystem/blobgate/internal/auth"
)

func (r *rig) limit(maxBytes, maxObjects int) {
	r.t.Helper()
	if code, d := r.admin("PUT", "/v1/admin/quotas/main/001", map[string]any{"max_bytes": maxBytes, "max_objects": maxObjects}); code != 200 {
		r.t.Fatalf("the quota: %d %s", code, d)
	}
}

func (r *rig) newUpload(key string) string {
	r.t.Helper()
	_, d, _ := r.core("POST", "/v1/stores/main/objects/"+key, "uploads", nil, nil)
	var u struct {
		ID string `json:"upload_id"`
	}
	if json.Unmarshal(d, &u) != nil || u.ID == "" {
		r.t.Fatalf("begin %s: %s", key, d)
	}
	return u.ID
}

func (r *rig) sendPart(key, id, n string, p []byte) (int, []byte) {
	code, d, _ := r.core("PUT", "/v1/stores/main/objects/"+key, "upload="+id+"&part="+n, map[string]string{"X-Content-SHA256": sha(p)}, p)
	return code, d
}

// The limit is decided by the transaction that records the bytes: of two writers that both passed the check beforehand, one is refused and the tenant is not over its limit.
func TestQuotaHoldsForWritersAtOnce(t *testing.T) {
	for what, limit := range map[string][2]int{"bytes": {10000, 0}, "objects": {0, 1}} {
		r := newRig(t, nil)
		r.mirror(0)
		r.limit(limit[0], limit[1])
		reached, release := pauseAt(r.s, "commit:before")
		var first int
		var said []byte
		var wg sync.WaitGroup
		wg.Add(1)
		go func() {
			defer wg.Done()
			first, said = r.put("acme/a", pattern(6000))
		}()
		<-reached
		if code, d := r.put("acme/b", pattern(6000)); code != 200 {
			t.Fatalf("%s: the second writer: %d %s", what, code, d)
		}
		close(release)
		wg.Wait()
		if first != 507 || !strings.Contains(string(said), "quota_exceeded") {
			t.Fatalf("%s: the first writer: %d %s", what, first, said)
		}
		if n := r.count("storage_objects"); n != 1 {
			t.Fatalf("%s: %d objects", what, n)
		}
		if f := files(r.nodes[0]); len(f) != 1 {
			t.Fatalf("%s: the refused write left blocks: %v", what, f)
		}
	}
}

// The parts of uploads that have not ended count: they are room on the nodes. A replacement counts once, an abort gives the room back, and the completion of an upload does not count its parts twice.
func TestQuotaCountsParts(t *testing.T) {
	r := newRig(t, nil)
	r.mirror(0)
	r.limit(10000, 0)
	a, b := r.newUpload("acme/a"), r.newUpload("acme/b")
	if code, d := r.sendPart("acme/a", a, "1", pattern(6000)); code != 200 {
		t.Fatalf("part of a: %d %s", code, d)
	}
	if code, d := r.sendPart("acme/b", b, "1", pattern(6000)); code != 507 || !strings.Contains(string(d), "have not ended") {
		t.Fatalf("part of b over the limit: %d %s", code, d)
	}
	if code, d := r.put("acme/c", pattern(6000)); code != 507 {
		t.Fatalf("a write over what the parts leave: %d %s", code, d)
	}
	if code, d := r.sendPart("acme/a", a, "1", pattern(7000)); code != 200 { // the same part again is not counted twice
		t.Fatalf("a part sent again: %d %s", code, d)
	}
	if code, d, _ := r.core("POST", "/v1/stores/main/objects/acme/a", "upload="+a, nil, []byte(`{"parts":[[1,"`+etag(sha(pattern(7000)), 7000)+`"]]}`)); code != 200 {
		t.Fatalf("completion: %d %s", code, d)
	}
	if code, d := r.put("acme/c", pattern(3000)); code != 200 {
		t.Fatalf("what is left: %d %s", code, d)
	}
	if code, _, _ := r.core("DELETE", "/v1/stores/main/objects/acme/b", "upload="+b, nil, nil); code != 200 {
		t.Fatal("abort")
	}
}

// Two parts that pass the check beforehand at once: the transaction refuses one, and no row stays for it.
func TestQuotaHoldsForPartsAtOnce(t *testing.T) {
	r := newRig(t, nil)
	r.mirror(0)
	r.limit(10000, 0)
	a, b := r.newUpload("acme/a"), r.newUpload("acme/b")
	reached, release := pauseAt(r.s, "part:after-block")
	var first int
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		first, _ = r.sendPart("acme/a", a, "1", pattern(6000))
	}()
	<-reached
	if code, d := r.sendPart("acme/b", b, "1", pattern(6000)); code != 200 {
		t.Fatalf("the second part: %d %s", code, d)
	}
	close(release)
	wg.Wait()
	if first != 507 || r.count("storage_parts") != 1 {
		t.Fatalf("the first part: %d, %d rows", first, r.count("storage_parts"))
	}
}

// An object that comes back from the trash counts again.
func TestQuotaAtRestore(t *testing.T) {
	r := newRig(t, nil)
	r.mirrorSoft(3600)
	r.limit(10000, 0)
	r.put("acme/a", pattern(6000))
	if code, _, _ := r.core("DELETE", "/v1/stores/main/objects/acme/a", "", nil, nil); code != 200 {
		t.Fatal("delete")
	}
	if code, d := r.put("acme/b", pattern(6000)); code != 200 {
		t.Fatalf("the trash does not count: %d %s", code, d)
	}
	if code, d, _ := r.core("POST", "/v1/stores/main/objects/acme/a", "restore", nil, nil); code != 507 {
		t.Fatalf("restore over the limit: %d %s", code, d)
	}
	if code, _, _ := r.core("DELETE", "/v1/stores/main/objects/acme/b", "", nil, nil); code != 200 {
		t.Fatal("delete b")
	}
	if code, d, _ := r.core("POST", "/v1/stores/main/objects/acme/a", "restore", nil, nil); code != 200 {
		t.Fatalf("restore: %d %s", code, d)
	}
	if code, _, _ := r.core("POST", "/v1/stores/main/objects/acme/none", "restore", nil, nil); code != 404 {
		t.Fatal("restore of what is not there")
	}
}

// With more than one tenant, one whose clients stop cannot take every place: the others still get one.
func TestOneTenantCannotTakeEveryPlace(t *testing.T) {
	const held = 4
	was := holdWait
	holdWait = 2 * time.Second
	t.Cleanup(func() { holdWait = was })
	r := newRig(t, func(c *Config) { c.MaxHeld = held; c.ClientIdle = 30 * time.Second })
	r.mirror(0)
	body := pattern(2*BlockSize + 10)
	if code, d := r.put("k/big", body); code != 200 {
		t.Fatalf("put: %d %s", code, d)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	var wg, started sync.WaitGroup
	defer func() {
		cancel()
		close(done)
		wg.Wait()
		for i := 0; i < 100 && len(r.s.held) > 0; i++ { // the calls of the server end a moment after those of the clients, and remove what they wrote
			time.Sleep(100 * time.Millisecond)
		}
	}()
	for i := 0; i < 6; i++ { // readers of tenant 001 that take one byte and stop
		wg.Add(1)
		go func() {
			defer wg.Done()
			resp := r.open(ctx, "k/big")
			if resp.StatusCode == 200 {
				_, _ = io.ReadFull(resp.Body, make([]byte, 1))
				<-ctx.Done()
			}
			resp.Body.Close()
		}()
	}
	started.Add(3)
	for i := 0; i < 3; i++ { // and its writers
		wg.Add(1)
		go func() {
			defer wg.Done()
			r.stalledPut("k/w"+string(rune('a'+i)), 3<<20, true, &started, done)
		}()
	}
	time.Sleep(3 * time.Second)
	if n := len(r.s.held); n != 3 {
		t.Fatalf("tenant 001 holds %d places of %d, want 3", n, held)
	}
	// tenant 002 writes an object of many blocks: it needs a place and finds one
	path := "/v1/stores/_default/objects/other"
	hdr := map[string]string{"X-Content-SHA256": sha(body)}
	if code, d := r.as("002", "the-secret-of-shard-002-go-test", http.MethodPut, path, hdr, body); code != 200 {
		t.Fatalf("the other tenant: %d %s", code, d)
	}
}

// as is a call of another tenant.
func (r *rig) as(tenant, secret, method, path string, hdr map[string]string, body []byte) (int, []byte) {
	h := map[string]string{"Authorization": "Bearer " + auth.SignTarget(secret, tenant, method, path, time.Now().Unix()), "X-Blobgate-Tenant": tenant}
	for k, v := range hdr {
		h[k] = v
	}
	code, d, _ := r.do(method, path, h, body)
	return code, d
}
