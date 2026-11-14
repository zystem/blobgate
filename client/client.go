// Package client is a Go client of the HTTP API of blobgate (internal/api/openapi.yaml): it signs the calls of a tenant, writes and reads objects, lists them, and writes with a check before the write
// (PutOrLink: bytes that the store keeps already are not sent). It has no dependency besides the standard library and the signing of the tokens of this module.
package client

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/zystem/blobgate/internal/auth"
)

// Client speaks for one tenant to one service.
type Client struct {
	Base   string       // the address of the service, for example http://blobgate:18090
	Tenant string       // the name of the tenant
	Secret string       // its secret (BLOBGATE_TENANTS)
	HTTP   *http.Client // nil: http.DefaultClient
}

// New makes a client.
func New(base, tenant, secret string) *Client {
	return &Client{Base: strings.TrimRight(base, "/"), Tenant: tenant, Secret: secret}
}

// Error is an answer of the service that says no: its status, the word of the error (`no_such_object`, `quota_exceeded`, `unavailable`, ...) and the words that tell what to do.
type Error struct {
	Status int
	Kind   string
	Detail string
}

func (e *Error) Error() string { return fmt.Sprintf("blobgate: %d %s: %s", e.Status, e.Kind, e.Detail) }

// IsNotFound says whether the error is "no such object".
func IsNotFound(err error) bool {
	var e *Error
	return errors.As(err, &e) && e.Status == 404 && (e.Kind == "no_such_object" || e.Kind == "")
}

// Options are what a write may say about an object.
type Options struct {
	ContentType string
	Meta        map[string]string // up to 32 pairs; a name is letters, digits, - and _
	SHA256      string            // the hex digest of the body, which the service checks; PutOrLink makes it
	IfNoneMatch string            // "*": write only if there is no such object
	IfMatch     string            // an ETag: write only if the object is that version
}

// Info is what is known of an object.
type Info struct {
	Size        int64
	ETag        string
	ContentType string
	Modified    time.Time
	Meta        map[string]string
}

func (c *Client) do(ctx context.Context, method, path string, q url.Values, hdr http.Header, body io.Reader, length int64) (*http.Response, error) {
	target := path
	if len(q) > 0 {
		target += "?" + q.Encode()
	}
	u := (&url.URL{Path: path, RawQuery: q.Encode()}).String()
	req, err := http.NewRequestWithContext(ctx, method, c.Base+u, body)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.ContentLength = length
	}
	for k, v := range hdr {
		req.Header[k] = v
	}
	req.Header.Set("Authorization", "Bearer "+auth.SignTarget(c.Secret, c.Tenant, method, target, time.Now().Unix()))
	req.Header.Set("X-Blobgate-Tenant", c.Tenant)
	h := c.HTTP
	if h == nil {
		h = http.DefaultClient
	}
	return h.Do(req)
}

func (c *Client) obj(store, key string) string { return "/v1/stores/" + store + "/objects/" + key }

func failure(resp *http.Response) error {
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	e := &Error{Status: resp.StatusCode}
	var j struct{ Error, Detail string }
	if json.Unmarshal(data, &j) == nil {
		e.Kind, e.Detail = j.Error, j.Detail
	} else {
		e.Detail = strings.TrimSpace(string(data))
	}
	return e
}

func header(o *Options) http.Header {
	h := http.Header{}
	if o == nil {
		return h
	}
	if o.ContentType != "" {
		h.Set("Content-Type", o.ContentType)
	}
	for k, v := range o.Meta {
		h.Set("X-Blobgate-Meta-"+k, v)
	}
	if o.SHA256 != "" {
		h.Set("X-Content-SHA256", o.SHA256)
	}
	if o.IfNoneMatch != "" {
		h.Set("If-None-Match", o.IfNoneMatch)
	}
	if o.IfMatch != "" {
		h.Set("If-Match", o.IfMatch)
	}
	return h
}

// Put writes an object from body. size is the length of the body, or -1 when it is not known (the service then cuts it into blocks as it comes).
func (c *Client) Put(ctx context.Context, store, key string, body io.Reader, size int64, o *Options) error {
	if size == 0 {
		body = http.NoBody
	}
	resp, err := c.do(ctx, http.MethodPut, c.obj(store, key), nil, header(o), body, size)
	if err != nil {
		return err
	}
	if resp.StatusCode != 200 {
		return failure(resp)
	}
	resp.Body.Close()
	return nil
}

// Link makes the key the bytes with this digest and size when the store keeps them already for this tenant (the store's setting dedup is "tenant"), without sending them. It says whether it did; false
// with no error means that the store does not have them: send them with Put.
func (c *Client) Link(ctx context.Context, store, key, sha256hex string, size int64, o *Options) (bool, error) {
	b, _ := json.Marshal(map[string]any{"sha256": sha256hex, "size": size})
	h := header(o)
	h.Del("X-Content-SHA256")
	resp, err := c.do(ctx, http.MethodPost, c.obj(store, key), url.Values{"link": {""}}, h, bytes.NewReader(b), int64(len(b)))
	if err != nil {
		return false, err
	}
	if resp.StatusCode == 200 {
		resp.Body.Close()
		return true, nil
	}
	if e, ok := failure(resp).(*Error); ok && e.Status == 404 && e.Kind == "no_such_content" {
		return false, nil
	} else {
		return false, e
	}
}

// PutOrLink writes the bytes of r (which is read twice at most: once to make its digest, once to send it), and does not send them when the store has them. linked says which happened. A store that is
// not set to dedup "tenant" answers the link with an error that is not an answer of "no": PutOrLink then sends the bytes.
func (c *Client) PutOrLink(ctx context.Context, store, key string, r io.ReadSeeker, o *Options) (linked bool, err error) {
	h := sha256.New()
	size, err := io.Copy(h, r)
	if err != nil {
		return false, err
	}
	sum := hex.EncodeToString(h.Sum(nil))
	if size > 0 {
		ok, err := c.Link(ctx, store, key, sum, size, o)
		var e *Error
		switch {
		case err == nil && ok:
			return true, nil
		case err == nil, errors.As(err, &e) && e.Kind == "dedup_not_tenant":
		default:
			return false, err
		}
	}
	if _, err := r.Seek(0, io.SeekStart); err != nil {
		return false, err
	}
	oo := Options{}
	if o != nil {
		oo = *o
	}
	oo.SHA256 = sum
	return false, c.Put(ctx, store, key, r, size, &oo)
}

func infoOf(resp *http.Response) *Info {
	in := &Info{ETag: resp.Header.Get("ETag"), ContentType: resp.Header.Get("Content-Type"), Meta: map[string]string{}}
	in.Size, _ = strconv.ParseInt(resp.Header.Get("Content-Length"), 10, 64)
	if cr := resp.Header.Get("Content-Range"); cr != "" {
		if i := strings.LastIndexByte(cr, '/'); i >= 0 {
			in.Size, _ = strconv.ParseInt(cr[i+1:], 10, 64)
		}
	}
	in.Modified, _ = http.ParseTime(resp.Header.Get("Last-Modified"))
	for k, v := range resp.Header {
		if rest, ok := strings.CutPrefix(k, "X-Blobgate-Meta-"); ok && len(v) > 0 {
			in.Meta[strings.ToLower(rest)] = v[0]
		}
	}
	return in
}

// Stat is what is known of an object, without its bytes.
func (c *Client) Stat(ctx context.Context, store, key string) (*Info, error) {
	resp, err := c.do(ctx, http.MethodHead, c.obj(store, key), nil, nil, nil, 0)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, &Error{Status: resp.StatusCode, Kind: map[bool]string{true: "no_such_object"}[resp.StatusCode == 404], Detail: http.StatusText(resp.StatusCode)}
	}
	return infoOf(resp), nil
}

// Get reads bytes from..to (both included) of an object; to < 0 reads to its end, and from = 0 with to < 0 reads it all. The body is to be closed.
func (c *Client) Get(ctx context.Context, store, key string, from, to int64) (io.ReadCloser, *Info, error) {
	h := http.Header{}
	if from > 0 || to >= 0 {
		r := "bytes=" + strconv.FormatInt(from, 10) + "-"
		if to >= 0 {
			r += strconv.FormatInt(to, 10)
		}
		h.Set("Range", r)
	}
	resp, err := c.do(ctx, http.MethodGet, c.obj(store, key), nil, h, nil, 0)
	if err != nil {
		return nil, nil, err
	}
	if resp.StatusCode != 200 && resp.StatusCode != 206 {
		return nil, nil, failure(resp)
	}
	return resp.Body, infoOf(resp), nil
}

// Delete removes an object (nothing is an error: deleting what is not there is done). In a store with a trash the object is kept there for the time the store says.
func (c *Client) Delete(ctx context.Context, store, key string) error {
	resp, err := c.do(ctx, http.MethodDelete, c.obj(store, key), nil, nil, nil, 0)
	if err != nil {
		return err
	}
	if resp.StatusCode != 200 {
		return failure(resp)
	}
	resp.Body.Close()
	return nil
}

// ListOptions say which keys, and how many.
type ListOptions struct {
	Prefix    string
	Delimiter string // "/": the keys under a folder are folded into one prefix
	After     string // the cursor of the page before (Listing.Next)
	Limit     int    // 0: the service's default
}

// Listed is one object of a listing.
type Listed struct {
	Key         string `json:"key"`
	Size        int64  `json:"size"`
	ETag        string `json:"etag"`
	ContentType string `json:"content_type"`
	Modified    int64  `json:"modified"`
}

// Listing is a page of keys.
type Listing struct {
	Objects  []Listed `json:"objects"`
	Prefixes []string `json:"prefixes"`
	Next     string   `json:"next"` // "": the end
}

// List lists the keys of the tenant in the store, one page.
func (c *Client) List(ctx context.Context, store string, o ListOptions) (*Listing, error) {
	q := url.Values{}
	if o.Prefix != "" {
		q.Set("prefix", o.Prefix)
	}
	if o.Delimiter != "" {
		q.Set("delimiter", o.Delimiter)
	}
	if o.After != "" {
		q.Set("after", o.After)
	}
	if o.Limit > 0 {
		q.Set("limit", strconv.Itoa(o.Limit))
	}
	resp, err := c.do(ctx, http.MethodGet, "/v1/stores/"+store+"/objects", q, nil, nil, 0)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != 200 {
		return nil, failure(resp)
	}
	defer resp.Body.Close()
	var l Listing
	if err := json.NewDecoder(resp.Body).Decode(&l); err != nil {
		return nil, err
	}
	return &l, nil
}

// ListAll calls f for every object under the prefix, page after page.
func (c *Client) ListAll(ctx context.Context, store, prefix string, f func(Listed) error) error {
	after := ""
	for {
		l, err := c.List(ctx, store, ListOptions{Prefix: prefix, After: after, Limit: 1000})
		if err != nil {
			return err
		}
		for _, o := range l.Objects {
			if err := f(o); err != nil {
				return err
			}
		}
		if l.Next == "" {
			return nil
		}
		after = l.Next
	}
}
