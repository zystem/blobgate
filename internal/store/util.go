package store

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/zystem/blobgate/internal/space"
)

func bytesReader(b []byte) io.Reader { return bytes.NewReader(b) }

// garageSpace reads the Prometheus text of Garage's admin port: garage_local_disk_avail and garage_local_disk_total of the data volume (the node with the least room when there are several).
// A store that does not answer in 3 s, or does not say, gives no answer, and nothing is refused.
func garageSpace(ctx context.Context, url string) (space.Info, bool) {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return space.Info{}, false
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return space.Info{}, false
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return space.Info{}, false
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return space.Info{}, false
	}
	var avail, total []int64
	for _, line := range strings.Split(string(data), "\n") {
		if !strings.Contains(line, `volume="data"`) {
			continue
		}
		switch {
		case strings.HasPrefix(line, "garage_local_disk_avail{"):
			if v, ok := lastNumber(line); ok {
				avail = append(avail, v)
			}
		case strings.HasPrefix(line, "garage_local_disk_total{"):
			if v, ok := lastNumber(line); ok {
				total = append(total, v)
			}
		}
	}
	if len(avail) == 0 || len(total) == 0 {
		return space.Info{}, false
	}
	at := 0 // the tightest node, and the total of that node
	for i, a := range avail {
		if a < avail[at] {
			at = i
		}
	}
	return space.Info{Known: true, Free: avail[at], Total: total[min(at, len(total)-1)]}, true
}

func lastNumber(line string) (int64, bool) {
	f := strings.Fields(line)
	if len(f) < 2 {
		return 0, false
	}
	x, err := strconv.ParseFloat(f[len(f)-1], 64)
	if err != nil {
		return 0, false
	}
	return int64(x), true
}
