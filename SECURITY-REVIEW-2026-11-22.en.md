# Blobgate security and reliability review

**22 November 2026 · Go 1.26.11 · Linux amd64 · base commit 58a5dc5**

**Release assessment.** Blobgate verifies block contents before returning them, keeps tenant-scoped grants, and commits object metadata and replacement bookkeeping in a database transaction. Those are sound foundations for an artifact store. Before relying on it for irreplaceable backups, concurrent create-once operations, or narrowly delegated S3 writes, address the first four findings below. The highest-impact risk is a maintenance worker deleting a generation that a writer has just made live. A healthy checksum detects missing or damaged data; it cannot restore bytes deleted from every replica.

**Scope.** The target was the Go repository `blobgate`. A temporary snapshot included all tracked files as present in the working tree, including eight pre-existing modified files in the API, tests, chart, and documentation. Untracked build outputs were excluded. SHA-256 of the tracked binary diff against HEAD was `8d0f7672837a6c759f3346d95bcd37efd163a318a2a6277ec5ea8021eca33c0c`. Review covered authentication, S3 dispatch, conditional writes, encrypted storage, replication, cleanup, resource limits and the database client. Graphify assisted navigation. Findings describe source-level behavior and preconditions. The test results later in this report are separate evidence.

## Status of the findings (added 2026-11-27, revised 2026-11-28)

The review below is as it was written, against commit 58a5dc5. This section was added later and says what was done about it. "Fixed" means changed and covered by tests of the repository; it is not a second review. **The review did not find everything**: two defects that lose data were found afterwards, while its acceptance tests were being written (below).

| Finding | Status | Release |
|---|---|---|
| 1. Upload expiry and reconciliation can delete a live generation | Fixed: the sweep claims the upload before it removes anything, a commit requires it, a queued removal asks again who owns the directory | 0.12.0 |
| 2. Operation semantics outside the verified signature | Fixed: headers that decide the operation must be signed; small bodies, declared checksums and trailers are verified | 0.12.0 |
| 3. Write preconditions evaluated before commit | Fixed: decided in the writing transaction; `If-Match` compares strongly | 0.12.0 |
| 4. Numeric encryption-key aliases | Fixed: canonical ids only; a key in use cannot be changed or dropped | 0.12.0 |
| 5. Repair settlement and lost enqueue failures | Fixed: settled by revision; a write is refused when a missed copy cannot be recorded | 0.12.0 |
| 6. Quotas and buffer slots | Fixed: quotas decided in the transaction and counting unfinished parts; a per-tenant limit of held blocks; metrics. The per-tenant limit is per replica; a write in one call can occupy node space above the quota until it ends | 0.12.0, 0.13.0 |
| Error text in responses | Fixed: a kind and a reference; the text goes to the log. The admin API's refusals of a store's settings still carry the node's error | 0.13.0 |
| Unknown access key | Same answer and the same work as a wrong signature. Failed calls are not rate-limited by the program | 0.13.0 |
| One SHA-256 for the seal and store keys | **Not changed**: the documents require random secrets. No password KDF: passphrases are not supported | 0.13.0 (documents) |
| Reconcile memory and pagination | Fixed in part: the database is asked in batches and the report is bounded; the listing of one directory of a node is still held whole | 0.13.0 |
| Large handlers | Fixed in part: split into files by concern and the sweep into claim and removal; the code was moved, not rewritten | 0.13.0 |
| Integer decoding | Fixed: exact integers; a cell of the wrong type is an error | 0.13.0 |
| Benchmarks | Done for local nodes only ([docs/benchmark.md](docs/benchmark.md)). **Not run**: remote backends, network loss, a cluster | 0.13.0 |

### Found after the review

Both lose data, both were present from the first release (0.1.0), and neither is a finding of the review.

| Defect | Release that fixes it |
|---|---|
| An abort of an upload removed the blocks of its directory without asking whether the upload had ended: after a completion they are the object's (with deduplication, of every object that shares them). An abort at the same moment as a completion could acknowledge the completion and remove its blocks | 0.13.1 |
| Every attempt of a part wrote the same file: a retry that failed removed the block of the part that had been accepted, and in a sealed store a late retry replaced it with other bytes | 0.14.0 (not compatible with earlier releases) |

A sweep that died after it had claimed an upload and before it had written down the removal left blocks that nothing remembered (no data was lost; fixed in 0.13.1).

### The acceptance tests that the review asks for

| Asked | State |
|---|---|
| 1. Cleanup against completion: both orders, deduplication, two replicas, a worker restart | Tests (`sweeprace_test.go`, `abort_test.go`) |
| 2. Mismatches over signed and presigned requests: PUT, copy, part copy, bulk delete, completion; nothing changes when refused | Tests (`s3signed_test.go`, `acceptance_presign_test.go`). The body of a presigned request is not covered by its signature in S3, and is not here |
| 3. Two writers under one condition on two replicas | Tests (`cond_test.go`) |
| 4. Ambiguous key identifiers are refused; reads after a rotation | Tests (`encrypt_test.go`) |
| 5. A replaced repair task is not closed by the old one; a task that cannot be written down | Tests (`repairrev_test.go`); the write is refused rather than recovered later |
| 6. Mixed reads, sealed uploads in parts and a slow replica together | Test with local nodes, one of them slowed (`acceptance_mixed_test.go`). Not run with a remote slow node or a real network |
| Remote and database failures in responses and logs | Tests with failures put into the nodes and the database, and with one real WebDAV server stopped (`acceptance_errors_test.go`). The log lines of the failed calls are redacted; the lines of the worker and of the libraries are not checked or redacted |

The tests of rows 2, 6 and the last were added after 0.14.0 and are not part of a release.

Not verified by any of this: external storage services under failure beyond the above, multi-node Raft failover, timing side channels. The Ceph tests were not run for the changes of 0.13.0 to 0.14.0 on the machine of the author, and CI skips them.

## Priority findings and acceptance criteria

### 1. High · Upload expiry and reconciliation can delete a live generation

SweepPass selects uploads by creation time, then removes their directories from the nodes before deleting the upload rows. Meanwhile, commit can publish the same generation and remove its upload row. Once the worker has selected a candidate, it does not atomically claim that generation against completion. A long-running upload near expiry is enough for these decisions to overlap. The maintenance lease serializes workers; API writers do not participate in that lease. Both normal and deduplicated objects can depend on the physical generation.

Reconciliation has a related gap. It lists nodes and reads live objects, dedup sets, and uploads in separate queries. An upload can become an object between these reads. An old-enough directory can consequently be classified as stray. A queued purgepath later calls RemoveDir without checking current ownership. This path requires an applied reconciliation, an eligible directory age and a concurrent state change. These are code-review findings, not observed losses in the test run. [SweepPass and repair execution](internal/maint/maint.go) · [Reconciliation](internal/maint/reconcile.go) · [Object commit](internal/api/objects.go)

**Recommendation.** Introduce a durable generation state. Expiry should atomically transition an uncommitted upload to deleting. Completion must require the same upload to remain active in its commit transaction. Destructive repair should operate only on a claimed generation that cannot become live again. Make reconciliation reports advisory until this protection exists. An immediate operational mitigation is to keep automatic apply off and set the expiry comfortably beyond the longest supported upload, including retries.

**Acceptance test.** Pause cleanup after candidate selection, complete the upload, and resume cleanup. Also run the reverse ordering and repeat for deduplication, two service replicas and worker restart. Exactly one decision may win. Every successful completion must still read correctly after all maintenance queues drain.

### 2. High for delegated S3 writes · Some operation semantics are outside the verified signature

VerifySigV4 reconstructs the signed-header list and requires host. S3 dispatch independently acts on operation-changing headers, including the copy source. The verifier does not enforce signed coverage of all such headers. Separately, the bulk-delete and multipart-completion XML paths read and interpret body bytes without comparing a supplied signed payload digest with the bytes received. Ordinary object PUT and part upload do have digest checks. A presigned capability or a request crossing an untrusted intermediary can therefore have weaker scope or integrity than its issuer intended. The reviewed copy path still applies the authenticated tenant; cross-tenant access was not demonstrated. [Verifier](internal/auth/sigv4.go) · [Dispatch and XML handling](internal/api/s3.go) · [Copy handling](internal/api/s3copy.go) · [AWS SigV4 presigned request contract](https://docs.aws.amazon.com/AmazonS3/latest/API/sigv4-query-string-auth.html)

**Recommendation.** Carry verified signed-header membership into dispatch; require coverage for headers that change operation, source selection or authenticated metadata. Centralize body verification before XML decoding and before any side effect. Allowlist supported payload modes. Where checksum trailers are accepted, verify the declared checksum or explicitly reject unsupported modes. A server-computed digest records received bytes; it cannot establish that they are the bytes the sender designated.

**Acceptance test.** Exercise ordinary PUT, copy, part copy, bulk delete and completion through signed-header and presigned requests. A mismatch in a protected field or advertised digest must leave objects, parts and repair tasks unchanged. Retain positive official AWS SDK cases.

### 3. High for clients relying on compare-and-swap · Write preconditions are evaluated before commit

precondition reads the object before block transfer or metadata updates. The later commit independently upserts the object. Two callers can both satisfy a create-only or same-version precondition, and the second commit can overwrite the first. This matters for build locks, manifests and immutable names. The source comment already identifies the concurrency limitation. The common ETag matcher also strips the weak marker for If-Match, which requires strong comparison. [Preconditions and commit](internal/api/objects.go) · [HTTP conditional requests](https://www.rfc-editor.org/rfc/rfc9110.html#section-13.1.1) · [S3 conditional writes](https://docs.aws.amazon.com/AmazonS3/latest/userguide/conditional-writes.html)

**Recommendation.** Keep an early check for efficiency, then enforce the authoritative condition in the transaction that publishes or deletes the object. Roll back reference-count and purge-task changes together on failure. Give If-Match and If-None-Match distinct comparison rules. Document the current limitation for callers that use conditional requests as locks.

**Acceptance test.** Start two writers under the same condition on different replicas. For create-only writes, at most one succeeds. A stale update fails, the winning bytes remain readable, and only unreferenced generations enter cleanup.

### 4. High impact, configuration-dependent · Numeric encryption-key aliases can select different masters

Config.Check accepts decimal key ID strings. NewCipher parses the strings and stores the results under uint32 IDs. Distinct spellings such as 1 and 01 can designate different key texts in configuration and the same ID in memory. Go map iteration does not define which value is assigned last. With such a configuration, replicas or restarts may disagree about the master for the ID recorded in ciphertext. AEAD detects the mismatch, but reads can fail. This requires an ambiguous operator configuration. [Configuration validation](internal/store/config.go) · [Key construction](internal/store/encrypt.go)

**Recommendation.** Accept only canonical decimal IDs and reject duplicate numeric identities before building the cipher. Reject changing a key's material under an existing ID in the ordinary update path. Rotate by adding a new ID and keep old material available until referenced objects and retained backups are accounted for. If aliases already exist, preserve both original secrets and validate representative reads before migrating.

**Acceptance test.** Reject ambiguous configuration deterministically at startup and update. A rejected update leaves the prior store usable; restarting after an ordinary rotation can read both old and new ciphertext.

### 5. Medium · Repair settlement can acknowledge a newer task, and enqueue failures can disappear

The worker identifies a claimed row at settlement by store, key, node and since. The timestamp is measured in whole seconds, and an upsert can replace the task with another instruction in the same second. The old worker can delete or postpone the replacement. Several queue helpers also discard database write errors, while success counters can advance independently of settlement. A later integrity pass may rediscover a missing replica, but an acknowledged write can stay under-replicated longer than expected. [RepairPass and settle](internal/maint/maint.go) · [API queue](internal/api/objects.go)

**Recommendation.** Assign each replacement a monotonically increasing revision or unique task ID. Settle by exact revision and check the affected-row count. Record partial-write repair obligations durably with object publication, or surface a degraded-durability result when persistence fails. Monitor oldest outstanding repair age and enqueue errors.

**Acceptance test.** Freeze time, replace an in-flight task twice, then settle the older task. The newest instruction must survive. Inject a database failure between a partial node write and repair registration and verify that recovery can discover the obligation.

### 6. Medium for shared deployments · Quotas and buffer slots bound different resources

Quota checks use an inventory read followed by a separate commit. Concurrent writers can exceed the configured tenant limit, as the project documents. Unknown-length writes receive a final size check, but can consume backend capacity before it. The working-tree read/write slot split improves fairness between operations; slots belong to each service process and do not reserve a tenant's storage or bound every allocation. Streaming SigV4 can additionally buffer a chunk of up to 16 MiB. Sealing needs plaintext and ciphertext buffers, and replication has its own costs. [Quota](internal/api/quota.go) · [Read/write slots](internal/api/objects.go) · [Chunk decoder](internal/auth/sigv4.go)

**Recommendation.** Label present quotas as soft. For hard isolation, reserve capacity transactionally per tenant, grow a reservation as unknown-length uploads progress, and release it on commit, abort and expiry. Include staged uploads and all service replicas in the capacity model. Keep the new read/write split; monitor slot wait, 503 rate, staged bytes, RSS and per-tenant saturation. Test mixed reads, encrypted multipart writes and a slow replica together.

## Cryptography, timing and information disclosure

- SHA-256 verification of stored bytes and fallback to a verified replica are valuable. AES-GCM protects encrypted blocks, with positional binding for ordinary blocks. Convergent encryption deliberately exposes equality within its deduplication scope. Use tenant-scoped deduplication for unrelated tenants; require explicit risk acceptance for store-wide sharing. Keep the existing tenant-only restriction on digest-based linking. [Block reads](internal/store/mirror.go) · [Encryption](internal/store/encrypt.go) · [Link scope](internal/api/link.go)
- Token MACs use HMAC-SHA-256 and fixed-length digest comparisons use Go's constant-time comparison. Length mismatch and unknown access-key handling can take different paths. The S3 API explicitly returns a different result for an unknown key. No timing-based secret recovery was established, and no statistical timing experiment was performed. If tenant-name enumeration is sensitive, return a uniform public authentication failure, retain the detailed reason in protected logs and rate-limit failures.
- Credential sealing derives an AES key with one SHA-256 operation from operator-supplied text. A length check is not an entropy check. Generate random secrets for the seal key and store encryption keys. If human passphrases must be supported, introduce a versioned password KDF with a migration path and retain recovery material securely. The Go code uses immutable strings for some secrets; do not promise deterministic erasure of all copies. [Credential sealing](internal/seal/seal.go) · [Block encryption](internal/store/encrypt.go)
- Some HTTP failures return underlying database or backend error text. This can expose internal locations, node identifiers or object layout. Credential disclosure was not verified. Return a stable public error code and correlation ID; apply redaction before detailed protected logging. Exercise representative remote and database failures in response and log-redaction tests. [Error mapping](internal/api/api.go) · [Database client](internal/rq/rq.go)

## Performance and maintainability recommendations

- Reconcile retains a map of listed generations per node, a map of known generations, and a list of strays. The sets and uploads queries are not paginated. Large inventories can exhaust heap or exceed the database client's 64 MiB response-read cap. Compare ordered, paged inventories against a consistent ownership view; bound report size, expose continuation, and measure peak memory against inventory size and replica count. [Reconcile](internal/maint/reconcile.go) · [rqlite client](internal/rq/rq.go)
- Separate request authentication, body-integrity policy and S3-to-native translation in the large handlers. One reviewed policy per operation is easier to test than scattered exceptions. Likewise, keep maintenance eligibility, claim, side effect and settlement visibly distinct.
- Decode database integer cells without going through float64 where exact counts or sizes matter. The current Int helper silently converts unexpected cell types to zero; large integers beyond 2^53 lose exact precision. Use json.Number or typed results and propagate conversion errors. [Numeric decoding](internal/rq/rq.go)
- Benchmark sustained useful throughput, p95/p99 time to first byte, backend bytes per logical byte and repair lag. Compare plain, encrypted and tenant-deduplicated stores with identical payloads and min_acks. A local-directory fixture measures gateway overhead; named remote backends and network loss require separate runs.

## Verification performed

The main run used a dedicated loopback rqlite 10.5.1 container and isolated test keys and storage. The tracked working tree was copied before building. The first sandboxed run could not open the required sockets; the figures below come from the subsequent socket-enabled runs.

| Check | Observed result |
|---|---|
| Root module: go test -p 1 -race -count=1 -cover -json ./... | 165 passing test/subtest events, 28 skipped, zero failures. Skips include opt-in backend and process fixtures. Race instrumentation reported no race on the paths exercised. |
| Selected package coverage, main run | API 84.9%, auth 87.1%, rq 94.0%, seal 85.7%, store 49.8%. Package-local coverage understates integration exercised through other packages. |
| Slow-client memory tests, working-tree slot changes | With four configured places and 24 stalled clients, the tests logged 24 MiB for readers, 25 MiB for unknown-length writers and 25 MiB for S3 part uploads. These are test-reported heap observations, not per-process RSS limits. |
| go vet ./... | Completed with no diagnostics. |
| Supplemental process and recovery tests | All three passed individually: backup and restore into an empty database, verification of every restored block, and forced process termination during a write. |

The test results cover a local, single-node environment. External storage services, multi-node Raft failover, production throughput and timing side channels require separate validation. A clean Go race run checks for memory races on executed paths. The storage findings concern ordering across operations and need deterministic concurrency tests.

## Recommended release order

1. Fix generation cleanup first. Gate release on an interleaving test in which a committed generation survives both expiry and reconciliation, including a restart.
2. Before enabling delegated S3 writes, bind operation-defining headers and request bodies to verification. Exercise each supported signing mode and require rejection before any mutation when authenticated inputs disagree.
3. Before promising conditional writes, enforce preconditions within the metadata commit. Require exactly one successful writer when two clients update against the same strong validator.
4. Validate the encryption keyring before opening storage. Reject ambiguous key identifiers and verify that every existing canonical identifier still decrypts its fixture after rotation.
5. Give repair tasks durable identities, surface queue failures, and measure repair backlog during backend outages. For shared deployments, define per-tenant admission limits and verify that one stalled tenant cannot exhaust capacity for all others.
