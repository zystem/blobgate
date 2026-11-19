<?php
// What the S3 door answers to the PHP SDK of AWS (aws/aws-sdk-php): objects, parts, copies, lists, presigned addresses, deletes, uploads in progress.
// usage: php php_check.php ENDPOINT ACCESS_KEY SECRET_KEY BUCKET     (the keys: `blobgate s3-key <tenant> <secret>`)
// needs: composer require aws/aws-sdk-php. Exit status 0 when every step passed.
require 'vendor/autoload.php';

use Aws\S3\S3Client;
use Aws\S3\MultipartUploader;
use Aws\S3\MultipartCopy;
use Aws\S3\Exception\S3Exception;

[, $endpoint, $ak, $sk, $bucket] = $argv;
$s3 = new S3Client(['version' => 'latest', 'region' => 'eu-west-1', 'endpoint' => $endpoint, 'use_path_style_endpoint' => true,
    'credentials' => ['key' => $ak, 'secret' => $sk]]);
$failed = [];

function step(string $name, callable $fn): void {
    global $failed;
    try { $fn(); echo "PASS $name\n"; }
    catch (Throwable $e) { $failed[] = $name; echo "FAIL $name -> " . get_class($e) . ' ' . substr($e->getMessage(), 0, 300) . "\n"; }
}
function ok(bool $c, string $m = 'assertion failed'): void { if (!$c) throw new RuntimeException($m); }
function body(int $n): string {
    $b = '';
    for ($i = 0; $i < $n; $i += 4096) { $b .= pack('C*', ...array_map(fn($j) => (($i + $j) * 7 + (($i + $j) >> 8)) & 255, range(0, min(4096, $n - $i) - 1))); }
    return $b;
}
$big = body(20 * 1024 * 1024 + 321);
$small = 'hello from php';
$part = 8 * 1024 * 1024;

step('put, head, get, the MD5 ETag', function () use ($s3, $bucket, $small) {
    $r = $s3->putObject(['Bucket' => $bucket, 'Key' => 'p/small', 'Body' => $small, 'ContentType' => 'text/x-php', 'Metadata' => ['owner' => 'php']]);
    ok($r['ETag'] === '"' . md5($small) . '"', $r['ETag']);
    $h = $s3->headObject(['Bucket' => $bucket, 'Key' => 'p/small']);
    ok($h['ContentLength'] == strlen($small) && $h['ContentType'] === 'text/x-php' && $h['Metadata'] === ['owner' => 'php'], json_encode($h['Metadata']));
    ok((string)$s3->getObject(['Bucket' => $bucket, 'Key' => 'p/small'])['Body'] === $small);
});
step('range read', function () use ($s3, $bucket, $small) {
    $r = $s3->getObject(['Bucket' => $bucket, 'Key' => 'p/small', 'Range' => 'bytes=2-5']);
    ok((string)$r['Body'] === substr($small, 2, 4));
});
step('MultipartUploader of 20 MiB in parts of 8 MiB, download, the ETag of S3', function () use ($s3, $bucket, $big, $part) {
    $u = new MultipartUploader($s3, GuzzleHttp\Psr7\Utils::streamFor($big), ['bucket' => $bucket, 'key' => 'p/big', 'part_size' => $part]);
    $u->upload();
    ok(hash('sha256', (string)$s3->getObject(['Bucket' => $bucket, 'Key' => 'p/big'])['Body']) === hash('sha256', $big), 'the object differs');
    $md5s = '';
    foreach (str_split($big, $part) as $p) { $md5s .= md5($p, true); }
    $want = '"' . md5($md5s) . '-' . ceil(strlen($big) / $part) . '"';
    $got = $s3->headObject(['Bucket' => $bucket, 'Key' => 'p/big'])['ETag'];
    ok($got === $want, "ETag $got, S3 makes $want");
});
step('CopyObject, metadata REPLACE, MultipartCopy in parts', function () use ($s3, $bucket, $small, $big, $part) {
    $s3->copyObject(['Bucket' => $bucket, 'Key' => 'p/small-copy', 'CopySource' => "$bucket/p/small"]);
    ok((string)$s3->getObject(['Bucket' => $bucket, 'Key' => 'p/small-copy'])['Body'] === $small);
    $s3->copyObject(['Bucket' => $bucket, 'Key' => 'p/small-meta', 'CopySource' => "$bucket/p/small", 'MetadataDirective' => 'REPLACE', 'Metadata' => ['owner' => 'other']]);
    ok($s3->headObject(['Bucket' => $bucket, 'Key' => 'p/small-meta'])['Metadata'] === ['owner' => 'other']);
    $c = new MultipartCopy($s3, "/$bucket/p/big", ['bucket' => $bucket, 'key' => 'p/big-copy', 'part_size' => $part, 'source_metadata' => $s3->headObject(['Bucket' => $bucket, 'Key' => 'p/big'])->toArray()]);
    $c->copy();
    ok(hash('sha256', (string)$s3->getObject(['Bucket' => $bucket, 'Key' => 'p/big-copy'])['Body']) === hash('sha256', $big), 'the copy differs');
});
step('listing, pages, delimiter', function () use ($s3, $bucket) {
    $keys = [];
    foreach ($s3->getPaginator('ListObjectsV2', ['Bucket' => $bucket, 'Prefix' => 'p/', 'MaxKeys' => 2]) as $page) {
        foreach ($page['Contents'] ?? [] as $o) { $keys[] = $o['Key']; }
    }
    sort($keys);
    ok($keys === ['p/big', 'p/big-copy', 'p/small', 'p/small-copy', 'p/small-meta'], implode(',', $keys));
    $r = $s3->listObjectsV2(['Bucket' => $bucket, 'Delimiter' => '/']);
    ok(in_array('p/', array_column($r['CommonPrefixes'] ?? [], 'Prefix'), true), json_encode($r['CommonPrefixes'] ?? null));
});
step('presigned GET and PUT', function () use ($s3, $bucket, $small) {
    $get = (string)$s3->createPresignedRequest($s3->getCommand('GetObject', ['Bucket' => $bucket, 'Key' => 'p/small']), '+5 minutes')->getUri();
    ok(file_get_contents($get) === $small);
    $put = (string)$s3->createPresignedRequest($s3->getCommand('PutObject', ['Bucket' => $bucket, 'Key' => 'p/presigned-put']), '+5 minutes')->getUri();
    $ctx = stream_context_create(['http' => ['method' => 'PUT', 'header' => 'Content-Type: text/plain', 'content' => 'through a presigned address', 'ignore_errors' => true]]);
    file_get_contents($put, false, $ctx);
    ok(str_contains($http_response_header[0] ?? '', '200'), $http_response_header[0] ?? 'no answer');
    ok((string)$s3->getObject(['Bucket' => $bucket, 'Key' => 'p/presigned-put'])['Body'] === 'through a presigned address');
});
step('uploads in progress: list, pages, abort', function () use ($s3, $bucket) {
    $ids = [];
    for ($i = 0; $i < 3; $i++) { $ids[] = $s3->createMultipartUpload(['Bucket' => $bucket, 'Key' => "p/u/$i"])['UploadId']; }
    $r = $s3->listMultipartUploads(['Bucket' => $bucket, 'Prefix' => 'p/u/']);
    $keys = array_column($r['Uploads'] ?? [], 'Key'); sort($keys);
    ok($keys === ['p/u/0', 'p/u/1', 'p/u/2'] && !$r['IsTruncated'], json_encode($keys));
    $keys = []; $km = null; $im = null;
    do {
        $a = ['Bucket' => $bucket, 'Prefix' => 'p/u/', 'MaxUploads' => 1];
        if ($km !== null) { $a['KeyMarker'] = $km; $a['UploadIdMarker'] = $im; }
        $p = $s3->listMultipartUploads($a);
        foreach ($p['Uploads'] ?? [] as $u) { $keys[] = $u['Key']; }
        $more = $p['IsTruncated']; $km = $p['NextKeyMarker'] ?? null; $im = $p['NextUploadIdMarker'] ?? null;
    } while ($more);
    ok($keys === ['p/u/0', 'p/u/1', 'p/u/2'], json_encode($keys));
    foreach ($ids as $i => $id) { $s3->abortMultipartUpload(['Bucket' => $bucket, 'Key' => "p/u/$i", 'UploadId' => $id]); }
    ok(empty($s3->listMultipartUploads(['Bucket' => $bucket, 'Prefix' => 'p/u/'])['Uploads']));
});
step('a key that is not there', function () use ($s3, $bucket) {
    foreach (['headObject', 'getObject'] as $op) {
        try { $s3->$op(['Bucket' => $bucket, 'Key' => 'p/not-there']); ok(false, "$op found it"); }
        catch (S3Exception $e) { ok($e->getStatusCode() === 404, "$op: " . $e->getStatusCode()); }
    }
});
step('delete, deleteObjects', function () use ($s3, $bucket) {
    $s3->deleteObject(['Bucket' => $bucket, 'Key' => 'p/small']);
    $r = $s3->deleteObjects(['Bucket' => $bucket, 'Delete' => ['Objects' => array_map(fn($k) => ['Key' => $k], ['p/big', 'p/big-copy', 'p/small-copy', 'p/small-meta', 'p/presigned-put'])]]);
    ok(empty($r['Errors']), json_encode($r['Errors'] ?? null));
    ok(($s3->listObjectsV2(['Bucket' => $bucket, 'Prefix' => 'p/'])['KeyCount'] ?? 0) == 0);
});
echo 'aws-sdk-php ' . Aws\Sdk::VERSION . ' - ' . ($failed ? 'FAILED: ' . implode(', ', $failed) : 'all passed') . "\n";
exit($failed ? 1 : 0);
