# providers/gcs

`github.com/maqian/oss-client/providers/gcs` — native driver for
**Google Cloud Storage (GCS)** via [`cloud.google.com/go/storage`](https://godoc.org/cloud.google.com/go/storage).

| Field | Value |
| --- | --- |
| Module path | `github.com/maqian/oss-client/providers/gcs` |
| Tag | `providers/gcs/v0.1.1` |
| Vendor SDK | `cloud.google.com/go/storage v1.62.1` |
| Provider id | `"gcs"` (the `uos.Config.Provider` value) |
| AuthScheme | `AuthOAuth2` (Service Account JSON / ADC); `AuthHMAC` (HMAC keys, SignURL only) |
| Milestone | M4 |

## Install

```bash
go get github.com/maqian/oss-client/providers/gcs@providers/gcs/v0.1.1
```

## Quickstart

```go
package main

import (
    "context"
    "log"
    "os"
    "strings"

    "github.com/maqian/oss-client/pkg/uos"
    "github.com/maqian/oss-client/pkg/uos/credential"
    gcsdrv "github.com/maqian/oss-client/providers/gcs"
    _ "github.com/maqian/oss-client/providers/gcs" // registers Factory
)

func main() {
    ctx := context.Background()

    saJSON, err := os.ReadFile("/path/to/service-account.json")
    if err != nil {
        log.Fatal(err)
    }

    cfg := uos.Config{
        Provider: "gcs",
        // Region is NOT required — GCS resolves region from bucket metadata.
        DriverConfig: &gcsdrv.DriverConfig{
            ProjectID: "my-gcp-project", // required for BucketService.Create / List
        },
        CredentialProvider: credential.NewStatic(credential.Credential{
            Scheme: credential.AuthOAuth2,
            Opaque: &gcsdrv.ServiceAccountCredential{
                JSON: saJSON,
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

`Config.Region` is **not required** — GCS resolves region from the bucket's metadata and the global `storage.googleapis.com` endpoint handles all regions.

The `*gcs.DriverConfig` escape hatch accepts:

- `ProjectID string` — GCP project ID. Required for `BucketService.Create` and `BucketService.List`; optional for all object-scoped operations.
- `SignerEmail string` — overrides the GoogleAccessID used when signing URLs. Defaults to the service account email from the credential.
- `SignerPrivateKey []byte` — PEM-encoded private key for `SignedURL`. Defaults to the key inside the Service Account JSON. **Required for signing**: ADC backed by Compute Engine / GKE / Workload Identity has no local key — `Signer.SignURL` returns `ErrUnsupported{CapSignedURLRead/Write}` in that case.
- `SigningScheme string` — `"v4"` (default, GCS-recommended) or `"v2"`.
- `EmulatorEndpoint string` — overrides the storage endpoint URL. Used for the fake-GCS emulator (e.g. `http://localhost:4443/storage/v1/`).

Two credential payload types are supported. `*gcs.ServiceAccountCredential{JSON, ClientEmail, PrivateKeyPEM}` is the standard production path. `*gcs.RawClientOptions` is the escape hatch for Workload Identity Federation, custom `oauth2.TokenSource`, or caller-supplied `http.Client` transports.

The GCS SDK's internal retry layer is disabled via `client.SetRetry(storage.WithMaxAttempts(1), storage.WithPolicy(storage.RetryNever))` at construction time.

## Capability shape

First non-S3-family driver — **8 Supported + 1 Unsupported + 3 Conditional + 1 ExtensionOnly**:

- `CapSignedURLRead` and `CapSignedURLWrite` are **Conditional** (footnote 1): require a credential with a private signing key.
- `CapDirectGrant` is **Unsupported** (footnote 5): GCS uses presigned URL.
- `CapObjectACL` is **Conditional** (footnote 10): Uniform Bucket-Level Access disables per-object ACL writes.
- `CapVersioning` is **Supported** (generation-number-based; VersionID round-trips as decimal-encoded `int64`).
- `CapNativeMove` is **ExtensionOnly** (footnote 12): `ObjectHandle.Move` exists but is HNS-only; flat buckets fall back to Copy+Delete.

See [`docs/provider_matrix.md`](../../docs/provider_matrix.md) for the full 13-cell breakdown of the `gcs` column.

## Multipart mapping notes

GCS has **no S3-style multipart upload**. The driver maps `MultipartService` onto GCS **Resumable Upload** using an in-process session registry:

- `Initiate` creates a `*storage.Writer` wrapped in a session-handle struct, keyed by a synthetic `UploadID` (not persisted across process restarts).
- `UploadPart` writes a chunk to the open `Writer` sequentially (GCS resumable uploads are sequential-only — no out-of-order parts).
- `Complete` closes the `Writer`; `ObjectAttrs.Generation` is mapped to `VersionID`.
- `Abort` closes the `Writer` with an error to cancel the resumable session.
- `List` always returns an empty page — GCS does not expose a multi-process queryable upload list.

Cross-process resumability requires the SDK's `NewWriterFromAppendableObject` path (preview API, gRPC-only) via `Client.As(target)`.

## Testing

```bash
# Default unit tests (always passes — no Docker needed):
cd providers/gcs && go test -short -race -count=1 ./...

# Contract suite (spawns MinIO for wiring smoke test; RunSuite SKIPs — GCS JSON API != MinIO SigV4):
go test -tags=docker -count=1 ./...

# Cloud-nightly contract against real GCS:
export OMC_GCS_NIGHTLY_KEY=/path/to/service-account.json
export OMC_GCS_NIGHTLY_BUCKET=my-gcs-bucket
export OMC_GCS_NIGHTLY_PROJECT=my-gcp-project
# Optional: OMC_GCS_NIGHTLY_ENDPOINT=http://localhost:4443 (for fake-GCS emulator)
go test -tags=docker -count=1 ./...
```

For non-S3 providers (gcs, azure, qiniu, upyun): `TestRunSuite` SKIPs by default (wire dialect is incompatible with MinIO SigV4); cloud-nightly env vars enable the real-vendor contract.

## See also

- [`docs/provider_matrix.md`](../../docs/provider_matrix.md) — capability matrix
- [`docs/provider_roadmap.md`](../../docs/provider_roadmap.md) — milestone + per-milestone Lessons
- [`CHANGELOG.md`](../../CHANGELOG.md) — full per-module release log
- [`examples/quickstart/`](../../examples/quickstart/) — runnable end-to-end demo
