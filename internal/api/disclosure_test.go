package api

import (
	"strings"
	"testing"

	"github.com/zystem/blobgate/internal/rq"
)

// What a caller is told of a failure of a node or of the database does not name the inside: it has a reference of the log line.
func TestErrorsDoNotTellTheInside(t *testing.T) {
	r := newRig(t, nil)
	r.mirror(0)
	r.readOnly(1)
	code, data := r.put("k/a", pattern(100))
	if code != 503 || !strings.Contains(string(data), "(ref ") {
		t.Fatalf("a node that cannot be written: %d %s", code, data)
	}
	for _, inside := range []string{r.nodes[0], r.nodes[1], "local", "permission", "001/k"} {
		if strings.Contains(string(data), inside) {
			t.Errorf("the answer names %q: %s", inside, data)
		}
	}
	good := r.s.DB
	r.s.DB = rq.New("http://127.0.0.1:1")
	defer func() { r.s.DB = good }()
	code, data, _ = r.do("GET", "/readyz", nil, nil)
	if code != 503 || !strings.Contains(string(data), "unreachable") || strings.Contains(string(data), "127.0.0.1") {
		t.Fatalf("/readyz with no database: %d %s", code, data)
	}
}
