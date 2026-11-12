package store

import (
	"context"
	"testing"

	"github.com/rclone/rclone/fs"
)

func TestValidateLocalNode(t *testing.T) {
	dir := t.TempDir()
	spec := NodeSpec{Backend: "local", Path: dir}
	f, err := spec.Open(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	n := NewNode(spec, f)
	cs := Validate(context.Background(), n)
	if !Usable(cs) {
		t.Fatalf("%+v", cs)
	}
	for _, c := range cs {
		if c.Required && !c.OK {
			t.Errorf("%s: %s", c.Name, c.Detail)
		}
	}
	entries, _ := f.List(context.Background(), "")
	for _, e := range entries {
		t.Errorf("left behind: %s", e.Remote())
	}
	_ = fs.ErrorObjectNotFound
}
