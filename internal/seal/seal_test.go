package seal

import (
	"encoding/hex"
	"strings"
	"testing"
)

// A fixed value, so that a change of the format (the key derivation, the layout, the additional data) fails here and does not quietly make the stored secrets unreadable.
const (
	fixedSecret = "a-seal-key-for-the-vectors"
	legacyKey   = "084e2538b4d12779c3a4b9998686e4658dcb0ef6c2a0d3064dd4bcc26808528f" // of the first version: SHA-256("cinim/storage/seal/v2|" + secret)
	fixedKey    = "3940fe2228aea3fa9f9875476853c316e36503e3edd08dcb9ea0c746f9df9e55" // SHA-256("blobgate/seal/v3|" + secret)
	fixedStore  = "garage-main"
	fixedPlain  = "secret-access-key/with+chars"
	fixedSealed = "v2.0b30e0b185d851984bf3ea61f93bb82092b84b2f0be1825477184918295347b1babbd8effa5000ef451e266a7e7812e24970f4dda1c02d58"
)

func TestFixedVector(t *testing.T) {
	k := Key(fixedSecret)
	if hex.EncodeToString(k.Current) != fixedKey || hex.EncodeToString(k.Legacy) != legacyKey {
		t.Fatalf("keys %x %x", k.Current, k.Legacy)
	}
	// a row that the first version sealed ("v2.") is still opened, and what is sealed now is "v3."
	got, err := Open(k, fixedStore, fixedSealed)
	if err != nil || got != fixedPlain {
		t.Fatalf("got %q, %v", got, err)
	}
	if now, _ := Seal(k, fixedStore, fixedPlain); !strings.HasPrefix(now, "v3.") {
		t.Fatalf("sealed as %q", now[:3])
	} else if back, err := Open(k, fixedStore, now); err != nil || back != fixedPlain {
		t.Fatalf("v3: %q %v", back, err)
	}
	if _, err := Open(k, "another-store", fixedSealed); err == nil {
		t.Fatal("opened for another store")
	}
	if _, err := Open(Key("x"), fixedStore, fixedSealed); err == nil {
		t.Fatal("opened with another key")
	}
}

func TestRoundTripAndDamage(t *testing.T) {
	k := Key("a-key-of-some-length")
	a, _ := Seal(k, "s", "plain")
	b, _ := Seal(k, "s", "plain")
	if a == b {
		t.Fatal("the nonce is not fresh")
	}
	if p, err := Open(k, "s", a); err != nil || p != "plain" {
		t.Fatal(p, err)
	}
	for _, bad := range []string{"", "v1.", "v1.zz", "v4." + a[3:], a[:len(a)-2] + "00", "v1.00"} {
		if _, err := Open(k, "s", bad); err == nil {
			t.Errorf("opened %q", bad)
		}
	}
}

func FuzzOpen(f *testing.F) {
	f.Add("v1.00")
	k := Key("k")
	f.Fuzz(func(t *testing.T, s string) { _, _ = Open(k, "s", s) })
}
