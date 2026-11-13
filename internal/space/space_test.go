package space

import (
	"strings"
	"testing"
)

func TestHuman(t *testing.T) {
	for n, want := range map[int64]string{-5: "0 B", 0: "0 B", 1023: "1023 B", 1024: "1.0 KiB", 524288: "512.0 KiB", 7 << 30: "7.0 GiB", 3 << 40: "3.0 TiB", 5 << 50: "5120.0 TiB"} {
		if got := Human(n); got != want {
			t.Errorf("%d: %q, want %q", n, got, want)
		}
	}
}

func TestProblem(t *testing.T) {
	if Problem(Info{}, 1<<40, DefaultReserve, "s") != "" {
		t.Fatal("a store that does not say refused a write")
	}
	if Problem(Info{Known: true, Free: 10 << 30, Total: 12 << 30}, 1<<20, DefaultReserve, "s") != "" {
		t.Fatal("a write that fits was refused")
	}
	why := Problem(Info{Known: true, Free: 512 << 10, Total: 12 << 30}, 83<<20, DefaultReserve, "garage-main")
	for _, want := range []string{"garage-main", "nearly full", "512.0 KiB free of 12.0 GiB", "83.0 MiB", "256.0 MiB", "cache.retention"} {
		if !strings.Contains(why, want) {
			t.Errorf("%q is not in %q", want, why)
		}
	}
	if Problem(Info{Known: true, Free: -5, Total: 10}, 1, 0, "s") == "" {
		t.Fatal("a store with no room took a write")
	}
}
