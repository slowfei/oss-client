# providers/alibaba

`github.com/maqian/oss-client/providers/alibaba` — native driver for
**Alibaba Cloud Object Storage Service (OSS)** via [`github.com/aliyun/aliyun-oss-go-sdk/oss`](https://godoc.org/github.com/aliyun/aliyun-oss-go-sdk/oss).

| Field | Value |
| --- | --- |
| Module path | `github.com/maqian/oss-client/providers/alibaba` |
| Tag | `providers/alibaba/v0.1.1` |
| Vendor SDK | `github.com/aliyun/aliyun-oss-go-sdk v3.0.2+incompatible` |
| Provider id | `"alibaba"` (the `uos.Config.Provider` value) |
| AuthScheme | `AuthHMAC` (OSS HMAC v1/v2/v4 selectable via `DriverConfig.AuthVersion`) |
| Milestone | M3 |

## Install

```bash
go get github.com/maqian/oss-client/providers/alibaba@providers/alibaba/v0.1.1
```

## Quickstart

```go
package main

import (
    "context"
    "log"
    "strings"

    "github.com/maqian/oss-client/pkg/uos"
    "github.com/maqian/oss-client/pkg/uos/credential"
    _ "github.com/maqian/oss-client/providers/alibaba" // registers Factory
)

func main() {
    ctx := context.Background()
    cfg := uos.Config{
        Provider: "alibaba",
        Region:   "cn-hangzhou", // derives endpoint "https://oss-cn-hangzhou.aliyuncs.com"
        CredentialProvider: credential.NewStatic(credential.Credential{
            Scheme: credential.AuthHMAC,
            Opaque: &credential.EnvHMACCredential{
                AccessKeyID:     "LTAI5tExampleKey",
                SecretAccessKey: "ExampleSecretKey",
            },
        }),
    }
    cli, err := uos.DefaultRegistry().Open(ctx, cfg)
    if err != nil {
        log.Fatal(err)
    }
    defer cli.Close()

    _, err = cli.Objects("my-bucket").Put(ctx, uos.PutObjectRequest{
        Key:  "hello.txt",
        Body: strings.NewReader("hello world"),
        Size: int64(len("hello world")),
    })
    if err != nil {
        log.Fatal(err)
    }
}
```

## Provider-specific configuration

Either `Config.Region` **or** `Config.Endpoint` is required. When only `Config.Region` is set, the driver auto-derives the endpoint as `https://oss-<region>.aliyuncs.com`. When `Config.Endpoint` is set it takes precedence.

The `*alibaba.DriverConfig` escape hatch accepts:

- `UseCNAME bool` — treats `Config.Endpoint` as a custom CNAME domain (`oss.UseCname`). Use for CDN-backed OSS endpoints.
- `PathStyle bool` — forces path-style addressing. Automatically enabled when the endpoint does not contain `.aliyuncs.com` (custom / non-standard hosts).
- `AuthVersion string` — selects the OSS signing algorithm: `""` or `"v1"` (SDK default), `"v2"`, `"v4"`. Use `"v4"` for newer regions that require SigV4-style signing.
- `DisableSSLVerify bool` — skips TLS verification (test environments only).

The aliyun-oss-go-sdk uses the OSS HMAC wire dialect, which is incompatible with AWS SigV4. The Authorization header prefix is `"OSS ..."` (v1/v2) or `"OSS4-HMAC-SHA256 ..."` (v4), not `"AWS4-HMAC-SHA256 ..."`. As a result the contract suite SKIPs against testcontainers MinIO in PR gates; the full suite runs against real OSS in cloud-nightly gated on `OMC_ALIBABA_NIGHTLY_*` secrets.

## Capability shape

S3-family default shape — **9 Supported + 1 Unsupported + 2 Conditional + 1 ExtensionOnly**:

- `CapDirectGrant` is **Unsupported** (footnote 5): OSS uses presigned URL (SignURL), not a direct-grant model.
- `CapVersioning` and `CapObjectACL` are **Conditional**: require bucket versioning enabled and permissive bucket policy respectively.
- `CapNativeMove` is **ExtensionOnly** (footnote 12): no native rename; default is Copy+Delete.

See [`docs/provider_matrix.md`](../../docs/provider_matrix.md) for the full 13-cell breakdown of the `alibaba` column.

## Multipart mapping notes

Multipart maps onto OSS multipart primitives: `InitiateMultipartUpload` → `Initiate`, `UploadPart` → `UploadPart`, `CompleteMultipartUpload` → `Complete`, `AbortMultipartUpload` → `Abort`, `ListMultipartUploads` → `List`. The driver uses the shared `pkg/uos/s3common` helpers for metadata key normalisation (lower-cased) and HTTP status mapping, extracted from the M2 AWS+MinIO drivers.

## Testing

```bash
# Default unit tests (always passes — no Docker needed):
cd providers/alibaba && go test -short -race -count=1 ./...

# Contract suite (spawns MinIO for wiring smoke test; RunSuite SKIPs — OSS HMAC != MinIO SigV4):
go test -tags=docker -count=1 ./...

# Cloud-nightly contract against real Alibaba OSS:
export OMC_ALIBABA_NIGHTLY_KEY=...
export OMC_ALIBABA_NIGHTLY_SECRET=...
export OMC_ALIBABA_NIGHTLY_BUCKET=...
export OMC_ALIBABA_NIGHTLY_REGION=cn-hangzhou
# Optional: OMC_ALIBABA_NIGHTLY_ENDPOINT=https://oss-cn-hangzhou.aliyuncs.com
go test -tags=docker -count=1 ./...
```

For S3-family providers (aws, minio, alibaba, tencent, huawei, volcengine): the contract suite passes against testcontainers MinIO out of the box for `aws` and `minio`. For `alibaba`, `tencent`, `huawei`, and `volcengine`: `TestRunSuite` SKIPs by default (wire dialect is incompatible with MinIO SigV4); `TestSpawnMinIOSmoke` validates the testkit wiring.

## See also

- [`docs/provider_matrix.md`](../../docs/provider_matrix.md) — capability matrix
- [`docs/provider_roadmap.md`](../../docs/provider_roadmap.md) — milestone + per-milestone Lessons
- [`CHANGELOG.md`](../../CHANGELOG.md) — full per-module release log
- [`examples/quickstart/`](../../examples/quickstart/) — runnable end-to-end demo
