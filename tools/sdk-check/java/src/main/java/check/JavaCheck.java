package check;

// What the S3 door answers to the Java SDK of AWS (v2): objects, parts (the transfer manager), copies, lists, presigned addresses, deletes.
// usage: mvn -q compile exec:java -Dexec.mainClass=check.JavaCheck -Dexec.args="ENDPOINT ACCESS_KEY SECRET_KEY BUCKET"   (the keys: `blobgate s3-key <tenant> <secret>`)
// Exit status 0 when every step passed.

import java.io.IOException;
import java.net.URI;
import java.net.http.HttpClient;
import java.net.http.HttpRequest;
import java.net.http.HttpResponse;
import java.nio.file.Files;
import java.nio.file.Path;
import java.security.MessageDigest;
import java.time.Duration;
import java.util.ArrayList;
import java.util.Arrays;
import java.util.HexFormat;
import java.util.List;
import java.util.Map;
import java.util.concurrent.Callable;

import software.amazon.awssdk.auth.credentials.AwsBasicCredentials;
import software.amazon.awssdk.auth.credentials.StaticCredentialsProvider;
import software.amazon.awssdk.core.sync.RequestBody;
import software.amazon.awssdk.regions.Region;
import software.amazon.awssdk.services.s3.S3AsyncClient;
import software.amazon.awssdk.services.s3.S3Client;
import software.amazon.awssdk.services.s3.model.*;
import software.amazon.awssdk.services.s3.presigner.S3Presigner;
import software.amazon.awssdk.services.s3.presigner.model.GetObjectPresignRequest;
import software.amazon.awssdk.services.s3.presigner.model.PutObjectPresignRequest;
import software.amazon.awssdk.transfer.s3.S3TransferManager;
import software.amazon.awssdk.transfer.s3.model.CopyRequest;
import software.amazon.awssdk.transfer.s3.model.DownloadFileRequest;
import software.amazon.awssdk.transfer.s3.model.UploadFileRequest;

public class JavaCheck {
    static String bucket;
    static final List<String> failed = new ArrayList<>();

    static void step(String name, Callable<Void> fn) {
        try {
            fn.call();
            System.out.println("PASS " + name);
        } catch (Throwable e) {
            failed.add(name);
            String m = String.valueOf(e.getMessage());
            System.out.println("FAIL " + name + " -> " + e.getClass().getSimpleName() + " " + m.substring(0, Math.min(300, m.length())));
        }
    }

    static void ok(boolean c, String m) {
        if (!c) throw new AssertionError(m);
    }

    static String sha(byte[] b) throws Exception {
        return HexFormat.of().formatHex(MessageDigest.getInstance("SHA-256").digest(b));
    }

    static byte[] body(int n) {
        byte[] b = new byte[n];
        for (int i = 0; i < n; i++) b[i] = (byte) (i * 7 + (i >> 8));
        return b;
    }

    public static void main(String[] a) throws Exception {
        String endpoint = a[0], ak = a[1], sk = a[2];
        bucket = a[3];
        var creds = StaticCredentialsProvider.create(AwsBasicCredentials.create(ak, sk));
        S3Client s3 = S3Client.builder().endpointOverride(URI.create(endpoint)).region(Region.EU_WEST_1).credentialsProvider(creds).forcePathStyle(true).build();
        S3AsyncClient async = S3AsyncClient.builder().endpointOverride(URI.create(endpoint)).region(Region.EU_WEST_1).credentialsProvider(creds).forcePathStyle(true)
                .multipartEnabled(true).multipartConfiguration(c -> c.minimumPartSizeInBytes(8L * 1024 * 1024).thresholdInBytes(8L * 1024 * 1024)).build();
        S3TransferManager tm = S3TransferManager.builder().s3Client(async).build();
        S3Presigner presigner = S3Presigner.builder().endpointOverride(URI.create(endpoint)).region(Region.EU_WEST_1).credentialsProvider(creds)
                .serviceConfiguration(software.amazon.awssdk.services.s3.S3Configuration.builder().pathStyleAccessEnabled(true).build()).build();
        byte[] small = "hello from java".getBytes();
        byte[] big = body(20 * 1024 * 1024 + 321);

        step("put, head, get, the MD5 ETag", () -> {
            var r = s3.putObject(PutObjectRequest.builder().bucket(bucket).key("k/small").contentType("text/x-java").metadata(Map.of("owner", "java")).build(), RequestBody.fromBytes(small));
            ok(r.eTag().equals("\"" + HexFormat.of().formatHex(MessageDigest.getInstance("MD5").digest(small)) + "\""), r.eTag());
            var h = s3.headObject(HeadObjectRequest.builder().bucket(bucket).key("k/small").build());
            ok(h.contentLength() == small.length && h.contentType().equals("text/x-java") && "java".equals(h.metadata().get("owner")), h.metadata().toString());
            ok(Arrays.equals(s3.getObjectAsBytes(GetObjectRequest.builder().bucket(bucket).key("k/small").build()).asByteArray(), small), "body");
            return null;
        });
        step("range read", () -> {
            var r = s3.getObjectAsBytes(GetObjectRequest.builder().bucket(bucket).key("k/small").range("bytes=2-5").build());
            ok(Arrays.equals(r.asByteArray(), Arrays.copyOfRange(small, 2, 6)), "range");
            return null;
        });
        step("transfer manager: upload of 20 MiB in parts, download, compare", () -> {
            Path src = Files.createTempFile("src", ".bin"), dst = Files.createTempFile("dst", ".bin");
            Files.delete(dst);
            Files.write(src, big);
            tm.uploadFile(UploadFileRequest.builder().putObjectRequest(p -> p.bucket(bucket).key("k/big")).source(src).build()).completionFuture().join();
            tm.downloadFile(DownloadFileRequest.builder().getObjectRequest(g -> g.bucket(bucket).key("k/big")).destination(dst).build()).completionFuture().join();
            ok(sha(Files.readAllBytes(dst)).equals(sha(big)), "the downloaded file differs");
            return null;
        });
        step("CopyObject, metadata REPLACE, the transfer manager's copy in parts", () -> {
            s3.copyObject(CopyObjectRequest.builder().sourceBucket(bucket).sourceKey("k/small").destinationBucket(bucket).destinationKey("k/small-copy").build());
            ok(Arrays.equals(s3.getObjectAsBytes(GetObjectRequest.builder().bucket(bucket).key("k/small-copy").build()).asByteArray(), small), "copy");
            s3.copyObject(CopyObjectRequest.builder().sourceBucket(bucket).sourceKey("k/small").destinationBucket(bucket).destinationKey("k/small-meta")
                    .metadataDirective(MetadataDirective.REPLACE).metadata(Map.of("owner", "other")).build());
            ok("other".equals(s3.headObject(HeadObjectRequest.builder().bucket(bucket).key("k/small-meta").build()).metadata().get("owner")), "metadata");
            tm.copy(CopyRequest.builder().copyObjectRequest(c -> c.sourceBucket(bucket).sourceKey("k/big").destinationBucket(bucket).destinationKey("k/big-copy")).build()).completionFuture().join();
            ok(sha(s3.getObjectAsBytes(GetObjectRequest.builder().bucket(bucket).key("k/big-copy").build()).asByteArray()).equals(sha(big)), "big copy");
            return null;
        });
        step("listing, pages, delimiter", () -> {
            List<String> keys = new ArrayList<>();
            for (var p : s3.listObjectsV2Paginator(ListObjectsV2Request.builder().bucket(bucket).prefix("k/").maxKeys(2).build())) {
                p.contents().forEach(o -> keys.add(o.key()));
            }
            keys.sort(null);
            ok(keys.equals(List.of("k/big", "k/big-copy", "k/small", "k/small-copy", "k/small-meta")), keys.toString());
            var r = s3.listObjectsV2(ListObjectsV2Request.builder().bucket(bucket).delimiter("/").build());
            ok(r.commonPrefixes().stream().anyMatch(c -> c.prefix().equals("k/")), r.commonPrefixes().toString());
            return null;
        });
        step("presigned GET and PUT", () -> {
            var http = HttpClient.newHttpClient();
            var g = presigner.presignGetObject(GetObjectPresignRequest.builder().signatureDuration(Duration.ofMinutes(5)).getObjectRequest(r -> r.bucket(bucket).key("k/small")).build());
            ok(Arrays.equals(http.send(HttpRequest.newBuilder(g.url().toURI()).build(), HttpResponse.BodyHandlers.ofByteArray()).body(), small), "presigned get");
            var p = presigner.presignPutObject(PutObjectPresignRequest.builder().signatureDuration(Duration.ofMinutes(5)).putObjectRequest(r -> r.bucket(bucket).key("k/presigned-put")).build());
            var resp = http.send(HttpRequest.newBuilder(p.url().toURI()).PUT(HttpRequest.BodyPublishers.ofString("through a presigned address")).build(), HttpResponse.BodyHandlers.ofString());
            ok(resp.statusCode() == 200, "status " + resp.statusCode() + " " + resp.body());
            ok(new String(s3.getObjectAsBytes(GetObjectRequest.builder().bucket(bucket).key("k/presigned-put").build()).asByteArray()).equals("through a presigned address"), "presigned put");
            return null;
        });
        step("uploads in progress: list, pages, abort", () -> {
            List<String> ids = new ArrayList<>();
            for (int i = 0; i < 3; i++) ids.add(s3.createMultipartUpload(CreateMultipartUploadRequest.builder().bucket(bucket).key("k/u/" + i).build()).uploadId());
            var r = s3.listMultipartUploads(ListMultipartUploadsRequest.builder().bucket(bucket).prefix("k/u/").build());
            ok(r.uploads().stream().map(MultipartUpload::key).sorted().toList().equals(List.of("k/u/0", "k/u/1", "k/u/2")) && !r.isTruncated(), r.uploads().toString());
            List<String> keys = new ArrayList<>();
            String km = null, im = null;
            do {
                var p = s3.listMultipartUploads(ListMultipartUploadsRequest.builder().bucket(bucket).prefix("k/u/").maxUploads(1).keyMarker(km).uploadIdMarker(im).build());
                p.uploads().forEach(u -> keys.add(u.key()));
                km = p.isTruncated() ? p.nextKeyMarker() : null;
                im = p.isTruncated() ? p.nextUploadIdMarker() : null;
            } while (km != null);
            ok(keys.equals(List.of("k/u/0", "k/u/1", "k/u/2")), keys.toString());
            for (int i = 0; i < 3; i++) s3.abortMultipartUpload(AbortMultipartUploadRequest.builder().bucket(bucket).key("k/u/" + i).uploadId(ids.get(i)).build());
            ok(s3.listMultipartUploads(ListMultipartUploadsRequest.builder().bucket(bucket).prefix("k/u/").build()).uploads().isEmpty(), "left over");
            return null;
        });
        step("a key that is not there", () -> {
            try {
                s3.headObject(HeadObjectRequest.builder().bucket(bucket).key("k/not-there").build());
                ok(false, "found");
            } catch (NoSuchKeyException e) { /* S3 answers 404 to a head: the SDK names it so */ }
            try {
                s3.getObject(GetObjectRequest.builder().bucket(bucket).key("k/not-there").build());
                ok(false, "read");
            } catch (NoSuchKeyException e) { /* as it should */ }
            return null;
        });
        step("delete, DeleteObjects", () -> {
            s3.deleteObject(DeleteObjectRequest.builder().bucket(bucket).key("k/small").build());
            var ids = Arrays.stream(new String[]{"k/big", "k/big-copy", "k/small-copy", "k/small-meta", "k/presigned-put"}).map(k -> ObjectIdentifier.builder().key(k).build()).toList();
            var r = s3.deleteObjects(DeleteObjectsRequest.builder().bucket(bucket).delete(d -> d.objects(ids)).build());
            ok(!r.hasErrors() || r.errors().isEmpty(), r.errors().toString());
            ok(s3.listObjectsV2(ListObjectsV2Request.builder().bucket(bucket).prefix("k/").build()).keyCount() == 0, "left over");
            return null;
        });
        System.out.println("aws-sdk java v2 " + software.amazon.awssdk.core.util.VersionInfo.SDK_VERSION + " - " + (failed.isEmpty() ? "all passed" : "FAILED: " + String.join(", ", failed)));
        tm.close();
        System.exit(failed.isEmpty() ? 0 : 1);
    }
}
