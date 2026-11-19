// What the S3 door answers to the .NET SDK of AWS (AWSSDK.S3): objects, parts (TransferUtility), copies, lists, presigned addresses, deletes, uploads in progress.
// usage: dotnet run -- ENDPOINT ACCESS_KEY SECRET_KEY BUCKET     (the keys: `blobgate s3-key <tenant> <secret>`)
// Exit status 0 when every step passed.
using System.Security.Cryptography;
using Amazon.S3;
using Amazon.S3.Model;
using Amazon.S3.Transfer;
using Amazon.Runtime;

var (endpoint, ak, sk, bucket) = (args[0], args[1], args[2], args[3]);
var cfg = new AmazonS3Config { ServiceURL = endpoint, ForcePathStyle = true, AuthenticationRegion = "eu-west-1" };
var s3 = new AmazonS3Client(new BasicAWSCredentials(ak, sk), cfg);
var failed = new List<string>();

async Task Step(string name, Func<Task> fn)
{
    try { await fn(); Console.WriteLine("PASS " + name); }
    catch (Exception e) { failed.Add(name); var m = e.Message; Console.WriteLine($"FAIL {name} -> {e.GetType().Name} {m[..Math.Min(300, m.Length)]}"); }
}
void Ok(bool c, string m = "assertion failed") { if (!c) throw new Exception(m); }
string Hex(byte[] b) => Convert.ToHexString(b).ToLowerInvariant();
string Sha(byte[] b) => Hex(SHA256.HashData(b));
async Task<byte[]> Read(string key, string range = null)
{
    var r = await s3.GetObjectAsync(new GetObjectRequest { BucketName = bucket, Key = key, ByteRange = range == null ? null : new ByteRange(range) });
    using var ms = new MemoryStream(); await r.ResponseStream.CopyToAsync(ms); return ms.ToArray();
}

var big = new byte[20 * 1024 * 1024 + 321];
for (int i = 0; i < big.Length; i++) big[i] = (byte)(i * 7 + (i >> 8));
var small = System.Text.Encoding.ASCII.GetBytes("hello from dotnet");
const int Part = 8 * 1024 * 1024;

await Step("put, head, get, the MD5 ETag", async () =>
{
    var r = await s3.PutObjectAsync(new PutObjectRequest { BucketName = bucket, Key = "d/small", InputStream = new MemoryStream(small), ContentType = "text/x-dotnet", Metadata = { ["owner"] = "dotnet" } });
    Ok(r.ETag == "\"" + Hex(MD5.HashData(small)) + "\"", r.ETag);
    var h = await s3.GetObjectMetadataAsync(new GetObjectMetadataRequest { BucketName = bucket, Key = "d/small" });
    Ok(h.ContentLength == small.Length && h.Headers.ContentType == "text/x-dotnet" && h.Metadata["owner"] == "dotnet", h.Metadata.Keys.Count.ToString());
    Ok((await Read("d/small")).SequenceEqual(small));
});
await Step("range read", async () => Ok((await Read("d/small", "bytes=2-5")).SequenceEqual(small[2..6])));
await Step("TransferUtility upload of 20 MiB in parts of 8 MiB, download, the ETag of S3", async () =>
{
    var tu = new TransferUtility(s3);
    await tu.UploadAsync(new TransferUtilityUploadRequest { BucketName = bucket, Key = "d/big", InputStream = new MemoryStream(big), PartSize = Part });
    Ok(Sha(await Read("d/big")) == Sha(big), "the object differs");
    var md5s = new List<byte>();
    for (int o = 0; o < big.Length; o += Part) md5s.AddRange(MD5.HashData(big.AsSpan(o, Math.Min(Part, big.Length - o))));
    var want = "\"" + Hex(MD5.HashData(md5s.ToArray())) + "-" + ((big.Length + Part - 1) / Part) + "\"";
    var got = (await s3.GetObjectMetadataAsync(new GetObjectMetadataRequest { BucketName = bucket, Key = "d/big" })).ETag;
    Ok(got == want, $"ETag {got}, S3 makes {want}");
});
await Step("CopyObject, metadata REPLACE, CopyPart by ranges", async () =>
{
    await s3.CopyObjectAsync(new CopyObjectRequest { SourceBucket = bucket, SourceKey = "d/small", DestinationBucket = bucket, DestinationKey = "d/small-copy" });
    Ok((await Read("d/small-copy")).SequenceEqual(small));
    var rq = new CopyObjectRequest { SourceBucket = bucket, SourceKey = "d/small", DestinationBucket = bucket, DestinationKey = "d/small-meta", MetadataDirective = S3MetadataDirective.REPLACE };
    rq.Metadata["owner"] = "other";
    await s3.CopyObjectAsync(rq);
    Ok((await s3.GetObjectMetadataAsync(new GetObjectMetadataRequest { BucketName = bucket, Key = "d/small-meta" })).Metadata["owner"] == "other");
    var up = await s3.InitiateMultipartUploadAsync(new InitiateMultipartUploadRequest { BucketName = bucket, Key = "d/big-copy" });
    var parts = new List<PartETag>();
    for (int n = 1, o = 0; o < big.Length; n++, o += Part)
    {
        var r = await s3.CopyPartAsync(new CopyPartRequest { SourceBucket = bucket, SourceKey = "d/big", DestinationBucket = bucket, DestinationKey = "d/big-copy", UploadId = up.UploadId, PartNumber = n,
            FirstByte = o, LastByte = Math.Min(o + Part, big.Length) - 1 });
        parts.Add(new PartETag(n, r.ETag));
    }
    await s3.CompleteMultipartUploadAsync(new CompleteMultipartUploadRequest { BucketName = bucket, Key = "d/big-copy", UploadId = up.UploadId, PartETags = parts });
    Ok(Sha(await Read("d/big-copy")) == Sha(big), "the copy differs");
});
await Step("listing, pages, delimiter", async () =>
{
    var keys = new List<string>(); string token = null;
    do
    {
        var r = await s3.ListObjectsV2Async(new ListObjectsV2Request { BucketName = bucket, Prefix = "d/", MaxKeys = 2, ContinuationToken = token });
        keys.AddRange(r.S3Objects.Select(o => o.Key)); token = r.IsTruncated == true ? r.NextContinuationToken : null;
    } while (token != null);
    keys.Sort(StringComparer.Ordinal);
    Ok(string.Join(",", keys) == "d/big,d/big-copy,d/small,d/small-copy,d/small-meta", string.Join(",", keys));
    var d = await s3.ListObjectsV2Async(new ListObjectsV2Request { BucketName = bucket, Delimiter = "/" });
    Ok(d.CommonPrefixes.Contains("d/"), string.Join(",", d.CommonPrefixes));
});
await Step("presigned GET and PUT", async () =>
{
    using var http = new HttpClient();
    var get = s3.GetPreSignedURL(new GetPreSignedUrlRequest { BucketName = bucket, Key = "d/small", Expires = DateTime.UtcNow.AddMinutes(5), Protocol = Protocol.HTTP });
    Ok((await http.GetByteArrayAsync(get)).SequenceEqual(small));
    var put = s3.GetPreSignedURL(new GetPreSignedUrlRequest { BucketName = bucket, Key = "d/presigned-put", Verb = HttpVerb.PUT, Expires = DateTime.UtcNow.AddMinutes(5), Protocol = Protocol.HTTP });
    var res = await http.PutAsync(put, new StringContent("through a presigned address"));
    Ok(res.IsSuccessStatusCode, "status " + (int)res.StatusCode + " " + await res.Content.ReadAsStringAsync());
    Ok(System.Text.Encoding.ASCII.GetString(await Read("d/presigned-put")) == "through a presigned address");
});
await Step("uploads in progress: list, pages, abort", async () =>
{
    var ids = new List<string>();
    for (int i = 0; i < 3; i++) ids.Add((await s3.InitiateMultipartUploadAsync(new InitiateMultipartUploadRequest { BucketName = bucket, Key = $"d/u/{i}" })).UploadId);
    var r = await s3.ListMultipartUploadsAsync(new ListMultipartUploadsRequest { BucketName = bucket, Prefix = "d/u/" });
    var all = (r.MultipartUploads ?? new()).Select(u => u.Key).OrderBy(k => k, StringComparer.Ordinal).ToList();
    Ok(string.Join(",", all) == "d/u/0,d/u/1,d/u/2" && r.IsTruncated != true, string.Join(",", all));
    var keys = new List<string>(); string km = null, im = null;
    do
    {
        var p = await s3.ListMultipartUploadsAsync(new ListMultipartUploadsRequest { BucketName = bucket, Prefix = "d/u/", MaxUploads = 1, KeyMarker = km, UploadIdMarker = im });
        keys.AddRange((p.MultipartUploads ?? new()).Select(u => u.Key));
        km = p.IsTruncated == true ? p.NextKeyMarker : null; im = p.IsTruncated == true ? p.NextUploadIdMarker : null;
    } while (km != null);
    Ok(string.Join(",", keys) == "d/u/0,d/u/1,d/u/2", string.Join(",", keys));
    for (int i = 0; i < 3; i++) await s3.AbortMultipartUploadAsync(new AbortMultipartUploadRequest { BucketName = bucket, Key = $"d/u/{i}", UploadId = ids[i] });
    Ok(((await s3.ListMultipartUploadsAsync(new ListMultipartUploadsRequest { BucketName = bucket, Prefix = "d/u/" })).MultipartUploads?.Count ?? 0) == 0); // the lists of this SDK are null when empty
});
await Step("a key that is not there", async () =>
{
    try { await s3.GetObjectMetadataAsync(new GetObjectMetadataRequest { BucketName = bucket, Key = "d/not-there" }); Ok(false, "found"); }
    catch (AmazonS3Exception e) { Ok((int)e.StatusCode == 404, e.StatusCode.ToString()); }
    try { await s3.GetObjectAsync(new GetObjectRequest { BucketName = bucket, Key = "d/not-there" }); Ok(false, "read"); }
    catch (AmazonS3Exception e) { Ok(e.ErrorCode == "NoSuchKey", e.ErrorCode); }
});
await Step("delete, DeleteObjects", async () =>
{
    await s3.DeleteObjectAsync(new DeleteObjectRequest { BucketName = bucket, Key = "d/small" });
    var r = await s3.DeleteObjectsAsync(new DeleteObjectsRequest { BucketName = bucket, Objects = new[] { "d/big", "d/big-copy", "d/small-copy", "d/small-meta", "d/presigned-put" }.Select(k => new KeyVersion { Key = k }).ToList() });
    Ok((r.DeleteErrors?.Count ?? 0) == 0, string.Join(",", (r.DeleteErrors ?? new()).Select(e => e.Code)));
    Ok(((await s3.ListObjectsV2Async(new ListObjectsV2Request { BucketName = bucket, Prefix = "d/" })).KeyCount ?? 0) == 0);
});
Console.WriteLine("AWSSDK.S3 " + typeof(AmazonS3Client).Assembly.GetName().Version + " - " + (failed.Count == 0 ? "all passed" : "FAILED: " + string.Join(", ", failed)));
return failed.Count == 0 ? 0 : 1;
