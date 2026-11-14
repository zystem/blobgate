# Use the Go client

How-to page. The package `github.com/zystem/blobgate/client` speaks the HTTP API of the core (`internal/api/openapi.yaml`) for one tenant: it signs the calls, and has `Put`, `Get` (a range), `Stat`, `Delete`, `List`,
`ListAll`, `Link` and `PutOrLink`. It needs nothing but the standard library and the signing of this module.

```go
c := client.New("http://blobgate:18090", "acme", os.Getenv("BLOBGATE_TENANT_SECRET"))
ctx := context.Background()

f, _ := os.Open("layer.tar") // an io.ReadSeeker
linked, err := c.PutOrLink(ctx, "main", "layers/app.tar", f, &client.Options{ContentType: "application/x-tar"})
// linked is true when the store kept these bytes already and nothing was sent

body, info, err := c.Get(ctx, "main", "layers/app.tar", 0, -1) // the whole object; Get(ctx, store, key, 100, 199) is a range
defer body.Close()
fmt.Println(info.Size, info.ETag)
```

* `PutOrLink` reads the file once to make its digest, asks the store to link (a store with `dedup: "tenant"`, [deduplication.md](deduplication.md)), and sends the bytes only when the store does not have them or
  does not dedup. `Put(ctx, store, key, reader, size, opts)` always sends; `size` of -1 is "not known" (the service cuts the body into blocks as it comes).
* An answer that says no is an `*client.Error` with `Status`, `Kind` (`no_such_object`, `quota_exceeded`, `unavailable`, `precondition_failed`, ...) and `Detail` (the words that say what to do);
  `client.IsNotFound(err)` is the test for a missing object. A 503 (`unavailable`, `database_unavailable`) is to be tried again after the `Retry-After` the service gives; the client does not retry.
* `ListAll(ctx, store, prefix, func(client.Listed) error)` walks the keys page after page; `List` is one page with the cursor `Next`.
* Multipart upload is not in the client: an object of any size is one `Put` (the service cuts the body into blocks of 8 MiB), and only a writer that wants parts in parallel needs the multipart calls of the API.

What was run (2026-11-14): `TestGoClient` in `internal/api/client_test.go` against the service in the test: a `Put` of 9 MiB with metadata, a `PutOrLink` of the same bytes that sent nothing (`linked`) and of new bytes that were sent, a whole
read and a range, `Stat`, `IsNotFound`, `ListAll`, a refused `IfNoneMatch: "*"`, a `Delete`, and `PutOrLink` against a store that does not dedup. Not run: a service behind TLS, a slow or broken network.
