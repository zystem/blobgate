// Package space says in words when a store has no room for an upload (docs/storage-module.md, "Room in the store"): a full store must refuse with 507 and a reason, not stall.
package space

import "fmt"

// Hint is what a refusal for want of room tells the person to do. The operator sets it for the product that uses the module (BLOBGATE_FULL_HINT), so that it can name that product's own settings.
var Hint = "free space in it, delete data that is no longer needed, or give the store a bigger volume"

// DefaultReserve is what is kept free in a store (BLOBGATE_RESERVE_BYTES, 256 MiB).
const DefaultReserve int64 = 256 << 20

// Info is how much room a store has; Known is false when it cannot say (nothing is refused then).
type Info struct {
	Known       bool
	Free, Total int64
}

// Human is a size for a person: "7.6 GiB", "512 KiB".
func Human(n int64) string {
	units := []string{"B", "KiB", "MiB", "GiB", "TiB"}
	v := float64(max(n, 0))
	u := 0
	for v >= 1024 && u < len(units)-1 {
		v /= 1024
		u++
	}
	if u == 0 {
		return fmt.Sprintf("%d B", int64(v))
	}
	r := int64(v*10 + 0.5)
	return fmt.Sprintf("%d.%d %s", r/10, r%10, units[u])
}

// Problem is "" when need more bytes fit (or the store does not say), else the sentence that tells a person what is wrong and what to do.
func Problem(i Info, need, reserve int64, store string) string {
	if !i.Known || i.Free-need >= reserve {
		return ""
	}
	return fmt.Sprintf("the store %s is nearly full: %s free of %s, this upload needs %s and %s are kept free; %s",
		store, Human(max(i.Free, 0)), Human(i.Total), Human(need), Human(reserve), Hint)
}
