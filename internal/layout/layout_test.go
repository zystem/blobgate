package layout

import (
	"strings"
	"testing"
)

func TestEscape(t *testing.T) {
	cases := map[string]string{
		"app.txt":     "app.txt",
		"a b":         "a%20b",
		".hidden":     "%2Ehidden",
		"end.":        "end%2E",
		"a~b":         "a%7Eb",
		"a/b":         "a%2Fb",
		"café":        "caf%C3%A9",
		"a:b*c?d":     "a%3Ab%2Ac%3Fd",
		"..":          "%2E%2E",
		"001":         "001",
		"x+y=z@h,(1)": "x+y=z@h,(1)",
	}
	for in, want := range cases {
		if got := EscapeSegment(in); got != want {
			t.Errorf("%q: got %q want %q", in, got, want)
		}
	}
}

func TestLongSegment(t *testing.T) {
	long := strings.Repeat("é", 200)
	a := EscapeSegment(long)
	b := EscapeSegment(long + "x")
	if len(a) > 200 || a == b || !strings.Contains(a, "~") {
		t.Fatalf("%q %q", a, b)
	}
	i := strings.Index(a, "~")
	if tail := a[:i]; strings.HasSuffix(tail, "%") || strings.HasSuffix(tail[:max(0, len(tail)-1)], "%") {
		t.Fatal("cut inside an escape: " + a)
	}
}

func TestValid(t *testing.T) {
	for _, k := range []string{"", "/a", "a/", "a//b", "a/./b", "a/../b", "a\x00b", "a\nb", strings.Repeat("a", MaxKeyLen+1)} {
		if Valid(k) == "" {
			t.Errorf("%q accepted", k)
		}
	}
	for _, k := range []string{"001/acme/s1_x/dist/app.txt", "a", "a b/c"} {
		if why := Valid(k); why != "" {
			t.Errorf("%q: %s", k, why)
		}
	}
}

func TestBlockPathNoCollision(t *testing.T) {
	// the generation suffix cannot be made by a key: `~` is always escaped inside a segment
	if BlockPath("a/b", "g1", 0) != "a/b~g1/0" {
		t.Fatal(BlockPath("a/b", "g1", 0))
	}
	if Rel("a/b~g1") == BlockDir("a/b", "g1") {
		t.Fatal("a key named like a block directory collides with it")
	}
}

func FuzzEscape(f *testing.F) {
	f.Add("a/b~c")
	f.Fuzz(func(t *testing.T, key string) {
		if Valid(key) != "" {
			return
		}
		for _, seg := range strings.Split(Rel(key), "/") {
			if seg == "" || seg == "." || seg == ".." || len(seg) > maxSegment+hashHexLen+1 {
				t.Fatalf("bad segment %q of %q", seg, key)
			}
		}
	})
}

func TestRepairKeys(t *testing.T) {
	for _, full := range []string{"001/a/b", "001/we~ird/x~y/1", "002/k"} {
		f, g, n, blk, ok := ParseKey(BlockKey(full, "abc123", 7))
		if !ok || !blk || f != full || g != "abc123" || n != 7 {
			t.Errorf("%q: %q %q %d %v %v", full, f, g, n, blk, ok)
		}
		f, g, _, blk, ok = ParseKey(DirKey(full, "abc123"))
		if !ok || blk || f != full || g != "abc123" {
			t.Errorf("dir %q: %q %q %v %v", full, f, g, blk, ok)
		}
	}
	if _, _, _, _, ok := ParseKey("nothing"); ok {
		t.Error("a key with no generation parsed")
	}
}
