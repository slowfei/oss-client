# Migration Guide: vendor SDK → `pkg/uos`

How to switch existing code from a single-vendor object-storage SDK
(`aws-sdk-go-v2`, `minio-go/v7`, `aliyun-oss-go-sdk`, `cos-go-sdk-v5`,
`huaweicloud-sdk-go-obs`, `ve-tos-golang-sdk`, `cloud.google.com/go/storage`,
`azblob`, `qiniu/go-sdk/v7`, `upyun/go-sdk/v3`) to the unified `pkg/uos`
surface.

> **Status**: Companion to [`docs/architecture_plan.md`](architecture_plan.md)
> §1.1 (Core Abstractions, frozen) and [`docs/provider_matrix.md`](provider_matrix.md)
> (capability cells per provider). Read those first if you need the binding
> design rationale.

## When to migrate

The pkg/uos abstraction earns its keep when at least one of the following
is true:

- **Multi-cloud or migrate-out**: your application is multi-tenant and
  different tenants need different cloud backends, or you anticipate
  switching providers (cost, compliance, geographic).
- **国内多云**: serving users on AWS + Aliyun + Tencent + Huawei (4 vendors,
  3 different HMAC dialects) is exactly what M3 was designed for.
- **Cross-paradigm**: shipping the same business code against S3 (URL
  presign), Azure (SAS), GCS (OAuth2), and Qiniu (Upload Token) without
  vendor-specific branches.
- **Long-term decoupling**: keeping your business code outside any specific
  vendor's SDK lifecycle (vendor SDK breaking changes, deprecation,
  abandonment).

When NOT to migrate:

- **Single vendor + heavy vendor-specific features**: if you only target
  AWS S3 and use 50% of S3-only features (replication, inventory, lifecycle
  rules, lambda integration), the unified surface adds friction without
  benefit. Reach for `Client.As(target *s3.Client)` if you need both.
- **Performance hotpath that needs vendor SDK micro-tuning**: the unified
  surface has 1-2 µs of dispatch overhead per call. Negligible for the
  90+% case but possibly relevant for ultra-high-QPS signing benchmarks.
  See [`benchmarks/`](../benchmarks/README.md).

## Migration philosophy

`pkg/uos` does **not** wrap your existing vendor SDK calls one-by-one.
Instead, replace the vendor `Client` with `uos.Client` at the application
boundary. The driver internally keeps using the vendor SDK; your business
code switches to the unified API.

```
                ┌───────────────────────────────────────┐
                │              your business code        │
                │   (no vendor imports past this point)  │
                └───────────────────────────────────────┘
                                    │
                            uos.Client interface
                                    │
            ┌───────────────────┴────────────────────┐
            ▼                                        ▼
       providers/aws                      providers/qiniu
       (uses aws-sdk-go-v2)              (uses qiniu/go-sdk/v7)
            │                                        │
            ▼                                        ▼
        AWS S3 endpoint                     Qiniu Kodo endpoint
```

Vendor SDK imports stay in `providers/<name>/driver.go` and never leak into
your business code. To temporarily reach the underlying vendor type for a
feature pkg/uos doesn't expose, use `Client.As(target any)`:

```go
import "github.com/aws/aws-sdk-go-v2/service/s3"

var s3Client *s3.Client
if cli.As(&s3Client) {
    // use s3Client for AWS-only features (e.g. PutBucketReplication)
}
```

`As` is a documented escape hatch (per
[`docs/architecture_plan.md`](architecture_plan.md) §1.1). Each driver's
README documents what target types it accepts.

## Common patterns

### Open: `s3.NewFromConfig(...)` → `uos.DefaultRegistry().Open(ctx, cfg)`

Vendor pattern (AWS):

```go
cfg, _ := config.LoadDefaultConfig(ctx, config.WithRegion("us-east-1"))
client := s3.NewFromConfig(cfg)
```

Unified:

```go
import (
    "github.com/maqian/object-storage-client/pkg/uos"
    "github.com/maqian/object-storage-client/pkg/uos/credential"
    _ "github.com/maqian/object-storage-client/providers/aws" // registers Factory
)

cli, err := uos.DefaultRegistry().Open(ctx, uos.Config{
    Provider: "aws",
    Region:   "us-east-1",
    CredentialProvider: credential.NewStatic(credential.Credential{
        Scheme: credential.AuthHMAC,
        Opaque: &credential.EnvHMACCredential{
            AccessKeyID:     ak,
            SecretAccessKey: sk,
        },
    }),
})
```

For environment-driven credentials use `credential.NewEnv()` (reads
`OSC_*` then `AWS_*` env vars). For chained discovery use
`credential.NewChain(env, fallback, ...)`.

### Put: `client.PutObject(ctx, &s3.PutObjectInput{...})` → `cli.Objects(bucket).Put(ctx, uos.PutObjectRequest{...})`

```go
// Vendor (AWS):
_, err := client.PutObject(ctx, &s3.PutObjectInput{
    Bucket:      aws.String("my-bucket"),
    Key:         aws.String("hello.txt"),
    Body:        strings.NewReader("hello"),
    ContentType: aws.String("text/plain"),
})

// Unified:
_, err := cli.Objects("my-bucket").Put(ctx, uos.PutObjectRequest{
    Key:     "hello.txt",
    Body:    strings.NewReader("hello"),
    Size:    int64(len("hello")),
    Content: uos.ContentHeaders{ContentType: "text/plain"},
})
```

`cli.Objects(bucket)` binds a default bucket so the request struct doesn't
need it. `uos.ContentHeaders` groups the standard content-* fields
(ContentType, ContentEncoding, CacheControl, etc.).

### Get: `client.GetObject(...)` → `cli.Objects(bucket).Get(...)`

```go
// Vendor:
out, _ := client.GetObject(ctx, &s3.GetObjectInput{
    Bucket: aws.String("my-bucket"),
    Key:    aws.String("hello.txt"),
})
defer out.Body.Close()
data, _ := io.ReadAll(out.Body)

// Unified:
out, _ := cli.Objects("my-bucket").Get(ctx, uos.GetObjectRequest{
    Key: "hello.txt",
})
defer out.Body.Close()
data, _ := io.ReadAll(out.Body)

// Range read:
out, _ = cli.Objects("my-bucket").Get(ctx, uos.GetObjectRequest{
    Key:   "hello.txt",
    Range: &uos.ByteRange{Start: 0, End: 1023}, // first KiB
})
```

`out.Info` (an `ObjectInfo`) carries ContentType, Size, ETag, LastModified,
StorageClass, VersionID, Metadata, Checksum.

### Multipart: 3-step vendor flow → `cli.Multipart(bucket).Initiate / UploadPart / Complete`

```go
mp := cli.Multipart("my-bucket")

init, _ := mp.Initiate(ctx, uos.InitiateMultipartRequest{
    Key:     "large.bin",
    Content: uos.ContentHeaders{ContentType: "application/octet-stream"},
})
parts := make([]uos.UploadedPart, 0, 3)
for i, chunk := range chunks {
    p, _ := mp.UploadPart(ctx, uos.UploadPartRequest{
        UploadID:   init.UploadID,
        Key:        "large.bin",
        PartNumber: i + 1, // 1-based
        Body:       bytes.NewReader(chunk),
        Size:       int64(len(chunk)),
    })
    parts = append(parts, *p)
}
result, _ := mp.Complete(ctx, uos.CompleteMultipartRequest{
    UploadID: init.UploadID,
    Key:      "large.bin",
    Parts:    parts,
})
// On any failure path, call mp.Abort(ctx, uos.AbortMultipartRequest{...})
// to release vendor-side state.
```

Drivers map this onto their native multipart primitive: S3-family uses
S3 multipart; GCS uses resumable upload (sequential-only — see
[providers/gcs/README.md](../providers/gcs/README.md)); Azure uses Block
Blob (4 MiB minimum block size — see
[providers/azure/README.md](../providers/azure/README.md)); Qiniu uses
ResumeUploaderV2; Upyun uses chunked upload via `X-Upyun-Multi-*`
headers. The unified contract is the same; the vendor primitive varies.

A runnable demo lives at [`examples/multipart/`](../examples/multipart/).

### Sign: presign → `cli.Signer(bucket).SignURL(ctx, uos.SignURLRequest{...})`

```go
// Vendor (AWS, presigned GET valid for 1h):
ps := s3.NewPresignClient(client)
req, _ := ps.PresignGetObject(ctx, &s3.GetObjectInput{
    Bucket: aws.String("my-bucket"),
    Key:    aws.String("hello.txt"),
}, s3.WithPresignExpires(time.Hour))
url := req.URL

// Unified:
signed, _ := cli.Signer("my-bucket").SignURL(ctx, uos.SignURLRequest{
    Method:    "GET",
    Key:       "hello.txt",
    ExpiresIn: time.Hour,
})
url := signed.URL
```

For non-URL grant authorization (Qiniu Upload Token, Upyun FORM), use
`Signer.IssueDirectGrant`:

```go
grant, _ := cli.Signer("my-bucket").IssueDirectGrant(ctx, uos.DirectGrantRequest{
    Key:       "uploads/hello.jpg",
    Operation: uos.DirectGrantUpload,
    ExpiresIn: 30 * time.Minute,
    MaxBytes:  10 * 1024 * 1024,
    ContentType: "image/jpeg",
})
switch grant.Mode {
case uos.DirectGrantModeURL:
    // S3-family read presign; caller GETs grant.URL
case uos.DirectGrantModeToken:
    // Qiniu Upload Token; caller POSTs multipart/form-data with token=grant.Token
case uos.DirectGrantModeForm:
    // Upyun FORM upload; caller POSTs multipart/form-data with grant.FormFields
case uos.DirectGrantModeHeaders:
    // Reserved for future vendors; not used by any v1 provider
}
```

Runnable demos:
[`examples/direct_grant_qiniu/`](../examples/direct_grant_qiniu/) (Token)
and [`examples/direct_grant_upyun/`](../examples/direct_grant_upyun/) (Form).

## Per-vendor SDK mapping

| Old import | New import (in addition to `pkg/uos`) | Provider id |
| --- | --- | --- |
| `github.com/aws/aws-sdk-go-v2/service/s3` | `_ "github.com/maqian/object-storage-client/providers/aws"` | `"aws"` |
| `github.com/minio/minio-go/v7` | `_ ".../providers/minio"` | `"minio"` |
| `github.com/aliyun/aliyun-oss-go-sdk/oss` | `_ ".../providers/alibaba"` | `"alibaba"` |
| `github.com/tencentyun/cos-go-sdk-v5` | `_ ".../providers/tencent"` | `"tencent"` |
| `github.com/huaweicloud/huaweicloud-sdk-go-obs/obs` | `_ ".../providers/huawei"` | `"huawei"` |
| `github.com/volcengine/ve-tos-golang-sdk/v2/tos` | `_ ".../providers/volcengine"` | `"volcengine"` |
| `cloud.google.com/go/storage` | `_ ".../providers/gcs"` | `"gcs"` |
| `github.com/Azure/azure-sdk-for-go/sdk/storage/azblob` | `_ ".../providers/azure"` | `"azure"` |
| `github.com/qiniu/go-sdk/v7` | `_ ".../providers/qiniu"` | `"qiniu"` |
| `github.com/upyun/go-sdk/v3` | `_ ".../providers/upyun"` | `"upyun"` |

The blank `_` import registers the Factory on `uos.DefaultRegistry()` via
the driver's package `init()`. You can register multiple drivers to support
multi-vendor workloads.

## Capability discovery

Before calling a feature that may not be supported on a given vendor,
query the driver's capability report:

```go
report, _ := cli.Capabilities(ctx)
if report.Has(capability.CapDirectGrant) {
    // safe to call IssueDirectGrant
} else {
    // fall back to SignURL or refuse the request
}
```

`Has` returns true only for `Supported`; `Conditional` and `ExtensionOnly`
both return false because the unified API can't promise the call succeeds
without runtime probing or vendor-SDK access. To distinguish:

```go
status, ok := report.Get(capability.CapVersioning)
switch status.Availability {
case capability.Supported:    // call freely
case capability.Conditional:  // try; check err.Code == ErrUnsupported
case capability.ExtensionOnly: // use Client.As(target)
case capability.Unsupported:  // do not attempt
}
```

The 13-cell capability vocabulary is the SAME ACROSS all 10 providers
(per [`docs/architecture_plan.md`](architecture_plan.md) §7.2 freeze rule).
The cell value differs per vendor — see
[`docs/provider_matrix.md`](provider_matrix.md) for the matrix.

## Error handling translation

All driver errors implement `errors.Is`/`errors.As` against `*uos.Error`:

```go
err := cli.Objects("my-bucket").Get(ctx, uos.GetObjectRequest{Key: "x"})
if err != nil {
    var uerr *uos.Error
    if errors.As(err, &uerr) {
        switch uerr.Code {
        case uos.ErrNotFound:        // 404
        case uos.ErrUnauthenticated: // 401
        case uos.ErrPermissionDenied:// 403
        case uos.ErrUnsupported:     // capability not supported by this vendor
                                     // → uerr.Capability tells you which one
        case uos.ErrTimeout:         // network or vendor-side timeout
        case uos.ErrRateLimited:     // throttle / quota
        // ... see pkg/uos.AllCodes() for all 14 frozen Codes
        }
        // uerr.Provider, uerr.Operation, uerr.Bucket, uerr.Key are populated
        // uerr.Cause is the wrapped vendor SDK error (use Unwrap to reach it)
    }
}
```

The 14 `Code` constants are frozen (per [`docs/architecture_plan.md`](architecture_plan.md)
§7.1); your error-handling switch never needs to grow as new providers are
added.

## Multi-vendor support pattern

To support multiple backends in one binary, register all drivers and
dispatch by provider id stored in your config:

```go
import (
    _ "github.com/maqian/object-storage-client/providers/aws"
    _ "github.com/maqian/object-storage-client/providers/minio"
    _ "github.com/maqian/object-storage-client/providers/qiniu"
)

// One Open call per backend; cache the *Clients in your service:
clientFor := map[string]uos.Client{}
for _, backend := range myConfig.Backends {
    cli, err := uos.DefaultRegistry().Open(ctx, uos.Config{
        Provider: uos.Provider(backend.Provider),
        Region:   backend.Region,
        Endpoint: backend.Endpoint,
        CredentialProvider: backend.Creds,
    })
    if err != nil { return err }
    clientFor[backend.Name] = cli
}

// Use:
cli := clientFor[req.BackendName]
out, err := cli.Objects(req.Bucket).Get(ctx, uos.GetObjectRequest{Key: req.Key})
```

Same business code; per-tenant routing decided by config. The 10-driver
abstraction validation (M2-M5) proved this works without `pkg/uos`
modification across the full vendor matrix.

## Limitations

The frozen 14-Code / 13-Capability / 4-DirectGrantMode vocabulary covers
the cross-vendor common ground. Vendor-specific features that don't fit
(per-bucket replication policy, per-object lifecycle rules, vendor-side
event subscription, media processing pipelines, etc.) are explicitly
**out-of-scope** for v1. Reach via `Client.As(target)` when needed.

See `docs/provider_matrix.md` for the per-cell breakdown:
- ✅ Supported — covered by the unified surface; contract test passes
- 🟡 Conditional — covered, but works only under specific config (e.g.
  Azure Versioning needs account-level enablement)
- 🧩 ExtensionOnly — concept exists in the vendor but pkg/uos does NOT
  abstract it; reach via `As(target)` and the vendor SDK
- ❌ Unsupported — vendor doesn't expose; returns `ErrUnsupported`

## See also

- [`README.md`](../README.md) — top-level project intro + module table
- [`docs/architecture_plan.md`](architecture_plan.md) — binding architecture
- [`docs/provider_matrix.md`](provider_matrix.md) — capability cells
- [`docs/provider_roadmap.md`](provider_roadmap.md) — milestone log + per-milestone Lessons
- [`examples/quickstart/`](../examples/quickstart/) — minimal end-to-end demo
- [`examples/multipart/`](../examples/multipart/) — multipart upload demo
- [`examples/direct_grant_qiniu/`](../examples/direct_grant_qiniu/) — Token-mode grant demo
- [`examples/direct_grant_upyun/`](../examples/direct_grant_upyun/) — Form-mode grant demo
- [`benchmarks/`](../benchmarks/README.md) — per-provider throughput benchmarks
- [Per-provider quickstart READMEs](../README.md#provider-quickstarts) — vendor-specific config
