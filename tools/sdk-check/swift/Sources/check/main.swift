import AWSS3
import AWSSDKIdentity
import Foundation

var fails = 0
func check(_ ok: Bool, _ msg: String) {
    if ok { print("ok:   \(msg)") } else { print("FAIL: \(msg)"); fails += 1 }
}

// a body that is the same for the same seed
func bytes(_ n: Int, _ seed: UInt64) -> Data {
    var s = seed &* 6364136223846793005 &+ 1442695040888963407
    var d = Data(count: n)
    for i in 0..<n {
        s = s &* 6364136223846793005 &+ 1442695040888963407
        d[i] = UInt8(truncatingIfNeeded: s >> 33)
    }
    return d
}

let args = CommandLine.arguments
let endpoint = args.count > 1 ? args[1] : "http://127.0.0.1:18093"
let access = args.count > 2 ? args[2] : "acme"
let secret = args.count > 3 ? args[3] : "x"
let bucket = "demo"

func run() async throws {
    let creds = AWSCredentialIdentity(accessKey: access, secret: secret)
    let resolver = try StaticAWSCredentialIdentityResolver(creds)
    let cfg = try await S3Client.S3ClientConfiguration(
        awsCredentialIdentityResolver: resolver, region: "us-east-1", forcePathStyle: true, endpoint: endpoint)
    let c = S3Client(config: cfg)

    // one byte with a type and metadata
    let one = bytes(1, 1)
    _ = try await c.putObject(input: PutObjectInput(body: .data(one), bucket: bucket, contentType: "application/x-test", key: "swift/one", metadata: ["owner": "swift"]))
    let g = try await c.getObject(input: GetObjectInput(bucket: bucket, key: "swift/one"))
    let got = try await g.body?.readData()
    check(got == one, "the byte is the one sent")
    check(g.contentType == "application/x-test", "content type kept: \(g.contentType ?? "nil")")
    check(g.metadata?["owner"] == "swift", "metadata owner kept")

    // 30 MB in four parts of 8 MiB by hand
    let part = 8 << 20
    let whole = bytes(30_000_007, 2)
    let cu = try await c.createMultipartUpload(input: CreateMultipartUploadInput(bucket: bucket, key: "swift/big"))
    var done: [S3ClientTypes.CompletedPart] = []
    var n = 1
    var off = 0
    while off < whole.count {
        let chunk = whole.subdata(in: off..<min(off + part, whole.count))
        let up = try await c.uploadPart(input: UploadPartInput(body: .data(chunk), bucket: bucket, key: "swift/big", partNumber: n, uploadId: cu.uploadId))
        done.append(S3ClientTypes.CompletedPart(checksumCRC32: up.checksumCRC32, eTag: up.eTag, partNumber: n))
        n += 1
        off += part
    }
    _ = try await c.completeMultipartUpload(input: CompleteMultipartUploadInput(bucket: bucket, key: "swift/big", multipartUpload: S3ClientTypes.CompletedMultipartUpload(parts: done), uploadId: cu.uploadId))
    check(done.count == 4, "four parts of 8 MiB were sent")
    let h = try await c.headObject(input: HeadObjectInput(bucket: bucket, key: "swift/big"))
    check(h.contentLength == whole.count, "HeadObject length \(h.contentLength ?? -1)")
    print("info: ETag of the 30 MB object \(h.eTag ?? "nil")")
    check((h.eTag ?? "").hasSuffix("-4\""), "the ETag of a multipart object ends with -4")
    let gb = try await c.getObject(input: GetObjectInput(bucket: bucket, key: "swift/big"))
    check(try await gb.body?.readData() == whole, "the downloaded object is the one sent")
    let gr = try await c.getObject(input: GetObjectInput(bucket: bucket, key: "swift/big", range: "bytes=8388600-8388700"))
    check(try await gr.body?.readData() == whole.subdata(in: 8388600..<8388701), "the range across a block border is the right bytes")

    // copy, list, delete
    _ = try await c.copyObject(input: CopyObjectInput(bucket: bucket, copySource: "\(bucket)/swift/big", key: "swift/copy"))
    let l = try await c.listObjectsV2(input: ListObjectsV2Input(bucket: bucket, prefix: "swift/"))
    check(l.contents?.count == 3, "ListObjectsV2 shows 3 objects: \(l.contents?.count ?? -1)")
    for k in ["swift/one", "swift/big", "swift/copy"] {
        _ = try await c.deleteObject(input: DeleteObjectInput(bucket: bucket, key: k))
    }
    let l2 = try await c.listObjectsV2(input: ListObjectsV2Input(bucket: bucket, prefix: "swift/"))
    check((l2.contents?.count ?? 0) == 0, "the list is empty after the deletes")
}

do { try await run() } catch { print("FAIL: exception: \(error)"); fails += 1 }
print(fails == 0 ? "ALL OK 0" : "FAILED \(fails)")
exit(fails == 0 ? 0 : 1)
