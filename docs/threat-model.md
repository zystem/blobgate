# Threat model

What is protected, from whom, and how. The claims are argued in [assurance-case.md](assurance-case.md).

## Assets

The data blocks (artifacts and caches of builds); the secrets of the stores (passwords and keys of the nodes); the secrets of the tenants, the admin token, the seal key, the key of the backups; the records in rqlite (they
say what exists); the backups of the database (they hold everything of the tenant).

## Who and where

| Actor | Trust | Reaches |
|---|---|---|
| the CiNim core | trusted with its tenant | the main port, with a token for each call |
| a step's shim | semi-trusted: it runs code of a build | the shim port, with a grant for one upload or one object |
| a tool of S3 (the S3 door, if turned on) | trusted with its tenant | the S3 port, with a Signature Version 4 made from a key pair of the tenant |
| the operator | trusted | the admin API, the environment, the nodes' passwords |
| a node of a store | not trusted for integrity or, with sealing, for confidentiality | what the module sends it |
| rqlite | trusted for the records; holds no secret in the clear | the records |
| a network attacker | not trusted | the ports, and the paths between the module and the others |
| a user of another tenant | not trusted | the same module, with their own secret |

Trust boundaries: network to port; the shim port to the module (a grant, nothing else is served); the module to the nodes; the module to rqlite; the admin API to the rest.

## Threats and what answers them

| Threat | Answer | Evidence |
|---|---|---|
| a call made without a token, with another's, or changed in flight | HMAC over tenant, method, target and time; refused with 401 | `internal/auth`, `TestAuthAndTenants`, fuzz test |
| a call to the S3 door made without a signature, with another tenant's key, changed in flight, or replayed | SigV4 over method, path, query, signed headers and payload digest, within 15 minutes; the secret key is derived from the tenant's secret and is not it; every chunk of a streamed body is checked; the tenant is the access key, so a key of another tenant is another namespace | `internal/auth/sigv4_test.go` (the examples of the documentation of AWS, refusals, fuzz), `TestS3Refusals` |
| a signed call replayed within its 15 minutes, or a presigned address kept | not answered: a signed request is valid for the time it says (a presigned address, up to 7 days), which is how SigV4 works; keep the S3 port on TLS, and change the tenant's secret to end every key | |
| a tenant learns what bytes other tenants hold (deduplication) | the scope `tenant` (the default of `?link` and of the advice in docs/deduplication.md) finds sets of the same tenant only; `?link` is refused with the scope `store` | `TestLinkNeedsTenantScope`, `TestScopeOfASet` |
| a shim writes or reads what it should not | a grant is for one tenant, store, key, upload and mode; the port serves only a part PUT and a range GET | `TestRefusalsOfTheShimPort` |
| a key that leaves its place (`..`, empty segment, control characters, too long) | the key is validated and every segment is escaped to a safe file name; ids are checked against hex | `internal/layout`, fuzz test |
| a body that is not the one named, or a part changed on its way | the digest is checked before the write is kept; refused with 400, nothing left | `TestWrongDigestLeavesNothing` |
| a node returns damaged or other bytes | the read checks the digest and goes to the next node; the check and the repair find it | `TestReadChecksBlocks`, `TestCheckFindsALostDiskAndDamage` |
| the process dies in the middle of a write | one row fixes an object; the removal of what is replaced is written in the same transaction; the sweep finds what is left | `TestCrashAtEveryStep`, `TestKillNineInTheMiddleOfAWrite` |
| a stolen database | the secrets of the stores are sealed under a key that is not in it; the backups are sealed | `internal/seal`, `internal/backup` |
| a stolen node | a store can seal its blocks | `TestSealedBlocks` |
| a full disk stalls writes without a word | the module asks for the room and refuses with words, and recognises a node that says it is full | `TestStoreFull`, `TestNodeOutOfSpaceIsAnInformativeRefusal` |
| a flood of requests | the number of blocks held for reads and sealing is bounded; the number of writes at once to a node is bounded; header timeouts | not load-tested for abuse: see the limits below |
| an attacker with the admin token | out of scope: it is the operator's | |
| a wrong or old release | releases are signed with provenance; the build is repeatable | [verifying-releases.md](verifying-releases.md) |

## Limits (what is not defended)

A compromised host; replay of a token inside its two minutes; the wire without TLS; the names and sizes of objects on a node; a denial of service by a caller that holds a valid token; a rqlite that is not available.
[security-requirements.md](security-requirements.md) lists what a user can and cannot expect.
