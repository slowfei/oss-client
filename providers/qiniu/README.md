# providers/qiniu

`github.com/slowfei/oss-client/providers/qiniu` — native driver for
**Qiniu Cloud Storage (Kodo)** via [`github.com/qiniu/go-sdk/v7`](https://godoc.org/github.com/qiniu/go-sdk/v7).

| Field | Value |
| --- | --- |
| Module path | `github.com/slowfei/oss-client/providers/qiniu` |
| Tag | `providers/qiniu/v0.1.1` |
| Vendor SDK | `github.com/qiniu/go-sdk/v7 v7.26.10` |
| Provider id | `"qiniu"` (the `uos.Config.Provider` value) |
| AuthScheme | `AuthCustom` (Upload Token / Download Token / Manage Token, all derived from a single AK/SK pair) |
| Milestone | M5 |

## Install

```bash
go get github.com/slowfei/oss-client/providers/qiniu@providers/qiniu/v0.1.1
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
    qiniudrv "github.com/slowfei/oss-client/providers/qiniu"
    _ "github.com/slowfei/oss-client/providers/qiniu" // registers Factory
)

func main() {
    ctx := context.Background()
    cfg := uos.Config{
        Provider: "qiniu",
        Region:   "z0", // Qiniu zone: z0=east China, z1=north, z2=south, na0=NA, as0=SE Asia
        DriverConfig: &qiniudrv.DriverConfig{
            Region:   "z0",
            Domain:   "download.example.com", // required for SignURL (private download)
            UseHTTPS: true,
        },
        CredentialProvider: credential.NewStatic(credential.Credential{
            Scheme: credential.AuthCustom,
            Opaque: &qiniudrv.Credentials{
                AccessKey: "your-access-key",
                SecretKey: "your-secret-key",
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

Either `Config.Region` (Qiniu zone ID) **or** at least one of `Config.Endpoint` / `DriverConfig.{Rs,Rsf,Api,Io,Up}Host` must be set.

Supported zone IDs: `z0` (East China), `z1` (North China), `z2` (South China), `na0` (North America), `as0` (Southeast Asia), `cn-east-2`.

The `*qiniu.DriverConfig` escape hatch accepts:

- `Region string` — Qiniu zone ID (overrides `Config.Region` when both are set).
- `UseHTTPS bool` — forces HTTPS. Defaults to `true` when no explicit endpoint is given.
- `UseCDNDomains bool` — routes downloads through Qiniu CDN domains.
- `Domain string` — the bucket's bound CDN/source domain. **Required for `Signer.SignURL` read**: `storage.MakePrivateURL` needs it. If empty, `SignURL` with GET returns `ErrUnsupported`.
- `UploadEndpoint string` — overrides the upload host in `IssueDirectGrant.URL`.
- `RsHost`, `RsfHost`, `ApiHost`, `IoHost`, `UpHost` — override SDK endpoint defaults for self-hosted Kodo or non-standard zones.

Qiniu uses a **single AK/SK pair** to derive three distinct token families at the wire level: Upload Token (form-field `"token"` POST), Download Token (signed URL query parameter), and Manage Token (signed admin request). The `*qiniu.Credentials{AccessKey, SecretKey}` payload covers all three; the SDK picks the correct signing scope per call site.

**`CapSignedURLWrite` is not supported**: Qiniu write authorization is non-URL. Use `IssueDirectGrant` (which returns a `DirectGrant{Mode: DirectGrantModeToken, Token: uploadToken}`) for upload authorization. This is the M5 `DirectGrantModeToken` validation moment in a distinct context from the Azure SAS pattern.

## Capability shape

Qiniu has the most divergent column in the matrix — **6 Supported + 1 Conditional + 3 ExtensionOnly + 1 Unsupported + 1 ExtensionOnly(NativeMove)**:

- `CapSignedURLWrite` is **Conditional**: returns `ErrUnsupported` — use `IssueDirectGrant` (footnote 4).
- `CapDirectGrant` is **Supported**: Upload Token + Download Token as `DirectGrantModeToken` (M5 validation moment).
- `CapVersioning` is **Unsupported** (footnote 9): no object versioning data-plane capability.
- `CapObjectTagging`, `CapObjectACL`, `CapManagedEncryption` are **ExtensionOnly** (footnote 7): reach via `Client.As(target *storage.BucketManager)`.
- `CapNativeMove` is **ExtensionOnly**: `BucketManager.Move` exists; unified default is Copy+Delete.

See [`docs/provider_matrix.md`](../../docs/provider_matrix.md) for the full 13-cell breakdown.

## Multipart mapping notes

Qiniu uses its own **Resumable Upload v2 (RUv2)** protocol. The driver maps `MultipartService` onto `storage.ResumeUploaderV2`: `InitParts` → `Initiate`, `UploadParts` → `UploadPart`, `CompleteParts` → `Complete`. The driver synthesises an opaque `UploadID` from the SDK's `InitPartsRet.UploadID`. Parts are **sequential per block** — RUv2 does not support out-of-order part uploads. `List` returns in-process sessions only; `BucketManager` does not expose a cross-process upload listing.

## Testing

```bash
# Default unit tests (always passes — no Docker needed):
cd providers/qiniu && go test -short -race -count=1 ./...

# Contract suite (spawns MinIO for wiring smoke test; RunSuite SKIPs — Kodo token auth != MinIO SigV4):
go test -tags=docker -count=1 ./...

# Cloud-nightly contract against real Qiniu Kodo:
export OMC_QINIU_NIGHTLY_KEY=your-access-key
export OMC_QINIU_NIGHTLY_SECRET=your-secret-key
export OMC_QINIU_NIGHTLY_BUCKET=my-bucket
export OMC_QINIU_NIGHTLY_ZONE=z0
# Optional: OMC_QINIU_NIGHTLY_DOMAIN=download.example.com
go test -tags=docker -count=1 ./...
```

## See also

- [`docs/provider_matrix.md`](../../docs/provider_matrix.md) — capability matrix
- [`docs/provider_roadmap.md`](../../docs/provider_roadmap.md) — milestone + per-milestone Lessons
- [`CHANGELOG.md`](../../CHANGELOG.md) — full per-module release log
- [`examples/quickstart/`](../../examples/quickstart/) — runnable end-to-end demo
