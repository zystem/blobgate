# The status page

How-to and reference. The module tells which nodes of its stores answer, which lag and which are dead, on a page and as JSON, on the main port (`BLOBGATE_PORT`), next to `/healthz`.

## Look at it

Open `http://<module>:18090/status`. It shows every store with a coloured word for each node and refreshes every 5 seconds. With no token it shows the **public view**; type the admin token in the field at the bottom (it is kept in that tab only,
in `sessionStorage`, and sent in the `Authorization` header to `/v1/admin/status`) to see the **details**.

```bash
curl -s http://127.0.0.1:18090/status.json
```

```text
{"status":"yellow","stores":[{"name":"store 1","state":"yellow","nodes":[{"name":"rack-a","state":"ok"},{"name":"webdav-2","state":"lagging"}]}]}
```

The details, with the admin token (`nodes_needed_for_a_write` is the store's `min_acks`; the output is cut here):

```bash
curl -s -H "Authorization: Bearer $BLOBGATE_ADMIN_TOKEN" http://127.0.0.1:18090/v1/admin/status
```

```text
{"status":"yellow","stores":[{"name":"main","state":"yellow","nodes_needed_for_a_write":1,"nodes":[{"name":"rack-a","state":"ok","id":"local-3f9a1c2b","backend":"local","latency_ms":1,"checked_at":1795000000}, ...
```

## Name the nodes

The public view names a node by its **label**: a short name that you give it when you register the store, and that is public by design (do not put a host, a path or a secret in it). Without a label the name is the backend and the
place of the node in its store: `webdav-2` is the second node of its store. A label is 1 to 32 letters, digits, `.`, `_` or `-`.

```bash
curl -X PUT http://127.0.0.1:18090/v1/admin/stores/main -H "Authorization: Bearer $BLOBGATE_ADMIN_TOKEN" \
  -d '{"kind":"mirror","nodes":[{"remote":":webdav,url=https://a.example:/x","label":"rack-a"},{"remote":":webdav,url=https://b.example:/x","label":"rack-b"}]}'
```

A label is not part of the identity of a node: giving a node a label, or changing it, does not make a new node (the repair queue and the metrics name a node by its `id`, which is the backend and a hash of its settings).

## The states of a node

The first rule that holds wins. The numbers are fixed in the program (`internal/api/status.go`); they are not settings.

| State | Colour | When |
|---|---|---|
| `unknown` | grey | the node was not asked yet (the module has just started, or `BLOBGATE_PROBE_SECONDS=0`) |
| `dead` | red | the last 3 probes in a row failed (about 30 seconds): the node did not list its root within 3 seconds |
| `full` | yellow | the node says how much room it has and it is less than the reserve (`BLOBGATE_RESERVE_BYTES`, the rule that gives 507 `store_full`); a node that cannot say is never `full` |
| `lagging` | yellow | the node answers, but a copy for it has waited in the repair queue for 5 minutes or more |
| `slow` | yellow | the node answers, but its last probe took more than 2 seconds |
| `ok` | green | the node answers and nothing waits for it |

A store is `green` when every node is `ok`; `red` when fewer nodes than a write needs (`min_acks`) can take one (`dead`, `full` and `unknown` nodes cannot): writes are refused; `unknown` when no node was asked yet; `yellow` in every other case.
The word at the top is the worst of the stores.

## What the module asks

A probe is a listing of the root of the node (a root that is not made yet counts as an answer), and, when the node can say it, how much room it has. Every node is asked every `BLOBGATE_PROBE_SECONDS` (10), all at once, by
the replica that serves the page; the queue comes from the database and is shared by all replicas, and is read at most every 5 seconds, so many viewers do not load the database or the nodes: the page shows what the last probe found.

## What the public view does not show

No name of a store (`store 1`, `store 2`, in the order of their names), no id of a node, no host or path, no error text, no figure (room, queue, times). The details are for the admin token.
See [threat-model.md](threat-model.md): the page can be reached by whoever can reach the port, so it says what `/readyz` says in more words and no more.

## What it does not tell

* **A node that lists its root but cannot take a write** (a read-only mount, a quota in the middle of a directory) is `ok`: the probe does not write.
* The view is of **one replica**: two replicas may disagree for a few seconds when a node has just failed.
* The probe sees a node from the module; a node that is alive but cut from the module is `dead` for the module, which is what matters to it.

## Settings

See [configuration.md](configuration.md): `BLOBGATE_PROBE_SECONDS` (a restart is needed to change it).

## What was run

On 2026-11-18, in the tests of `internal/api` (`TestStatusPage`, `TestNodeAndStoreStates`) against a real rqlite: the states in order; two failed probes leave `ok` and the third makes `dead`; a store with a dead node that a write needs is `red`;
a copy that waited 10 minutes makes `lagging`; the public view contains no path, id, error or store name; the details wait for the admin token (401 with none and with a wrong one); the headers (`Cache-Control: no-store`,
`Content-Security-Policy`, `X-Frame-Options`, `X-Content-Type-Options`); a label with a space or a tag is refused. Also run by hand the same day: a module with two local nodes, the page in the browser pane of the Claude desktop app (the dark theme): both nodes `OK` and the word GREEN; one node's directory replaced by a file, after about 12 seconds that node `DEAD` and the store DOWN
(the store needs all its nodes, `min_acks` was not set); the admin token typed in the field gave the details (id, room, the error of the node). On real servers (`TestRealS3DoorDedupCopyAndStatus`, 2026-11-18, a WebDAV node and an FTP node of SFTPGo): the FTP container stopped, after three probes it is `dead` and the WebDAV node `ok`; a write with it away (`min_acks` 1) is queued; the container started again and, with the queued copy made 10 minutes old in the database, the node is `lagging`; after the repair pass both are `ok`. `TestRealMinIOAndSFTP`: a stopped SFTP node is `dead`, and `ok` when started again. The page was looked at on 2026-11-19 in the browser pane of the Claude desktop app, in the light and the dark theme, at 1400 pixels, at about 1000 and at 375 pixels (a phone), with nodes `ok`, `dead` and `lagging`, in the public view and with the details: on a screen narrower than 72 rem a node of the details is a small card of labels and values (nothing is scrolled sideways, the page does not scroll sideways either), on a wider one it is a row of a table. Two replicas (`TestTwoReplicasAndStatus`, 2026-11-20, two servers on one rqlite and the same two nodes): what one writes the other reads; a store made or changed through one is seen by the other; a node that one replica found dead is `ok` in the page of the other until that one has probed it three times, then `dead` in both; a write with the node away (`min_acks` 1) is queued in the database and both pages show the same queue. By hand with two processes of the binary (2026-11-20, one rqlite, two local nodes, `BLOBGATE_PROBE_SECONDS=3`): both pages `green`; a write through one was read through the other; one process stopped, the other kept the lease and the page. A node of the `local` backend whose directory cannot be read (mode 000) is **not** found by the probe: rclone logs the error and answers with an empty listing, so the node stays `ok` (nodes of a network backend are found; this was not checked for every backend). **Not run:** an S3 node that goes away; a real phone (the phone size was emulated); a screen reader; a browser other than the one of the pane (Chromium).
