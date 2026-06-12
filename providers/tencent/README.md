# providers/tencent

`github.com/slowfei/oss-client/providers/tencent` — native driver for
**Tencent Cloud Object Storage (COS)** via [`github.com/tencentyun/cos-go-sdk-v5`](https://godoc.org/github.com/tencentyun/cos-go-sdk-v5).

| Field | Value |
| --- | --- |
| Module path | `github.com/slowfei/oss-client/providers/tencent` |
| Tag | `providers/tencent/v0.1.1` |
| Vendor SDK | `github.com/tencentyun/cos-go-sdk-v5 v0.7.73` |
| Provider id | `"tencent"` (the `uos.Config.Provider` value) |
| AuthScheme | `AuthHMAC` (COS HMAC v1 — q-sign-algorithm=sha1) |
| Milestone | M3 |

## Install

```bash
go get github.com/slowfei/oss-client/providers/tencent@providers/tencent/v0.1.1
```

## Quickstart

```go
package main

import (
    "context"
    "log"
    "strings"

    "github.com/slowfei/oss-client/pkg/uos"
    "github.com/slowfei/oss-client/pkg/uos/credential"
    tencentdrv "github.com/slowfei/oss-client/providers/tencent"
    _ "github.com/slowfei/oss-client/providers/tencent" // registers Factory
)

func main() {
    ctx := context.Background()
    cfg := uos.Config{
        Provider: "tencent",
        Region:   "ap-guangzhou",
        DriverConfig: &tencentdrv.DriverConfig{
            AppID: "1250000000", // auto-suffixed onto bucket names as "<name>-1250000000"
        },
        CredentialProvider: credential.NewStatic(credential.Credential{
            Scheme: credential.AuthHMAC,
            Opaque: &credential.EnvHMACCredential{
                AccessKeyID:     "AKIDExampleKey",
                SecretAccessKey: "ExampleSecretKey",
            },
        }),
    }
    cli, err := uos.DefaultRegistry().Open(ctx, cfg)
    if err != nil {
        log.Fatal(err)
    }
    defer cli.Close()

    _, err = cli.Objects("examplebucket-1250000000").Put(ctx, uos.PutObjectRequest{
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

`Config.Region` is **required** (e.g. `"ap-guangzhou"`, `"ap-shanghai"`, `"ap-beijing"`). The driver constructs per-bucket URLs as `https://<bucket>-<appid>.cos.<region>.myqcloud.com`.

The `*tencent.DriverConfig` escape hatch accepts:

- `AppID string` — when non-empty, automatically appended to bucket names with a `-` separator when constructing the `BucketURL`. The Tencent COS bucket-naming convention is `<name>-<appid>` (e.g. `examplebucket-1250000000`). Callers MAY instead supply the fully-suffixed name on every request and omit `AppID`.
- `UseHTTP bool` — builds bucket URLs with `http://` instead of the default `https://`.

The bucket-name-with-appid quirk is the most common source of confusion: Tencent COS requires `<name>-<appid>` everywhere in the API, including `CreateBucket`, `GetBucketInfo`, and all object operations. `Validate` does not enforce the suffix because it cannot tell which mode the caller intends; the COS wire layer returns `InvalidBucketName` mapped to `ErrInvalidArgument` on malformed names.

The COS HMAC v1 signing scheme (`q-sign-algorithm=sha1`) differs from AWS SigV4 in canonical-string construction and header format, making it incompatible with MinIO. The contract suite SKIPs against MinIO in PR gates; cloud-nightly runs against real COS.

## Capability shape

S3-family default shape — **9 Supported + 1 Unsupported + 2 Conditional + 1 ExtensionOnly**:

- `CapDirectGrant` is **Unsupported** (footnote 5): COS uses presigned URL (GetPresignedURL), not a direct-grant model.
- `CapVersioning` and `CapObjectACL` are **Conditional**: require bucket versioning and permissive bucket policy respectively.
- `CapNativeMove` is **ExtensionOnly** (footnote 12): no native rename; default is Copy+Delete.

See [`docs/provider_matrix.md`](../../docs/provider_matrix.md) for the full 13-cell breakdown of the `tencent` column.

## Multipart mapping notes

Multipart maps onto COS multipart primitives via `cos-go-sdk-v5`: `Object.InitiateMultipartUpload` → `Initiate`, `Object.UploadPart` → `UploadPart`, `Object.CompleteMultipartUpload` → `Complete`, `Object.AbortMultipartUpload` → `Abort`, `Object.ListUploads` → `List`. The SDK's internal `RetryOpt.Count` is set to 1 (disabled) at construction time to prevent double-retry. The SDK enables CRC64 verification by default; this is preserved for end-to-end integrity.

## Testing

```bash
# Default unit tests (always passes — no Docker needed):
cd providers/tencent && go test -short -race -count=1 ./...

# Contract suite (spawns MinIO for wiring smoke test; RunSuite SKIPs — COS HMAC v1 != MinIO SigV4):
go test -tags=docker -count=1 ./...

# Cloud-nightly contract against real Tencent COS:
export OMC_TENCENT_NIGHTLY_KEY=...
export OMC_TENCENT_NIGHTLY_SECRET=...
export OMC_TENCENT_NIGHTLY_BUCKET=examplebucket-1250000000
export OMC_TENCENT_NIGHTLY_REGION=ap-guangzhou
# Optional: OMC_TENCENT_NIGHTLY_APPID=1250000000
# Optional: OMC_TENCENT_NIGHTLY_ENDPOINT=...
go test -tags=docker -count=1 ./...
```

## See also

- [`docs/provider_matrix.md`](../../docs/provider_matrix.md) — capability matrix
- [`docs/provider_roadmap.md`](../../docs/provider_roadmap.md) — milestone + per-milestone Lessons
- [`CHANGELOG.md`](../../CHANGELOG.md) — full per-module release log
- [`examples/quickstart/`](../../examples/quickstart/) — runnable end-to-end demo
