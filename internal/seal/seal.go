// Package seal makes and opens the sealed secrets of storage_stores.secret: "v3." + hex(nonce || ciphertext with tag),
// AES-256-GCM (standard library, hardware-accelerated), key = SHA-256("blobgate/seal/v3|" + secret), additional data = the name of the store,
// so that a value copied to another store's row does not open. Rows of the first version ("v2.", key under the domain "cinim/storage/seal/v2") are still opened, and are sealed again as v3 when
// their store is saved. The nonce is 12 random bytes; a store's secret is sealed a handful of times in its life, far below the
// limit of random nonces under one key. This is not the format of the Nim module ("v1.", XChaCha20-Poly1305): the stores start empty (D-60), nothing is read from it.
package seal

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
)

const (
	tag    = "v3."
	tagOld = "v2."
)

// Keys are the keys a secret text makes: the one that seals, and the one of the first version, which only opens.
type Keys struct{ Current, Legacy []byte }

// Key is the 32-byte key made from whatever text the operator gave.
func Key(secret string) Keys {
	k := sha256.Sum256([]byte("blobgate/seal/v3|" + secret))
	old := sha256.Sum256([]byte("cinim/storage/seal/v2|" + secret))
	return Keys{Current: k[:], Legacy: old[:]}
}

func gcm(key []byte) (cipher.AEAD, error) {
	b, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(b)
}

// Seal seals plain for the named store with a fresh random nonce.
func Seal(keys Keys, store, plain string) (string, error) {
	a, err := gcm(keys.Current)
	if err != nil {
		return "", err
	}
	nonce := make([]byte, a.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}
	return tag + hex.EncodeToString(a.Seal(nonce, nonce, []byte(plain), []byte(store))), nil
}

// ErrOpen is the one error of Open: a wrong key, another store's row, or damage.
var ErrOpen = errors.New("seal: cannot open")

// Open opens a sealed value for the named store.
func Open(keys Keys, store, sealed string) (string, error) {
	key, t := keys.Current, tag
	if strings.HasPrefix(sealed, tagOld) {
		key, t = keys.Legacy, tagOld
	} else if !strings.HasPrefix(sealed, tag) {
		return "", ErrOpen
	}
	raw, err := hex.DecodeString(sealed[len(t):])
	a, err2 := gcm(key)
	if err != nil || err2 != nil || len(raw) < a.NonceSize()+a.Overhead() {
		return "", ErrOpen
	}
	n := a.NonceSize()
	plain, err := a.Open(nil, raw[:n], raw[n:], []byte(store))
	if err != nil {
		return "", ErrOpen
	}
	return string(plain), nil
}
