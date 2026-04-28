# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and each module in this multi-module workspace adheres to [Semantic
Versioning](https://semver.org/spec/v2.0.0.html) independently. See
[RELEASING.md](./RELEASING.md) for the per-module tag protocol.

## [Unreleased]

(Nothing pending — the next entries belong here when M2 work begins.)

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
