package api

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/zystem/blobgate/internal/auth"
)

func heapNow() uint64 {
	var m runtime.MemStats
	runtime.GC()
	runtime.GC()
	runtime.ReadMemStats(&m)
	return m.HeapAlloc
}

// stalledPut sends a body of a known or unknown length in chunks and then stops with the connection open (a writer that has gone quiet), until done is closed.
func (r *rig) stalledPut(key string, sent int, unsized bool, started *sync.WaitGroup, done <-chan struct{}) {
	path := "/v1/stores/_default/objects/" + key
	pr, pw := io.Pipe()
	req, _ := http.NewRequest("PUT", r.ts.URL+path, pr)
	if !unsized {
		req.ContentLength = 6 << 20
	} else {
		req.ContentLength = -1 // chunked: the size is not known before the end
	}
	req.Header.Set("Authorization", "Bearer "+auth.SignTarget(secret1, "001", "PUT", path, time.Now().Unix()))
	req.Header.Set("X-Blobgate-Tenant", "001")
	go func() {
		_, _ = pw.Write(pattern(sent))
		started.Done()
		<-done
		pw.CloseWithError(io.ErrClosedPipe)
	}()
	if resp, err := http.DefaultClient.Do(req); err == nil {
		resp.Body.Close()
	}
}

// What the module holds for clients that go slow or quiet is bounded: N readers that take one byte of a big object and stop, and N writers that send a few MiB of a body of no known length and stop,
// hold at most BLOBGATE_HELD_BLOCKS blocks (before the limit existed, 48 of them held 385 MiB each way: a block each); the others are told to come back (503, Retry-After); and a client that sends or takes no byte
// for the idle time is ended, so its place is free again.
func TestMemoryOfSlowClients(t *testing.T) {
	const n, held = 24, 4
	was := holdWait
	holdWait = 2 * time.Second
	t.Cleanup(func() { holdWait = was })
	r := newRig(t, func(c *Config) { c.MaxHeld = held; c.ClientIdle = 4 * time.Second })
	r.mirror(0)
	body := pattern(2*BlockSize + 10)
	if code, d := r.put("k/big", body); code != 200 {
		t.Fatalf("put: %d %s", code, d)
	}
	base := heapNow()
	const limit = (held + 3) << 23 // the places, and some room for what the runtime has not given back yet

	// readers: the first `held` get a block each and stop; the others find no place and are told so
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var mu sync.Mutex
	var wg sync.WaitGroup
	codes := map[int]int{}
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			resp := r.open(ctx, "k/big")
			mu.Lock()
			codes[resp.StatusCode]++
			mu.Unlock()
			if resp.StatusCode == 503 {
				if resp.Header.Get("Retry-After") == "" {
					t.Error("a 503 of busy with no Retry-After")
				}
				resp.Body.Close()
				return
			}
			_, _ = io.ReadFull(resp.Body, make([]byte, 1))
			<-ctx.Done() // goes quiet; the module ends the call by itself, the test only closes it at the end
			resp.Body.Close()
		}()
	}
	time.Sleep(3 * time.Second) // the waiting ones have given up (2 s), the quiet ones are not yet cut (4 s)
	if got := heapNow() - base; got > limit {
		t.Fatalf("%d readers hold %d MiB, more than %d places of a block", n, got>>20, held)
	} else {
		t.Logf("%d readers that stopped after one byte hold %d MiB with %d places (a block each, 8 MiB)", n, got>>20, held)
	}
	mu.Lock()
	if codes[200] != held || codes[503] != n-held {
		t.Errorf("answers %v: want %d of 200 and %d of 503", codes, held, n-held)
	}
	mu.Unlock()
	// the quiet ones are ended by the module after the idle time: the places come back
	deadline := time.Now().Add(15 * time.Second)
	for len(r.s.held) != 0 {
		if time.Now().After(deadline) {
			t.Fatalf("%d places stay taken by clients that sent nothing for the idle time", len(r.s.held))
		}
		time.Sleep(200 * time.Millisecond)
	}
	cancel()
	wg.Wait()
	if code, d := r.get("k/big", 0, len(body)-1); code != 200 && code != 206 || !bytes.Equal(d, body) {
		t.Fatalf("read after the quiet ones were ended: %d", code)
	}

	// writers of no known length: a buffer of a block each, bounded the same way, ended the same way
	base = heapNow()
	done := make(chan struct{})
	var started sync.WaitGroup
	started.Add(n)
	for i := 0; i < n; i++ {
		go r.stalledPut("k/w"+string(rune('a'+i%26)), 3<<20, true, &started, done)
	}
	started.Wait()
	time.Sleep(3 * time.Second)
	if got := heapNow() - base; got > limit {
		t.Fatalf("%d quiet writers hold %d MiB, more than %d places of a block", n, got>>20, held)
	} else {
		t.Logf("%d writers of no known length that stopped after 3 MiB hold %d MiB with %d places", n, got>>20, held)
	}
	deadline = time.Now().Add(15 * time.Second)
	for len(r.s.held) != 0 {
		if time.Now().After(deadline) {
			t.Fatalf("%d places stay taken by quiet writers", len(r.s.held))
		}
		time.Sleep(200 * time.Millisecond)
	}
	close(done)
	if rows := r.count("storage_uploads"); rows != 0 {
		time.Sleep(time.Second)
		if rows = r.count("storage_uploads"); rows != 0 {
			t.Errorf("%d rows of uploads left by writers that were ended or told to come back", rows)
		}
	}
}

// Writers into a store that seals hold a place of `BLOBGATE_THREADS` while they send a block: quiet ones must not take them for good.
func TestQuietWritersOfASealedStoreDoNotStopOthers(t *testing.T) {
	r := newRig(t, func(c *Config) { c.MaxReads = 2; c.ClientIdle = 3 * time.Second })
	r.mirrorSealed(0, 1, map[string]string{"1": key1})
	done := make(chan struct{})
	defer close(done)
	var started sync.WaitGroup
	started.Add(4)
	for i := 0; i < 4; i++ {
		go r.stalledPut("k/q"+string(rune('a'+i)), 1<<20, false, &started, done)
	}
	started.Wait()
	t0 := time.Now()
	if code, d := r.put("k/other", pattern(5000)); code != 200 {
		t.Fatalf("a write beside quiet writers: %d %s", code, d)
	}
	t.Logf("the write beside four quiet writers took %v (idle time 3 s, two places)", time.Since(t0).Round(100*time.Millisecond))
}
