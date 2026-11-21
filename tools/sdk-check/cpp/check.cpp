#include <aws/core/Aws.h>
#include <aws/core/client/DefaultRetryStrategy.h>
#include <aws/core/auth/AWSCredentials.h>
#include <aws/core/utils/memory/stl/AWSStringStream.h>
#include <aws/core/utils/threading/Executor.h>
#include <aws/s3/S3Client.h>
#include <aws/s3/model/CopyObjectRequest.h>
#include <aws/s3/model/DeleteObjectRequest.h>
#include <aws/s3/model/GetObjectRequest.h>
#include <aws/s3/model/HeadObjectRequest.h>
#include <aws/s3/model/ListObjectsV2Request.h>
#include <aws/s3/model/PutObjectRequest.h>
#include <aws/transfer/TransferManager.h>
#include <fstream>
#include <iostream>
#include <random>
#include <sstream>

using namespace Aws;
static int fails = 0;
#define CHECK(c, msg) do { if (!(c)) { std::cout << "FAIL: " << msg << std::endl; fails++; } else { std::cout << "ok:   " << msg << std::endl; } } while (0)

static std::string data(size_t n, unsigned seed) {
  std::mt19937 g(seed); std::string s(n, 0);
  for (auto &c : s) c = (char)g();
  return s;
}
static std::string slurp(const std::string &p) { std::ifstream f(p, std::ios::binary); std::stringstream b; b << f.rdbuf(); return b.str(); }

int main(int argc, char **argv) {
  SDKOptions opt; InitAPI(opt);
  {
    Client::ClientConfiguration cfg;
    cfg.endpointOverride = argc > 1 ? argv[1] : "127.0.0.1:18093";
    cfg.scheme = Aws::Http::Scheme::HTTP;
    cfg.region = "us-east-1";
    cfg.retryStrategy = Aws::MakeShared<Aws::Client::DefaultRetryStrategy>("t", 1);
    Auth::AWSCredentials cred(argc > 2 ? argv[2] : "acme", argc > 3 ? argv[3] : "x");
    auto client = Aws::MakeShared<S3::S3Client>("t", cred, cfg, Aws::Client::AWSAuthV4Signer::PayloadSigningPolicy::RequestDependent, false);
    const Aws::String bucket = "demo";
    {
      std::string body = data(1, 1);
      S3::Model::PutObjectRequest r; r.SetBucket(bucket); r.SetKey("cpp/one");
      auto ss = Aws::MakeShared<Aws::StringStream>("t"); ss->write(body.data(), body.size()); r.SetBody(ss);
      r.SetContentType("application/x-test"); r.AddMetadata("owner", "cpp");
      auto o = client->PutObject(r);
      CHECK(o.IsSuccess(), "PutObject of 1 byte: " << (o.IsSuccess() ? "" : o.GetError().GetMessage()));
      S3::Model::GetObjectRequest g; g.SetBucket(bucket); g.SetKey("cpp/one");
      auto go = client->GetObject(g);
      CHECK(go.IsSuccess(), "GetObject");
      if (go.IsSuccess()) {
        std::stringstream b; b << go.GetResult().GetBody().rdbuf();
        CHECK(b.str() == body, "the byte is the one sent");
        CHECK(go.GetResult().GetContentType() == "application/x-test", "content type kept: " << go.GetResult().GetContentType());
        auto &m = go.GetResult().GetMetadata();
        CHECK(m.count("owner") && m.at("owner") == "cpp", "metadata owner kept");
      }
    }
    {
      std::string body = data(30000000 + 7, 2);
      { std::ofstream f("/tmp/up.bin", std::ios::binary); f << body; }
      auto exec = Aws::MakeShared<Aws::Utils::Threading::PooledThreadExecutor>("t", 4);
      Transfer::TransferManagerConfiguration tc(exec.get());
      tc.s3Client = client; tc.bufferSize = 8 * 1024 * 1024; tc.transferBufferMaxHeapSize = 64 * 1024 * 1024;
      auto tm = Transfer::TransferManager::Create(tc);
      auto up = tm->UploadFile("/tmp/up.bin", bucket, "cpp/big", "application/octet-stream", {});
      up->WaitUntilFinished();
      CHECK(up->GetStatus() == Transfer::TransferStatus::COMPLETED, "TransferManager upload of 30 MB: " << (int)up->GetStatus() << " " << up->GetLastError().GetMessage());
      auto dn = tm->DownloadFile(bucket, "cpp/big", "/tmp/down.bin");
      dn->WaitUntilFinished();
      CHECK(dn->GetStatus() == Transfer::TransferStatus::COMPLETED, "TransferManager download: " << (int)dn->GetStatus());
      CHECK(slurp("/tmp/down.bin") == body, "the downloaded file is the file sent");
      S3::Model::HeadObjectRequest h; h.SetBucket(bucket); h.SetKey("cpp/big");
      auto ho = client->HeadObject(h);
      CHECK(ho.IsSuccess() && ho.GetResult().GetContentLength() == (long long)body.size(), "HeadObject length");
      if (ho.IsSuccess()) {
        std::string et = ho.GetResult().GetETag();
        std::cout << "info: ETag of the 30 MB object " << et << std::endl;
        CHECK(et.size() > 3 && et.find("-4") != std::string::npos, "the ETag of a multipart object ends with -4");
      }
      S3::Model::GetObjectRequest g; g.SetBucket(bucket); g.SetKey("cpp/big"); g.SetRange("bytes=8388600-8388700");
      auto go = client->GetObject(g);
      CHECK(go.IsSuccess(), "ranged GetObject across a block border");
      if (go.IsSuccess()) { std::stringstream b; b << go.GetResult().GetBody().rdbuf(); CHECK(b.str() == body.substr(8388600, 101), "the range is the right bytes"); }
    }
    {
      S3::Model::CopyObjectRequest c; c.SetBucket(bucket); c.SetKey("cpp/copy"); c.SetCopySource(bucket + "/cpp/big");
      auto co = client->CopyObject(c);
      CHECK(co.IsSuccess(), "CopyObject: " << (co.IsSuccess() ? "" : co.GetError().GetMessage()));
      S3::Model::ListObjectsV2Request l; l.SetBucket(bucket); l.SetPrefix("cpp/");
      auto lo = client->ListObjectsV2(l);
      CHECK(lo.IsSuccess() && lo.GetResult().GetContents().size() == 3, "ListObjectsV2 shows 3 objects");
      for (auto k : {"cpp/one", "cpp/big", "cpp/copy"}) {
        S3::Model::DeleteObjectRequest d; d.SetBucket(bucket); d.SetKey(k);
        CHECK(client->DeleteObject(d).IsSuccess(), std::string("DeleteObject ") + k);
      }
      lo = client->ListObjectsV2(l);
      CHECK(lo.IsSuccess() && lo.GetResult().GetContents().size() == 0, "the list is empty after the deletes");
    }
  }
  ShutdownAPI(opt);
  std::cout << (fails ? "FAILED " : "ALL OK ") << fails << std::endl;
  return fails ? 1 : 0;
}
