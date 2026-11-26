// Package rq is a small client of the rqlite HTTP API: statements in, rows out. The module keeps no state of its own on a disk; everything is in rqlite.
package rq

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Client talks to one rqlite address. It is safe for concurrent use.
type Client struct {
	base string
	http *http.Client
}

// New makes a client for the base URL (http://host:4001).
func New(base string) *Client {
	return &Client{base: strings.TrimRight(base, "/"), http: &http.Client{Timeout: 15 * time.Second}}
}

// Stmt is one statement with its parameters.
type Stmt struct {
	SQL  string
	Args []any
}

// S makes a statement.
func S(sql string, args ...any) Stmt { return Stmt{SQL: sql, Args: args} }

// Rows is the result of a query: the rows as decoded JSON values (a number is a json.Number; read it with Int).
type Rows [][]any

// Error is a failure of the database (not of a statement's logic): the caller answers 503.
type Error struct{ Msg string }

func (e *Error) Error() string { return "rqlite: " + e.Msg }

func (s Stmt) encode() any {
	if len(s.Args) == 0 {
		return s.SQL
	}
	return append([]any{s.SQL}, s.Args...)
}

func (c *Client) post(ctx context.Context, path string, stmts []Stmt) ([]json.RawMessage, error) {
	enc := make([]any, len(stmts))
	for i, s := range stmts {
		enc[i] = s.encode()
	}
	body, _ := json.Marshal(enc)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+path, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, &Error{err.Error()}
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		return nil, &Error{err.Error()}
	}
	if resp.StatusCode/100 != 2 {
		return nil, &Error{fmt.Sprintf("HTTP %d: %.200s", resp.StatusCode, data)}
	}
	var out struct {
		Results []json.RawMessage `json:"results"`
		Error   string            `json:"error"`
	}
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, &Error{err.Error()}
	}
	// 200 is not success: during a change of leader rqlite answers {"results":[],"error":"..."} with 200
	if out.Error != "" {
		return nil, &Error{out.Error}
	}
	for _, r := range out.Results {
		var e struct {
			Error string `json:"error"`
		}
		if json.Unmarshal(r, &e) == nil && e.Error != "" {
			return nil, &Error{e.Error}
		}
	}
	return out.Results, nil
}

// Exec runs statements; with tx they are one transaction. It returns the number of rows affected by each.
func (c *Client) Exec(ctx context.Context, tx bool, stmts ...Stmt) ([]int64, error) {
	path := "/db/execute"
	if tx {
		path += "?transaction"
	}
	res, err := c.post(ctx, path, stmts)
	if err != nil {
		return nil, err
	}
	n := make([]int64, len(res))
	for i, r := range res {
		var e struct {
			Rows int64 `json:"rows_affected"`
		}
		_ = json.Unmarshal(r, &e)
		n[i] = e.Rows
	}
	return n, nil
}

// Query runs one SELECT at the strong level (a consensus read, so that what was written is read whoever wrote it).
func (c *Client) Query(ctx context.Context, s Stmt) (Rows, error) { return c.query(ctx, "strong", s) }

// QueryWeak runs one SELECT at the weak level: the leader answers from its own state, with no round of consensus (about as fast as a local read, and what was written through the leader is read). For the
// reads on the hot path of a request; a decision that needs to be linearizable (the lease) uses Query.
func (c *Client) QueryWeak(ctx context.Context, s Stmt) (Rows, error) { return c.query(ctx, "weak", s) }

func (c *Client) query(ctx context.Context, level string, s Stmt) (Rows, error) {
	res, err := c.post(ctx, "/db/query?level="+level, []Stmt{s})
	if err != nil {
		return nil, err
	}
	if len(res) != 1 {
		return nil, &Error{"no result"}
	}
	var r struct {
		Types  []string `json:"types"`
		Values [][]any  `json:"values"`
	}
	dec := json.NewDecoder(bytes.NewReader(res[0]))
	dec.UseNumber() // a number stays as it was written: through float64 an integer above 2^53 would not be the one in the database
	if err := dec.Decode(&r); err != nil {
		return nil, &Error{err.Error()}
	}
	if err := typed(r.Types, r.Values); err != nil {
		return nil, err
	}
	return r.Values, nil
}

// typed refuses an answer with a cell that is not what its column is. SQLite keeps whatever it is given (a text in a column of integers too), and Int and Str would read such a cell as 0 or "": a
// size, a reference count or a generation that is silently wrong. NULL is a value of any column.
func typed(types []string, rows [][]any) error {
	for _, row := range rows {
		for i, v := range row {
			if v == nil || i >= len(types) {
				continue
			}
			ok := true
			switch types[i] {
			case "integer", "int", "bigint", "boolean":
				n, is := v.(json.Number)
				if ok = is; is {
					_, err := n.Int64()
					ok = err == nil
				}
			case "text":
				_, ok = v.(string)
			}
			if !ok {
				return &Error{fmt.Sprintf("column %d of the answer is %s and holds something else (%T): the database is damaged", i+1, types[i], v)}
			}
		}
	}
	return nil
}

// Ready says whether the database answers.
func (c *Client) Ready(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	_, err := c.Query(ctx, S("SELECT 1"))
	return err
}

// IsDB reports whether err is a failure of the database.
func IsDB(err error) bool {
	var e *Error
	return errors.As(err, &e)
}

// Str reads a text cell; a missing or null cell is "".
func Str(v any) string {
	s, _ := v.(string)
	return s
}

// Int reads a number cell; a cell that is no number is 0.
func Int(v any) int64 {
	switch n := v.(type) {
	case json.Number:
		if i, err := n.Int64(); err == nil {
			return i
		}
		f, _ := n.Float64()
		return int64(f)
	case int64:
		return n
	case float64:
		return int64(n)
	}
	return 0
}

// Bool reads a 0/1 cell.
func Bool(v any) bool { return Int(v) == 1 }
