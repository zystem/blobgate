# What a user can and cannot expect, in security

## Can expect

- **A call is authenticated.** The calls of the core carry a token that is an HMAC-SHA256 of the tenant, the method, the target (path and query) and the time it stops being good (120 s), under the secret of that tenant:
  a token made for one call is no good for another path, query, method or tenant. The calls of a step's shim carry a grant for one upload or one object, valid two hours, on a port that serves nothing else. The admin
  API and the metrics want the admin token. Tokens are compared in constant time.
- **A block is what it was written as.** The digest of every block is kept in the database; a read, a repair and a check compare it, and a block that does not match counts as missing on that node. A write whose
  body is not the one `X-Content-SHA256` names is refused and leaves nothing behind.
- **A crash does not lose or mix data.** An object is fixed by one row; a process killed at any step leaves the object as it was or as it will be, and what it left is found and removed
  ([data-lifecycle.md](data-lifecycle.md), tested with SIGKILL).
- **Secrets are not stored in the clear.** The passwords and keys of the stores are sealed in the database (AES-256-GCM) under a key that is in the environment and not in the database; a store can also seal the blocks
  on the nodes ([encryption.md](encryption.md)); the backups of the database are sealed ([backup.md](backup.md)).
- **Inputs are checked against what is allowed**: keys, names of stores, ids of uploads, ranges, sizes of bodies, JSON bodies of the admin API, the settings of the environment; anything else is refused with words,
  and the parsers of tokens, keys and sealed text have fuzz tests.
- **A full store says so**: a refusal with 507 and the words, not a stall.
- **A small attack surface**: one static program, no shell in the image, a user that is not root, a read-only root, nothing written to a disk; secrets can be files; the ports can be TLS 1.2 or later.
- **Releases you can check**: signed, with build provenance and a bill of materials, built repeatably ([verifying-releases.md](verifying-releases.md)).

## Cannot expect

- **Protection from a compromised host or process.** Whoever can read the memory of the module has the secrets of the tenants, the admin token, the seal key and the keys of the stores.
- **Protection from whoever holds the admin token**: the admin API can register a store that sends the data anywhere, and can read what the module reads. Keep it to the operator.
- **Confidentiality from the nodes, unless the store seals its blocks.** A node holds the blocks as they are (and the names of the keys of the objects, escaped but readable); a store with `encryption` holds sealed bytes,
  but the names and sizes of the objects are still visible on the node.
- **Protection from replay of a token within its two minutes**: a token is bound to one call (method, target) but has no one-time number; a grant is valid for two hours for the one upload or object it names.
- **Protection of the wire unless TLS is on.** Without `BLOBGATE_TLS_CERT_FILE` the ports are plain HTTP (the bodies are not hidden); put a mesh or TLS in front, or turn it on.
- **Isolation between tenants beyond their secrets and the prefix of the keys**: a tenant cannot ask for the objects of another, but the tenants share the database, the stores and the nodes.
- **Availability when the database is away**: the module answers 503 until rqlite is back; it holds no state of its own.
- **A node that lies without being caught**: a node that gives back wrong bytes is caught by the digest; one that withholds or deletes is found by the check and repaired from another, if there is another.
- **Fixes for old releases** beyond what [SECURITY.md](../SECURITY.md) says.
