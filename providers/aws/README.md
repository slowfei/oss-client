# providers/aws

`github.com/maqian/oss-client/providers/aws` — native driver for
**Amazon Simple Storage Service (S3)** via [`github.com/aws/aws-sdk-go-v2/service/s3`](https://godoc.org/github.com/aws/aws-sdk-go-v2/service/s3).

| Field | Value |
| --- | --- |
| Module path | `github.com/maqian/oss-client/providers/aws` |
| Tag | `providers/aws/v0.1.0` |
| Vendor SDK | `github.com/aws/aws-sdk-go-v2/service/s3 v1.100.0` |
| Provider id | `"aws"` (the `uos.Config.Provider` value) |
| AuthScheme | `AuthHMAC` (SigV4 HMAC; `AuthAnonymous` for public buckets) |
| Milestone | M2 |

## Install

```bash
go get github.com/maqian/oss-client/providers/aws@providers/aws/v0.1.0
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
    _ "github.com/maqian/oss-client/providers/aws" // registers Factory
)

func main() {
    ctx := context.Background()
    cfg := uos.Config{
        Provider: "aws",
        Region:   "us-east-1",
        CredentialProvider: credential.NewStatic(credential.Credential{
            Scheme: credential.AuthHMAC,
            Opaque: &credential.EnvHMACCredential{
                AccessKeyID:     "AKIAIOSFODNN7EXAMPLE",
                SecretAccessKey: "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY",
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

The `DriverConfig` escape hatch (`*aws.DriverConfig`) accepts three fields:

- `PathStyle bool` — forces path-style addressing (`bucket` in URL path instead of virtual-host). Implicitly enabled when `Config.Endpoint` is non-empty (required for MinIO and other S3-compatible targets).
- `DisableHTTPS bool` — allows `http://` endpoints when `Config.Endpoint` is set.
- `AccelerateEndpoint bool` — enables S3 Transfer Acceleration for real AWS (ignored when `Config.Endpoint` is set).

When `Config.Endpoint` is non-empty the driver routes every request to a static endpoint resolver (`staticEndpointResolver`), making it suitable as a drop-in S3-compatible client for MinIO, Ceph, LocalStack, and similar targets. The SigV4 region field is still required by the AWS SDK even for S3-compat targets — pass any non-empty string (e.g. `"us-east-1"`).

The aws-sdk-go-v2 internal retryer is disabled (`MaxAttempts=1`) at construction time. All retry logic is delegated to `pkg/uos.RetryPolicy` to prevent double-retry storms. `DeleteObjects` injects a `Content-MD5` middleware so MinIO and older S3-compat targets (which require it) do not return `MissingContentMD5`.

The `us-east-1` region is special: `CreateBucket` in `us-east-1` must NOT include a `LocationConstraint`; all other regions require it. The driver handles this automatically.

## Capability shape

S3-family default shape — **9 Supported + 1 Unsupported + 2 Conditional + 1 ExtensionOnly**:

- `CapDirectGrant` is **Unsupported** (footnote 5 in `docs/provider_matrix.md`): S3 uses presigned URLs instead of a non-URL direct-grant model. Use `Signer.SignURL` for both read and write grants.
- `CapVersioning` and `CapObjectACL` are **Conditional**: require bucket-level versioning enabled and Object Ownership configuration respectively.
- `CapNativeMove` is **ExtensionOnly** (footnote 12): no native rename; the default is Copy+Delete.

See [`docs/provider_matrix.md`](../../docs/provider_matrix.md) for the full 13-cell breakdown of the `aws` column.

## Multipart mapping notes

Multipart upload maps directly onto raw S3 multipart primitives: `CreateMultipartUpload` → `Initiate`, `UploadPart` → `UploadPart`, `CompleteMultipartUpload` → `Complete`, `AbortMultipartUpload` → `Abort`, `ListMultipartUploads` → `List`. The `s3manager.Uploader` helper is **bypassed** in v0.1; `pkg/uos/transfer.Manager` promotion is deferred to v0.2 once two providers have shipped multipart. Each `UploadPart` call requires a known `Size ≥ 0`; the AWS SDK rejects unknown-length bodies on `UploadPart`.

## Testing

```bash
# Default unit tests (always passes — no Docker needed):
cd providers/aws && go test -short -race -count=1 ./...

# Contract suite against testcontainers MinIO (PR gate):
go test -tags=docker -count=1 ./...

# Cloud-nightly contract against real AWS S3
# (set the env vars below, these run in .github/workflows/cloud-nightly.yml):
export AWS_ACCESS_KEY_ID=...
export AWS_SECRET_ACCESS_KEY=...
export AWS_REGION=us-east-1
go test -tags=docker -count=1 ./...
```

The PR-gate contract suite runs against a testcontainers MinIO instance using the aws driver in S3-compat mode (`PathStyle=true`, `DisableHTTPS=true`). Two cases are permanently skipped against MinIO: the `CapDirectGrant` shape case (AWS has no DirectGrant model) and the special-char key with `?`/`%FF` (SigV4 vs MinIO canonicalisation mismatch; real AWS works and is validated in cloud-nightly).

## See also

- [`docs/provider_matrix.md`](../../docs/provider_matrix.md) — capability matrix
- [`docs/provider_roadmap.md`](../../docs/provider_roadmap.md) — milestone + per-milestone Lessons
- [`CHANGELOG.md`](../../CHANGELOG.md) — full per-module release log
- [`examples/quickstart/`](../../examples/quickstart/) — runnable end-to-end demo
