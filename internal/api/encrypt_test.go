package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zystem/blobgate/internal/maint"
)

const (
	key1 = "the-first-block-key-of-the-test"
	key2 = "the-second-block-key-of-the-test"
)

// mirrorSealed registers the two-node store `main` that seals new blocks with key `cur` (the keys are id -> text).
func (r *rig) mirrorSealed(minAcks int, cur int, keys map[string]string) {
	r.t.Helper()
	body := map[string]any{"kind": "mirror", "min_acks": minAcks, "default": true,
		"nodes": []any{map[string]any{"remote": ":local:" + r.nodes[0]}, map[string]any{"remote": ":local:" + r.nodes[1]}}}
	if keys != nil {
		body["encryption"] = map[string]any{"key_id": cur, "keys": keys}
	}
	if code, data := r.admin("PUT", "/v1/admin/stores/main", body); code != 200 {
		r.t.Fatalf("register the store: %d %s", code, data)
	}
}

func anyFileHolds(dir string, needle []byte) bool {
	for _, f := range files(dir) {
		if data, err := os.ReadFile(filepath.Join(dir, f)); err == nil && bytes.Contains(data, needle) {
			return true
		}
	}
	return false
}

// DAT-004 (sealing on the fly): the nodes hold only sealed bytes; the object reads back as it was written, by ranges, across the border of blocks too; the size is that of the object.
func TestSealedBlocks(t *testing.T) {
	r := newRig(t, nil)
	r.mirrorSealed(0, 1, map[string]string{"1": key1})
	marker := []byte("PLAINTEXT-MARKER-0123456789")
	body := append(bytes.Repeat([]byte("x"), 5000), append(marker, pattern(20000)...)...)
	if code, d := r.put("acme/s1/secret.bin", body); code != 200 {
		t.Fatalf("put %d %s", code, d)
	}
	for _, n := range r.nodes {
		if anyFileHolds(n, marker) {
			t.Fatalf("%s holds the plain bytes", n)
		}
		for _, f := range files(n) {
			if st, _ := os.Stat(filepath.Join(n, f)); st.Size() != int64(len(body))+33 {
				t.Fatalf("%s is %d bytes, want %d", f, st.Size(), len(body)+33)
			}
		}
	}
	if code, d := r.get("acme/s1/secret.bin", 4990, 5040); code != 206 || !bytes.Equal(d, body[4990:5041]) {
		t.Fatalf("range %d", code)
	}
	_, data, _ := r.core("GET", "/v1/stores/main/objects/acme/s1/secret.bin", "stat", nil, nil)
	if !strings.Contains(string(data), fmt.Sprintf(`"size":%d`, len(body))) {
		t.Fatalf("stat %s", data)
	}
	// a digest that is not the body's: refused before anything is written
	r2 := newRig(t, nil)
	r2.mirrorSealed(0, 1, map[string]string{"1": key1})
	code, d, _ := r2.core("PUT", "/v1/stores/main/objects/k/bad", "", map[string]string{"X-Content-SHA256": sha([]byte("other"))}, []byte("body"))
	if code != 400 || !strings.Contains(string(d), "sha256_mismatch") || len(files(r2.nodes[0])) != 0 {
		t.Fatalf("%d %s %v", code, d, files(r2.nodes[0]))
	}
}

// A multipart upload into a sealed store: the module keeps the digest of what the shim sent (which the core compares with what the shim reported), seals the blocks, and the object reads across them.
func TestSealedMultipart(t *testing.T) {
	r := newRig(t, nil)
	r.mirrorSealed(0, 1, map[string]string{"1": key1})
	whole := pattern(250000)
	parts := [][]byte{whole[:100000], whole[100000:200000], whole[200000:]}
	id, etags := (&crashRig{rig: r}).multipart("acme/big", parts, "")
	_, data, _ := r.core("GET", "/v1/stores/main/objects/acme/big", "upload="+id+"&parts", nil, nil)
	if !strings.Contains(string(data), sha(parts[1])) {
		t.Fatalf("the digest recorded is not the one of what was sent: %s", data)
	}
	if code, d, _ := r.core("POST", "/v1/stores/main/objects/acme/big", "upload="+id, nil, completeBody(etags)); code != 200 || !strings.Contains(string(d), "250000") {
		t.Fatalf("complete %d %s", code, d)
	}
	if code, d := r.get("acme/big", 99990, 200010); code != 206 || !bytes.Equal(d, whole[99990:200011]) {
		t.Fatalf("across the borders %d", code)
	}
	for _, n := range r.nodes {
		if anyFileHolds(n, whole[1000:1032]) {
			t.Fatal("plain bytes on a node")
		}
	}
}

// Sealing is switched on in a store that holds plain objects: they stay readable, and the new ones are sealed. Then the key is changed: the old ones are still read with the old key, which must stay in the settings.
func TestSealingSwitchedOnAndKeyRotation(t *testing.T) {
	r := newRig(t, nil)
	r.mirror(0)
	plain := pattern(3000)
	r.put("k/plain", plain)
	r.mirrorSealed(0, 1, map[string]string{"1": key1})
	sealed1 := pattern(4000)
	r.put("k/sealed1", sealed1)
	r.mirrorSealed(0, 2, map[string]string{"1": key1, "2": key2})
	sealed2 := pattern(5000)
	r.put("k/sealed2", sealed2)
	for name, want := range map[string][]byte{"k/plain": plain, "k/sealed1": sealed1, "k/sealed2": sealed2} {
		if code, d := r.get(name, 0, len(want)-1); code != 206 || !bytes.Equal(d, want) {
			t.Fatalf("%s: %d", name, code)
		}
	}
	// the id of the key is in the header of the block
	idOf := func(name string) byte {
		for _, f := range files(r.nodes[0]) {
			if strings.Contains(f, name+"~") {
				data, _ := os.ReadFile(filepath.Join(r.nodes[0], f))
				return data[4]
			}
		}
		t.Fatalf("no block of %s", name)
		return 0
	}
	if idOf("k/sealed1") != 1 || idOf("k/sealed2") != 2 {
		t.Fatal("the blocks are not sealed with the keys they should be")
	}
	// the first key is dropped: what it sealed cannot be read, and the answer says why
	r.mirrorSealed(0, 2, map[string]string{"2": key2})
	code, d := r.get("k/sealed1", 0, 10)
	if code != 500 || !strings.Contains(string(d), "encrypted_no_key") || !strings.Contains(string(d), "key 1") {
		t.Fatalf("a dropped key: %d %s", code, d)
	}
	if code, d := r.get("k/sealed2", 0, 4999); code != 206 || !bytes.Equal(d, sealed2) {
		t.Fatal("what the other key sealed is no longer read")
	}
	// the same id with another text: the block does not open, and the answer says so, not wrong bytes
	r.mirrorSealed(0, 2, map[string]string{"1": "another-text-under-the-same-id", "2": key2})
	if code, d := r.get("k/sealed1", 0, 10); code != 500 || !strings.Contains(string(d), "decrypt_failed") {
		t.Fatalf("a wrong key text: %d %s", code, d)
	}
}

// The repair and the checks need no key: they copy and compare the sealed bytes by the digests in the database.
func TestRepairOfSealedBlocks(t *testing.T) {
	r := newRig(t, nil)
	r.mirrorSealed(1, 1, map[string]string{"1": key1})
	r.readOnly(1)
	body := pattern(30000)
	if code, _ := r.put("acme/a", body); code != 200 {
		t.Fatal("put with a node down")
	}
	r.writable(1)
	w := r.worker("w1", nil)
	w.Lease(context.Background(), "repair")
	for i := 0; i < 3 && r.count("storage_repair") > 0; i++ {
		time.Sleep(1100 * time.Millisecond)
		w.Pass(context.Background())
	}
	f := files(r.nodes[0])
	g := files(r.nodes[1])
	if len(f) != 1 || len(g) != 1 {
		t.Fatalf("%v %v", f, g)
	}
	a, _ := os.ReadFile(filepath.Join(r.nodes[0], f[0]))
	b, _ := os.ReadFile(filepath.Join(r.nodes[1], g[0]))
	if !bytes.Equal(a, b) {
		t.Fatal("the repaired block is not the sealed one")
	}
	// a node that lost its disk: found by the check by the length of the sealed block (with the nine bytes more than the plain one), repaired, and read from alone
	_ = os.RemoveAll(r.nodes[1])
	_ = os.Mkdir(r.nodes[1], 0o755)
	if made := w.CheckPass(context.Background()); made != 1 {
		t.Fatalf("the check made %d tasks", made)
	}
	w.Pass(context.Background())
	if len(files(r.nodes[1])) != 1 {
		t.Fatal("not repaired")
	}
	if made := w.CheckPass(context.Background()); made != 0 {
		t.Fatalf("after the repair the check still finds %d blocks wrong (the length of a sealed block is mistaken)", made)
	}
	deep := r.worker("w1", func(c *maint.Config) { c.Deep = true })
	deep.Lease(context.Background(), "repair")
	if made := deep.CheckPass(context.Background()); made != 0 {
		t.Fatalf("the deep check finds %d blocks wrong", made)
	}
	_ = os.Remove(filepath.Join(r.nodes[0], f[0]))
	if code, d := r.get("acme/a", 0, 29999); code != 206 || !bytes.Equal(d, body) {
		t.Fatalf("read from the repaired node alone: %d", code)
	}
}

func TestAdminListShowsKeyIdsNotKeys(t *testing.T) {
	r := newRig(t, nil)
	r.mirrorSealed(0, 1, map[string]string{"1": key1})
	code, data := r.admin("GET", "/v1/admin/stores", nil)
	var v struct {
		Stores []struct {
			Encryption map[string]any `json:"encryption"`
		} `json:"stores"`
	}
	if code != 200 || json.Unmarshal(data, &v) != nil || v.Stores[0].Encryption["key_id"].(float64) != 1 || strings.Contains(string(data), key1) {
		t.Fatalf("%d %s", code, data)
	}
	if code, _ := r.admin("PUT", "/v1/admin/stores/bad", map[string]any{"kind": "mirror", "nodes": []any{map[string]any{"remote": ":local:" + r.nodes[0]}},
		"encryption": map[string]any{"key_id": 3, "keys": map[string]string{"1": key1}}}); code != 400 {
		t.Fatal("a current key that is not among the keys was accepted")
	}
	if code, _ := r.admin("PUT", "/v1/admin/stores/bad", map[string]any{"kind": "mirror", "nodes": []any{map[string]any{"remote": ":local:" + r.nodes[0]}},
		"encryption": map[string]any{"key_id": 1, "keys": map[string]string{"1": "short"}}}); code != 400 {
		t.Fatal("a short key was accepted")
	}
}
