package api

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
)

// hurtNode damages every file of a node with `how` and returns what the files held.
func hurtNode(t *testing.T, dir string, how func([]byte) []byte) map[string][]byte {
	t.Helper()
	was := map[string][]byte{}
	for _, f := range files(dir) {
		b, err := os.ReadFile(filepath.Join(dir, f))
		if err != nil {
			t.Fatal(err)
		}
		was[f] = b
		if err := os.WriteFile(filepath.Join(dir, f), how(b), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if len(was) == 0 {
		t.Fatalf("%s holds no file", dir)
	}
	return was
}

// A node that lost a block (a disk replaced, a directory removed by hand) is put right by the read that walks round it, as one that gives back damaged bytes is: no check is needed. A node that the
// reads do not reach (the first one answers) is put right by the check of the inventory.
func TestLostBlockIsRepairedByARead(t *testing.T) {
	body := pattern(BlockSize + 5000)
	for _, mode := range []string{"plain", "sealed"} {
		t.Run(mode, func(t *testing.T) {
			r := newRig(t, nil)
			if mode == "sealed" {
				r.mirrorSealed(0, 1, map[string]string{"1": key1})
			} else {
				r.mirror(0)
			}
			if code, d := r.put("k/big", body); code != 200 {
				t.Fatalf("put: %d %s", code, d)
			}
			was := map[string][]byte{}
			for _, f := range files(r.nodes[0]) {
				was[f], _ = os.ReadFile(filepath.Join(r.nodes[0], f))
			}
			for f := range was {
				_ = os.Remove(filepath.Join(r.nodes[0], f))
			}
			if code, d := r.get("k/big", 0, len(body)-1); code != 200 && code != 206 || !bytes.Equal(d, body) {
				t.Fatalf("read with a block lost on node 1: %d", code)
			}
			if got := r.count("storage_repair"); got != 2 {
				t.Fatalf("%d repair tasks after the read, want 2 (one for each block)", got)
			}
			w := r.worker("w1", nil)
			w.Lease(context.Background(), "repair")
			w.Pass(context.Background())
			for f, b := range was {
				if got, _ := os.ReadFile(filepath.Join(r.nodes[0], f)); !bytes.Equal(got, b) {
					t.Fatalf("%s was not put back", f)
				}
			}
			if r.count("storage_repair") != 0 {
				t.Fatal("tasks are left")
			}
			// a read that is answered by the first node meets nothing: no task for a node it did not ask
			for f := range was {
				_ = os.Remove(filepath.Join(r.nodes[1], f))
			}
			if code, d := r.get("k/big", 0, len(body)-1); code != 200 && code != 206 || !bytes.Equal(d, body) {
				t.Fatalf("read: %d", code)
			}
			if got := r.count("storage_repair"); got != 0 {
				t.Fatalf("%d tasks for a node that the read did not ask", got)
			}
		})
	}
}

// A copy of an object, and a part that is a range of one, never take damaged bytes from a node: the copy is what the object is, on both nodes.
func TestCopiesOfADamagedNodeAreGood(t *testing.T) {
	r := newRig(t, nil)
	r.dedupStore("", 0, "") // no sets: a copy reads the source and writes the destination
	c := r.s3Client("001", secret1)
	ctx := context.Background()
	body := pattern(BlockSize + 777)
	if _, err := c.PutObject(ctx, &s3.PutObjectInput{Bucket: aws.String("main"), Key: aws.String("src"), Body: bytes.NewReader(body)}); err != nil {
		t.Fatal(err)
	}
	was := hurtNode(t, r.nodes[0], func(b []byte) []byte { c := bytes.Clone(b); c[len(c)/3] ^= 0x55; return c })
	if got := readBack(t, c, "main", "src"); !bytes.Equal(got, body) {
		t.Fatal("the source is not read from the good node")
	}
	if _, err := c.CopyObject(ctx, &s3.CopyObjectInput{Bucket: aws.String("main"), Key: aws.String("dst"), CopySource: aws.String("main/src")}); err != nil {
		t.Fatalf("copy: %v", err)
	}
	// the copy is read from each node alone: the files of the source are all the damaged ones, so take them away from the node that is read last
	for node := 0; node < 2; node++ {
		other := 1 - node
		for _, f := range files(r.nodes[other]) {
			if _, old := was[f]; !old { // a file of the copy
				_ = os.Rename(filepath.Join(r.nodes[other], f), filepath.Join(r.nodes[other], f+".away"))
			}
		}
		if got := readBack(t, c, "main", "dst"); !bytes.Equal(got, body) {
			t.Fatalf("the copy read from node %d is not the object", node+1)
		}
		for _, f := range files(r.nodes[other]) {
			if len(f) > 5 && f[len(f)-5:] == ".away" {
				_ = os.Rename(filepath.Join(r.nodes[other], f), filepath.Join(r.nodes[other], f[:len(f)-5]))
			}
		}
	}
	// a part that is a range of the damaged source
	cu, err := c.CreateMultipartUpload(ctx, &s3.CreateMultipartUploadInput{Bucket: aws.String("main"), Key: aws.String("parted")})
	if err != nil {
		t.Fatal(err)
	}
	pc, err := c.UploadPartCopy(ctx, &s3.UploadPartCopyInput{Bucket: aws.String("main"), Key: aws.String("parted"), UploadId: cu.UploadId, PartNumber: aws.Int32(1),
		CopySource: aws.String("main/src"), CopySourceRange: aws.String("bytes=100-90000")})
	if err != nil {
		t.Fatalf("part copy: %v", err)
	}
	if _, err := c.CompleteMultipartUpload(ctx, &s3.CompleteMultipartUploadInput{Bucket: aws.String("main"), Key: aws.String("parted"), UploadId: cu.UploadId,
		MultipartUpload: &types.CompletedMultipartUpload{Parts: []types.CompletedPart{{ETag: pc.CopyPartResult.ETag, PartNumber: aws.Int32(1)}}}}); err != nil {
		t.Fatalf("complete: %v", err)
	}
	if got := readBack(t, c, "main", "parted"); !bytes.Equal(got, body[100:90001]) {
		t.Fatalf("the part copy is not the range: %d bytes", len(got))
	}
}

// With deduplication the blocks belong to a set that two objects share; a damaged copy of one of them is walked round for both, and one repair (two blocks, once each) puts it right.
func TestDedupSetWithADamagedNode(t *testing.T) {
	r := newRig(t, nil)
	r.dedupStore("tenant", 0, "")
	c := r.s3Client("001", secret1)
	ctx := context.Background()
	body := pattern(BlockSize + 4000)
	for _, k := range []string{"one", "two"} {
		if _, err := c.PutObject(ctx, &s3.PutObjectInput{Bucket: aws.String("main"), Key: aws.String(k), Body: bytes.NewReader(body)}); err != nil {
			t.Fatal(err)
		}
	}
	if n := r.count("storage_sets"); n != 1 {
		t.Fatalf("%d sets, want one shared", n)
	}
	was := hurtNode(t, r.nodes[0], func(b []byte) []byte { return b[:len(b)/2] })
	for _, k := range []string{"one", "two"} {
		if got := readBack(t, c, "main", k); !bytes.Equal(got, body) {
			t.Fatalf("%s is not read from the good node", k)
		}
	}
	if got := r.count("storage_repair"); got != 2 {
		t.Fatalf("%d tasks, want 2: the blocks of the set, once each however many objects read them", got)
	}
	w := r.worker("w1", nil)
	w.Lease(context.Background(), "repair")
	w.Pass(context.Background())
	for f, b := range was {
		if got, _ := os.ReadFile(filepath.Join(r.nodes[0], f)); len(got) != len(b) {
			t.Fatalf("%s is not repaired: %d bytes, was %d", f, len(got), len(b))
		}
	}
	if r.count("storage_repair") != 0 {
		t.Fatal("tasks are left")
	}
}
