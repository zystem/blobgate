package seal

import (
	"encoding/hex"
	"testing"
)

// A fixed value, so that a change of the format (the key derivation, the layout, the additional data) fails here and does not quietly make the stored secrets unreadable.
const (
	fixedSecret = "a-seal-key-for-the-vectors"
	fixedKey    = "084e2538b4d12779c3a4b9998686e4658dcb0ef6c2a0d3064dd4bcc26808528f"
	fixedStore  = "garage-main"
	fixedPlain  = "secret-access-key/with+chars"
	fixedSealed = "v2.0b30e0b185d851984bf3ea61f93bb82092b84b2f0be1825477184918295347b1babbd8effa5000ef451e266a7e7812e24970f4dda1c02d58"
)

func TestFixedVector(t *testing.T) {
	k := Key(fixedSecret)
	if hex.EncodeToString(k) != fixedKey {
		t.Fatalf("key %x", k)
	}
	got, err := Open(k, fixedStore, fixedSealed)
	if err != nil || got != fixedPlain {
		t.Fatalf("got %q, %v", got, err)
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
	for _, bad := range []string{"", "v1.", "v1.zz", "v3." + a[3:], a[:len(a)-2] + "00", "v1.00"} {
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
