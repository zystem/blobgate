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

// Rows is the result of a query: the rows as decoded JSON values (numbers are float64 unless read with the helpers).
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
		Values [][]any `json:"values"`
	}
	if err := json.Unmarshal(res[0], &r); err != nil {
		return nil, &Error{err.Error()}
	}
	return r.Values, nil
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

// Int reads a number cell.
func Int(v any) int64 {
	f, _ := v.(float64)
	return int64(f)
}

// Bool reads a 0/1 cell.
func Bool(v any) bool { return Int(v) == 1 }
