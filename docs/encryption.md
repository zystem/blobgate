# Sealing the blocks of a store

How-to and reference. A store can seal the blocks it writes, so that the nodes (an FTP server, a bucket of someone else) hold bytes that mean nothing without the key.

## Turn it on

Register the store again with `encryption` (the admin call replaces the settings of the store, so everything else is sent again):

```bash
curl -X PUT "$STORAGE/v1/admin/stores/main" -H "Authorization: Bearer $ADMIN" -d '{
  "kind": "mirror", "default": true,
  "nodes": [{"remote": ":ftp,host=h,user=u:/blobs", "secrets": {"pass": "..."}}, {"remote": ":webdav,url=...:/blobs", "secrets": {"pass": "..."}}],
  "encryption": {"key_id": 1, "keys": {"1": "a-long-random-text-of-at-least-16-characters"}}}'
```

It answers `{"name":"main","checked":true}` as for any store (not run as a command in this form: the tests do the same through `internal/api/encrypt_test.go`).

| Setting | Meaning |
|---|---|
| `encryption.keys` | Every key an object was ever sealed with: id (a number from 1) to text (16..256 printable characters). The key is made of the text; it is kept sealed in the database with the module's own key, and never shown (the list shows the id only). |
| `encryption.key_id` | The key new blocks are sealed with. Omit `encryption.key_id` (and keep `keys`) to stop sealing new blocks and still read the sealed ones. |

## What it does and does not

* **New blocks are sealed.** Blocks already written stay as they are (a plain object stays plain and is read as before). There is no call that seals what is written: rewrite the object.
* **Rotation.** Add key 2 to `keys` and set `key_id` to 2. Objects sealed with key 1 are read with key 1, which must **stay in `keys`** as long as one of them exists. A key that is dropped makes what it
  sealed unreadable: the answer is `500 encrypted_no_key`, with the id. The same id with another text gives `500 decrypt_failed`, never wrong bytes.
* **The nodes hold only sealed bytes, and need no key.** The digests in the database are those of the sealed bytes, so the repair, the checks and the reconcile copy and compare blocks without the key.
  A sealed block is 33 bytes longer than what it holds.
* **What an attacker with a node gets:** the sealed blocks and the sizes and the names of the paths (the key of an object, which is not sealed). Not the contents, and not a block moved to another place:
  the place of a block (its key, generation and number) is part of what the seal checks. What an attacker with the database gets: the keys are sealed with `CINIM_STORAGE_SEAL_KEY`, so also not these.
* **Memory.** A block is sealed whole (the tag is at its end), so a write into a sealed store holds one block (at most 8 MiB) in memory, and at most `CINIM_STORAGE_THREADS` (8) at once; a plain store
  holds none. This is the cost to measure before a sealed store is made busy (not measured).
* **Algorithm.** AES-256-GCM; for every generation of an object a key made of the master key, the object's key and the generation (HMAC-SHA256), a random 96-bit nonce, so that no key seals more than one
  generation. The block is `1 byte version | 4 bytes key id | 12 bytes nonce | ciphertext | 16 bytes tag`.

## What was run (2026-11-12) and what was not

Run (`internal/store/encrypt_test.go`, `internal/api/encrypt_test.go`, local nodes): the nodes hold no plain bytes and a sealed block is 33 bytes longer; ranges across blocks; a multipart upload (the digest
kept is that of what the shim sent); switched on in a store with plain objects; rotation; a key dropped and a wrong text; a block moved to another place or changed (refused); the repair of a node that missed a
write and of a node that lost its disk, and the checks (including the deep one), without any key; a digest that is not the body's (refused before anything is written). Not run: speed and memory of a sealed
store; sealed stores on real FTP, WebDAV, SFTP or S3 nodes (the same code as a plain one, which was run); rewriting existing objects to seal them.
