// What the S3 door answers to the JavaScript SDK of AWS (v3): objects, parts (lib-storage), copies, lists, presigned addresses, deletes.
// usage: node js_check.mjs ENDPOINT ACCESS_KEY SECRET_KEY BUCKET    (the keys: `blobgate s3-key <tenant> <secret>`)
// needs: @aws-sdk/client-s3, @aws-sdk/lib-storage, @aws-sdk/s3-request-presigner. Exit status 0 when every step passed.
import { createHash } from "node:crypto";
import {
  S3Client, PutObjectCommand, GetObjectCommand, HeadObjectCommand, CopyObjectCommand, ListObjectsV2Command, DeleteObjectCommand, DeleteObjectsCommand,
  CreateMultipartUploadCommand, UploadPartCopyCommand, CompleteMultipartUploadCommand, ListMultipartUploadsCommand, AbortMultipartUploadCommand,
} from "@aws-sdk/client-s3";
import { Upload } from "@aws-sdk/lib-storage";
import { getSignedUrl } from "@aws-sdk/s3-request-presigner";

const [endpoint, accessKeyId, secretAccessKey, Bucket] = process.argv.slice(2);
const s3 = new S3Client({ endpoint, region: "eu-west-1", forcePathStyle: true, credentials: { accessKeyId, secretAccessKey } });
const sha = (b) => createHash("sha256").update(b).digest("hex");
const body = (n) => Buffer.from(Array.from({ length: n }, (_, i) => (i * 7 + (i >> 8)) & 255));
const text = async (r) => Buffer.from(await r.Body.transformToByteArray());
const big = body(20 * 1024 * 1024 + 321);
const small = Buffer.from("hello from js");
const failed = [];

async function step(name, fn) {
  try { await fn(); console.log("PASS", name); } catch (e) { failed.push(name); console.log("FAIL", name, "->", e.name, String(e.message).slice(0, 300)); }
}
const ok = (c, m) => { if (!c) throw new Error(m || "assertion failed"); };

await step("put, head, get, the MD5 ETag", async () => {
  const r = await s3.send(new PutObjectCommand({ Bucket, Key: "j/small", Body: small, ContentType: "text/x-js", Metadata: { owner: "js" } }));
  ok(r.ETag === `"${createHash("md5").update(small).digest("hex")}"`, r.ETag);
  const h = await s3.send(new HeadObjectCommand({ Bucket, Key: "j/small" }));
  ok(h.ContentLength === small.length && h.ContentType === "text/x-js" && h.Metadata.owner === "js", JSON.stringify(h.Metadata));
  ok((await text(await s3.send(new GetObjectCommand({ Bucket, Key: "j/small" })))).equals(small));
});
await step("range read", async () => {
  const r = await s3.send(new GetObjectCommand({ Bucket, Key: "j/small", Range: "bytes=2-5" }));
  ok((await text(r)).equals(small.subarray(2, 6)) && r.$metadata.httpStatusCode === 206);
});
await step("lib-storage Upload of 20 MiB in parts of 8 MiB, and read back", async () => {
  await new Upload({ client: s3, params: { Bucket, Key: "j/big", Body: big }, partSize: 8 * 1024 * 1024, queueSize: 3 }).done();
  ok(sha(await text(await s3.send(new GetObjectCommand({ Bucket, Key: "j/big" })))) === sha(big));
});
await step("lib-storage Upload with its default part size (5 MiB) of 12 MiB", async () => {
  const b = body(12 * 1024 * 1024);
  await new Upload({ client: s3, params: { Bucket, Key: "j/default-parts", Body: b } }).done();
  ok(sha(await text(await s3.send(new GetObjectCommand({ Bucket, Key: "j/default-parts" })))) === sha(b));
});
await step("CopyObject, metadata REPLACE, UploadPartCopy by ranges", async () => {
  await s3.send(new CopyObjectCommand({ Bucket, Key: "j/small-copy", CopySource: `${Bucket}/j/small` }));
  ok((await text(await s3.send(new GetObjectCommand({ Bucket, Key: "j/small-copy" })))).equals(small));
  await s3.send(new CopyObjectCommand({ Bucket, Key: "j/small-meta", CopySource: `${Bucket}/j/small`, MetadataDirective: "REPLACE", Metadata: { owner: "other" } }));
  ok((await s3.send(new HeadObjectCommand({ Bucket, Key: "j/small-meta" }))).Metadata.owner === "other");
  const { UploadId } = await s3.send(new CreateMultipartUploadCommand({ Bucket, Key: "j/big-copy" }));
  const parts = [];
  for (let n = 1, off = 0; off < big.length; n++, off += 8 * 1024 * 1024) {
    const end = Math.min(off + 8 * 1024 * 1024, big.length) - 1;
    const r = await s3.send(new UploadPartCopyCommand({ Bucket, Key: "j/big-copy", UploadId, PartNumber: n, CopySource: `${Bucket}/j/big`, CopySourceRange: `bytes=${off}-${end}` }));
    parts.push({ ETag: r.CopyPartResult.ETag, PartNumber: n });
  }
  await s3.send(new CompleteMultipartUploadCommand({ Bucket, Key: "j/big-copy", UploadId, MultipartUpload: { Parts: parts } }));
  ok(sha(await text(await s3.send(new GetObjectCommand({ Bucket, Key: "j/big-copy" })))) === sha(big));
});
await step("listing, pages, delimiter", async () => {
  const keys = []; let token;
  do {
    const r = await s3.send(new ListObjectsV2Command({ Bucket, Prefix: "j/", MaxKeys: 2, ContinuationToken: token }));
    keys.push(...(r.Contents ?? []).map((o) => o.Key)); token = r.IsTruncated ? r.NextContinuationToken : undefined;
  } while (token);
  ok(JSON.stringify(keys.sort()) === JSON.stringify(["j/big", "j/big-copy", "j/default-parts", "j/small", "j/small-copy", "j/small-meta"]), keys.join(","));
  const r = await s3.send(new ListObjectsV2Command({ Bucket, Delimiter: "/" }));
  ok((r.CommonPrefixes ?? []).some((p) => p.Prefix === "j/"), JSON.stringify(r.CommonPrefixes));
});
await step("presigned GET and PUT", async () => {
  const g = await getSignedUrl(s3, new GetObjectCommand({ Bucket, Key: "j/small" }), { expiresIn: 300 });
  ok(Buffer.from(await (await fetch(g)).arrayBuffer()).equals(small));
  const p = await getSignedUrl(s3, new PutObjectCommand({ Bucket, Key: "j/presigned-put" }), { expiresIn: 300 });
  const r = await fetch(p, { method: "PUT", body: "through a presigned address" });
  ok(r.ok, "status " + r.status);
  ok((await text(await s3.send(new GetObjectCommand({ Bucket, Key: "j/presigned-put" })))).toString() === "through a presigned address");
});
await step("uploads in progress: list, pages, abort", async () => {
  const ids = [];
  for (let i = 0; i < 3; i++) ids.push((await s3.send(new CreateMultipartUploadCommand({ Bucket, Key: `j/u/${i}` }))).UploadId);
  let r = await s3.send(new ListMultipartUploadsCommand({ Bucket, Prefix: "j/u/" }));
  ok(JSON.stringify((r.Uploads ?? []).map((u) => u.Key).sort()) === JSON.stringify(["j/u/0", "j/u/1", "j/u/2"]) && !r.IsTruncated, JSON.stringify(r.Uploads));
  const keys = []; let km, im;
  do {
    r = await s3.send(new ListMultipartUploadsCommand({ Bucket, Prefix: "j/u/", MaxUploads: 1, KeyMarker: km, UploadIdMarker: im }));
    keys.push(...(r.Uploads ?? []).map((u) => u.Key)); km = r.IsTruncated ? r.NextKeyMarker : undefined; im = r.IsTruncated ? r.NextUploadIdMarker : undefined;
  } while (km);
  ok(JSON.stringify(keys) === JSON.stringify(["j/u/0", "j/u/1", "j/u/2"]), keys.join(","));
  for (let i = 0; i < 3; i++) await s3.send(new AbortMultipartUploadCommand({ Bucket, Key: `j/u/${i}`, UploadId: ids[i] }));
  ok(((await s3.send(new ListMultipartUploadsCommand({ Bucket, Prefix: "j/u/" }))).Uploads ?? []).length === 0);
});
await step("a key that is not there", async () => {
  try { await s3.send(new HeadObjectCommand({ Bucket, Key: "j/not-there" })); ok(false, "found"); } catch (e) { ok(e.$metadata?.httpStatusCode === 404, e.name); }
  try { await s3.send(new GetObjectCommand({ Bucket, Key: "j/not-there" })); ok(false, "read"); } catch (e) { ok(e.name === "NoSuchKey", e.name); }
});
await step("delete, DeleteObjects", async () => {
  await s3.send(new DeleteObjectCommand({ Bucket, Key: "j/small" }));
  const r = await s3.send(new DeleteObjectsCommand({ Bucket, Delete: { Objects: ["j/big", "j/big-copy", "j/default-parts", "j/small-copy", "j/small-meta", "j/presigned-put"].map((Key) => ({ Key })) } }));
  ok(!r.Errors?.length, JSON.stringify(r.Errors));
  ok(((await s3.send(new ListObjectsV2Command({ Bucket, Prefix: "j/" }))).KeyCount ?? 0) === 0);
});
console.log("aws-sdk js v3 -", failed.length ? "FAILED: " + failed.join(", ") : "all passed");
process.exit(failed.length ? 1 : 0);
