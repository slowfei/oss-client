# Object Storage Client (`pkg/uos`)

A unified, vendor-agnostic Go SDK for object storage. One `Client` interface
across **10 providers** (AWS S3, MinIO, Alibaba OSS, Tencent COS, Huawei OBS,
Volcengine TOS, Google Cloud Storage, Azure Blob Storage, Qiniu Kodo, Upyun
USS) — write your code once, switch providers via config.

```go
cfg := uos.Config{Provider: "aws", Region: "us-east-1", /* ... */}
cli, _ := uos.DefaultRegistry().Open(ctx, cfg)
defer cli.Close()

_, err := cli.Objects("my-bucket").Put(ctx, uos.PutObjectRequest{
    Key:  "hello.txt",
    Body: strings.NewReader("hello world"),
})
```

## Status

**v0.1.x** — abstraction validation complete. All 10 v1-target providers
shipped against a frozen API surface (14 error Codes / 13 Capabilities /
4 DirectGrant modes). M6 stabilization in progress (benchmarks, examples,
OTel alignment) for v1.0.0.

The frozen-surface contract is enforced by [`pkg/uos/surface_test.go`'s
`TestFrozenSurface`](pkg/uos/surface_test.go) — adding a value to any of
the three frozen sets requires (a) ≥2 providers needing the same semantic
and (b) a minor version bump.

## Why

Most "universal" object storage SDKs either:
- Wrap a single S3-compat dialect and break on real vendor differences (signed
  URL formats, multipart minimum sizes, error vocabularies, CDN/source domain
  splits, FORM upload), or
- Expose the lowest-common-denominator and lose vendor capabilities.

`pkg/uos` takes the third path: a small frozen surface that absorbs all 10
v1-target vendors **without** vendor-specific public types. The variation
lives inside drivers and surfaces through the unified `Capability` /
`DirectGrant` / `Code` vocabulary.

## Quickstart

### 1. Install

Each provider is a separate Go module — pull only what you use:

```bash
# Pull the unified API (zero third-party deps):
go get github.com/maqian/object-storage-client@v0.1.0

# Pull one or more provider drivers (each registers itself via init):
go get github.com/maqian/object-storage-client/providers/aws@v0.1.0
go get github.com/maqian/object-storage-client/providers/minio@v0.1.1
# ... or any of the other 8 providers
```

### 2. Open a Client

```go
package main

import (
    "context"
    "log"
    "strings"

    "github.com/maqian/object-storage-client/pkg/uos"
    "github.com/maqian/object-storage-client/pkg/uos/credential"
    _ "github.com/maqian/object-storage-client/providers/aws" // registers Factory
)

func main() {
    ctx := context.Background()

    cfg := uos.Config{
        Provider: "aws",
        Region:   "us-east-1",
        Credentials: credential.NewStaticProvider(credential.HMAC{
            AccessKeyID:     "AKIA...",
            SecretAccessKey: "...",
        }),
    }
    cli, err := uos.DefaultRegistry().Open(ctx, cfg)
    if err != nil { log.Fatal(err) }
    defer cli.Close()

    _, err = cli.Objects("my-bucket").Put(ctx, uos.PutObjectRequest{
        Key:  "hello.txt",
        Body: strings.NewReader("hello world"),
    })
    if err != nil { log.Fatal(err) }
}
```

A runnable end-to-end demo (Put + Get + Sign against a `testcontainers`
MinIO) lives at [`examples/quickstart/main.go`](examples/quickstart/main.go).

### 3. Switch providers

Same code, different `cfg.Provider` string + provider-specific config fields.
Runtime-`As(target)` escape hatch is available for accessing vendor-specific
SDK features not covered by the unified surface.

## Modules

| Module path | Tag | Purpose |
| --- | --- | --- |
| `github.com/maqian/object-storage-client` | `pkg/uos/v0.1.0` | Unified `Client` API (stdlib-only). |
| `.../pkg/testkit/contract` | `pkg/testkit/contract/v0.1.0` | Cross-provider contract test suite (testcontainers MinIO). |
| `.../providers/aws` | `providers/aws/v0.1.0` | AWS S3 native driver (`aws-sdk-go-v2`). |
| `.../providers/minio` | `providers/minio/v0.1.1` | MinIO native driver (`minio-go/v7`). |
| `.../providers/alibaba` | `providers/alibaba/v0.1.1` | Alibaba OSS (`aliyun-oss-go-sdk`). |
| `.../providers/tencent` | `providers/tencent/v0.1.1` | Tencent COS (`cos-go-sdk-v5`). |
| `.../providers/huawei` | `providers/huawei/v0.1.1` | Huawei OBS (`huaweicloud-sdk-go-obs`). |
| `.../providers/volcengine` | `providers/volcengine/v0.1.1` | Volcengine TOS (`ve-tos-golang-sdk/v2`). |
| `.../providers/gcs` | `providers/gcs/v0.1.1` | Google Cloud Storage (`cloud.google.com/go/storage`). |
| `.../providers/azure` | `providers/azure/v0.1.1` | Azure Blob Storage (`azure-sdk-for-go/sdk/storage/azblob`). |
| `.../providers/qiniu` | `providers/qiniu/v0.1.1` | Qiniu Kodo (`qiniu/go-sdk/v7`). |
| `.../providers/upyun` | `providers/upyun/v0.1.1` | Upyun USS (`upyun/go-sdk/v3`). |

Each provider module imports only its own vendor SDK + `pkg/uos`. Pulling
`providers/aws` does **not** drag in Azure, GCS, or any other vendor's
transitive chain. The root `pkg/uos` module ships **stdlib-only**.

## Capability matrix

Per-provider feature support is tracked in
[`docs/provider_matrix.md`](docs/provider_matrix.md). Quick legend:

| Symbol | Meaning |
| --- | --- |
| ✅ | Supported (contract test passes). |
| 🟡 | Conditional (works under specific config / credential / bucket state). |
| 🧩 | ExtensionOnly (reach via `Client.As(target)` and the vendor SDK). |
| ❌ | Unsupported (vendor doesn't expose; returns `ErrUnsupported`). |

The matrix is the authoritative answer to "does provider X support feature Y?"

## Architecture

The binding architecture document is
[`docs/architecture_plan.md`](docs/architecture_plan.md) (the v1 freeze
spec). Companion docs:

- [`docs/provider_roadmap.md`](docs/provider_roadmap.md) — milestone order
  + per-milestone "Lessons" log.
- [`docs/provider_matrix.md`](docs/provider_matrix.md) — capability cells
  per provider.
- [`AGENTS.md`](AGENTS.md) — multi-agent workflow notes (Appendix A
  tracks deferred follow-ups).
- [`RELEASING.md`](RELEASING.md) — per-module tag protocol.
- [`CHANGELOG.md`](CHANGELOG.md) — per-module release log.

## Provider quickstarts

Each driver ships a minimum-30-line README in its module directory:

- [providers/aws/README.md](providers/aws/README.md)
- [providers/minio/README.md](providers/minio/README.md)
- [providers/alibaba/README.md](providers/alibaba/README.md)
- [providers/tencent/README.md](providers/tencent/README.md)
- [providers/huawei/README.md](providers/huawei/README.md)
- [providers/volcengine/README.md](providers/volcengine/README.md)
- [providers/gcs/README.md](providers/gcs/README.md)
- [providers/azure/README.md](providers/azure/README.md)
- [providers/qiniu/README.md](providers/qiniu/README.md)
- [providers/upyun/README.md](providers/upyun/README.md)

## Testing

The cross-provider contract suite lives in `pkg/testkit/contract`. PR-gate
runs against MinIO via `testcontainers-go`; cloud-nightly runs against real
vendor endpoints when credentials are set:

```bash
# Unit tests across the unified surface:
go test -short -race ./...

# Contract suite against testcontainers MinIO (requires Docker):
cd pkg/testkit/contract && go test -tags=docker -count=1 ./...

# Real-cloud contract for one provider (set the OMC_<VENDOR>_NIGHTLY_* env vars):
cd providers/aws && go test -tags=docker -count=1 ./...
```

The freezing tripwire fences off accidental surface drift:

```bash
go test ./pkg/uos -run TestFrozenSurface -count=1 -v
```

Three subtests (`codes_frozen_14`, `capabilities_frozen_13`,
`direct_grant_modes_frozen_4`) MUST pass before any release.

## Versioning

Each module follows [SemVer 2.0.0](https://semver.org/) **independently**.
There is no umbrella repo-wide version. Per-module bump rules and the
release checklist are in [`RELEASING.md`](RELEASING.md).

## License

Apache-2.0. See [`LICENSE`](LICENSE).
