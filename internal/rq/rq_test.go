package rq

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// a stand-in for rqlite: it answers what the module has to tell apart
func stub(t *testing.T) *Client {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var stmts []any
		_ = json.NewDecoder(r.Body).Decode(&stmts)
		first, _ := json.Marshal(stmts[0])
		switch {
		case strings.Contains(string(first), "LEADER"):
			_, _ = w.Write([]byte(`{"results":[],"error":"leadership transfer in progress"}`)) // 200 is not success
		case strings.Contains(string(first), "STATEMENT"):
			_, _ = w.Write([]byte(`{"results":[{"error":"no such table: x"}]}`))
		case strings.Contains(string(first), "HTTP500"):
			http.Error(w, "boom", 500)
		case strings.Contains(string(first), "JUNK"):
			_, _ = w.Write([]byte(`not json`))
		case strings.HasPrefix(r.URL.Path, "/db/query"):
			_, _ = w.Write([]byte(`{"results":[{"columns":["a","b","c"],"values":[["x",3,1],[null,0,0]]}]}`))
		default:
			_, _ = w.Write([]byte(`{"results":[{"rows_affected":2},{"rows_affected":5}]}`))
		}
	}))
	t.Cleanup(srv.Close)
	return New(srv.URL + "/")
}

func TestClient(t *testing.T) {
	c := stub(t)
	ctx := context.Background()
	n, err := c.Exec(ctx, true, S("INSERT 1"), S("INSERT 2", 1, "a"))
	if err != nil || len(n) != 2 || n[0] != 2 || n[1] != 5 {
		t.Fatalf("%v %v", n, err)
	}
	rows, err := c.Query(ctx, S("SELECT"))
	if err != nil || len(rows) != 2 || Str(rows[0][0]) != "x" || Int(rows[0][1]) != 3 || !Bool(rows[0][2]) || Str(rows[1][0]) != "" || Int(rows[1][1]) != 0 || Bool(rows[1][2]) {
		t.Fatalf("%v %v", rows, err)
	}
	if rows, err := c.QueryWeak(ctx, S("SELECT")); err != nil || len(rows) != 2 {
		t.Fatalf("weak: %v %v", rows, err)
	}
	if err := c.Ready(ctx); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{"LEADER", "STATEMENT", "HTTP500", "JUNK"} {
		_, err := c.Exec(ctx, false, S(bad))
		if err == nil || !IsDB(err) || !strings.HasPrefix(err.Error(), "rqlite: ") {
			t.Errorf("%s: %v", bad, err)
		}
	}
	// an address nobody listens on
	dead := New("http://127.0.0.1:1")
	if _, err := dead.Query(ctx, S("SELECT 1")); !IsDB(err) {
		t.Fatalf("%v", err)
	}
	if dead.Ready(ctx) == nil {
		t.Fatal("ready with no database")
	}
	if IsDB(context.Canceled) {
		t.Fatal("a cancelled context is not a failure of the database")
	}
	if Str(1.5) != "" || Int("x") != 0 {
		t.Fatal("a cell of another kind is the zero value")
	}
}
