package api

import (
	"bytes"
	"strings"
	"sync"
	"testing"
	"time"
)

// pauseAt makes the first call that reaches `point` on a server wait until `release` is closed; `reached` is closed when it waits. The other calls pass.
func pauseAt(s *Server, point string) (reached chan struct{}, release chan struct{}) {
	reached, release = make(chan struct{}), make(chan struct{})
	var once sync.Once
	s.cfg.CrashAt = func(p string) {
		if p != point {
			return
		}
		first := false
		once.Do(func() { first = true })
		if first {
			close(reached)
			<-release
		}
	}
	return
}

func etagOfObject(t *testing.T, r *rig, key string) string {
	t.Helper()
	code, _, h := r.core("HEAD", "/v1/stores/_default/objects/"+key, "", nil, nil)
	if code != 200 || h.Get("ETag") == "" {
		t.Fatalf("head %s: %d", key, code)
	}
	return h.Get("ETag")
}

// Finding 3 of the review of 2026-11-22: the condition of a write is decided by the transaction that writes. Two writers under the same condition, on two replicas of the module, with the first held
// back after it has passed its early check and sent its blocks: the second one writes, the first one is refused (412) and its blocks are removed; the winning bytes stay.
func TestTwoWritersUnderOneCondition(t *testing.T) {
	a := newRig(t, nil)
	a.mirror(0)
	b := a.replica()
	body1, body2 := pattern(BlockSize+999), pattern(BlockSize+777)
	body2[0] ^= 0xff

	// create only: If-None-Match: *
	reached, release := pauseAt(a.s, "commit:before")
	var codeA int
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		codeA, _, _ = a.core("PUT", "/v1/stores/_default/objects/lock", "", map[string]string{"If-None-Match": "*", "X-Content-SHA256": sha(body1)}, body1)
	}()
	<-reached
	if code, d, _ := b.core("PUT", "/v1/stores/_default/objects/lock", "", map[string]string{"If-None-Match": "*", "X-Content-SHA256": sha(body2)}, body2); code != 200 {
		t.Fatalf("the second writer: %d %s", code, d)
	}
	close(release)
	wg.Wait()
	if codeA != 412 {
		t.Fatalf("the first writer, which was told at its check that nothing was there: %d, want 412", codeA)
	}
	if code, got := a.get("lock", 0, len(body2)-1); (code != 200 && code != 206) || !bytes.Equal(got, body2) {
		t.Fatalf("the object after the race is not the winner's: %d", code)
	}
	if n := a.count("storage_objects"); n != 1 {
		t.Fatalf("%d objects", n)
	}
	for i := range a.nodes {
		if f := files(a.nodes[i]); len(f) != 2 { // the two blocks of the winner and nothing of the loser
			t.Fatalf("node %d holds %d files after the race, want the 2 blocks of the winner: %v", i+1, len(f), f)
		}
	}
	if n := a.count("storage_uploads"); n != 0 {
		t.Fatalf("%d upload rows left by the loser", n)
	}

	// the same version: If-Match with the tag that both read
	tag := etagOfObject(t, a, "lock")
	reached, release = pauseAt(a.s, "commit:before")
	wg.Add(1)
	go func() {
		defer wg.Done()
		codeA, _, _ = a.core("PUT", "/v1/stores/_default/objects/lock", "", map[string]string{"If-Match": tag, "X-Content-SHA256": sha(body1)}, body1)
	}()
	<-reached
	if code, d, _ := b.core("PUT", "/v1/stores/_default/objects/lock", "", map[string]string{"If-Match": tag, "X-Content-SHA256": sha(body2)}, body2); code != 200 {
		t.Fatalf("the second writer with the same tag: %d %s", code, d)
	}
	close(release)
	wg.Wait()
	if codeA != 412 {
		t.Fatalf("a stale update: %d, want 412", codeA)
	}
	if code, got := a.get("lock", 0, len(body2)-1); (code != 200 && code != 206) || !bytes.Equal(got, body2) {
		t.Fatalf("the object after the stale update is not the winner's: %d", code)
	}

	// a delete under If-Match, held back before its write, while the object is replaced
	tag = etagOfObject(t, a, "lock")
	reached, release = pauseAt(a.s, "delete:before-write")
	wg.Add(1)
	go func() {
		defer wg.Done()
		codeA, _, _ = a.core("DELETE", "/v1/stores/_default/objects/lock", "", map[string]string{"If-Match": tag}, nil)
	}()
	<-reached
	body3 := pattern(5000)
	if code, d, _ := b.core("PUT", "/v1/stores/_default/objects/lock", "", map[string]string{"If-Match": tag, "X-Content-SHA256": sha(body3)}, body3); code != 200 {
		t.Fatalf("the replacement: %d %s", code, d)
	}
	close(release)
	wg.Wait()
	if codeA != 412 {
		t.Fatalf("a delete of a version that was replaced meanwhile: %d, want 412", codeA)
	}
	if code, got := a.get("lock", 0, len(body3)-1); (code != 200 && code != 206) || !bytes.Equal(got, body3) {
		t.Fatalf("the object after the stale delete: %d", code)
	}
}

// If-Match needs the strong comparison: a weak tag never matches; If-None-Match takes the weak one.
func TestWeakTagsAndIfMatch(t *testing.T) {
	r := newRig(t, nil)
	r.mirror(0)
	body := pattern(2000)
	if code, d := r.put("w", body); code != 200 {
		t.Fatalf("%d %s", code, d)
	}
	tag := etagOfObject(t, r, "w")
	put := func(h map[string]string) int {
		h["X-Content-SHA256"] = sha(body)
		code, _, _ := r.core("PUT", "/v1/stores/_default/objects/w", "", h, body)
		return code
	}
	if c := put(map[string]string{"If-Match": "W/" + tag}); c != 412 {
		t.Errorf("If-Match with a weak tag: %d, want 412", c)
	}
	if c := put(map[string]string{"If-Match": tag}); c != 200 {
		t.Errorf("If-Match with the strong tag: %d, want 200", c)
	}
	tag = etagOfObject(t, r, "w")
	if c := put(map[string]string{"If-None-Match": "W/" + tag}); c != 412 {
		t.Errorf("If-None-Match with the weak form of the tag: %d, want 412", c)
	}
	if code, _, _ := r.core("GET", "/v1/stores/_default/objects/w", "", map[string]string{"If-Match": "W/" + tag}, nil); code != 412 {
		t.Errorf("a read with If-Match and a weak tag: %d, want 412", code)
	}
	_ = strings.TrimSpace
	_ = time.Second
}
