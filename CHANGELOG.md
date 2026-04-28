# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and each module in this multi-module workspace adheres to [Semantic
Versioning](https://semver.org/spec/v2.0.0.html) independently. See
[RELEASING.md](./RELEASING.md) for the per-module tag protocol.

## [Unreleased]

(Nothing pending. Next entries belong here when M3 work begins — Alibaba / Tencent / Huawei / Volcengine native drivers per `docs/provider_roadmap.md`.)

## [providers/aws/v0.1.0] — 2026-04-28 — M2 (AWS native driver)

First tagged release of the AWS S3 native driver. Implements every
`pkg/uos.Client` method against `aws-sdk-go-v2 + service/s3`. Passes
the cross-provider contract test kit (`pkg/testkit/contract.RunSuite`)
against a `testcontainers-go` MinIO endpoint in S3-compat mode.

### Added

- `providers/aws/factory.go`: `factoryImpl` registers itself on
  `pkg/uos.DefaultRegistry` via `init()`. `Provider() = "aws"`,
  `Validate(cfg)` requires Region, `Open(ctx, cfg)` constructs an
  `aws.Config` with a custom `EndpointResolverV2` (when
  `cfg.Endpoint` is set, for S3-compat targets), `aws.NopRetryer{}`
  (deliberate — pkg/uos owns retry per `RetryPolicy`), and a
  credentials adapter that pulls AK/SK from
  `cfg.CredentialProvider`. Path-style addressing on opt-in via
  `DriverConfig.PathStyle` (forced when a custom endpoint is set).
- `providers/aws/driver.go`: `driverImpl` implements `Client` plus
  the four sub-services (`BucketService`, `ObjectService`,
  `MultipartService`, `Signer`). Notable choices:
  - Multipart uses raw `s3.CreateMultipartUpload` /
    `UploadPart` / `CompleteMultipartUpload` (does not route
    through `pkg/uos/transfer.Manager` — see Notes below).
  - `DeleteMany` auto-batches keys into S3's 1000-per-request cap.
  - `Signer.SignURL` uses `s3.PresignClient`. `IssueDirectGrant`
    returns `ErrUnsupported{Capability: CapDirectGrant}` per
    matrix footnote 5 (S3-family uses presigned URL).
  - `As(target)` exposes `**s3.Client` and `**s3.PresignClient`.
- `providers/aws/error_map.go`: translates `*types.NoSuchKey`,
  `*types.NoSuchBucket`, `*types.BucketAlreadyExists`,
  `*types.BucketAlreadyOwnedByYou`, `*types.NotFound`, and generic
  `smithy.APIError` codes into the 14 frozen `pkg/uos.Code`
  values; HTTP status fallback for unmapped errors. `RequestID`
  and `SecondaryID` populated from awsmiddleware metadata.
- `providers/aws/capabilities.go`: returns the 13-cell
  `capability.Report` matching the aws column of
  `docs/provider_matrix.md` (9 ✅, 2 🟡 [Versioning, ObjectACL —
  see footnotes 13/14], 1 ❌ [DirectGrant], 1 🧩 [NativeMove]).
- `providers/aws/driver_test.go` (build tag `docker`): spawns
  MinIO via `pkg/testkit/contract.SpawnMinIO`, configures the
  AWS SDK to point at the S3-compat endpoint, runs the contract
  suite end-to-end. 28 PASS, 17 SKIP (3 driver-level SkipCases
  for MinIO/SDK canonicalisation drift on `?` and `%FF` keys —
  cloud-nightly will validate against real AWS).

### Notes

- Bypassed `pkg/uos/transfer.Manager` in favor of raw
  `s3.UploadPart` orchestration. See `RELEASING.md` §5
  (ADR Follow-up #1) — this answer, plus the parallel MinIO
  driver's identical bypass, motivates the v0.2.0 `Uploader`
  interface refactor that the Architect originally proposed.
- `go.mod` floor is `go 1.25.0` because `aws-sdk-go-v2 v1.41.6+`
  transitively requires it. Root `pkg/uos` remains at `go 1.22`.
- Real AWS smoke tests are gated by the cloud-nightly workflow
  (`.github/workflows/cloud-nightly.yml`) which exits SKIP when
  `OMC_AWS_NIGHTLY_KEY` / `OMC_AWS_NIGHTLY_SECRET` are absent.

## [providers/minio/v0.1.0] — 2026-04-28 — M2 (MinIO native driver)

First tagged release of the MinIO native driver. Implements every
`pkg/uos.Client` method against `minio-go/v7`. Passes the
cross-provider contract test kit against a `testcontainers-go`
MinIO endpoint.

### Added

- `providers/minio/factory.go`: `factoryImpl` registers on
  `pkg/uos.DefaultRegistry` via `init()`. `Provider() = "minio"`,
  `Validate(cfg)` requires Endpoint + CredentialProvider,
  `Open(ctx, cfg)` constructs a `minio.Client` with
  `BucketLookup: BucketLookupPath` (path-style is the MinIO
  default) and `MaxRetries: 1` (pkg/uos owns retry per
  `RetryPolicy`).
- `providers/minio/driver.go`: `driverImpl` implements `Client`
  plus the four sub-services. Notable choices:
  - `Get` uses `minio.Core.GetObject` (raw API) instead of the
    high-level streaming reader, because the latter ignores
    explicit `Range` options. Required for the contract suite's
    `range_returns_slice` case.
  - Multipart delegated to `minio.Client.PutObject` (vendor
    handles size-based dispatch + parallel parts + abort) plus
    `Core` for raw `Initiate/UploadPart/Complete/Abort`.
  - `Signer.SignURL` uses `minio.PresignedGet/Put`.
    `IssueDirectGrant` returns the same typed-Unsupported error
    as AWS.
  - `As(target)` exposes `**minio.Client` and `**minio.Core`.
- `providers/minio/error_map.go`: translates `minio.ErrorResponse`
  codes (`NoSuchKey`, `NoSuchBucket`, `BucketAlreadyOwnedByYou`,
  `AccessDenied`, `SignatureDoesNotMatch`, `SlowDown`,
  `RequestTimeout`, etc.) into the 14 frozen `pkg/uos.Code`
  values. Catch-all is `ErrInternal` with `Cause` populated.
- `providers/minio/capabilities.go`: same shape as the AWS
  driver (S3-family).
- `providers/minio/driver_test.go` (build tag `docker`): spawns
  MinIO via the testkit helper, runs the contract suite. All
  cases pass; 1 driver-level `SkipCases` entry for
  `signer/issue_direct_grant_shape` (the capability-gating case
  for the same path passes — proving the typed-Unsupported
  contract).

### Notes

- Bypassed `pkg/uos/transfer.Manager` for the same reason as the
  AWS driver. See ADR Follow-up #1 in `RELEASING.md` §5.
- `go.mod` floor is `go 1.22` (matches root). `minio-go/v7` does
  not require Go 1.25.

## [pkg/uos/v0.1.0] — 2026-04-28 — M1 (Core Skeleton)

First tagged release of the universal object storage client SDK. Ships
the public API surface and shared internals; zero provider drivers.
M2 (AWS + MinIO) is unblocked from day one.

### Added

- **Public API in `pkg/uos`**:
  - `Client` interface and four sub-services: `BucketService`,
    `ObjectService`, `MultipartService`, `Signer`.
  - `Provider`, `Config`, `Factory`, `Registry`, plus default
    in-process `Registry` (`NewRegistry` / `DefaultRegistry`).
  - 14 frozen `Code` constants (`ErrUnsupported`, `ErrInvalidArgument`,
    `ErrNotFound`, `ErrAlreadyExists`, `ErrPermissionDenied`,
    `ErrUnauthenticated`, `ErrPreconditionFailed`, `ErrConflict`,
    `ErrRateLimited`, `ErrTimeout`, `ErrTemporary`,
    `ErrChecksumMismatch`, `ErrLengthRequired`, `ErrInternal`) and
    the concrete `*Error` type with `Is` / `Unwrap` matching contract.
  - `NewUnsupported` and `WrapMissingCapability` helpers for the
    capability-gap rich error.
  - Request / response value types: `BucketInfo`, `ObjectInfo`,
    `ObjectReader`, `ObjectList`, `Metadata`, `ContentHeaders`,
    `Checksum`, `DirectGrant`, `SignedURL`, plus per-service request
    families in `request_bucket.go`, `request_object.go`,
    `request_multipart.go`, `request_signer.go`.
  - `MultipartUpload` and `MultipartUploadList` shapes (Critic R1
    sign-off addition: explicit field set, not inferred).
- **`pkg/uos/capability`**: 13 frozen `Capability` constants
  (`bucket.crud`, `object.crud`, `object.list.prefix_delimiter`,
  `object.range_read`, `object.multipart_upload`, `signer.url_read`,
  `signer.url_write`, `signer.direct_grant`, `object.tagging`,
  `bucket.versioning`, `object.acl`, `object.encryption.managed`,
  `object.native_move`); `Availability` enum; `Report` with `Get` /
  `Has` / `Require` helpers and `MissingCapability` sentinel.
- **`pkg/uos/credential`**: `Provider` interface, `Credential` struct
  with `AuthScheme` enum, `StaticProvider`, `EnvProvider` (reads
  `OSC_*` and AWS-compatible `AWS_*` vars; AWS coupling tracked as
  v0.2.0 cleanup), `Chain` (first-success traversal).
- **`pkg/uos/transfer`**: `Manager` skeleton with planner, bounded
  worker pool, abort-on-failure semantics, and resume hook backed by
  a `StateStore` (memory implementation included). Local adapter
  types `UploadRequest` and `MultipartServiceLike` documented as
  the cycle-avoidance pattern (Critic R1 / Architect R3.ii).
- **`pkg/uos/middleware`**: `Logger`, `Metrics`, `Tracer` contracts;
  composer `Chain`; redaction list of 11 sensitive headers and 12
  sensitive query params.
- **`pkg/uos/httpx`**: `HTTPConfig` (Timeout, Proxy, RootCAs,
  InsecureSkipVerify, MaxIdleConns) and `NewClient` constructor;
  emits a runtime warning when `InsecureSkipVerify` is set.
- **`pkg/testkit/contract` (separate Go module)**: `RunSuite(t, FactoryUnderTest)`
  plus case files for bucket, object, multipart, signer, capability,
  and error coverage. `minio.go` (build tag `docker`) wraps a
  `testcontainers-go` MinIO container for live cross-provider checks.
  Lives at `github.com/maqian/object-storage-client/pkg/testkit/contract`
  with its own `go.mod`, isolating the testcontainers / Docker / OTel
  transitive chain from `pkg/uos` consumers.
- **Frozen surface fence**: `pkg/uos/surface_test.go` /
  `TestFrozenSurface` — three subtests literal-pin the 14 Codes,
  13 Capabilities, and 4 DirectGrantModes (Critic R1 binding).
- **CI**: `.github/workflows/ci.yml` declares five jobs:
  - `unit-root` — matrix `ubuntu-latest`/`macos-latest` × Go `1.22`/`1.23`
    against the root module.
  - `unit-testkit` — same matrix at Go `1.25` against the testkit module.
  - `unit-docker` — `-tags=docker` contract suite from the testkit module.
  - `vet-fmt` — `go vet` and `gofmt -l .` enforced on both modules.
  - `surface` — the `TestFrozenSurface` tripwire on Go `1.22`.
- **Operational**: `Makefile` (test / vet / fmt / add-provider),
  `LICENSE` (Apache-2.0 placeholder), `go.work` enumerating root +
  testkit + future provider modules, `scripts/add-provider.sh` for
  multi-module provider scaffolding (writes `go 1.22` into
  scaffolded provider go.mod, matching the root floor).

### Notes

- **Go directive**: root `go.mod` targets **Go 1.22** (the originally
  planned floor). `pkg/testkit/contract` declares **Go 1.25** because
  its `testcontainers-go` chain transitively requires it; that cost
  is contained inside the testkit module and never reaches `pkg/uos`
  consumers.
- **ADR Follow-up #3 — `pkg/testkit/contract` module hoist** —
  resolved **before tagging v0.1.0**. Originally planned as M6
  (conditional), then provisionally promoted to v0.2.0 mandatory
  during M1, the hoist landed inside the v0.1.0 release. Result:
  root `go.sum` is empty (no third-party transitive entries from the
  contract testkit), satisfying NFR-008 at the module level for the
  first time. See `RELEASING.md` §5 for the post-resolution status.
- No provider drivers ship in v0.1; `providers/<name>/` directories
  arrive in M2+.
