# Metrics reference

Reference page. `GET /metrics` on the main port with the admin token, or on `BLOBGATE_METRICS_PORT` with no token. The labels have few values: the route is one of a fixed list, and no key of an object is ever a label.

| Metric | Type | Labels | Meaning |
|---|---|---|---|
| `blobgate_requests_total` | counter | route, method, status (`2xx`...) | Requests answered. Routes: `object_put`, `part_put`, `upload_begin`, `upload_complete`, `upload_abort`, `read`, `stat`, `parts`, `object_delete`, `stores`, `admin`, `shim_part_put`, `shim_read`, the routes of the S3 door (`s3_buckets`, `s3_bucket`, `s3_delete_many`, `s3_put`, `s3_get`, `s3_head`, `s3_delete`, `s3_part_put`, `s3_parts`, `s3_upload_begin`, `s3_upload_complete`, `s3_upload_abort`), `health`, `metrics`, `openapi`, `other`. |
| `blobgate_request_duration_seconds` | histogram | route | Time to answer. |
| `blobgate_bytes_total` | counter | direction (`in`, `out`) | Bytes of bodies received and sent. |
| `blobgate_node_errors_total` | counter | store, node, op (`write`, `read`, `damaged`) | Operations a node failed; `damaged` is a read that met a copy that is there and is not the block (wrong bytes or length), which is taken from the other node and queued for repair. The node is `<backend>-<hash>`, never a secret. |
| `blobgate_refused_total` | counter | reason (`store_full`, `busy`) | Calls refused: `store_full` a write for want of room, `busy` a call that waited for a place of `BLOBGATE_HELD_BLOCKS` and found none (`503`). |
| `blobgate_held_blocks` | gauge | kind (`read`, `write`) | Places of `BLOBGATE_HELD_BLOCKS` taken now; a kind takes at most three quarters. |
| `blobgate_held_wait_seconds` | histogram | kind | Time that a call which found no free place waited for one (the calls that found one at once are not counted). |
| `blobgate_staged_bytes` | gauge | store | Bytes of the parts of uploads that have not ended: room that is taken on each node and belongs to no object yet (read at most every 15 s). |
| `blobgate_store_free_bytes`, `blobgate_store_total_bytes` | gauge | store | Room a store said it had at the last write (stores that say). |
| `blobgate_repair_pending` | gauge | store | Repair tasks waiting. |
| `blobgate_repair_oldest_age_seconds` | gauge | | Age of the oldest task waiting: **alert on it**; a growing value means a node that does not come back. |
| `blobgate_repair_tasks_total` | counter | result (`done`, `failed`) | Tasks worked. |
| `blobgate_check_tasks_total` | counter | | Copy tasks made by the check of the inventory. |
| `blobgate_repair_enqueue_errors_total` | counter | | Repair tasks (a copy for a node that missed a write, a removal) that could not be written down in the database. A write whose copy cannot be written down is refused with 503. |
| `blobgate_swept_uploads_total` | counter | | Uploads that never ended whose blocks were removed. |
| `blobgate_dedup_objects_total` | counter | `store` | Objects written whose bytes were in the store already (they took a reference on the blocks that were there). |
| `blobgate_dedup_saved_bytes_total` | counter | `store` | Bytes of those objects: what was not kept a second time. |
| `blobgate_sets_removed_total` | counter | | Sets of blocks that no object pointed to any more, whose blocks were removed. |
| `blobgate_objects` | gauge | store | Objects in the inventory. |
| `blobgate_lease_held` | gauge | name | 1 while this replica holds the lease of a job (`repair`). |
| `blobgate_last_backup_timestamp_seconds` | gauge | | When this replica wrote the last backup of the database. |
| `blobgate_database_up` | gauge | | 1 when the database answered the last time it was asked (read at most every 15 s). |
| `go_*`, `process_*` | | | The runtime and the process (memory, file descriptors). |
