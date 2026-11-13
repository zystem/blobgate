# Assurance case

Why the security requirements of [security-requirements.md](security-requirements.md) are met. A claim, the argument, and the evidence (a test, a tool, a document). It is as strong as its evidence; where there is none, it says so.

## 1. The threat model and the trust boundaries

The assets, the actors, the boundaries and the threats are in [threat-model.md](threat-model.md): network to port; the shim port to the module; the module to the nodes; the module to rqlite; the admin API. Every
call that crosses a boundary is authenticated (a token, a grant, the admin token) or is a read of a public description (`/healthz`, `/readyz`, `/v1/openapi.yaml`).

## 2. Secure design principles (Saltzer and Schroeder) are applied

| Principle | How |
|---|---|
| Economy of mechanism | one static program; no plugin, no script, no template; the storage is rclone's (maintained elsewhere) and the module adds only the checks, the mirror and the records |
| Fail-safe defaults | no store, no key, no tenant: nothing is served (`409 not_configured`, `401`); a node that fails a check is not trusted; a full store refuses; secrets are required at start (the program stops without them) |
| Complete mediation | every route, on every port, checks the token or grant before it looks at the key or the body (`coreObject`, `shim`, `admin`); a test lists every refusal (`TestRefusalsOf...`) |
| Open design | the design, the formats and the API are documented; the security does not rest on the secrecy of the design |
| Separation of privilege | three secrets with three powers: the secret of a tenant (calls), the admin token (stores, metrics), the seal key (the secrets in the database); the grant of a shim is for one object |
| Least privilege | a grant opens one upload or one object, on a port that serves nothing else; the image runs as 65532 with no capability and a read-only root; a tenant sees only its own keys |
| Least common mechanism | each tenant has its own secret; stores are separate; the nodes of a store are separate |
| Psychological acceptability | one admin call makes a store and tries it first; `blobgate validate` tells what a node lacks; every refusal says what is wrong and what to do |

## 3. Common implementation weaknesses are countered

| Weakness | Argument | Evidence |
|---|---|---|
| Injection (SQL) | every statement with a value uses parameters (`rq.S(sql, args...)`); the only text put into a statement is a name from a fixed list (the tables of the migration) | `internal/rq`, `internal/db`; `gosec` in CI |
| Path traversal | a key is validated (no empty, `.` or `..` segment, no control character or backslash, at most 1024 bytes) and every segment is escaped to a safe file name; ids of uploads are checked against hex; names of stores against a pattern | `internal/layout` tests and fuzz test; `TestRefusalsOfTheCoreAPI` |
| Broken authentication | HMAC-SHA256 tokens bound to the call and to the time, constant-time comparison, a clock skew limit; grants bound to the object; the admin token compared in constant time and at least 16 characters | `internal/auth` tests and fuzz test (vectors made by the first implementation of the same tokens) |
| Sensitive data exposure | secrets sealed in the database (AES-256-GCM, the name of the store as additional data); no secret in a log line, a metric label or an error; secrets as files; sealed backups | `internal/seal`, `internal/backup` tests; the log line has the method, the path without the tenant, the status, the time and the bytes (`logged`) |
| Cryptographic failures | the standard library and `x/crypto` only; AES-256-GCM with random nonces and a key per generation for blocks; no SHA-1, no CBC, no home-made primitive; TLS 1.2 or later when TLS is on; certificates of nodes are verified (rclone's default) | `internal/store/encrypt_test.go`, `internal/tlsutil` |
| Insecure deserialisation, unbounded input | bodies are bounded (8 MiB a block, 1 MiB for JSON), JSON is read with limits, ranges are bounded (256 MiB), the number of parts is bounded (10000) | `TestRefusalsOfTheCoreAPI`, `TestWrongDigestLeavesNothing` |
| Race conditions and data loss on a crash | one row fixes an object; the cleanup is written in the same transaction; the repair is idempotent; found by killing the process at every step | `TestCrashAtEveryStep`, `TestKillNineInTheMiddleOfAWrite`, `TestConcurrentReplacementsLeaveOneGeneration`; the tests run under `-race` |
| Components with known vulnerabilities | `govulncheck` on every change, Dependabot, dependency review, Trivy on the image | CI |
| Resource exhaustion | the blocks held for reads and sealing, and the writes to one node at once, are bounded; the repair is one block at a time | `internal/api` (`MaxReads`), `internal/store` (`NodeWrites`); not load-tested for abuse |
| Logging and monitoring failures | one structured line per request, metrics of errors of nodes and of the repair queue, `readyz` tells when the database is away | `docs/metrics.md` |

## 4. What is not covered

The limits are listed in [threat-model.md](threat-model.md) and [security-requirements.md](security-requirements.md): a compromised host, replay inside the lifetime of a token, the wire without TLS, a denial of
service by a holder of a valid token. A review by someone other than the author has not been done; the maintainers review each other's changes, and an outside review is welcome.

## Who knows how to design secure software

The primary developer designs the module as above and has built and run other security software in the same family (a key gateway, [hsmgate](https://github.com/zystem/hsmgate), with a threat model, an audit and its
response). The knowledge used is that of the principles above, of the OWASP Top 10 and of the CWE Top 25: the table of section 3 names the weaknesses and what answers each.
