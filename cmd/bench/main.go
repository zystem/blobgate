// bench measures the module the way docs/store.md ("Speed") measured the Nim one: 8 MiB objects on 4 workers, 64 KiB objects on 8 workers, and 1 GiB in parts of 8 MiB, and the memory of the process.
//
//	go run ./cmd/bench -url http://127.0.0.1:18090 -secret <shard secret> -admin <admin token> -nodes /dev/shm/n1,/dev/shm/n2 -pid <pid of the module>
package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/zystem/blobgate/internal/auth"
)

var (
	url      = flag.String("url", "http://127.0.0.1:18090", "the module")
	secret   = flag.String("secret", "", "the secret of the tenant")
	tenant   = flag.String("tenant", "001", "the tenant")
	admin    = flag.String("admin", "", "the admin token (to register the store)")
	nodes    = flag.String("nodes", "", "directories of local nodes, comma separated (registers the store `bench` over them)")
	keyArg   = flag.String("seal", "", "a key text: the store seals its blocks")
	dedup    = flag.String("dedup", "", `the store's setting dedup ("tenant" or "store"); the loads write the same bytes under many keys, so with it they measure the cost of a copy`)
	pid      = flag.Int("pid", 0, "the pid of the module, for its memory")
	big      = flag.Int("gib", 1, "size of the multipart object, GiB")
	small    = flag.Int("small", 4000, "number of 64 KiB objects")
	large    = flag.Int("large", 64, "number of 8 MiB objects")
	workers  = flag.Int("workers", 4, "clients at once in the 8 MiB loads (more of them than BLOBGATE_THREADS wait for a place: that is what the memory limit is measured with)")
	minAcks  = flag.Int("minacks", 0, "the store's setting min_acks (0: every node)")
	distinct = flag.Bool("distinct", false, "every object has its own bytes (else the same bytes under many keys, as in the first measurements)")
	lag      = flag.Int("repair", 0, "after the loads, write this many 8 MiB objects with the second local node closed (needs -minacks 1), open it and say how long the repair took (needs -rqlite)")
	rqURL    = flag.String("rqlite", "", "the address of the module's rqlite, for -repair")
	noKA     = flag.Bool("nokeepalive", false, "a new connection for every request (the Nim module closes idle connections, and a request that meets one that is closing fails)")
)

var client *http.Client

func call(method, path, query string, hdr map[string]string, body []byte) (int, []byte) {
	t := path
	if query != "" {
		t += "?" + query
	}
	req, _ := http.NewRequest(method, *url+t, bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+auth.SignTarget(*secret, *tenant, method, t, time.Now().Unix()))
	req.Header.Set("X-Blobgate-Tenant", *tenant)
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	start := time.Now()
	resp, err := client.Do(req)
	if err != nil {
		fmt.Fprintln(os.Stderr, "request:", err)
		os.Exit(1)
	}
	first := time.Since(start) // the status is written when the first block is read and checked: the time to the first byte
	times.Lock()
	times.all = append(times.all, first)
	times.Unlock()
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, data
}

// the times to the first byte of the answers of the load that goes on
var times struct {
	sync.Mutex
	all []time.Duration
}

// percentiles of the times since the last call, which it forgets.
func percentiles() string {
	times.Lock()
	defer times.Unlock()
	if len(times.all) == 0 {
		return ""
	}
	slices.Sort(times.all)
	at := func(p float64) float64 {
		return float64(times.all[int(p*float64(len(times.all)-1))].Microseconds()) / 1000
	}
	out := fmt.Sprintf("first byte p50 %.1f ms, p95 %.1f ms, p99 %.1f ms", at(0.5), at(0.95), at(0.99))
	times.all = times.all[:0]
	return out
}

// body is `b` as it is, or with a number at its start, so that object i has bytes of its own.
func body(b []byte, i int) ([]byte, string) {
	if !*distinct {
		return b, sha(b)
	}
	own := append([]byte(nil), b...)
	copy(own, fmt.Sprintf("%020d", i))
	return own, sha(own)
}

// onNodes is the bytes of the files under the local nodes.
func onNodes() (n int64) {
	for _, d := range strings.Split(*nodes, ",") {
		_ = filepath.WalkDir(d, func(_ string, e fs.DirEntry, err error) error {
			if err == nil && !e.IsDir() {
				if i, err := e.Info(); err == nil {
					n += i.Size()
				}
			}
			return nil
		})
	}
	return n
}

// queued is the number of repair tasks that wait.
func queued() int {
	resp, err := client.Post(*rqURL+"/db/query?level=strong", "application/json", strings.NewReader(`["SELECT count(*) FROM storage_repair"]`))
	if err != nil {
		fmt.Fprintln(os.Stderr, "rqlite:", err)
		os.Exit(1)
	}
	defer resp.Body.Close()
	var v struct {
		Results []struct{ Values [][]int }
	}
	if json.NewDecoder(resp.Body).Decode(&v) != nil || len(v.Results) != 1 || len(v.Results[0].Values) != 1 {
		fmt.Fprintln(os.Stderr, "rqlite: no count")
		os.Exit(1)
	}
	return v.Results[0].Values[0][0]
}

func sha(b []byte) string { s := sha256.Sum256(b); return hex.EncodeToString(s[:]) }

func hwm() string {
	if *pid == 0 {
		return "-"
	}
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/status", *pid))
	if err != nil {
		return "?"
	}
	for _, l := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(l, "VmHWM:") {
			return strings.TrimSpace(strings.TrimPrefix(l, "VmHWM:"))
		}
	}
	return "?"
}

func parallel(workers, n int, f func(i int)) time.Duration {
	var wg sync.WaitGroup
	next := make(chan int)
	start := time.Now()
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range next {
				f(i)
			}
		}()
	}
	for i := 0; i < n; i++ {
		next <- i
	}
	close(next)
	wg.Wait()
	return time.Since(start)
}

func report(name string, n int, size int64, d time.Duration) {
	fmt.Printf("%-34s %8.1f MB/s %9.0f /s   (%d x %d B in %.2fs)   process peak %s\n", name, float64(n)*float64(size)/1e6/d.Seconds(), float64(n)/d.Seconds(), n, size, d.Seconds(), hwm())
	if p := percentiles(); p != "" {
		fmt.Printf("%-34s %s\n", "", p)
	}
}

func must(code int, data []byte, what string) {
	if code != 200 && code != 206 {
		fmt.Fprintf(os.Stderr, "%s: %d %s\n", what, code, data)
		os.Exit(1)
	}
}

func main() {
	flag.Parse()
	client = &http.Client{Transport: &http.Transport{MaxIdleConnsPerHost: 64, DisableKeepAlives: *noKA}}
	if *secret == "" {
		fmt.Fprintln(os.Stderr, "-secret is needed")
		os.Exit(2)
	}
	if *nodes != "" {
		var ns []any
		for _, d := range strings.Split(*nodes, ",") {
			ns = append(ns, map[string]any{"remote": ":local:" + d})
		}
		body := map[string]any{"kind": "mirror", "nodes": ns, "default": true, "dedup": *dedup, "min_acks": *minAcks}
		if *keyArg != "" {
			body["encryption"] = map[string]any{"key_id": 1, "keys": map[string]string{"1": *keyArg}}
		}
		b, _ := json.Marshal(body)
		req, _ := http.NewRequest("PUT", *url+"/v1/admin/stores/bench", bytes.NewReader(b))
		req.Header.Set("Authorization", "Bearer "+*admin)
		resp, err := client.Do(req)
		if err != nil || resp.StatusCode != 200 {
			fmt.Fprintln(os.Stderr, "register:", err, resp)
			os.Exit(1)
		}
	}
	fmt.Printf("sealed=%v dedup=%q   process peak at the start %s\n", *keyArg != "", *dedup, hwm())
	obj := "/v1/stores/bench/objects/"

	b8 := bytes.Repeat([]byte("0123456789abcdef"), 8<<20/16)
	h8 := sha(b8)
	percentiles() // the registration is no part of a load
	d := parallel(*workers, *large, func(i int) {
		b, h := body(b8, i)
		c, data := call("PUT", obj+fmt.Sprintf("bench/l%d", i), "", map[string]string{"X-Content-SHA256": h}, b)
		must(c, data, "put 8 MiB")
	})
	report(fmt.Sprintf("PUT 8 MiB, %d workers", *workers), *large, 8<<20, d)
	d = parallel(*workers, *large, func(i int) {
		c, data := call("GET", obj+fmt.Sprintf("bench/l%d", i), "", map[string]string{"Range": fmt.Sprintf("bytes=0-%d", 8<<20-1)}, nil)
		must(c, data, "get 8 MiB")
	})
	report(fmt.Sprintf("GET 8 MiB, %d workers", *workers), *large, 8<<20, d)

	b64 := bytes.Repeat([]byte("x"), 64<<10)
	d = parallel(8, *small, func(i int) {
		b, h := body(b64, i)
		c, data := call("PUT", obj+fmt.Sprintf("bench/s%d", i), "", map[string]string{"X-Content-SHA256": h}, b)
		must(c, data, "put 64 KiB")
	})
	report("PUT 64 KiB, 8 workers", *small, 64<<10, d)
	if *nodes != "" {
		logical := int64(*large)*8<<20 + int64(*small)*64<<10
		on := onNodes()
		fmt.Printf("%-34s %.2f bytes on the nodes for a byte written (%d for %d, %d nodes)\n", "room", float64(on)/float64(logical), on, logical, len(strings.Split(*nodes, ",")))
	}
	d = parallel(8, *small, func(i int) {
		c, data := call("GET", obj+fmt.Sprintf("bench/s%d", i), "", map[string]string{"Range": fmt.Sprintf("bytes=0-%d", 64<<10-1)}, nil)
		must(c, data, "get 64 KiB")
	})
	report("GET 64 KiB, 8 workers", *small, 64<<10, d)

	if *big > 0 {
		parts := *big * 128
		c, data := call("POST", obj+"bench/huge", "uploads", nil, nil)
		must(c, data, "begin")
		var up struct {
			ID string `json:"upload_id"`
		}
		_ = json.Unmarshal(data, &up)
		etags := make([][]any, parts)
		var mu sync.Mutex
		d = parallel(1, parts, func(i int) {
			c, data := call("PUT", obj+"bench/huge", fmt.Sprintf("upload=%s&part=%d", up.ID, i+1), map[string]string{"X-Content-SHA256": h8}, b8)
			must(c, data, "part")
			var e struct{ Etag string }
			_ = json.Unmarshal(data, &e)
			mu.Lock()
			etags[i] = []any{i + 1, e.Etag}
			mu.Unlock()
		})
		report(fmt.Sprintf("PUT %d GiB in parts, 1 worker", *big), parts, 8<<20, d)
		cb, _ := json.Marshal(map[string]any{"parts": etags})
		c, data = call("POST", obj+"bench/huge", "upload="+up.ID, nil, cb)
		must(c, data, "complete")
		d = parallel(1, parts, func(i int) {
			c, data := call("GET", obj+"bench/huge", "", map[string]string{"Range": fmt.Sprintf("bytes=%d-%d", i*8<<20, (i+1)*8<<20-1)}, nil)
			must(c, data, "get range")
		})
		report(fmt.Sprintf("GET %d GiB in ranges, 1 worker", *big), parts, 8<<20, d)
	}
	call("DELETE", obj+"bench/huge", "", nil, nil)
	if *lag > 0 {
		repairLag(obj, b8)
	}
}

// repairLag closes the second node, writes, opens it again and says how long it took until no repair task waited.
func repairLag(obj string, b8 []byte) {
	ds := strings.Split(*nodes, ",")
	if len(ds) < 2 || *minAcks != 1 || *rqURL == "" {
		fmt.Fprintln(os.Stderr, "-repair needs two -nodes, -minacks 1 and -rqlite")
		os.Exit(2)
	}
	closeDirs := func(mode fs.FileMode) {
		_ = filepath.WalkDir(ds[1], func(p string, e fs.DirEntry, err error) error {
			if err == nil && e.IsDir() {
				_ = os.Chmod(p, mode) // #nosec G122 -- a tool for measurements that opens and closes the directories it was pointed at
			}
			return nil
		})
	}
	closeDirs(0o500)
	for i := 0; i < *lag; i++ {
		b, h := body(b8, 1_000_000+i)
		c, data := call("PUT", obj+fmt.Sprintf("bench/r%d", i), "", map[string]string{"X-Content-SHA256": h}, b)
		must(c, data, "put with a node closed")
	}
	waiting, start := queued(), time.Now()
	closeDirs(0o700)
	for queued() > 0 {
		if time.Since(start) > 10*time.Minute {
			fmt.Printf("%-34s %d of %d tasks still wait after 10 minutes\n", "repair", queued(), waiting)
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
	fmt.Printf("%-34s %d tasks (%d x 8 MiB) done %.1f s after the node came back\n", "repair", waiting, *lag, time.Since(start).Seconds())
}
