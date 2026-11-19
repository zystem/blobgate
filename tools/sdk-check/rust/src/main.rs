// What the S3 door answers to the Rust SDK of AWS (aws-sdk-s3): objects, parts (by hand: the SDK has no transfer manager), copies, lists, presigned addresses, deletes, uploads in progress.
// usage: cargo run --release -- ENDPOINT ACCESS_KEY SECRET_KEY BUCKET     (the keys: `blobgate s3-key <tenant> <secret>`)
// Exit status 0 when every step passed.
use aws_sdk_s3::config::{BehaviorVersion, Credentials, Region};
use aws_sdk_s3::presigning::PresigningConfig;
use aws_sdk_s3::primitives::ByteStream;
use aws_sdk_s3::types::{CompletedMultipartUpload, CompletedPart, Delete, MetadataDirective, ObjectIdentifier};
use aws_sdk_s3::{Client, Config};
use md5::{Digest, Md5};
use sha2::Sha256;
use std::time::Duration;

type R = Result<(), Box<dyn std::error::Error>>;

fn ok(c: bool, m: &str) -> R {
    if c { Ok(()) } else { Err(m.into()) }
}
fn hex(b: &[u8]) -> String {
    b.iter().map(|x| format!("{:02x}", x)).collect()
}
fn sha(b: &[u8]) -> String {
    hex(&Sha256::digest(b))
}
fn md5(b: &[u8]) -> Vec<u8> {
    Md5::digest(b).to_vec()
}
async fn read(s3: &Client, bucket: &str, key: &str, range: Option<&str>) -> Result<Vec<u8>, Box<dyn std::error::Error>> {
    let mut r = s3.get_object().bucket(bucket).key(key);
    if let Some(x) = range {
        r = r.range(x);
    }
    Ok(r.send().await?.body.collect().await?.into_bytes().to_vec())
}
async fn step(name: &str, failed: &mut Vec<String>, f: impl std::future::Future<Output = R>) {
    match f.await {
        Ok(()) => println!("PASS {}", name),
        Err(e) => {
            failed.push(name.to_string());
            let m = e.to_string();
            println!("FAIL {} -> {}", name, &m[..m.len().min(300)]);
        }
    }
}

#[tokio::main]
async fn main() {
    let a: Vec<String> = std::env::args().collect();
    let (endpoint, ak, sk, bucket) = (a[1].clone(), a[2].clone(), a[3].clone(), a[4].clone());
    let conf = Config::builder()
        .behavior_version(BehaviorVersion::latest())
        .region(Region::new("eu-west-1"))
        .endpoint_url(endpoint)
        .credentials_provider(Credentials::new(ak, sk, None, None, "static"))
        .force_path_style(true)
        .build();
    let s3 = Client::from_conf(conf);
    let b = bucket.as_str();
    let big: Vec<u8> = (0..(20 * 1024 * 1024 + 321usize)).map(|i| (i * 7 + (i >> 8)) as u8).collect();
    let small = b"hello from rust".to_vec();
    const PART: usize = 8 * 1024 * 1024;
    let mut failed: Vec<String> = vec![];

    step("put, head, get, the MD5 ETag", &mut failed, async {
        let r = s3.put_object().bucket(b).key("x/small").body(ByteStream::from(small.clone())).content_type("text/x-rust").metadata("owner", "rust").send().await?;
        ok(r.e_tag() == Some(&format!("\"{}\"", hex(&md5(&small)))), "the ETag of the put")?;
        let h = s3.head_object().bucket(b).key("x/small").send().await?;
        ok(h.content_length() == Some(small.len() as i64) && h.content_type() == Some("text/x-rust") && h.metadata().and_then(|m| m.get("owner")).map(|s| s.as_str()) == Some("rust"), "head")?;
        ok(read(&s3, b, "x/small", None).await? == small, "get")
    })
    .await;
    step("range read", &mut failed, async { ok(read(&s3, b, "x/small", Some("bytes=2-5")).await? == small[2..6].to_vec(), "range") }).await;
    step("upload of 20 MiB in parts of 8 MiB, download, the ETag of S3", &mut failed, async {
        let up = s3.create_multipart_upload().bucket(b).key("x/big").send().await?;
        let id = up.upload_id().ok_or("no upload id")?.to_string();
        let mut done = vec![];
        for (i, p) in big.chunks(PART).enumerate() {
            let r = s3.upload_part().bucket(b).key("x/big").upload_id(&id).part_number(i as i32 + 1).body(ByteStream::from(p.to_vec())).send().await?;
            done.push(CompletedPart::builder().part_number(i as i32 + 1).set_e_tag(r.e_tag().map(String::from)).build());
        }
        s3.complete_multipart_upload().bucket(b).key("x/big").upload_id(&id).multipart_upload(CompletedMultipartUpload::builder().set_parts(Some(done)).build()).send().await?;
        ok(sha(&read(&s3, b, "x/big", None).await?) == sha(&big), "the object differs")?;
        let all: Vec<u8> = big.chunks(PART).flat_map(|p| md5(p)).collect();
        let want = format!("\"{}-{}\"", hex(&md5(&all)), big.chunks(PART).count());
        let got = s3.head_object().bucket(b).key("x/big").send().await?.e_tag().unwrap_or("").to_string();
        ok(got == want, &format!("ETag {}, S3 makes {}", got, want))
    })
    .await;
    step("CopyObject, metadata REPLACE, UploadPartCopy by ranges", &mut failed, async {
        s3.copy_object().bucket(b).key("x/small-copy").copy_source(format!("{}/x/small", b)).send().await?;
        ok(read(&s3, b, "x/small-copy", None).await? == small, "copy")?;
        s3.copy_object().bucket(b).key("x/small-meta").copy_source(format!("{}/x/small", b)).metadata_directive(MetadataDirective::Replace).metadata("owner", "other").send().await?;
        ok(s3.head_object().bucket(b).key("x/small-meta").send().await?.metadata().and_then(|m| m.get("owner")).map(|s| s.as_str()) == Some("other"), "metadata")?;
        let up = s3.create_multipart_upload().bucket(b).key("x/big-copy").send().await?;
        let id = up.upload_id().ok_or("no upload id")?.to_string();
        let mut done = vec![];
        let mut n = 1;
        let mut off = 0;
        while off < big.len() {
            let end = (off + PART).min(big.len()) - 1;
            let r = s3.upload_part_copy().bucket(b).key("x/big-copy").upload_id(&id).part_number(n).copy_source(format!("{}/x/big", b)).copy_source_range(format!("bytes={}-{}", off, end)).send().await?;
            done.push(CompletedPart::builder().part_number(n).set_e_tag(r.copy_part_result().and_then(|c| c.e_tag()).map(String::from)).build());
            n += 1;
            off += PART;
        }
        s3.complete_multipart_upload().bucket(b).key("x/big-copy").upload_id(&id).multipart_upload(CompletedMultipartUpload::builder().set_parts(Some(done)).build()).send().await?;
        ok(sha(&read(&s3, b, "x/big-copy", None).await?) == sha(&big), "the copy differs")
    })
    .await;
    step("listing, pages, delimiter", &mut failed, async {
        let mut keys = vec![];
        let mut p = s3.list_objects_v2().bucket(b).prefix("x/").max_keys(2).into_paginator().send();
        while let Some(page) = p.next().await {
            for o in page?.contents() {
                keys.push(o.key().unwrap_or("").to_string());
            }
        }
        keys.sort();
        ok(keys == ["x/big", "x/big-copy", "x/small", "x/small-copy", "x/small-meta"], &keys.join(","))?;
        let d = s3.list_objects_v2().bucket(b).delimiter("/").send().await?;
        ok(d.common_prefixes().iter().any(|c| c.prefix() == Some("x/")), "common prefixes")
    })
    .await;
    step("presigned GET and PUT", &mut failed, async {
        let cfg = PresigningConfig::expires_in(Duration::from_secs(300))?;
        let g = s3.get_object().bucket(b).key("x/small").presigned(cfg.clone()).await?;
        let got = tokio::task::spawn_blocking({ let u = g.uri().to_string(); move || { let mut v = vec![]; ureq::get(&u).call().map(|r| r.into_reader().read_to_end(&mut v)).map(|_| v) } }).await??;
        ok(got == small, "presigned get")?;
        let p = s3.put_object().bucket(b).key("x/presigned-put").presigned(cfg).await?;
        let u = p.uri().to_string();
        let st = tokio::task::spawn_blocking(move || ureq::put(&u).send_string("through a presigned address").map(|r| r.status())).await??;
        ok(st == 200, "presigned put")?;
        ok(read(&s3, b, "x/presigned-put", None).await? == b"through a presigned address".to_vec(), "presigned put body")
    })
    .await;
    step("uploads in progress: list, pages, abort", &mut failed, async {
        let mut ids = vec![];
        for i in 0..3 {
            ids.push(s3.create_multipart_upload().bucket(b).key(format!("x/u/{}", i)).send().await?.upload_id().unwrap_or("").to_string());
        }
        let r = s3.list_multipart_uploads().bucket(b).prefix("x/u/").send().await?;
        let mut all: Vec<String> = r.uploads().iter().map(|u| u.key().unwrap_or("").to_string()).collect();
        all.sort();
        ok(all == ["x/u/0", "x/u/1", "x/u/2"] && r.is_truncated() != Some(true), &all.join(","))?;
        let (mut keys, mut km, mut im) = (vec![], None::<String>, None::<String>);
        loop {
            let p = s3.list_multipart_uploads().bucket(b).prefix("x/u/").max_uploads(1).set_key_marker(km.clone()).set_upload_id_marker(im.clone()).send().await?;
            keys.extend(p.uploads().iter().map(|u| u.key().unwrap_or("").to_string()));
            if p.is_truncated() != Some(true) {
                break;
            }
            km = p.next_key_marker().map(String::from);
            im = p.next_upload_id_marker().map(String::from);
        }
        ok(keys == ["x/u/0", "x/u/1", "x/u/2"], &keys.join(","))?;
        for (i, id) in ids.iter().enumerate() {
            s3.abort_multipart_upload().bucket(b).key(format!("x/u/{}", i)).upload_id(id).send().await?;
        }
        ok(s3.list_multipart_uploads().bucket(b).prefix("x/u/").send().await?.uploads().is_empty(), "left over")
    })
    .await;
    step("a key that is not there", &mut failed, async {
        match s3.head_object().bucket(b).key("x/not-there").send().await {
            Err(e) => ok(e.into_service_error().is_not_found(), "head: not a 404")?,
            Ok(_) => return Err("found".into()),
        }
        match s3.get_object().bucket(b).key("x/not-there").send().await {
            Err(e) => ok(e.into_service_error().is_no_such_key(), "get: not NoSuchKey"),
            Ok(_) => Err("read".into()),
        }
    })
    .await;
    step("delete, DeleteObjects", &mut failed, async {
        s3.delete_object().bucket(b).key("x/small").send().await?;
        let ids: Vec<ObjectIdentifier> = ["x/big", "x/big-copy", "x/small-copy", "x/small-meta", "x/presigned-put"].iter().map(|k| ObjectIdentifier::builder().key(*k).build().unwrap()).collect();
        let r = s3.delete_objects().bucket(b).delete(Delete::builder().set_objects(Some(ids)).build()?).send().await?;
        ok(r.errors().is_empty(), "errors")?;
        ok(s3.list_objects_v2().bucket(b).prefix("x/").send().await?.key_count().unwrap_or(0) == 0, "left over")
    })
    .await;
    println!("aws-sdk-s3 rust - {}", if failed.is_empty() { "all passed".to_string() } else { format!("FAILED: {}", failed.join(", ")) });
    std::process::exit(if failed.is_empty() { 0 } else { 1 });
}
