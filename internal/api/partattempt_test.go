package api

import (
	"context"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/zystem/blobgate/internal/maint"
)

// A part may be sent again at any time: the client that lost the answer retries, two attempts may be on their way at once, and one may still be on its way when the upload is completed. Every attempt
// writes a file of its own, so none can damage what another one was acknowledged for. In a plain, a sealed and a deduplicated store.

func partStores(t *testing.T, run func(t *testing.T, r *rig)) {
	for name, set := range map[string]func(r *rig){
		"plain": func(r *rig) { r.mirror(0) },
		"sealed": func(r *rig) {
			r.mirrorSealed(0, 1, map[string]string{"1": "a-long-random-text-of-at-least-16-characters"})
		},
		"dedup": func(r *rig) { r.dedupStore("tenant", 0, "a-long-random-text-of-at-least-16-characters") },
	} {
		t.Run(name, func(t *testing.T) {
			r := newRig(t, nil)
			set(r)
			run(t, r)
		})
	}
}

// completeAndRead completes the upload with the given parts and requires the object to read whole, and the nodes to hold nothing but its blocks.
func (r *rig) completeAndRead(t *testing.T, key, id string, parts ...[]byte) {
	t.Helper()
	var list []string
	var whole []byte
	for i, p := range parts {
		list = append(list, "["+strconv.Itoa(i+1)+`,"`+etag(sha(p), int64(len(p)))+`"]`)
		whole = append(whole, p...)
	}
	if code, d, _ := r.core("POST", "/v1/stores/main/objects/"+key, "upload="+id, nil, []byte(`{"parts":[`+strings.Join(list, ",")+`]}`)); code != 200 {
		t.Fatalf("completion: %d %s", code, d)
	}
	r.readsWhole(t, key, whole, len(parts))
}

func (r *rig) readsWhole(t *testing.T, key string, whole []byte, blocks int) {
	t.Helper()
	code, got := r.get(key, 0, len(whole)-1)
	if (code != 200 && code != 206) || string(got) != string(whole) {
		t.Fatalf("the object: %d, %d bytes", code, len(got))
	}
	for i, n := range r.nodes {
		if f := files(n); len(f) != blocks {
			t.Fatalf("node %d holds %v, want the %d blocks of the object", i+1, f, blocks)
		}
	}
}

func TestAFailedRetryKeepsTheAcknowledgedPart(t *testing.T) {
	partStores(t, func(t *testing.T, r *rig) {
		body := pattern(100_000)
		id := r.newUpload("acme/a")
		if code, d := r.sendPart("acme/a", id, "1", body); code != 200 {
			t.Fatalf("part: %d %s", code, d)
		}
		if code, d, _ := r.core("PUT", "/v1/stores/main/objects/acme/a", "upload="+id+"&part=1", map[string]string{"X-Content-SHA256": sha([]byte("other"))}, body); code != 400 {
			t.Fatalf("a retry with a wrong digest: %d %s", code, d)
		}
		r.limit(150_000, 0)
		if code, _ := r.sendPart("acme/a", id, "1", pattern(160_000)); code != 507 {
			t.Fatalf("a retry over the quota: %d", code)
		}
		r.limit(0, 0)
		r.completeAndRead(t, "acme/a", id, body)
	})
}

func TestALateRetryDoesNotTouchTheObject(t *testing.T) {
	partStores(t, func(t *testing.T, r *rig) {
		body := pattern(100_000)
		id := r.newUpload("acme/a")
		if code, d := r.sendPart("acme/a", id, "1", body); code != 200 {
			t.Fatalf("part: %d %s", code, d)
		}
		reached, release := pauseAt(r.s, "part:after-block")
		late := make(chan int)
		go func() { c, _ := r.sendPart("acme/a", id, "1", body); late <- c }()
		<-reached
		code, d, _ := r.core("POST", "/v1/stores/main/objects/acme/a", "upload="+id, nil, []byte(`{"parts":[[1,"`+etag(sha(body), int64(len(body)))+`"]]}`))
		close(release)
		if code != 200 {
			t.Fatalf("completion: %d %s", code, d)
		}
		if c := <-late; c != 404 {
			t.Fatalf("an attempt that ends after the completion: %d, want 404", c)
		}
		r.readsWhole(t, "acme/a", body, 1)
	})
}

func TestAPartSentAgainWithOtherBytes(t *testing.T) {
	partStores(t, func(t *testing.T, r *rig) {
		first, second := pattern(100_000), pattern(100_001)
		second[0] ^= 0xff
		id := r.newUpload("acme/a")
		r.sendPart("acme/a", id, "1", first)
		if code, d := r.sendPart("acme/a", id, "1", second); code != 200 {
			t.Fatalf("part again: %d %s", code, d)
		}
		r.completeAndRead(t, "acme/a", id, second)
	})
}

// The repair of a node that missed a part gives it the file of the attempt that was acknowledged.
func TestRepairOfAPart(t *testing.T) {
	r := newRig(t, nil)
	r.mirror(1)
	r.readOnly(1)
	body := pattern(30_000)
	id := r.newUpload("acme/a")
	if code, d := r.sendPart("acme/a", id, "1", body); code != 200 {
		t.Fatalf("part with a node down: %d %s", code, d)
	}
	r.completeAndRead2(t, "acme/a", id, body)
	r.writable(1)
	w := r.worker("w", func(c *maint.Config) { c.Deep = true })
	if ok, _ := w.Lease(context.Background(), "repair"); !ok {
		t.Fatal("lease")
	}
	w.Pass(context.Background())
	a, b := files(r.nodes[0]), files(r.nodes[1])
	if len(b) != 1 || len(a) != 1 || a[0] != b[0] || !strings.Contains(filepath.Base(b[0]), "0.") || r.count("storage_repair") != 0 {
		t.Fatalf("after the repair: node 1 %v, node 2 %v, %d tasks", a, b, r.count("storage_repair"))
	}
}

// completeAndRead2 is a completion with a node down: only the read is checked here.
func (r *rig) completeAndRead2(t *testing.T, key, id string, body []byte) {
	t.Helper()
	if code, d, _ := r.core("POST", "/v1/stores/main/objects/"+key, "upload="+id, nil, []byte(`{"parts":[[1,"`+etag(sha(body), int64(len(body)))+`"]]}`)); code != 200 {
		t.Fatalf("completion: %d %s", code, d)
	}
}
