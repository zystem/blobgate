package api

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/rclone/rclone/fs"

	"github.com/zystem/blobgate/internal/store"
)

// slowNode is a node that takes its time with every file it is asked to take or give.
type slowNode struct {
	fs.Fs
	wait  time.Duration
	slept *atomic.Int64 // how often it was made to wait: the test means nothing if the node was not used
}

func (s slowNode) Put(ctx context.Context, in io.Reader, src fs.ObjectInfo, opts ...fs.OpenOption) (fs.Object, error) {
	s.slept.Add(1)
	time.Sleep(s.wait)
	return s.Fs.Put(ctx, in, src, opts...)
}

func (s slowNode) NewObject(ctx context.Context, remote string) (fs.Object, error) {
	s.slept.Add(1)
	time.Sleep(s.wait)
	return s.Fs.NewObject(ctx, remote)
}

// Finding 6 of the review of 2026-11-22, its acceptance: mixed reads, sealed uploads in parts (some parts sent twice) and a slow node, all at once, with few places. Every call is answered (the SDK tries
// again when it is told to), every object reads as it was written, no place and no slot stays taken, and what a node missed is repaired.
func TestReadsSealedPartsAndASlowNodeTogether(t *testing.T) {
	r := newRig(t, func(c *Config) { c.MaxHeld = 6; c.MaxReads = 3 })
	var slept atomic.Int64
	r.s.Reg.OpenHook = func(m *store.Mirror) {
		m.Nodes[0].F = slowNode{Fs: m.Nodes[0].F, wait: 100 * time.Millisecond, slept: &slept}
	}
	r.mirrorSealed(1, 1, map[string]string{"1": "a-long-random-text-of-at-least-16-characters"})
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	c := r.s3ClientTries("001", secret1, 8)
	shared := pattern(2*BlockSize + 777)
	if _, err := c.PutObject(ctx, &s3.PutObjectInput{Bucket: aws.String("main"), Key: aws.String("shared"), Body: bytes.NewReader(shared)}); err != nil {
		t.Fatal(err)
	}
	read := func(key string, want []byte) error {
		got, err := c.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String("main"), Key: aws.String(key)})
		if err != nil {
			return err
		}
		defer got.Body.Close()
		data, err := io.ReadAll(got.Body)
		if err == nil && !bytes.Equal(data, want) {
			err = fmt.Errorf("%s: %d bytes that are not the object's", key, len(data))
		}
		return err
	}
	var wg sync.WaitGroup
	errs := make(chan error, 64)
	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 3 {
				if err := read("shared", shared); err != nil {
					errs <- fmt.Errorf("read: %w", err)
				}
			}
		}()
	}
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			key := fmt.Sprintf("parts-%d", i)
			cu, err := c.CreateMultipartUpload(ctx, &s3.CreateMultipartUploadInput{Bucket: aws.String("main"), Key: aws.String(key)})
			if err != nil {
				errs <- err
				return
			}
			var whole []byte
			var done []types.CompletedPart
			for n := 1; n <= 3; n++ {
				p := pattern(BlockSize)
				if n == 3 {
					p = pattern(1000 + i)
				}
				p = append([]byte{byte(i), byte(n)}, p[2:]...)
				var tag *string
				for range 2 { // every part is sent twice, as a client that lost the first answer does
					up, err := c.UploadPart(ctx, &s3.UploadPartInput{Bucket: aws.String("main"), Key: aws.String(key), UploadId: cu.UploadId, PartNumber: aws.Int32(int32(n)), Body: bytes.NewReader(p)})
					if err != nil {
						errs <- fmt.Errorf("part: %w", err)
						return
					}
					tag = up.ETag
				}
				whole = append(whole, p...)
				done = append(done, types.CompletedPart{ETag: tag, PartNumber: aws.Int32(int32(n))})
			}
			if _, err := c.CompleteMultipartUpload(ctx, &s3.CompleteMultipartUploadInput{Bucket: aws.String("main"), Key: aws.String(key), UploadId: cu.UploadId,
				MultipartUpload: &types.CompletedMultipartUpload{Parts: done}}); err != nil {
				errs <- fmt.Errorf("completion: %w", err)
				return
			}
			if err := read(key, whole); err != nil {
				errs <- fmt.Errorf("read of the upload: %w", err)
			}
		}()
	}
	wg.Wait()
	close(errs)
	if slept.Load() < 24 {
		t.Fatalf("the slow node was asked %d times", slept.Load())
	}
	t.Logf("the slow node was asked %d times", slept.Load())
	for err := range errs {
		t.Error(err)
	}
	for i := 0; i < 100 && (len(r.s.held) > 0 || len(r.s.reads) > 0 || len(r.s.seals) > 0); i++ {
		time.Sleep(100 * time.Millisecond)
	}
	if len(r.s.held) != 0 || len(r.s.reads) != 0 || len(r.s.seals) != 0 {
		t.Errorf("places taken after all calls ended: held %d, reads %d, seals %d", len(r.s.held), len(r.s.reads), len(r.s.seals))
	}
	w := r.worker("w", nil)
	if ok, _ := w.Lease(ctx, "repair"); !ok {
		t.Fatal("lease")
	}
	for i := 0; i < 10 && r.count("storage_repair") > 0; i++ {
		w.Pass(ctx)
		time.Sleep(time.Second)
	}
	if a, b := files(r.nodes[0]), files(r.nodes[1]); len(a) != len(b) || r.count("storage_repair") != 0 || r.count("storage_parts") != 0 || r.count("storage_uploads") != 0 {
		t.Errorf("after the repair: %d and %d files on the nodes, %d tasks, %d parts, %d uploads", len(a), len(b), r.count("storage_repair"), r.count("storage_parts"), r.count("storage_uploads"))
	}
}
