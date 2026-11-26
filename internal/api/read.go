package api

// Reading an object: its headers, ranges and the blocks sent to the client.

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/zystem/blobgate/internal/layout"
	"github.com/zystem/blobgate/internal/store"
)

func (s *Server) stat(w http.ResponseWriter, r *http.Request, m *store.Mirror, full string) {
	o, err := s.getRow(r.Context(), m.Name, full)
	if err != nil {
		s.fail(w, err)
		return
	}
	if o == nil {
		problem(w, 404, "no_such_object", "no such object")
		return
	}
	writeJSON(w, 200, map[string]any{"size": o.size, "etag": o.etag(), "content_type": o.meta.ContentType, "modified": o.written, "meta": o.meta.Pairs})
}

// parseRange reads one range of a Range header: `bytes=a-b`, `bytes=a-` (to the end) and `bytes=-n` (the last n bytes). ok is false for anything else, a list of ranges included. For a suffix, a is -1 and b is n.
func parseRange(h string) (a, b int64, ok bool) {
	spec, found := strings.CutPrefix(h, "bytes=")
	if !found || strings.Contains(spec, ",") {
		return 0, 0, false
	}
	lo, hi, found := strings.Cut(spec, "-")
	if !found {
		return 0, 0, false
	}
	var err error
	switch {
	case lo == "" && hi != "": // the last n bytes
		b, err = strconv.ParseInt(hi, 10, 64)
		return -1, b, err == nil && b > 0
	case lo != "" && hi == "": // from a to the end
		a, err = strconv.ParseInt(lo, 10, 64)
		return a, -1, err == nil && a >= 0
	case lo != "" && hi != "":
		a, err = strconv.ParseInt(lo, 10, 64)
		if err != nil {
			return 0, 0, false
		}
		b, err = strconv.ParseInt(hi, 10, 64)
		return a, b, err == nil && a >= 0 && b >= a
	}
	return 0, 0, false
}

// notModified answers If-None-Match / If-Match of a read: it returns true when the answer has been given (304 or 412).
func notModified(w http.ResponseWriter, r *http.Request, o *objectRow) bool {
	if im := r.Header.Get("If-Match"); im != "" && im != "*" && !etagMatches(im, o.etag(), true) {
		problem(w, 412, "precondition_failed", "the object is not the version If-Match names")
		return true
	}
	if inm := r.Header.Get("If-None-Match"); inm != "" && (inm == "*" || etagMatches(inm, o.etag(), false)) {
		o.setHeaders(w)
		w.WriteHeader(http.StatusNotModified)
		return true
	}
	return false
}

// read answers a GET: the whole object, or the range named by Range (206). The bytes are streamed, one block at a time.
func (s *Server) read(w http.ResponseWriter, r *http.Request, m *store.Mirror, full string) {
	h := r.Header.Get("Range")
	var a, b int64
	ranged := h != ""
	if ranged {
		var ok bool
		if a, b, ok = parseRange(h); !ok {
			problem(w, 400, "invalid_range", "Range: bytes=a-b, bytes=a- or bytes=-n (one range)")
			return
		}
	}
	o, err := s.getRow(r.Context(), m.Name, full)
	if err != nil {
		s.fail(w, err)
		return
	}
	if o == nil {
		problem(w, 404, "no_such_object", "no such object")
		return
	}
	if notModified(w, r, o) {
		return
	}
	if !ranged {
		if o.size == 0 {
			o.setHeaders(w)
			w.Header().Set("Content-Length", "0")
			w.WriteHeader(200)
			return
		}
		s.streamRange(w, r, m, full, o, 0, o.size-1, false)
		return
	}
	switch {
	case a < 0: // the last b bytes
		a, b = max(o.size-b, 0), o.size-1
	case b < 0: // to the end
		b = o.size - 1
	}
	if o.size == 0 || a >= o.size {
		w.Header().Set("Content-Range", fmt.Sprintf("bytes */%d", o.size))
		problem(w, 416, "range_not_satisfiable", fmt.Sprintf("the object is %d bytes", o.size))
		return
	}
	s.streamRange(w, r, m, full, o, a, min(b, o.size-1), true)
}

// head answers a HEAD: what a GET would say, with no body.
func (s *Server) head(w http.ResponseWriter, r *http.Request, m *store.Mirror, full string) {
	o, err := s.getRow(r.Context(), m.Name, full)
	if err != nil {
		s.fail(w, err)
		return
	}
	if o == nil {
		w.WriteHeader(404)
		return
	}
	if notModified(w, r, o) {
		return
	}
	o.setHeaders(w)
	w.Header().Set("Content-Length", strconv.FormatInt(o.size, 10))
	w.WriteHeader(200)
}

// streamRange sends bytes a..b of the object (b is within it). A block is read whole and checked against its digest before a byte of it is sent; the first block is read before the status is written, so
// that an object that cannot be read is an error and not a broken answer. partial says whether the answer is 206 (a range) or 200 (the whole).
func (s *Server) streamRange(w http.ResponseWriter, r *http.Request, m *store.Mirror, full string, o *objectRow, a, b int64, partial bool) {
	first, last := int(a/o.blockSize), int(b/o.blockSize)
	readBlock := func(i int) ([]byte, error) {
		select {
		case s.reads <- struct{}{}:
		case <-r.Context().Done():
			return nil, r.Context().Err()
		}
		defer func() { <-s.reads }()
		data, bad, err := m.ReadBlockBad(r.Context(), layout.BlockPath(o.dir(full), o.generation, i), o.blocks[i], o.storedLen(i))
		if len(bad) > 0 { // a copy that is there and is wrong: a good one is put in its place (the task is made even when this read ends with an error)
			s.queue(context.WithoutCancel(r.Context()), m.Name, layout.BlockKey(o.dir(full), o.generation, i), "copy", bad)
		}
		if err != nil || o.enc == 0 {
			return data, err
		}
		if m.Enc == nil {
			return nil, &store.ErrNoKey{ID: o.enc}
		}
		if o.cid != "" {
			return m.Enc.OpenConv(scopeOf(m, full), o.plain[i], data)
		}
		return m.Enc.Open(full, o.generation, i, data)
	}
	slice := func(i int, data []byte) []byte {
		start := int64(i) * o.blockSize
		lo, hi := max(a-start, 0), min(b-start+1, int64(len(data)))
		return data[lo:hi]
	}
	if err := s.hold(r.Context(), tenantOfKey(full), heldRead, true); err != nil {
		if errors.Is(err, errBusy) {
			busy(w)
		}
		return // the client has gone
	}
	data, err := readBlock(first)
	if err != nil {
		s.unhold(tenantOfKey(full), heldRead)
		s.fail(w, err)
		return
	}
	o.setHeaders(w)
	w.Header().Set("Content-Length", strconv.FormatInt(b-a+1, 10))
	if partial {
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", a, b, o.size))
		w.WriteHeader(206)
	} else {
		w.WriteHeader(200)
	}
	for i := first; ; i++ {
		_, werr := w.Write(slice(i, data))
		data = nil
		s.unhold(tenantOfKey(full), heldRead) // the block is out of memory once it is sent: a client that is slow holds a place for as long as it takes the block, and not a slot of reads
		if werr != nil || i == last {
			return
		}
		if err := s.hold(r.Context(), tenantOfKey(full), heldRead, false); err != nil {
			panic(http.ErrAbortHandler)
		}
		if data, err = readBlock(i + 1); err != nil {
			s.unhold(tenantOfKey(full), heldRead)
			// after the first piece an error cannot be a status: the connection is cut, and the reader, who gets less than it asked for, knows
			panic(http.ErrAbortHandler)
		}
	}
}
