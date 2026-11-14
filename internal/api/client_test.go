package api

import (
	"bytes"
	"context"
	"io"
	"strings"
	"testing"

	"github.com/zystem/blobgate/client"
)

func TestGoClient(t *testing.T) {
	r := newRig(t, nil)
	r.dedupStore("tenant", 0, "")
	c := client.New(r.ts.URL, "001", secret1)
	ctx := context.Background()
	big := pattern(9<<20 + 31)
	if err := c.Put(ctx, "main", "go/big", bytes.NewReader(big), int64(len(big)), &client.Options{ContentType: "application/x-big", Meta: map[string]string{"owner": "go"}}); err != nil {
		t.Fatal(err)
	}
	// the second copy is not sent
	linked, err := c.PutOrLink(ctx, "main", "go/copy", bytes.NewReader(big), &client.Options{ContentType: "application/x-big"})
	if err != nil || !linked {
		t.Fatalf("link: %v %v", linked, err)
	}
	if linked, err := c.PutOrLink(ctx, "main", "go/new", strings.NewReader("new bytes"), nil); err != nil || linked {
		t.Fatalf("new bytes: %v %v", linked, err)
	}
	body, info, err := c.Get(ctx, "main", "go/copy", 0, -1)
	if err != nil {
		t.Fatal(err)
	}
	data, _ := io.ReadAll(body)
	body.Close()
	if !bytes.Equal(data, big) || info.ContentType != "application/x-big" || info.Size != int64(len(big)) {
		t.Fatalf("get: %d bytes, %+v", len(data), info)
	}
	body, _, err = c.Get(ctx, "main", "go/big", 100, 199)
	if err != nil {
		t.Fatal(err)
	}
	if data, _ = io.ReadAll(body); !bytes.Equal(data, big[100:200]) {
		t.Fatal("range")
	}
	body.Close()
	st, err := c.Stat(ctx, "main", "go/big")
	if err != nil || st.Meta["owner"] != "go" || st.ETag == "" {
		t.Fatalf("stat: %v %+v", err, st)
	}
	if _, err := c.Stat(ctx, "main", "go/none"); !client.IsNotFound(err) {
		t.Fatalf("stat of nothing: %v", err)
	}
	var keys []string
	if err := c.ListAll(ctx, "main", "go/", func(l client.Listed) error { keys = append(keys, l.Key); return nil }); err != nil || strings.Join(keys, ",") != "go/big,go/copy,go/new" {
		t.Fatalf("list: %v %v", err, keys)
	}
	if err := c.Put(ctx, "main", "go/big", strings.NewReader("x"), 1, &client.Options{IfNoneMatch: "*"}); err == nil {
		t.Fatal("If-None-Match: * over an object")
	} else if e, ok := err.(*client.Error); !ok || e.Status != 412 {
		t.Fatalf("%v", err)
	}
	if err := c.Delete(ctx, "main", "go/copy"); err != nil {
		t.Fatal(err)
	}
	if len(files(r.nodes[0])) != 3 { // two blocks of the big object, one of "new bytes"
		t.Fatalf("the blocks: %v", files(r.nodes[0]))
	}
	// a store that does not dedup: PutOrLink sends the bytes
	r.dedupStore("", 0, "")
	if linked, err := c.PutOrLink(ctx, "main", "go/plain", bytes.NewReader(big[:5000]), nil); err != nil || linked {
		t.Fatalf("no dedup: %v %v", linked, err)
	}
}
