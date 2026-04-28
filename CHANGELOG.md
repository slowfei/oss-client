# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and each module in this multi-module workspace adheres to [Semantic
Versioning](https://semver.org/spec/v2.0.0.html) independently. See
[RELEASING.md](./RELEASING.md) for the per-module tag protocol.

## [Unreleased]

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
- **`pkg/testkit/contract`**: `RunSuite(t, FactoryUnderTest)` plus
  case files for bucket, object, multipart, signer, capability, and
  error coverage. `minio.go` (build tag `docker`) wraps a
  `testcontainers-go` MinIO container for live cross-provider checks.
- **Frozen surface fence**: `pkg/uos/surface_test.go` /
  `TestFrozenSurface` — three subtests literal-pin the 14 Codes,
  13 Capabilities, and 4 DirectGrantModes (Critic R1 binding).
- **CI**: `.github/workflows/ci.yml` declares four jobs — `unit`
  (matrix `ubuntu-latest`/`macos-latest` × Go `1.25`), `vet-fmt`,
  `unit-docker` (`-tags=docker` contract suite), and `surface`
  (the freezing tripwire job). All jobs use Go 1.25.
- **Operational**: `Makefile` (test / vet / fmt / add-provider),
  `LICENSE` (Apache-2.0 placeholder), `go.work` enumerating root +
  future provider modules, `scripts/add-provider.sh` for
  multi-module provider scaffolding (writes `go 1.25` into
  scaffolded provider go.mod).

### Notes

- **Go directive raised to `1.25.0`** in root `go.mod`. The plan
  baselined Go 1.22, but `testcontainers-go` and its transitive
  dependencies require Go 1.25; we accepted Go 1.25 as the new
  v0.1.0 floor rather than yank the contract test kit's MinIO
  driver. Documented as ADR Follow-up #3 promoted from "M6
  optional" to "v0.2.0 mandatory" — see `RELEASING.md` §5.
- No provider drivers ship in v0.1; `providers/<name>/` directories
  arrive in M2+.
- `pkg/testkit/contract` lives inside the root module for v0.1; if
  it evolves faster than `pkg/uos` it will be hoisted to its own
  module at v0.2.0 (ADR Follow-up #3).
