package api

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/zystem/blobgate/internal/auth"
)

// open is a signed GET of an object of the tenant 001, with the body not read yet.
func (r *rig) open(ctx context.Context, key string) *http.Response {
	r.t.Helper()
	path := "/v1/stores/_default/objects/" + key
	req, _ := http.NewRequestWithContext(ctx, "GET", r.ts.URL+path, nil)
	req.Header.Set("Authorization", "Bearer "+auth.SignTarget(secret1, "001", "GET", path, time.Now().Unix()))
	req.Header.Set("X-Blobgate-Tenant", "001")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		r.t.Fatal(err)
	}
	return resp
}

// slotsFree waits (a read that was on its way when the client left ends in a moment) for the places of reads and of held blocks to be free; a place that stays taken is a leak.
func slotsFree(t *testing.T, r *rig) {
	t.Helper()
	for i := 0; i < 100; i++ {
		if len(r.s.reads) == 0 && len(r.s.held) == 0 {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("places stay taken: %d of reads, %d of held blocks", len(r.s.reads), len(r.s.held))
}

func goroutinesSettle(t *testing.T, was int) {
	t.Helper()
	var now int
	for i := 0; i < 50; i++ {
		http.DefaultClient.CloseIdleConnections()
		runtime.GC()
		if now = runtime.NumGoroutine(); now <= was+3 {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	buf := make([]byte, 1<<16)
	t.Fatalf("goroutines: %d, were %d\n%s", now, was, buf[:runtime.Stack(buf, true)])
}

// A client that leaves in the middle of an object leaves nothing behind: its slot of reads is free, no goroutine waits, and the next reads are served. A client that is slow does not hold a slot while
// it is slow, so one stalled reader does not stop the others even when the module is set to one read at a time.
func TestReaderThatLeavesOrStalls(t *testing.T) {
	r := newRig(t, func(c *Config) { c.MaxReads = 1 })
	r.mirror(0)
	body := pattern(3*BlockSize + 100)
	if code, d := r.put("k/big", body); code != 200 {
		t.Fatalf("put: %d %s", code, d)
	}
	http.DefaultClient.CloseIdleConnections()
	was := runtime.NumGoroutine()

	// leaves after a part of the first block, 20 times; and after a block and a half
	for i := 0; i < 20; i++ {
		ctx, cancel := context.WithCancel(context.Background())
		resp := r.open(ctx, "k/big")
		if resp.StatusCode != 200 {
			t.Fatalf("status %d", resp.StatusCode)
		}
		n := 1000
		if i%2 == 1 {
			n = BlockSize + BlockSize/2
		}
		if _, err := io.CopyN(io.Discard, resp.Body, int64(n)); err != nil {
			t.Fatal(err)
		}
		cancel()
		resp.Body.Close()
	}
	goroutinesSettle(t, was)
	slotsFree(t, r)

	// a reader that has taken one byte and stops: another reader is served in full while it waits
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	slow := r.open(ctx, "k/big")
	if _, err := io.ReadFull(slow.Body, make([]byte, 1)); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		resp := r.open(context.Background(), "k/big")
		defer resp.Body.Close()
		d, err := io.ReadAll(resp.Body)
		if err == nil && !bytes.Equal(d, body) {
			err = errors.New("the other reader got other bytes")
		}
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(60 * time.Second):
		t.Fatal("a stalled reader stops the others (it holds the slot of reads)")
	}
	cancel()
	slow.Body.Close()
	goroutinesSettle(t, was)
}

// A node that gives out in the middle of an object, after the status and the first blocks went: the answer was 200 with the length of the whole object, so the reader must meet a cut (not a short
// "success"), and every byte it did get is the object's. With a good copy on the other node there is no cut at all.
func TestCutInTheMiddleOfAnObject(t *testing.T) {
	r := newRig(t, nil)
	r.mirror(0)
	body := pattern(3*BlockSize + 100)
	if code, d := r.put("k/big", body); code != 200 {
		t.Fatalf("put: %d %s", code, d)
	}
	// blocks have a file each; the third block of an object is the file named 2 (the last digits of the path)
	third := func(node int) string {
		for _, f := range files(r.nodes[node]) {
			if filepath.Base(f) == "2" {
				return filepath.Join(r.nodes[node], f)
			}
		}
		t.Fatalf("no third block on node %d", node+1)
		return ""
	}
	good, _ := os.ReadFile(third(0))
	readAll := func() ([]byte, int64, error) {
		resp := r.open(context.Background(), "k/big")
		defer resp.Body.Close()
		d, err := io.ReadAll(resp.Body)
		return d, resp.ContentLength, err
	}
	// node 1 alone is damaged: the whole object, no cut
	if err := os.WriteFile(third(0), good[:len(good)/2], 0o644); err != nil {
		t.Fatal(err)
	}
	if d, _, err := readAll(); err != nil || !bytes.Equal(d, body) {
		t.Fatalf("with one good copy: %v, %d bytes", err, len(d))
	}
	// both are damaged: the first two blocks come, then the cut
	if err := os.WriteFile(third(1), good[:len(good)/3], 0o644); err != nil {
		t.Fatal(err)
	}
	d, cl, err := readAll()
	if err == nil {
		t.Fatalf("a short body of %d bytes (length said %d) ended as a success", len(d), cl)
	}
	if cl != int64(len(body)) || int64(len(d)) >= cl || !bytes.Equal(d, body[:len(d)]) {
		t.Fatalf("the cut: %d bytes of %d (length %d), prefix right: %v", len(d), len(body), cl, bytes.Equal(d, body[:min(len(d), len(body))]))
	}
	if len(d) != 2*BlockSize {
		t.Logf("the reader got %d bytes before the cut (two whole blocks are %d)", len(d), 2*BlockSize)
	}
	slotsFree(t, r)
}
