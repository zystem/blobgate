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
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/zystem/blobgate/internal/auth"
)

var (
	url    = flag.String("url", "http://127.0.0.1:18090", "the module")
	secret = flag.String("secret", "", "the secret of the shard")
	shard  = flag.String("shard", "001", "the shard")
	admin  = flag.String("admin", "", "the admin token (to register the store)")
	nodes  = flag.String("nodes", "", "directories of local nodes, comma separated (registers the store `bench` over them)")
	keyArg = flag.String("seal", "", "a key text: the store seals its blocks")
	pid    = flag.Int("pid", 0, "the pid of the module, for its memory")
	big    = flag.Int("gib", 1, "size of the multipart object, GiB")
	small  = flag.Int("small", 4000, "number of 64 KiB objects")
	large  = flag.Int("large", 64, "number of 8 MiB objects")
	noKA   = flag.Bool("nokeepalive", false, "a new connection for every request (the Nim module closes idle connections, and a request that meets one that is closing fails)")
)

var client *http.Client

func call(method, path, query string, hdr map[string]string, body []byte) (int, []byte) {
	t := path
	if query != "" {
		t += "?" + query
	}
	req, _ := http.NewRequest(method, *url+t, bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+auth.SignTarget(*secret, *shard, method, t, time.Now().Unix()))
	req.Header.Set("X-Cinim-Shard", *shard)
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := client.Do(req)
	if err != nil {
		fmt.Fprintln(os.Stderr, "request:", err)
		os.Exit(1)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, data
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
		body := map[string]any{"kind": "mirror", "nodes": ns, "default": true}
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
	fmt.Printf("sealed=%v   process peak at the start %s\n", *keyArg != "", hwm())
	obj := "/v1/stores/bench/objects/"

	b8 := bytes.Repeat([]byte("0123456789abcdef"), 8<<20/16)
	h8 := sha(b8)
	d := parallel(4, *large, func(i int) {
		c, data := call("PUT", obj+fmt.Sprintf("bench/l%d", i), "", map[string]string{"X-Content-SHA256": h8}, b8)
		must(c, data, "put 8 MiB")
	})
	report("PUT 8 MiB, 4 workers", *large, 8<<20, d)
	d = parallel(4, *large, func(i int) {
		c, data := call("GET", obj+fmt.Sprintf("bench/l%d", i), "", map[string]string{"Range": fmt.Sprintf("bytes=0-%d", 8<<20-1)}, nil)
		must(c, data, "get 8 MiB")
	})
	report("GET 8 MiB, 4 workers", *large, 8<<20, d)

	b64 := bytes.Repeat([]byte("x"), 64<<10)
	h64 := sha(b64)
	d = parallel(8, *small, func(i int) {
		c, data := call("PUT", obj+fmt.Sprintf("bench/s%d", i), "", map[string]string{"X-Content-SHA256": h64}, b64)
		must(c, data, "put 64 KiB")
	})
	report("PUT 64 KiB, 8 workers", *small, 64<<10, d)
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
}
