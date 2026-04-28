# providers/minio

`github.com/maqian/oss-client/providers/minio` — native driver for
**MinIO Object Storage** via [`github.com/minio/minio-go/v7`](https://godoc.org/github.com/minio/minio-go/v7).

| Field | Value |
| --- | --- |
| Module path | `github.com/maqian/oss-client/providers/minio` |
| Tag | `providers/minio/v0.1.1` |
| Vendor SDK | `github.com/minio/minio-go/v7 v7.1.0` |
| Provider id | `"minio"` (the `uos.Config.Provider` value) |
| AuthScheme | `AuthHMAC` (SigV4 HMAC via `minio-go` credentials) |
| Milestone | M2 |

## Install

```bash
go get github.com/maqian/oss-client/providers/minio@providers/minio/v0.1.1
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
    _ "github.com/maqian/oss-client/providers/minio" // registers Factory
)

func main() {
    ctx := context.Background()
    cfg := uos.Config{
        Provider: "minio",
        Endpoint: "http://localhost:9000", // required — MinIO is always self-hosted
        Region:   "us-east-1",             // optional; minio-go derives from endpoint
        CredentialProvider: credential.NewStatic(credential.Credential{
            Scheme: credential.AuthHMAC,
            Opaque: &credential.EnvHMACCredential{
                AccessKeyID:     "minioadmin",
                SecretAccessKey: "minioadmin",
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

`Config.Endpoint` is **required** for the minio driver — there is no vendor-default endpoint. The endpoint scheme (`http://` vs `https://`) drives `minio-go`'s `Options.Secure` flag. The driver strips the scheme and any trailing path before handing the bare `host:port` to `minio-go.New`.

`Config.Region` is optional; minio-go auto-derives the region from the endpoint when needed. `Config.DriverConfig` is not used (no provider-specific DriverConfig type).

Path-style addressing is forced via `BucketLookup: miniogo.BucketLookupPath` at construction time. minio-go's internal retry count is set to `MaxRetries=1` (disabled) so all retry logic flows through `pkg/uos.RetryPolicy`.

The driver supports the full minio-go S3-compatible wire dialect, including SigV4 HMAC authentication, and passes the complete contract suite against a local MinIO instance out of the box.

## Capability shape

S3-family default shape — **9 Supported + 1 Unsupported + 2 Conditional + 1 ExtensionOnly**:

- `CapDirectGrant` is **Unsupported** (footnote 5): S3-family uses presigned URL, not direct grant.
- `CapVersioning` and `CapObjectACL` are **Conditional**: require bucket versioning enabled and appropriate bucket policy respectively.
- `CapNativeMove` is **ExtensionOnly** (footnote 12): no native rename; default is Copy+Delete.

See [`docs/provider_matrix.md`](../../docs/provider_matrix.md) for the full 13-cell breakdown of the `minio` column.

## Multipart mapping notes

Multipart maps onto `minio-go`'s `Core` client raw primitives: `Core.NewMultipartUpload` → `Initiate`, `Core.PutObjectPart` → `UploadPart`, `Core.CompleteMultipartUpload` → `Complete`, `Core.AbortMultipartUpload` → `Abort`, `Core.ListObjectParts` → `List`. The `Core` wrapper gives direct access to the S3 multipart wire protocol without the concurrency abstractions of the higher-level `minio-go` manager.

## Testing

```bash
# Default unit tests (always passes — no Docker needed):
cd providers/minio && go test -short -race -count=1 ./...

# Contract suite against testcontainers MinIO (PR gate):
go test -tags=docker -count=1 ./...

# Cloud-nightly contract against a real MinIO endpoint
# (set the env vars below):
export OMC_MINIO_NIGHTLY_KEY=...
export OMC_MINIO_NIGHTLY_SECRET=...
export OMC_MINIO_NIGHTLY_BUCKET=...
export OMC_MINIO_NIGHTLY_ENDPOINT=https://minio.example.com
go test -tags=docker -count=1 ./...
```

The minio driver is the reference S3-family driver: the contract suite passes against testcontainers MinIO with all cases enabled except `CapDirectGrant` (S3-family has no DirectGrant model).

## See also

- [`docs/provider_matrix.md`](../../docs/provider_matrix.md) — capability matrix
- [`docs/provider_roadmap.md`](../../docs/provider_roadmap.md) — milestone + per-milestone Lessons
- [`CHANGELOG.md`](../../CHANGELOG.md) — full per-module release log
- [`examples/quickstart/`](../../examples/quickstart/) — runnable end-to-end demo
