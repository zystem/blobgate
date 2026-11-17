package api

import (
	"bytes"
	"encoding/json"
	"encoding/xml"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/zystem/blobgate/internal/layout"
	"github.com/zystem/blobgate/internal/metrics"
	"github.com/zystem/blobgate/internal/rq"
	"github.com/zystem/blobgate/internal/store"
)

// pipeResp is the ResponseWriter that the read of the source writes into when an object is copied by bytes: what it writes goes to the pipe that the write of the destination reads, an error of the
// core (status 400 and more) is kept as it is and ends the pipe with an error.
type pipeResp struct {
	pw   *io.PipeWriter
	h    http.Header
	code int
	buf  bytes.Buffer
}

func (p *pipeResp) Header() http.Header { return p.h }
func (p *pipeResp) WriteHeader(c int) {
	if p.code == 0 {
		p.code = c
	}
}
func (p *pipeResp) Write(b []byte) (int, error) {
	if p.code == 0 {
		p.code = 200
	}
	if p.code >= 400 {
		return p.buf.Write(b)
	}
	return p.pw.Write(b)
}

// s3Copy is CopyObject (`PUT` with `x-amz-copy-source`). When the source and the destination are in one store and the source is a set of blocks (deduplication), nothing is read and nothing is written
// to a node: the destination takes a reference on the set, in one transaction (the same as `?link`, by the name of the set and not by a digest, so that an object made by a multipart upload is copied too).
// Any other copy reads the source and writes the destination, through a pipe, so that no more than a block is held. The metadata is kept (`x-amz-metadata-directive: COPY`, the default) or replaced by
// that of the request (`REPLACE`). Conditions on the source (`x-amz-copy-source-if-*`) and versions are not answered.
func (s *Server) s3Copy(w http.ResponseWriter, r *http.Request, tenant, bucket, key string) {
	ctx := r.Context()
	raw, query, _ := strings.Cut(r.Header.Get("X-Amz-Copy-Source"), "?")
	if query != "" {
		s3Fail(w, r, 501, "NotImplemented", "a version of the source (versionId) is not supported: a store keeps no versions")
		return
	}
	for name := range r.Header {
		if strings.HasPrefix(strings.ToLower(name), "x-amz-copy-source-if-") {
			s3Fail(w, r, 501, "NotImplemented", name+" is not supported")
			return
		}
	}
	p, err := url.PathUnescape(raw)
	sb, sk, ok := strings.Cut(strings.TrimPrefix(p, "/"), "/")
	if err != nil || !ok || sb == "" || sk == "" || layout.Valid(sk) != "" {
		s3Fail(w, r, 400, "InvalidArgument", "x-amz-copy-source is /bucket/key (the key is URL-encoded)")
		return
	}
	directive := strings.ToUpper(r.Header.Get("X-Amz-Metadata-Directive"))
	if directive == "" {
		directive = "COPY"
	}
	if directive != "COPY" && directive != "REPLACE" {
		s3Fail(w, r, 400, "InvalidArgument", "x-amz-metadata-directive is COPY or REPLACE")
		return
	}
	if directive == "COPY" && sb == bucket && sk == key {
		s3Fail(w, r, 400, "InvalidRequest", "an object cannot be copied onto itself unless its metadata is replaced (x-amz-metadata-directive: REPLACE)")
		return
	}
	sfull, dfull := tenant+"/"+sk, tenant+"/"+key
	cw := &s3w{w: w, h: http.Header{}}
	sm := s.loadStore(cw, ctx, sb)
	if sm == nil {
		cw.sendError(r, nil)
		return
	}
	dm := sm
	if sb != bucket {
		if dm = s.loadStore(cw, ctx, bucket); dm == nil {
			cw.sendError(r, nil)
			return
		}
	}
	o, err := s.getRow(ctx, sb, sfull)
	if err != nil {
		s3Fail(w, r, 500, "InternalError", err.Error())
		return
	}
	if o == nil {
		s3Fail(w, r, 404, "NoSuchKey", "the source of the copy is not there")
		return
	}
	// the metadata of the destination
	om := o.meta
	if directive == "REPLACE" {
		var he *herr
		if om, he = readMeta(s.internal(r, http.MethodPut, "/", nil, http.NoBody, 0, nil)); he != nil {
			he.write(cw)
			cw.sendError(r, nil)
			return
		}
	}
	if e := s.checkQuota(ctx, dm, dfull, o.size); e != nil {
		e.write(cw)
		cw.sendError(r, nil)
		return
	}
	if sb == bucket && o.cid != "" {
		if !s.copyBySet(w, r, dm, o, om, dfull) {
			return
		}
	} else if !s.copyByBytes(w, r, cw, o, om, tenant, sb, sk, bucket, key) {
		return
	}
	out := struct {
		XMLName      xml.Name `xml:"CopyObjectResult"`
		NS           string   `xml:"xmlns,attr"`
		ETag         string
		LastModified string
	}{NS: s3NS}
	if d, err := s.getRow(ctx, bucket, dfull); err == nil && d != nil {
		out.ETag, out.LastModified = d.etag(), time.Unix(d.written, 0).UTC().Format("2006-01-02T15:04:05.000Z")
	}
	s3Reply(w, 200, out)
}

// copyBySet makes the destination point to the set of the source. False when it answered with an error.
func (s *Server) copyBySet(w http.ResponseWriter, r *http.Request, m *store.Mirror, o *objectRow, om objMeta, dfull string) bool {
	ctx, st := r.Context(), m.Name
	old, err := s.getRowAny(ctx, st, dfull)
	if err != nil {
		s3Fail(w, r, 500, "InternalError", err.Error())
		return false
	}
	pj, _ := json.Marshal(om.Pairs)
	if om.Pairs == nil {
		pj = []byte("{}")
	}
	s.at("copy:before")
	stmts := []rq.Stmt{s.releaseSet(st, dfull, false)}
	stmts = append(stmts, s.purgeTasks(m, dfull, "")...)
	stmts = append(stmts,
		rq.S("UPDATE storage_sets SET refs = refs + 1, zero_since = 0 WHERE store = ? AND cid = ?", st, o.cid),
		// the cid is read from the set: a set that the worker removed meanwhile gives NULL, the column refuses it and the transaction is rolled back
		rq.S("INSERT INTO storage_objects (store, key, size, written_at, generation, block_size, blocks, enc, content_type, meta, deleted_at, cid, md5) "+
			"VALUES (?, ?, ?, ?, '', 0, '[]', 0, ?, ?, 0, (SELECT cid FROM storage_sets WHERE store = ? AND cid = ?), ?) "+
			"ON CONFLICT(store, key) DO UPDATE SET size = excluded.size, written_at = excluded.written_at, generation = '', block_size = 0, blocks = '[]', enc = 0, "+
			"content_type = excluded.content_type, meta = excluded.meta, deleted_at = 0, cid = excluded.cid, md5 = excluded.md5",
			st, dfull, o.size, s.now(), om.ContentType, string(pj), st, o.cid, o.md5))
	if _, err := s.DB.Exec(ctx, true, stmts...); err != nil {
		var re *rq.Error
		if errors.As(err, &re) && strings.Contains(re.Msg, "NOT NULL") {
			s3Fail(w, r, 404, "NoSuchKey", "the source of the copy was removed just now")
			return false
		}
		s3Fail(w, r, 500, "InternalError", err.Error())
		return false
	}
	s.at("copy:after-tx")
	if old != nil && old.cid == "" && old.generation != "" {
		s.purgeNow(ctx, m, dfull, old.generation)
	}
	metrics.Deduplicated(st, o.size)
	return true
}

// copyByBytes reads the source and writes the destination, a block at a time. False when it answered with an error.
func (s *Server) copyByBytes(w http.ResponseWriter, r *http.Request, cw *s3w, o *objectRow, om objMeta, tenant, sb, sk, bucket, key string) bool {
	pr, pw := io.Pipe()
	src := &pipeResp{pw: pw, h: http.Header{}}
	get := s.internal(r, http.MethodGet, "/v1/stores/"+sb+"/objects/"+sk, nil, http.NoBody, 0, nil)
	get.Header = http.Header{} // the conditions and the range of the copy are not those of the read of the source
	done := make(chan struct{})
	go func() {
		defer close(done)
		s.object(src, get, tenant, sb, sk)
		if src.code >= 400 {
			pw.CloseWithError(errors.New("the source of the copy could not be read"))
			return
		}
		_ = pw.Close()
	}()
	put := s.internal(r, http.MethodPut, "/v1/stores/"+bucket+"/objects/"+key, nil, pr, o.size, nil)
	for _, h := range []string{"Content-MD5", "If-Match", "If-None-Match", "Content-Type"} {
		put.Header.Del(h)
	}
	for k := range put.Header {
		if strings.HasPrefix(k, metaHeader) {
			delete(put.Header, k)
		}
	}
	if om.ContentType != "" {
		put.Header.Set("Content-Type", om.ContentType)
	}
	for k, v := range om.Pairs {
		put.Header.Set(metaHeader+k, v)
	}
	s.object(cw, put, tenant, bucket, key)
	_ = pr.CloseWithError(io.ErrClosedPipe) // a write that stopped early must not leave the read of the source waiting for a reader
	<-done
	switch {
	case src.code >= 400:
		var e apiError
		_ = json.Unmarshal(src.buf.Bytes(), &e)
		st, code := s3Code(src.code, e.Error)
		if src.code == 404 {
			code = "NoSuchKey"
		}
		s3Fail(w, r, st, code, "the source of the copy: "+e.Detail)
		return false
	case cw.failed():
		cw.sendError(r, nil)
		return false
	}
	return true
}
