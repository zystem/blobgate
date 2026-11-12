# blobgate

A storage service that keeps your blocks of data on any number of servers that [rclone](https://rclone.org) can talk to (FTP, WebDAV, SFTP, S3, a local disk, and more), checks every block against its SHA-256 when it reads it, and keeps the servers alike. It is the storage module of [CiNim](https://github.com/zystem/CiNim).

## When to use it, and when not

Use it when you need an HTTP API for objects (put, read a range, delete, multipart) over storage that you do not control or that is of different kinds, with a copy on more than one server, and you can run
an [rqlite](https://rqlite.io) next to it (it keeps its records there and nothing on a disk of its own).

Do not use it as a general S3 server (the API is the small one of `internal/api/openapi.yaml`, made for CiNim's core), when one bucket of one provider is enough (use that provider), or when you need
to list the contents of a store (it never lists; its inventory is the database).

## The parts

```
 CiNim core ──signed calls──▶ ┌──────────────┐ ──rclone──▶ node 1 (WebDAV)
 step shims ──grants────────▶ │   blobgate   │ ──rclone──▶ node 2 (FTP)       every block is on every node
                              └──────┬───────┘ ──rclone──▶ node 3 (S3 ...)    (min_acks says how many must confirm)
                                     │ records: stores, objects, repair queue, lease
                                     ▼
                                   rqlite
```

An object is a row in rqlite and a set of blocks (files) on the nodes. The row is the commit; a block that is on a node but is not in a row is a stray, and is removed.

## Try it (a minute, no hardware, Docker only)

```bash
docker build -t blobgate .
docker run -d --name rq -p 4001:4001 rqlite/rqlite -fk -disco-mode=
docker run -d --name blobgate --network host --read-only --tmpfs /data:rw,mode=1777,size=2g \
  -e CINIM_RQLITE_URL=http://127.0.0.1:4001 -e CINIM_STORAGE_PORT=18090 \
  -e CINIM_STORAGE_SHARDS=001=a-secret-of-at-least-16-chars \
  -e CINIM_STORAGE_ADMIN_TOKEN=an-admin-token-16-chars -e CINIM_STORAGE_SEAL_KEY=a-seal-key-16-chars-long blobgate
```

Check that it is up:

```bash
curl -s localhost:18090/readyz
```

```
{"checks":{"database":"ok"},"status":"ready"}
```

Make a store of two nodes (two directories here; any rclone connection string works):

```bash
curl -s -X PUT localhost:18090/v1/admin/stores/demo -H "Authorization: Bearer an-admin-token-16-chars" \
  -d '{"kind":"mirror","default":true,"nodes":[{"remote":":local:/data/node1"},{"remote":":local:/data/node2"}]}'
```

```
{"checked":true,"name":"demo"}
```

Put an object and read it back. A call of the core carries a token; `blobgate token` makes one (the output of the token is cut here):

```bash
T=$(docker run --rm --entrypoint /blobgate blobgate token 001 a-secret-of-at-least-16-chars PUT /v1/stores/_default/objects/acme/hello.txt)
curl -s -X PUT localhost:18090/v1/stores/_default/objects/acme/hello.txt -H "Authorization: Bearer $T" -H "X-Cinim-Shard: 001" \
  -H "X-Content-SHA256: $(printf 'hello blobgate' | sha256sum | cut -d' ' -f1)" --data-binary 'hello blobgate'
```

```
{"size":14}
```

```bash
T=$(docker run --rm --entrypoint /blobgate blobgate token 001 a-secret-of-at-least-16-chars GET /v1/stores/_default/objects/acme/hello.txt)
curl -s localhost:18090/v1/stores/_default/objects/acme/hello.txt -H "Authorization: Bearer $T" -H "X-Cinim-Shard: 001" -H "Range: bytes=0-13"
```

```
hello blobgate
```

Run on 2026-11-12 with the image of this repository, as shown. Next: [connect a real node](docs/configuration.md), [back up the database](docs/backup.md).

## Ways to install

| Way | For | Notes |
|---|---|---|
| `docker build` of the `Dockerfile` | production | static program, no shell, user 65532, works with a read-only root; the module writes nothing to a disk |
| `go build ./cmd/blobgate` (Go 1.26) | tests, a workstation | `go test ./...` skips what needs Docker or rqlite unless `CINIM_RQLITE_URL` and `BLOBGATE_DOCKER_TESTS=1` are set |
| the chart of CiNim | a cluster | not made yet |

## Words

* **store**: a named set of nodes (a mirror), or one S3 bucket; objects are put in a store.
* **node**: one server that rclone talks to; a block is written to every node of its store.
* **block**: a file of at most 8 MiB on a node; an object is one or more blocks.
* **generation**: one write of an object; its blocks lie under their own path and are never overwritten.
* **shard**: a CiNim tenant group; its number is the first part of every key, and each has its own secret.
* **grant**: a token the core gives a step's shim to put the parts of one upload or read one object on the port for shims.
* **repair**: copying a block to a node that missed it, from a node that has it, checked by its digest.

## The other documents

| You want to | Read |
|---|---|
| know how it keeps data, and what a crash leaves | [docs/data-lifecycle.md](docs/data-lifecycle.md), [docs/rclone-probe.md](docs/rclone-probe.md) |
| set it up | [docs/configuration.md](docs/configuration.md) (every setting), [docs/encryption.md](docs/encryption.md) |
| call it | [internal/api/openapi.yaml](internal/api/openapi.yaml), served at `/v1/openapi.yaml` |
| keep it safe | [docs/backup.md](docs/backup.md) |
| watch it | [docs/metrics.md](docs/metrics.md) |
| judge its speed | [docs/benchmark.md](docs/benchmark.md) |
| change it | [docs/README.md](docs/README.md) (the list of pages and their kinds), [docs/status.md](docs/status.md) (what is built and what is not) |
