package store

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"strconv"
)

// EncOverhead is what sealing adds to a block: a version byte, the id of the key (4 bytes), the nonce (12) and the tag (16).
const EncOverhead = 1 + 4 + 12 + 16

// EncConfig says that a store seals the blocks it writes, and with which key. The keys themselves are secrets (Secrets.EncKeys); the ids are here.
type EncConfig struct {
	KeyID uint32 `json:"key_id"` // the key new blocks are sealed with
}

// ErrNoKey is a read of a block sealed with a key the store does not have (it was dropped from the settings).
type ErrNoKey struct{ ID uint32 }

func (e *ErrNoKey) Error() string {
	return fmt.Sprintf("the object is sealed with key %d, which the settings of the store do not have; put it back in encryption.keys", e.ID)
}

// ErrDecrypt is a block that does not open with the key it names: the key text is not the one it was sealed with.
var ErrDecrypt = errors.New("a block does not open: the key with that id is not the one it was sealed with")

// Cipher seals and opens the blocks of a store: AES-256-GCM, one key for every generation of every object derived from the master key (HMAC-SHA256 of the key, the object and the generation),
// a random 96-bit nonce, and the place of the block (its key, generation and number) as additional data, so that a block copied to another place does not open there.
// The node holds only the sealed bytes, and the digests in the database are those of the sealed bytes: the repair and the checks need no key.
type Cipher struct {
	Current uint32 // the key new blocks are sealed with; 0: none (the keys are only for reading what was sealed)
	keys    map[uint32][]byte
}

// NewCipher makes the cipher of a store from its key ids and the key texts.
func NewCipher(current uint32, keys map[string]string) (*Cipher, error) {
	c := &Cipher{Current: current, keys: map[uint32][]byte{}}
	for id, text := range keys {
		n, err := strconv.ParseUint(id, 10, 32)
		if err != nil || n == 0 {
			return nil, fmt.Errorf("a key id is a number from 1: %q", id)
		}
		m := sha256.Sum256([]byte("blobgate/block/v1|" + text))
		c.keys[uint32(n)] = m[:]
	}
	if current != 0 && c.keys[current] == nil {
		return nil, fmt.Errorf("key %d is the current one and is not in encryption.keys", current)
	}
	return c, nil
}

func (c *Cipher) aead(id uint32, full, gen string) (cipher.AEAD, error) {
	master := c.keys[id]
	if master == nil {
		return nil, &ErrNoKey{id}
	}
	h := hmac.New(sha256.New, master)
	h.Write([]byte("blk|" + full + "|" + gen))
	b, err := aes.NewCipher(h.Sum(nil))
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(b)
}

func aadOf(full, gen string, n int) []byte { return []byte(full + "|" + gen + "|" + strconv.Itoa(n)) }

// Seal seals block n of a generation of an object with the current key.
func (c *Cipher) Seal(full, gen string, n int, plain []byte) ([]byte, error) {
	a, err := c.aead(c.Current, full, gen)
	if err != nil {
		return nil, err
	}
	out := make([]byte, 5, EncOverhead+len(plain))
	out[0] = 1
	binary.BigEndian.PutUint32(out[1:5], c.Current)
	nonce := make([]byte, a.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	out = append(out, nonce...)
	return a.Seal(out, nonce, plain, aadOf(full, gen, n)), nil
}

// Open opens a sealed block with the key its header names.
func (c *Cipher) Open(full, gen string, n int, data []byte) ([]byte, error) {
	if len(data) < EncOverhead || data[0] != 1 {
		return nil, ErrDecrypt
	}
	id := uint32(data[1])<<24 | uint32(data[2])<<16 | uint32(data[3])<<8 | uint32(data[4])
	a, err := c.aead(id, full, gen)
	if err != nil {
		return nil, err
	}
	plain, err := a.Open(nil, data[5:5+a.NonceSize()], data[5+a.NonceSize():], aadOf(full, gen, n))
	if err != nil {
		return nil, ErrDecrypt
	}
	return plain, nil
}
