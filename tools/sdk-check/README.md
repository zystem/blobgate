# Checks of the S3 door with the SDKs of other languages

Each script makes the calls a program makes with its SDK (objects with metadata, parts, copies, lists with pages and a delimiter, presigned addresses, deletes) against a running module and prints PASS or FAIL for every step.
They are not part of `go test`: they need the SDKs. The keys are `blobgate s3-key <tenant> <secret>`; the bucket is a store (use one with `"dedup": "tenant"` to see the copies cost nothing). What was run, with which versions: [../../docs/s3.md](../../docs/s3.md).

```bash
python3 tools/sdk-check/boto3_check.py http://127.0.0.1:18092 001 <secret key> main                 # boto3
node tools/sdk-check/js_check.mjs http://127.0.0.1:18092 001 <secret key> main                      # AWS SDK for JavaScript v3 (npm i @aws-sdk/client-s3 @aws-sdk/lib-storage @aws-sdk/s3-request-presigner)
cd tools/sdk-check/java && mvn -q compile org.codehaus.mojo:exec-maven-plugin:3.5.0:java \
  -Dexec.mainClass=check.JavaCheck -Dexec.args="http://127.0.0.1:18092 001 <secret key> main"        # AWS SDK for Java v2
ruby tools/sdk-check/ruby_check.rb http://127.0.0.1:18092 001 <secret key> main                        # gem install aws-sdk-s3
cd /path/with/composer && php tools/sdk-check/php_check.php http://127.0.0.1:18092 001 <secret key> main   # composer require aws/aws-sdk-php (run where vendor/ is)
cd tools/sdk-check/dotnet && dotnet run -- http://127.0.0.1:18092 001 <secret key> main                  # AWS SDK for .NET (net8.0)
cd tools/sdk-check/rust && cargo run --release -- http://127.0.0.1:18092 001 <secret key> main          # AWS SDK for Rust
```

Over TLS (`BLOBGATE_TLS_CERT_FILE` on the module, an `https://` endpoint): make each SDK trust the certificate. Python: `AWS_CA_BUNDLE=cert.pem SSL_CERT_FILE=cert.pem` (the second is for the presigned addresses, which the script fetches itself).
Node: `NODE_EXTRA_CA_CERTS=cert.pem`. Java: `MAVEN_OPTS="-Djavax.net.ssl.trustStore=trust.jks -Djavax.net.ssl.trustStorePassword=changeit"` (made with `keytool -importcert -file cert.pem -keystore trust.jks`).

