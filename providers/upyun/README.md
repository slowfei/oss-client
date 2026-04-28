# providers/upyun

`github.com/maqian/oss-client/providers/upyun` — native driver for
**Upyun Universal Storage Service (USS)** via [`github.com/upyun/go-sdk/v3/upyun`](https://godoc.org/github.com/upyun/go-sdk/v3/upyun).

| Field | Value |
| --- | --- |
| Module path | `github.com/maqian/oss-client/providers/upyun` |
| Tag | `providers/upyun/v0.1.1` |
| Vendor SDK | `github.com/upyun/go-sdk/v3 v3.0.4` |
| Provider id | `"upyun"` (the `uos.Config.Provider` value) |
| AuthScheme | `AuthCustom` (Unified-Authorization HMAC-SHA1, recommended); `AuthSharedKey` (legacy basic-auth fallback) |
| Milestone | M5 |

## Install

```bash
go get github.com/maqian/oss-client/providers/upyun@providers/upyun/v0.1.1
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
    upyundrv "github.com/maqian/oss-client/providers/upyun"
    _ "github.com/maqian/oss-client/providers/upyun" // registers Factory
)

func main() {
    ctx := context.Background()
    cfg := uos.Config{
        Provider: "upyun",
        // Config.Region is NOT required — service name encodes the storage location.
        DriverConfig: &upyundrv.DriverConfig{
            Bucket: "my-service-name", // required: Upyun service name (provisioned via portal)
        },
        CredentialProvider: credential.NewStatic(credential.Credential{
            Scheme: credential.AuthCustom,
            Opaque: &upyundrv.OperatorCredential{
                Operator: "my-operator",
                Password: "operator-plaintext-password", // SDK MD5s it — do NOT pre-hash
            },
        }),
    }
    cli, err := uos.DefaultRegistry().Open(ctx, cfg)
    if err != nil {
        log.Fatal(err)
    }
    defer cli.Close()

    _, err = cli.Objects("my-service-name").Put(ctx, uos.PutObjectRequest{
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

Upyun's storage namespace is the **"service"** (also called "bucket" in upstream docs). Services are **provisioned via the Upyun web portal** (`https://console.upyun.com/`) — there is no programmatic create-service API. Consequently, `BucketService.Create` and `BucketService.Delete` return `ErrUnsupported` with a reason pointing at the portal. `BucketService.Stat` returns the configured service via `Usage()`; `BucketService.List` returns the single configured service.

`*upyun.DriverConfig` is **required** and accepts:

- `Bucket string` — **required**: the Upyun service name.
- `Hosts map[string]string` — overrides per-host endpoint mapping (keys: upstream canonical hosts like `"v0.api.upyun.com"`; values: override URLs without scheme). Empty uses the SDK's default global endpoints.
- `UseHTTP bool` — sends requests over plain HTTP (default is HTTPS). Use for tests only.
- `UserAgent string` — overrides the SDK's default User-Agent header.

Two `AuthScheme` values are supported:
- `AuthCustom` (**recommended**): Unified-Authorization signature (HMAC-SHA1 over `method-uri-date-policy-md5`). Required for `IssueDirectGrant` (FORM upload).
- `AuthSharedKey` (fallback): legacy basic-auth on the deprecated REST API. Triggers `client.UseDeprecatedApi()` internally. Discouraged for production due to rate-limiting and weaker security.

The `OperatorCredential.Password` is the operator's **plaintext** password — the SDK applies MD5 before signing. Callers **must not** pre-hash.

**`CapSignedURLWrite` is not supported**: Upyun upload authorization is FORM-shaped, not URL. Use `IssueDirectGrant` which returns `DirectGrant{Mode: DirectGrantModeForm}` — this is the M5 `DirectGrantModeForm` validation moment, the final frozen `DirectGrantMode` not yet exercised by any previously shipped driver.

## Capability shape

Upyun has a **unique FORM-grant column** — the M5 milestone moment for `DirectGrantModeForm`:

- `CapDirectGrant` is **Supported**: FORM-upload expressed as `DirectGrant{Mode: DirectGrantModeForm}`. `FormFields` carries `policy` + `authorization`; the caller POSTs them to the upload endpoint.
- `CapSignedURLRead` is **Supported**: signed download URL via the `_upt` query parameter (GET only).
- `CapSignedURLWrite` is **Conditional**: returns `ErrUnsupported` — use `IssueDirectGrant` (footnote 3).
- `CapVersioning` is **Unsupported** (footnote 9): no object versioning data-plane capability.
- `CapObjectTagging`, `CapObjectACL`, `CapManagedEncryption` are **ExtensionOnly** (footnote 7): reach via `Client.As(target **upyun.UpYun)`.
- `CapNativeMove` is **ExtensionOnly**: native Move via `X-Upyun-Move-Source` exists; unified default is Copy+Delete.

See [`docs/provider_matrix.md`](../../docs/provider_matrix.md) for the full 13-cell breakdown.

## Multipart mapping notes

Upyun uses **REST PUT with `X-Upyun-Multi-*` headers** (Initiate / Upload / Complete stages). The driver maps `MultipartService` onto these: `InitMultipartUpload` → `Initiate` (returns SDK's `X-Upyun-Multi-Uuid` as `UploadID`), `UploadPart` → part body with `PartID`, `CompleteMultipartUpload` → `Complete`. Part size minimum is **1 MiB**; parts must be a multiple of 1 MiB; smaller or non-aligned parts return `ErrInvalidArgument`. `List` calls `ListMultipartUploads` — cross-process orphan listing **is** supported by the Upyun vendor (unlike GCS and Azure where `List` is in-process only).

## Testing

```bash
# Default unit tests (always passes — no Docker needed):
cd providers/upyun && go test -short -race -count=1 ./...

# Contract suite (spawns MinIO for wiring smoke test; RunSuite SKIPs — Upyun HMAC-SHA1 != MinIO SigV4):
go test -tags=docker -count=1 ./...

# Cloud-nightly contract against real Upyun USS:
export OMC_UPYUN_NIGHTLY_BUCKET=my-service-name
export OMC_UPYUN_NIGHTLY_OPERATOR=my-operator
export OMC_UPYUN_NIGHTLY_PASSWORD=operator-plaintext-password
go test -tags=docker -count=1 ./...
```

## See also

- [`docs/provider_matrix.md`](../../docs/provider_matrix.md) — capability matrix
- [`docs/provider_roadmap.md`](../../docs/provider_roadmap.md) — milestone + per-milestone Lessons
- [`CHANGELOG.md`](../../CHANGELOG.md) — full per-module release log
- [`examples/quickstart/`](../../examples/quickstart/) — runnable end-to-end demo
