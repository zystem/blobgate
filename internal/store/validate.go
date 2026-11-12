package store

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/rclone/rclone/fs"
)

// Check is one thing a node was asked to do.
type Check struct {
	Name     string `json:"name"`
	OK       bool   `json:"ok"`
	Required bool   `json:"required"` // a node that fails a required check cannot be used
	Detail   string `json:"detail,omitempty"`
}

// Validate asks a node, one by one, for what the module needs of it, and says what it could not do. It leaves nothing behind. A check that is not required is a warning (the module works, with a
// caveat the detail names).
func Validate(ctx context.Context, n *Node) []Check {
	var out []Check
	root := "_validate/" + func() string { b := make([]byte, 6); _, _ = rand.Read(b); return hex.EncodeToString(b) }()
	one := &Mirror{Name: "validate", Nodes: []*Node{n}, MinAcks: 1}
	add := func(name string, required bool, err error, detail string) {
		c := Check{Name: name, OK: err == nil, Required: required, Detail: detail}
		if err != nil {
			c.Detail = err.Error()
		}
		out = append(out, c)
	}
	put := func(rel string, data []byte) error {
		_, err := one.Put(ctx, rel, int64(len(data)), bytes.NewReader(data))
		return err
	}
	read := func(rel string, size int64) ([]byte, error) { return readWhole(ctx, n, rel, size) }

	small := []byte("blobgate validate")
	err := put(root+"/a/b/c~g/0", small)
	add("write into directories that do not exist", true, err, "")
	if err == nil {
		d, rerr := read(root+"/a/b/c~g/0", int64(len(small)))
		if rerr == nil && !bytes.Equal(d, small) {
			rerr = fmt.Errorf("another content came back")
		}
		add("read back", true, rerr, "")
	}

	big := make([]byte, 8<<20)
	_, _ = rand.Read(big)
	err = put(root+"/big~g/0", big)
	add("write a block of 8 MiB", true, err, "")
	if err == nil {
		o, oerr := n.F.NewObject(ctx, root+"/big~g/0")
		if oerr == nil {
			rc, e := o.Open(ctx, &fs.RangeOption{Start: 4 << 20, End: 4<<20 + 99})
			if e == nil {
				var buf bytes.Buffer
				_, _ = buf.ReadFrom(rc)
				rc.Close()
				if !bytes.Equal(buf.Bytes(), big[4<<20:4<<20+100]) {
					e = fmt.Errorf("a range came back wrong")
				}
			}
			oerr = e
		}
		add("read a range", false, oerr, "the module reads whole blocks, so a node without ranges still works")
	}

	err = put(root+"/big~g/0", small)
	if err == nil {
		var d []byte
		d, err = read(root+"/big~g/0", int64(len(small)))
		if err == nil && !bytes.Equal(d, small) {
			err = fmt.Errorf("the old content is still there")
		}
	}
	add("replace a file", true, err, "")

	// many writers under one parent at the same moment: some servers answer "file exists" to the second one that makes the directory
	var wg sync.WaitGroup
	errs := make([]error, 8)
	for i := range errs {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			errs[i] = put(fmt.Sprintf("%s/many/o%d~g/0", root, i), small)
		}(i)
	}
	wg.Wait()
	var first error
	for _, e := range errs {
		if e != nil {
			first = e
			break
		}
	}
	add("8 writers at once under one directory", true, first, "")

	for _, nm := range []struct{ name, seg string }{
		{"a name of 250 bytes", strings.Repeat("x", 250)},
		{"unicode, spaces and the characters Windows refuses", "h%C3%A9llo%20w%3A%2A%3F"},
	} {
		err := put(root+"/names/"+nm.seg+"~g/0", small)
		add(nm.name, false, err, "keys are escaped to safe names, so this matters only for the longest and oddest")
	}

	// case: two names that differ only in case must be two files
	e1, e2 := put(root+"/case/Aa~g/0", []byte("upper")), put(root+"/case/aa~g/0", []byte("lower"))
	var caseErr error
	if e1 != nil || e2 != nil {
		caseErr = fmt.Errorf("%v %v", e1, e2)
	} else if d, err := read(root+"/case/Aa~g/0", 5); err != nil || string(d) != "upper" {
		caseErr = fmt.Errorf("the node folds the case of names: two keys that differ only in case would be one file")
	}
	add("names that differ in case are different files", false, caseErr, "")

	// removal: a block, and a directory of blocks
	rm := n.RemoveDir(ctx, root+"/a/b/c~g")
	if rm == nil {
		if _, e := n.F.NewObject(ctx, root+"/a/b/c~g/0"); e == nil {
			rm = fmt.Errorf("the block is still there")
		}
	}
	add("remove a directory of blocks", true, rm, "")

	// room
	if ab := n.F.Features().About; ab == nil {
		out = append(out, Check{Name: "free space (about)", OK: false, Detail: "the node does not say how much room it has: a full disk is found only when a write fails, and refused then with a message"})
	} else {
		cctx, cancel := context.WithTimeout(ctx, 3*time.Second)
		u, e := ab(cctx)
		cancel()
		if e != nil || u == nil || u.Free == nil {
			out = append(out, Check{Name: "free space (about)", OK: false, Detail: "the node answers, with no figures"})
		} else {
			out = append(out, Check{Name: "free space (about)", OK: true, Detail: fmt.Sprintf("%d bytes free", *u.Free)})
		}
	}

	// clean up what is left (best effort)
	cctx := context.WithoutCancel(ctx)
	for _, dir := range []string{"big~g", "many", "names", "case", "a/b/c~g", "a/b", "a"} {
		if strings.Contains(dir, "~") || dir == "a/b" || dir == "a" {
			_ = n.RemoveDir(cctx, root+"/"+dir)
		}
	}
	purgeTree(cctx, n, root)
	_ = n.F.Rmdir(cctx, "_validate") // only if nothing else is validating at the moment
	return out
}

// purgeTree removes a directory with everything under it, the way a node without Purge needs it done.
func purgeTree(ctx context.Context, n *Node, dir string) {
	entries, err := n.F.List(ctx, dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		switch x := e.(type) {
		case fs.Object:
			_ = x.Remove(ctx)
		case fs.Directory:
			purgeTree(ctx, n, x.Remote())
		}
	}
	_ = n.F.Rmdir(ctx, dir)
}

// Usable says whether every required check passed.
func Usable(cs []Check) bool {
	for _, c := range cs {
		if c.Required && !c.OK {
			return false
		}
	}
	return true
}
