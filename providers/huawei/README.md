# providers/huawei

`github.com/maqian/oss-client/providers/huawei` — native driver for
**Huawei Cloud Object Storage Service (OBS)** via [`github.com/huaweicloud/huaweicloud-sdk-go-obs`](https://godoc.org/github.com/huaweicloud/huaweicloud-sdk-go-obs/obs).

| Field | Value |
| --- | --- |
| Module path | `github.com/maqian/oss-client/providers/huawei` |
| Tag | `providers/huawei/v0.1.1` |
| Vendor SDK | `github.com/huaweicloud/huaweicloud-sdk-go-obs v3.26.3+incompatible` |
| Provider id | `"huawei"` (the `uos.Config.Provider` value) |
| AuthScheme | `AuthHMAC` (OBS HMAC v2/v4/obs selectable via `DriverConfig.Signature`) |
| Milestone | M3 |

## Install

```bash
go get github.com/maqian/oss-client/providers/huawei@providers/huawei/v0.1.1
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
    _ "github.com/maqian/oss-client/providers/huawei" // registers Factory
)

func main() {
    ctx := context.Background()
    cfg := uos.Config{
        Provider: "huawei",
        // Endpoint is REQUIRED — region/endpoint pairing is strict on OBS.
        // A wrong pairing produces silent 301/307 redirects, not a clean error.
        Endpoint: "https://obs.cn-north-4.myhuaweicloud.com",
        Region:   "cn-north-4", // optional hint for v4-style signing
        CredentialProvider: credential.NewStatic(credential.Credential{
            Scheme: credential.AuthHMAC,
            Opaque: &credential.EnvHMACCredential{
                AccessKeyID:     "AccessKeyIDExample",
                SecretAccessKey: "SecretAccessKeyExample",
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

`Config.Endpoint` is **required** and **must exactly match the OBS endpoint for your region** (e.g. `https://obs.cn-north-4.myhuaweicloud.com`). Unlike the alibaba and tencent drivers, the huawei driver does NOT auto-derive an endpoint from `Config.Region`. This is intentional: Huawei OBS region/endpoint pairing is strict — a wrong pairing produces silent HTTP 301/307 redirects rather than a clean `ErrInvalidArgument`, and the SDK follows the redirect, causing a downstream signature failure that is hard to diagnose. Mandatory endpoint forces the misconfiguration to surface at `Validate` time.

The `*huawei.DriverConfig` escape hatch accepts:

- `UseCNAME bool` — treats `Config.Endpoint` as a CNAME domain (`obs.WithCustomDomainName(true)`).
- `PathStyle bool` — forces path-style addressing (SDK auto-enables this for IP address endpoints).
- `Signature string` — OBS signing algorithm: `""` or `"v2"` (SDK default), `"v4"` (required for some newer regions like `ap-southeast-2`), `"obs"` (OBS-native variant).
- `DisableSSLVerify bool` — skips TLS verification (`obs.WithSslVerify(false)`).

The SDK's internal retry layer is disabled via `obs.WithMaxRetryCount(0)` at construction time; `pkg/uos.RetryPolicy` is the sole retry surface.

## Capability shape

S3-family default shape — **9 Supported + 1 Unsupported + 2 Conditional + 1 ExtensionOnly**:

- `CapDirectGrant` is **Unsupported** (footnote 5): OBS uses presigned URL (CreateSignedUrl), not a direct-grant model.
- `CapVersioning` and `CapObjectACL` are **Conditional**: require bucket versioning and permissive bucket policy respectively.
- `CapNativeMove` is **ExtensionOnly** (footnote 12): no native rename; default is Copy+Delete.

See [`docs/provider_matrix.md`](../../docs/provider_matrix.md) for the full 13-cell breakdown of the `huawei` column.

## Multipart mapping notes

Multipart maps onto OBS multipart primitives: `InitiateMultipartUpload` → `Initiate`, `UploadPart` → `UploadPart`, `CompleteMultipartUpload` → `Complete`, `AbortMultipartUpload` → `Abort`, `ListMultipartUploads` → `List`. The OBS HMAC wire dialect uses `"OBS ..."` (v2/obs) or `"OBS4-HMAC-SHA256 ..."` (v4) Authorization-header prefixes, incompatible with MinIO SigV4. The contract suite SKIPs against MinIO in PR gates; cloud-nightly requires a real OBS endpoint.

## Testing

```bash
# Default unit tests (always passes — no Docker needed):
cd providers/huawei && go test -short -race -count=1 ./...

# Contract suite (spawns MinIO for wiring smoke test; RunSuite SKIPs — OBS HMAC != MinIO SigV4):
go test -tags=docker -count=1 ./...

# Cloud-nightly contract against real Huawei OBS:
# NOTE: Endpoint is REQUIRED (not optional like other providers)
export OMC_HUAWEI_NIGHTLY_KEY=...
export OMC_HUAWEI_NIGHTLY_SECRET=...
export OMC_HUAWEI_NIGHTLY_BUCKET=...
export OMC_HUAWEI_NIGHTLY_ENDPOINT=https://obs.cn-north-4.myhuaweicloud.com
# Optional: OMC_HUAWEI_NIGHTLY_REGION=cn-north-4
go test -tags=docker -count=1 ./...
```

## See also

- [`docs/provider_matrix.md`](../../docs/provider_matrix.md) — capability matrix
- [`docs/provider_roadmap.md`](../../docs/provider_roadmap.md) — milestone + per-milestone Lessons
- [`CHANGELOG.md`](../../CHANGELOG.md) — full per-module release log
- [`examples/quickstart/`](../../examples/quickstart/) — runnable end-to-end demo
