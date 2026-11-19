# What the S3 door answers to the Ruby SDK of AWS (aws-sdk-s3): objects, parts, copies, lists, presigned addresses, deletes, uploads in progress.
# usage: ruby ruby_check.rb ENDPOINT ACCESS_KEY SECRET_KEY BUCKET     (the keys: `blobgate s3-key <tenant> <secret>`)
# needs: the gem aws-sdk-s3. Exit status 0 when every step passed.
require "aws-sdk-s3"
require "digest"
require "net/http"
require "tempfile"

endpoint, ak, sk, bucket = ARGV
s3 = Aws::S3::Client.new(endpoint: endpoint, region: "eu-west-1", credentials: Aws::Credentials.new(ak, sk), force_path_style: true)
resource = Aws::S3::Resource.new(client: s3)
failed = []

def step(name, failed)
  yield
  puts "PASS #{name}"
rescue StandardError, ScriptError => e
  failed << name
  puts "FAIL #{name} -> #{e.class} #{e.message[0, 300]}"
end

def ok(cond, msg = "assertion failed")
  raise msg unless cond
end

def body(n)
  (0...n).map { |i| ((i * 7 + (i >> 8)) & 255).chr }.join.b
end

big = body(20 * 1024 * 1024 + 321)
small = "hello from ruby".b

step("put, head, get, the MD5 ETag", failed) do
  r = s3.put_object(bucket: bucket, key: "r/small", body: small, content_type: "text/x-ruby", metadata: { "owner" => "ruby" })
  ok r.etag == "\"#{Digest::MD5.hexdigest(small)}\"", r.etag
  h = s3.head_object(bucket: bucket, key: "r/small")
  ok h.content_length == small.bytesize && h.content_type == "text/x-ruby" && h.metadata == { "owner" => "ruby" }, h.metadata.inspect
  ok s3.get_object(bucket: bucket, key: "r/small").body.read == small
end
step("range read", failed) do
  r = s3.get_object(bucket: bucket, key: "r/small", range: "bytes=2-5")
  ok r.body.read == small[2..5]
end
step("upload_file of 20 MiB in parts, download, the ETag of S3", failed) do
  Tempfile.create("src") do |f|
    f.binmode
    f.write(big)
    f.flush
    resource.bucket(bucket).object("r/big").upload_file(f.path, multipart_threshold: 8 * 1024 * 1024)
  end
  Tempfile.create("dst") do |f|
    resource.bucket(bucket).object("r/big").download_file(f.path)
    ok Digest::SHA256.file(f.path).hexdigest == Digest::SHA256.hexdigest(big), "the downloaded file differs"
  end
  part = 8 * 1024 * 1024
  # the Ruby SDK cuts a file in 5 MiB parts at least, as many as it needs for 10000 parts: the ETag is made of the parts it sent, which the number in it tells
  etag = s3.head_object(bucket: bucket, key: "r/big").etag
  ok etag =~ /\A"[0-9a-f]{32}-\d+"\z/, etag
  n = etag[/-(\d+)"/, 1].to_i
  size = (big.bytesize / n.to_f).ceil
  size = (size / 1024.0).ceil * 1024 if size % 1024 != 0
  [5 * 1024 * 1024, 8 * 1024 * 1024, part].each do |ps|
    parts = big.bytes.each_slice(ps).map { |s| Digest::MD5.digest(s.pack("C*")) }
    next unless parts.length == n
    ok etag == "\"#{Digest::MD5.hexdigest(parts.join)}-#{n}\"", "ETag #{etag}, S3 makes #{Digest::MD5.hexdigest(parts.join)}-#{n} for parts of #{ps}"
  end
end
step("CopyObject, metadata REPLACE, copy_to of a big object in parts", failed) do
  s3.copy_object(bucket: bucket, key: "r/small-copy", copy_source: "#{bucket}/r/small")
  ok s3.get_object(bucket: bucket, key: "r/small-copy").body.read == small
  s3.copy_object(bucket: bucket, key: "r/small-meta", copy_source: "#{bucket}/r/small", metadata_directive: "REPLACE", metadata: { "owner" => "other" })
  ok s3.head_object(bucket: bucket, key: "r/small-meta").metadata == { "owner" => "other" }
  resource.bucket(bucket).object("r/big").copy_to(resource.bucket(bucket).object("r/big-copy"), multipart_copy: true, content_length: big.bytesize, min_part_size: 8 * 1024 * 1024) # its default part is 50 MiB; the door takes parts of 8 MiB at most
  ok Digest::SHA256.hexdigest(s3.get_object(bucket: bucket, key: "r/big-copy").body.read) == Digest::SHA256.hexdigest(big)
end
step("listing, pages, delimiter", failed) do
  keys = []
  s3.list_objects_v2(bucket: bucket, prefix: "r/", max_keys: 2).each_page { |p| keys += p.contents.map(&:key) }
  ok keys.sort == %w[r/big r/big-copy r/small r/small-copy r/small-meta], keys.inspect
  r = s3.list_objects_v2(bucket: bucket, delimiter: "/")
  ok r.common_prefixes.any? { |c| c.prefix == "r/" }, r.common_prefixes.inspect
end
step("presigned GET and PUT", failed) do
  signer = Aws::S3::Presigner.new(client: s3)
  ok Net::HTTP.get(URI(signer.presigned_url(:get_object, bucket: bucket, key: "r/small", expires_in: 300))).b == small
  uri = URI(signer.presigned_url(:put_object, bucket: bucket, key: "r/presigned-put", expires_in: 300))
  res = Net::HTTP.start(uri.host, uri.port) { |h| h.send_request("PUT", uri.request_uri, "through a presigned address") }
  ok res.code == "200", "status #{res.code} #{res.body}"
  ok s3.get_object(bucket: bucket, key: "r/presigned-put").body.read == "through a presigned address"
end
step("uploads in progress: list, pages, abort", failed) do
  ids = 3.times.map { |i| s3.create_multipart_upload(bucket: bucket, key: "r/u/#{i}").upload_id }
  r = s3.list_multipart_uploads(bucket: bucket, prefix: "r/u/")
  ok r.uploads.map(&:key).sort == %w[r/u/0 r/u/1 r/u/2] && !r.is_truncated, r.uploads.inspect
  keys = []
  km = im = nil
  loop do
    p = s3.list_multipart_uploads(bucket: bucket, prefix: "r/u/", max_uploads: 1, key_marker: km, upload_id_marker: im)
    keys += p.uploads.map(&:key)
    break unless p.is_truncated
    km, im = p.next_key_marker, p.next_upload_id_marker
  end
  ok keys == %w[r/u/0 r/u/1 r/u/2], keys.inspect
  ids.each_with_index { |id, i| s3.abort_multipart_upload(bucket: bucket, key: "r/u/#{i}", upload_id: id) }
  ok s3.list_multipart_uploads(bucket: bucket, prefix: "r/u/").uploads.empty?
end
step("a key that is not there", failed) do
  begin
    s3.head_object(bucket: bucket, key: "r/not-there")
    ok false, "found"
  rescue Aws::S3::Errors::NotFound
    # as S3 answers a head
  end
  begin
    s3.get_object(bucket: bucket, key: "r/not-there")
    ok false, "read"
  rescue Aws::S3::Errors::NoSuchKey
    # as it should
  end
end
step("delete, delete_objects", failed) do
  s3.delete_object(bucket: bucket, key: "r/small")
  r = s3.delete_objects(bucket: bucket, delete: { objects: %w[r/big r/big-copy r/small-copy r/small-meta r/presigned-put].map { |k| { key: k } } })
  ok r.errors.empty?, r.errors.inspect
  ok s3.list_objects_v2(bucket: bucket, prefix: "r/").key_count == 0
end
puts "aws-sdk-s3 ruby #{Aws::S3::GEM_VERSION} - #{failed.empty? ? 'all passed' : 'FAILED: ' + failed.join(', ')}"
exit(failed.empty? ? 0 : 1)
