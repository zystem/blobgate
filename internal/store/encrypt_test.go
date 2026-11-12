package store

import (
	"bytes"
	"errors"
	"testing"
)

func TestCipher(t *testing.T) {
	c, err := NewCipher(2, map[string]string{"1": "the-first-key-of-the-test", "2": "the-second-key-of-the-test"})
	if err != nil {
		t.Fatal(err)
	}
	plain := bytes.Repeat([]byte("data"), 1000)
	sealed, err := c.Seal("001/a/b", "gen1", 3, plain)
	if err != nil || len(sealed) != len(plain)+EncOverhead || bytes.Contains(sealed, []byte("datadata")) {
		t.Fatalf("%v %d", err, len(sealed))
	}
	if got, err := c.Open("001/a/b", "gen1", 3, sealed); err != nil || !bytes.Equal(got, plain) {
		t.Fatalf("open: %v", err)
	}
	// a block moved to another place, number, generation or object does not open there
	for _, where := range []struct {
		full, gen string
		n         int
	}{{"001/a/c", "gen1", 3}, {"001/a/b", "gen2", 3}, {"001/a/b", "gen1", 4}} {
		if _, err := c.Open(where.full, where.gen, where.n, sealed); !errors.Is(err, ErrDecrypt) {
			t.Errorf("%v opened in another place: %v", where, err)
		}
	}
	// a changed byte
	bad := append([]byte(nil), sealed...)
	bad[len(bad)-1] ^= 1
	if _, err := c.Open("001/a/b", "gen1", 3, bad); !errors.Is(err, ErrDecrypt) {
		t.Fatal("a changed block opened")
	}
	// the key by its id: one that was dropped, and another text under the same id
	old, _ := NewCipher(1, map[string]string{"1": "the-first-key-of-the-test"})
	s1, _ := old.Seal("k", "g", 0, plain)
	only2, _ := NewCipher(2, map[string]string{"2": "the-second-key-of-the-test"})
	var nk *ErrNoKey
	if _, err := only2.Open("k", "g", 0, s1); !errors.As(err, &nk) || nk.ID != 1 {
		t.Fatalf("a dropped key: %v", err)
	}
	other, _ := NewCipher(1, map[string]string{"1": "another-text-under-the-same-id"})
	if _, err := other.Open("k", "g", 0, s1); !errors.Is(err, ErrDecrypt) {
		t.Fatalf("a wrong key: %v", err)
	}
	// the current key must be one of the keys; ids are numbers from 1
	if _, err := NewCipher(3, map[string]string{"1": "x"}); err == nil {
		t.Fatal("a current key that is not there")
	}
	if _, err := NewCipher(0, map[string]string{"0": "x"}); err == nil {
		t.Fatal("key 0")
	}
	// two sealings of one block differ (a fresh nonce)
	a, _ := c.Seal("k", "g", 0, plain)
	b, _ := c.Seal("k", "g", 0, plain)
	if bytes.Equal(a, b) {
		t.Fatal("the nonce is not fresh")
	}
}

func FuzzOpen(f *testing.F) {
	c, _ := NewCipher(1, map[string]string{"1": "the-first-key-of-the-test"})
	s, _ := c.Seal("k", "g", 0, []byte("x"))
	f.Add(s)
	f.Fuzz(func(t *testing.T, data []byte) { _, _ = c.Open("k", "g", 0, data) })
}
