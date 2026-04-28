# providers/volcengine

`github.com/maqian/object-storage-client/providers/volcengine` — native driver for
**Volcengine Tinder Object Storage (TOS)** via [`github.com/volcengine/ve-tos-golang-sdk/v2/tos`](https://godoc.org/github.com/volcengine/ve-tos-golang-sdk/v2/tos).

| Field | Value |
| --- | --- |
| Module path | `github.com/maqian/object-storage-client/providers/volcengine` |
| Tag | `providers/volcengine/v0.1.1` |
| Vendor SDK | `github.com/volcengine/ve-tos-golang-sdk/v2 v2.9.4` |
| Provider id | `"volcengine"` (the `uos.Config.Provider` value) |
| AuthScheme | `AuthHMAC` (TOS SigV4-shaped HMAC with a distinct service-name in the canonical string) |
| Milestone | M3 |

## Install

```bash
go get github.com/maqian/object-storage-client/providers/volcengine@providers/volcengine/v0.1.1
```

## Quickstart

```go
package main

import (
    "context"
    "log"
    "strings"

    "github.com/maqian/object-storage-client/pkg/uos"
    "github.com/maqian/object-storage-client/pkg/uos/credential"
    _ "github.com/maqian/object-storage-client/providers/volcengine" // registers Factory
)

func main() {
    ctx := context.Background()
    cfg := uos.Config{
        Provider: "volcengine",
        Region:   "cn-beijing",
        // Endpoint derived as "https://tos-cn-beijing.volces.com" when empty.
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

`Config.Region` is **required**. When `Config.Endpoint` is empty the driver auto-derives it as `https://tos-<region>.volces.com`. Note the `tos-` prefix — concatenating the wrong prefix produces `SignatureDoesNotMatch` on every request.

The `*volcengine.DriverConfig` escape hatch accepts:

- `UseCustomDomain bool` — treats `Config.Endpoint` as a CNAME domain (`tos.WithCustomDomain(true)`).
- `PathAccessMode bool` — forces path-style addressing (`tos.WithPathAccessMode(true)`).
- `DisableSSLVerify bool` — skips TLS verification (`tos.WithEnableVerifySSL(false)`).
- `MaxRetryCount int` — overrides the SDK's internal retry count. Defaults to `0` (disabled) so `pkg/uos.RetryPolicy` is the single retry source. Raise this only when you want vendor-level retries in addition to the unified retry layer.

The TOS HMAC scheme is SigV4-shaped but uses a different service name in the canonical-string construction and different signed-header rules compared to AWS SigV4. This makes it wire-incompatible with MinIO. The contract suite SKIPs against MinIO in PR gates; cloud-nightly runs against real TOS.

## Capability shape

S3-family default shape — **9 Supported + 1 Unsupported + 2 Conditional + 1 ExtensionOnly**:

- `CapDirectGrant` is **Unsupported** (footnote 5): TOS uses presigned URL (PreSignedURL), not a direct-grant model.
- `CapVersioning` and `CapObjectACL` are **Conditional**: require bucket versioning and permissive bucket policy respectively.
- `CapNativeMove` is **ExtensionOnly** (footnote 12): `RenameObject` exists but is HNS-bucket-only (file-namespace); the generic default is Copy+Delete.

See [`docs/provider_matrix.md`](../../docs/provider_matrix.md) for the full 13-cell breakdown of the `volcengine` column.

## Multipart mapping notes

Multipart maps onto TOS v2 API primitives: `CreateMultipartUploadV2` → `Initiate`, `UploadPartV2` → `UploadPart`, `CompleteMultipartUploadV2` → `Complete`, `AbortMultipartUpload` → `Abort`, `ListMultipartUploadsV2` → `List`. TOS ListObjectsV2 uses Marker-based pagination (not ContinuationToken), which the driver normalises to the unified `NextToken` surface.

## Testing

```bash
# Default unit tests (always passes — no Docker needed):
cd providers/volcengine && go test -short -race -count=1 ./...

# Contract suite (spawns MinIO for wiring smoke test; RunSuite SKIPs — TOS HMAC != MinIO SigV4):
go test -tags=docker -count=1 ./...

# Cloud-nightly contract against real Volcengine TOS:
export OMC_VOLCENGINE_NIGHTLY_KEY=...
export OMC_VOLCENGINE_NIGHTLY_SECRET=...
export OMC_VOLCENGINE_NIGHTLY_BUCKET=...
export OMC_VOLCENGINE_NIGHTLY_REGION=cn-beijing
# Optional: OMC_VOLCENGINE_NIGHTLY_ENDPOINT=https://tos-cn-beijing.volces.com
go test -tags=docker -count=1 ./...
```

## See also

- [`docs/provider_matrix.md`](../../docs/provider_matrix.md) — capability matrix
- [`docs/provider_roadmap.md`](../../docs/provider_roadmap.md) — milestone + per-milestone Lessons
- [`CHANGELOG.md`](../../CHANGELOG.md) — full per-module release log
- [`examples/quickstart/`](../../examples/quickstart/) — runnable end-to-end demo
