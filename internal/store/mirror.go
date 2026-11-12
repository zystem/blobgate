package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"path"
	"sync"
	"time"

	"github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/fs/object"

	"github.com/zystem/blobgate/internal/space"
)

// Node is an opened node of a store.
type Node struct {
	ID   string
	Spec NodeSpec
	F    fs.Fs

	slots chan struct{} // how many writes a node takes at once (a server limits the connections and the handshakes it takes from one client; 24 writers made an SFTP server refuse some)
	mkMu  sync.Mutex    // directories are made one at a time on a node: two writers that make the same parent at the same moment make some servers answer "file exists" (SFTP) and fail
}

// makeDir makes the directory of a block before it is written, under the node's lock. A failure is not reported: the write says what really cannot be done.
func (n *Node) makeDir(ctx context.Context, rel string) {
	dir := path.Dir(rel)
	if dir == "." || dir == "/" {
		return
	}
	n.mkMu.Lock()
	defer n.mkMu.Unlock()
	_ = n.F.Mkdir(ctx, dir)
}

// Mirror writes to all its nodes, confirms by MinAcks, and reads from the first node that has a block that checks out.
type Mirror struct {
	Name    string
	Nodes   []*Node
	MinAcks int // already resolved: 1..len(Nodes)
	// SpaceURL, if set, is where an S3 store says how much room it has (Garage's /metrics)
	SpaceURL string

	spaceMu   sync.Mutex
	spaceAt   time.Time
	spaceInfo space.Info
}

// ErrNotFound is returned by a read of a block that no node has in a form that checks out and no node failed on: the block is not there.
var ErrNotFound = errors.New("no such block")

// ErrUnavailable is returned when nodes failed (not answered, or refused) and none could give the block.
type ErrUnavailable struct{ Detail string }

func (e *ErrUnavailable) Error() string { return e.Detail }

// WriteResult says which nodes took a block.
type WriteResult struct {
	SHA256 string
	Bytes  int64
	Failed []*Node // nodes that did not take it (the repair queue gets them)
	Errs   []error // why, in the same order
}

type fanout struct {
	pipes []*io.PipeWriter
	alive []bool
	errs  []error
}

// Put reads size bytes from src and writes them to every node under rel, at the same time, without holding the block: the slowest node sets the pace. It returns when every node
// is done. The error is non-nil when fewer than MinAcks nodes took the block (the bytes that did arrive are left for the caller to delete) or src failed.
func (m *Mirror) Put(ctx context.Context, rel string, size int64, src io.Reader) (WriteResult, error) {
	n := len(m.Nodes)
	// a place on every node, taken in the order of the nodes: two writers that wait for each other's nodes would never end if they took them in another order
	taken := 0
	defer func() {
		for i := 0; i < taken; i++ {
			<-m.Nodes[i].slots
		}
	}()
	for _, node := range m.Nodes {
		select {
		case node.slots <- struct{}{}:
			taken++
		case <-ctx.Done():
			return WriteResult{}, ctx.Err()
		}
	}
	fo := &fanout{pipes: make([]*io.PipeWriter, n), alive: make([]bool, n), errs: make([]error, n)}
	var wg sync.WaitGroup
	readers := make([]*io.PipeReader, n)
	for i, node := range m.Nodes {
		pr, pw := io.Pipe()
		readers[i], fo.pipes[i], fo.alive[i] = pr, pw, true
		wg.Add(1)
		go func(i int, node *Node, pr *io.PipeReader) {
			defer wg.Done()
			node.makeDir(ctx, rel)
			_, err := node.F.Put(ctx, pr, object.NewStaticObjectInfo(rel, time.Now(), size, true, nil, nil))
			if err != nil {
				fo.errs[i] = err
			}
			pr.CloseWithError(errors.New("node is done"))
		}(i, node, pr)
	}
	h := sha256.New()
	var total int64
	buf := make([]byte, 256<<10)
	var srcErr error
	for {
		k, err := src.Read(buf)
		if k > 0 {
			h.Write(buf[:k])
			total += int64(k)
			for i := range fo.pipes {
				if fo.alive[i] {
					if _, werr := fo.pipes[i].Write(buf[:k]); werr != nil {
						fo.alive[i] = false
					}
				}
			}
		}
		if err != nil {
			if err != io.EOF {
				srcErr = err
			}
			break
		}
	}
	if srcErr == nil && total != size {
		srcErr = fmt.Errorf("the body is %d bytes, not the %d announced", total, size)
	}
	for _, pw := range fo.pipes {
		if srcErr != nil {
			pw.CloseWithError(srcErr)
		} else {
			pw.Close()
		}
	}
	wg.Wait()
	res := WriteResult{SHA256: hex.EncodeToString(h.Sum(nil)), Bytes: total}
	ok := 0
	for i, node := range m.Nodes {
		if fo.errs[i] == nil && fo.alive[i] {
			ok++
			continue
		}
		e := fo.errs[i]
		if e == nil {
			e = errors.New("the node stopped reading")
		}
		res.Failed = append(res.Failed, node)
		res.Errs = append(res.Errs, e)
	}
	if srcErr != nil {
		return res, srcErr
	}
	if ok < m.MinAcks {
		var full []string
		detail := ""
		for i, f := range res.Failed {
			if IsNoSpace(res.Errs[i]) {
				full = append(full, f.Label())
				detail = res.Errs[i].Error()
			}
		}
		if len(full) > 0 {
			return res, &ErrNoSpace{Store: m.Name, Nodes: full, Detail: detail}
		}
		return res, &ErrUnavailable{fmt.Sprintf("%d of %d nodes took the block, %d are needed: %v", ok, n, m.MinAcks, res.Errs)}
	}
	return res, nil
}

// ReadBlock reads one whole block from the first node that has it and whose bytes have the wanted SHA-256: a block that fails the check counts as missing on that node. Memory is the block.
func (m *Mirror) ReadBlock(ctx context.Context, rel, wantSHA string, size int64) ([]byte, error) {
	var failures []string
	missing := 0
	for _, node := range m.Nodes {
		data, err := readWhole(ctx, node, rel, size)
		switch {
		case err == nil && sum(data) == wantSHA:
			return data, nil
		case err == nil:
			failures = append(failures, node.ID+": the block does not match its digest")
		case errors.Is(err, fs.ErrorObjectNotFound) || errors.Is(err, fs.ErrorDirNotFound):
			missing++
		default:
			failures = append(failures, node.ID+": "+err.Error())
		}
	}
	if len(failures) == 0 && missing == len(m.Nodes) {
		return nil, ErrNotFound
	}
	return nil, &ErrUnavailable{fmt.Sprintf("no node could give %s: %v", rel, failures)}
}

func sum(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

func readWhole(ctx context.Context, n *Node, rel string, size int64) ([]byte, error) {
	o, err := n.F.NewObject(ctx, rel)
	if err != nil {
		return nil, err
	}
	rc, err := o.Open(ctx)
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	data, err := io.ReadAll(io.LimitReader(rc, size+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) != size {
		return nil, fmt.Errorf("the block is %d bytes, not %d", len(data), size)
	}
	return data, nil
}

// DeleteDir removes a directory of blocks from every node (one that is not there is fine) and returns the nodes where it failed.
func (m *Mirror) DeleteDir(ctx context.Context, dir string) []*Node {
	var failed []*Node
	var mu sync.Mutex
	var wg sync.WaitGroup
	for _, node := range m.Nodes {
		wg.Add(1)
		go func(node *Node) {
			defer wg.Done()
			if err := removeDir(ctx, node, dir); err != nil {
				mu.Lock()
				failed = append(failed, node)
				mu.Unlock()
			}
		}(node)
	}
	wg.Wait()
	return failed
}

func removeDir(ctx context.Context, n *Node, dir string) error {
	entries, err := n.F.List(ctx, dir)
	if errors.Is(err, fs.ErrorDirNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, e := range entries {
		if o, ok := e.(fs.Object); ok {
			if err := o.Remove(ctx); err != nil && !errors.Is(err, fs.ErrorObjectNotFound) {
				return err
			}
		}
	}
	if err := n.F.Rmdir(ctx, dir); err != nil && !errors.Is(err, fs.ErrorDirNotFound) {
		return err
	}
	return nil
}

// Free is how much room the store has: the least of what its nodes say; Known is false when no node says.
func (m *Mirror) Free(ctx context.Context) space.Info {
	m.spaceMu.Lock()
	defer m.spaceMu.Unlock()
	if time.Since(m.spaceAt) < 5*time.Second {
		return m.spaceInfo
	}
	info := space.Info{}
	if m.SpaceURL != "" {
		if i, ok := garageSpace(ctx, m.SpaceURL); ok {
			info = i
		}
	} else {
		for _, n := range m.Nodes {
			ab := n.F.Features().About
			if ab == nil {
				continue
			}
			cctx, cancel := context.WithTimeout(ctx, 3*time.Second)
			u, err := ab(cctx)
			cancel()
			if err != nil || u == nil || u.Free == nil || u.Total == nil {
				continue
			}
			if !info.Known || *u.Free < info.Free {
				info = space.Info{Known: true, Free: *u.Free, Total: *u.Total}
			}
		}
	}
	m.spaceAt, m.spaceInfo = time.Now(), info
	return info
}

// Check is the round trip of the admin call `:check`: write a block, read it back, delete it, make and remove a directory. It returns "" or what failed.
func (m *Mirror) Check(ctx context.Context) string {
	body := []byte("blobgate store check")
	rel := "_check/probe~check/0"
	res, err := m.Put(ctx, rel, int64(len(body)), bytesReader(body))
	if err != nil {
		return "put: " + err.Error()
	}
	if len(res.Failed) > 0 {
		return fmt.Sprintf("put: node %s: %v", res.Failed[0].ID, res.Errs[0])
	}
	for _, n := range m.Nodes {
		data, err := readWhole(ctx, n, rel, int64(len(body)))
		if err != nil || string(data) != string(body) {
			return fmt.Sprintf("get: node %s: %v", n.ID, err)
		}
	}
	if failed := m.DeleteDir(ctx, "_check/probe~check"); len(failed) > 0 {
		return "delete: node " + failed[0].ID
	}
	for _, n := range m.Nodes {
		if _, err := n.F.NewObject(ctx, rel); err == nil {
			return "delete: the block is still on node " + n.ID
		}
	}
	return ""
}

// NodeByID finds a node of the store.
func (m *Mirror) NodeByID(id string) *Node {
	for _, n := range m.Nodes {
		if n.ID == id {
			return n
		}
	}
	return nil
}

// Verify says whether the node holds the block as it should be: with deep, the bytes are read and their SHA-256 compared; without, only the size (all that a node is asked for in the
// ordinary check). nil when it does; an error that wraps fs.ErrorObjectNotFound when the block is not there.
func (n *Node) Verify(ctx context.Context, rel string, size int64, sha string, deep bool) error {
	if deep {
		data, err := readWhole(ctx, n, rel, size)
		if err != nil {
			return err
		}
		if sum(data) != sha {
			return errors.New("the block does not match its digest")
		}
		return nil
	}
	o, err := n.F.NewObject(ctx, rel)
	if err != nil {
		return err
	}
	if o.Size() != size {
		return fmt.Errorf("the block is %d bytes, not %d", o.Size(), size)
	}
	return nil
}

// RemoveDir removes a directory of blocks from this node (one that is not there is fine).
func (n *Node) RemoveDir(ctx context.Context, dir string) error { return removeDir(ctx, n, dir) }

// RemoveBlock removes one block from this node (one that is not there is fine).
func (n *Node) RemoveBlock(ctx context.Context, rel string) error {
	o, err := n.F.NewObject(ctx, rel)
	if errors.Is(err, fs.ErrorObjectNotFound) || errors.Is(err, fs.ErrorDirNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if err := o.Remove(ctx); err != nil && !errors.Is(err, fs.ErrorObjectNotFound) {
		return err
	}
	return nil
}

// CopyTo gives a node the block, read from another node whose copy has the wanted SHA-256 (a node never checks anything: the digest is the one in the database). The block is held in
// memory while it is copied; repair does one at a time.
func (m *Mirror) CopyTo(ctx context.Context, dst *Node, rel, sha string, size int64) error {
	var data []byte
	var last error
	for _, n := range m.Nodes {
		if n == dst {
			continue
		}
		d, err := readWhole(ctx, n, rel, size)
		if err == nil && sum(d) == sha {
			data = d
			break
		}
		last = err
		if err == nil {
			last = errors.New("the block does not match its digest")
		}
	}
	if data == nil {
		return fmt.Errorf("no other node has a good copy of the block (%v)", last)
	}
	one := &Mirror{Name: m.Name, Nodes: []*Node{dst}, MinAcks: 1}
	res, err := one.Put(ctx, rel, size, bytesReader(data))
	if err != nil {
		return err
	}
	if res.SHA256 != sha {
		return errors.New("the copy does not match its digest")
	}
	return nil
}
