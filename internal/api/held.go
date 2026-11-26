package api

// The places that bound the blocks held in memory for clients.

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/zystem/blobgate/internal/metrics"
)

// errBusy is a call that waited for a place to hold a block and found none.
var errBusy = errors.New("the module holds as many blocks for clients as it may")

// The kinds of call that hold a block in memory for a client. Each may take at most three quarters of the places (BLOBGATE_HELD_BLOCKS), so that a flood of one kind (slow readers) cannot keep the
// other (writers, parts) out for as long as it lasts: the last quarter is for the other kind.
const (
	heldRead  = 0 // a block of a read, until it is sent
	heldWrite = 1 // the buffer of a write of many blocks or of no known size, and a part through the S3 door, until it is on the nodes
)

// hold takes a place for one block that is held in memory for a client: one of the kind, and one of the total. The first block of a call waits a short time and then gives up (the caller answers 503 with
// Retry-After); one that is on its way waits for as long as the client does.
func (s *Server) hold(ctx context.Context, tenant string, kind int, first bool) error {
	places := s.placesOf(tenant, kind)
	taken := 0
	for taken < len(places) { // nothing to wait for: the common case
		select {
		case places[taken] <- struct{}{}:
			taken++
			continue
		default:
		}
		break
	}
	if taken == len(places) {
		metrics.Held(kindName[kind], 1)
		return nil
	}
	release(places[:taken])
	start := time.Now()
	defer func() { metrics.HeldWait(kindName[kind], time.Since(start)) }()
	var timeout <-chan time.Time
	if first {
		t := time.NewTimer(holdWait)
		defer t.Stop()
		timeout = t.C
	}
	for i, p := range places { // always in the same order (tenant, kind, total), so that two calls cannot wait for each other
		select {
		case p <- struct{}{}:
		case <-ctx.Done():
			release(places[:i])
			return ctx.Err()
		case <-timeout:
			release(places[:i])
			metrics.Refused("busy")
			return errBusy
		}
	}
	metrics.Held(kindName[kind], 1)
	return nil
}

// placesOf is what a call has to take a place in: the places of its tenant (when a tenant has a limit of its own), of its kind, and all of them.
func (s *Server) placesOf(tenant string, kind int) []chan struct{} {
	if t := s.heldTenant[tenant]; t != nil {
		return []chan struct{}{t, s.heldKind[kind], s.held}
	}
	return []chan struct{}{s.heldKind[kind], s.held}
}

func release(places []chan struct{}) {
	for _, p := range places {
		<-p
	}
}

func (s *Server) unhold(tenant string, kind int) {
	release(s.placesOf(tenant, kind))
	metrics.Held(kindName[kind], -1)
}

var kindName = [2]string{"read", "write"}

// holdWait is how long the first block of a call waits for a place.
var holdWait = 30 * time.Second

func busy(w http.ResponseWriter) {
	w.Header().Set("Retry-After", "5")
	problem(w, 503, "busy", "the module holds as many blocks for clients as it may (BLOBGATE_HELD_BLOCKS); try again")
}
