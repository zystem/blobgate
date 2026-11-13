# Metrics reference

Reference page. `GET /metrics` on the main port with the admin token, or on `BLOBGATE_METRICS_PORT` with no token. The labels have few values: the route is one of a fixed list, and no key of an object is ever a label.

| Metric | Type | Labels | Meaning |
|---|---|---|---|
| `blobgate_requests_total` | counter | route, method, status (`2xx`...) | Requests answered. Routes: `object_put`, `part_put`, `upload_begin`, `upload_complete`, `upload_abort`, `read`, `stat`, `parts`, `object_delete`, `stores`, `admin`, `shim_part_put`, `shim_read`, `health`, `metrics`, `openapi`, `other`. |
| `blobgate_request_duration_seconds` | histogram | route | Time to answer. |
| `blobgate_bytes_total` | counter | direction (`in`, `out`) | Bytes of bodies received and sent. |
| `blobgate_node_errors_total` | counter | store, node, op (`write`, `read`) | Operations a node failed. The node is `<backend>-<hash>`, never a secret. |
| `blobgate_refused_total` | counter | reason (`store_full`) | Writes refused. |
| `blobgate_store_free_bytes`, `blobgate_store_total_bytes` | gauge | store | Room a store said it had at the last write (stores that say). |
| `blobgate_repair_pending` | gauge | store | Repair tasks waiting. |
| `blobgate_repair_oldest_age_seconds` | gauge | | Age of the oldest task waiting: **alert on it**; a growing value means a node that does not come back. |
| `blobgate_repair_tasks_total` | counter | result (`done`, `failed`) | Tasks worked. |
| `blobgate_check_tasks_total` | counter | | Copy tasks made by the check of the inventory. |
| `blobgate_swept_uploads_total` | counter | | Uploads that never ended whose blocks were removed. |
| `blobgate_objects` | gauge | store | Objects in the inventory. |
| `blobgate_lease_held` | gauge | name | 1 while this replica holds the lease of a job (`repair`). |
| `blobgate_last_backup_timestamp_seconds` | gauge | | When this replica wrote the last backup of the database. |
| `blobgate_database_up` | gauge | | 1 when the database answered the last time it was asked (read at most every 15 s). |
| `go_*`, `process_*` | | | The runtime and the process (memory, file descriptors). |
