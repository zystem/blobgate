package api

import (
	"context"
	"io"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

// A part that comes through the S3 door is held whole in memory while it is read from the client. A client that sends most of a part and goes quiet held that memory for ever, and many of them
// were not bounded at all (a part is 8 MiB). Now the places of BLOBGATE_HELD_BLOCKS bound them, and the idle time ends the quiet ones.
func TestMemoryOfQuietPartUploads(t *testing.T) {
	const n, held = 24, 4
	const kind = held - held/4 // a kind of call takes at most three quarters of the places
	was := holdWait
	holdWait = 2 * time.Second
	t.Cleanup(func() { holdWait = was })
	r := newRig(t, func(c *Config) { c.MaxHeld = held; c.ClientIdle = 6 * time.Second })
	r.mirror(0)
	c := r.s3Client("001", secret1)
	ctx := context.Background()
	cu, err := c.CreateMultipartUpload(ctx, &s3.CreateMultipartUploadInput{Bucket: aws.String("main"), Key: aws.String("big")})
	if err != nil {
		t.Fatal(err)
	}
	pre := s3.NewPresignClient(c)
	base := heapNow()
	const limit = (held + 3) << 23

	var mu sync.Mutex
	var wg sync.WaitGroup
	codes := map[int]int{}
	started := sync.WaitGroup{}
	started.Add(n)
	stop := make(chan struct{})
	for i := 1; i <= n; i++ {
		ps, err := pre.PresignUploadPart(ctx, &s3.UploadPartInput{Bucket: aws.String("main"), Key: aws.String("big"), UploadId: cu.UploadId, PartNumber: aws.Int32(int32(i)), ContentLength: aws.Int64(BlockSize)})
		if err != nil {
			t.Fatal(err)
		}
		pr, pw := io.Pipe()
		req, _ := http.NewRequest("PUT", ps.URL, pr)
		req.ContentLength = BlockSize
		for k, v := range ps.SignedHeader {
			if k != "Host" {
				req.Header[k] = v
			}
		}
		go func() {
			_, _ = pw.Write(pattern(6 << 20))
			started.Done()
			<-stop
			pw.CloseWithError(io.ErrClosedPipe)
		}()
		wg.Add(1)
		go func() {
			defer wg.Done()
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				return
			}
			resp.Body.Close()
			mu.Lock()
			codes[resp.StatusCode]++
			mu.Unlock()
		}()
	}
	started.Wait()
	time.Sleep(3 * time.Second) // the waiting ones have given up (2 s), the quiet ones are not yet cut (6 s)
	got := heapNow() - base
	t.Logf("%d part uploads that sent 6 MiB of 8 and stopped hold %d MiB with %d places", n, got>>20, held)
	if got > limit {
		t.Errorf("%d quiet part uploads hold %d MiB, more than %d places of a block", n, got>>20, held)
	}
	mu.Lock()
	t.Logf("answers so far: %v", codes)
	if codes[503] != n-kind {
		t.Errorf("answers %v: want %d of 503", codes, n-kind)
	}
	mu.Unlock()
	deadline := time.Now().Add(15 * time.Second)
	for len(r.s.held) != 0 {
		if time.Now().After(deadline) {
			t.Fatalf("%d places stay taken by part uploads that sent nothing for the idle time", len(r.s.held))
		}
		time.Sleep(200 * time.Millisecond)
	}
	close(stop)
	wg.Wait()
}
