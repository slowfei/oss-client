# Project
Build a Go universal object storage client SDK/library.

## Goal
Provide a unified object storage API for multiple providers:
AWS, MinIO, Alibaba Cloud OSS, Tencent COS, Huawei OBS, Volcano TOS,
Azure Blob, Google Cloud Storage, Qiniu Kodo, Upyun, and future providers.

## Hard boundaries
- This repository builds a client SDK/library only.
- Never build a proxy server, gateway, SaaS control plane, or credential-hosting service.
- Keep public API provider-neutral.
- Do not leak provider-specific request/response types into pkg/uos public interfaces.

## Source of truth
- docs/architecture_plan.md (binding; supersedes PRD on conflict)
- docs/provider_roadmap.md (binding; per-milestone provider rollout)
- docs/provider_matrix.md (binding; capability × provider matrix)
- docs/prd.md (background reference only)

## Delivery rules
- Work milestone by milestone.
- Do not implement all providers in one pass.
- Public abstractions must be justified by at least two providers or by the design doc.
- Unsupported provider features must be surfaced explicitly instead of being faked.

## Package boundaries
- Public API: pkg/uos (root go.mod)
- Provider drivers: providers/<name> (each its own go.mod; multi-module — see architecture_plan.md §3)
- Shared internals (root module only): internal/*
- Contract tests and reusable provider verification helpers: pkg/testkit/contract (public path; multi-module requires it)

## Engineering rules
- Run gofmt, go vet, and go test ./... after code changes.
- Add doc comments to exported APIs.
- Update docs/provider_matrix.md when provider support changes.
- Prefer small commits after each milestone.

## Appendix A: Deferred follow-ups (status)
Tracks the architecture_plan.md Appendix A cleanup items. Status reflects M1 (`pkg/uos/v0.1.0`) sign-off on 2026-04-28.

1. Source-of-truth pointer: this file references `docs/architecture_plan.md` (the binding spec) — done.
2. `go mod init`: root module initialized as `github.com/maqian/object-storage-client` with `go 1.22` (the originally planned floor). Root `go.sum` is empty (stdlib-only); third-party transitive cost is contained inside the hoisted `pkg/testkit/contract` module — see item 6 — done.
3. `go.work` bootstrap: `go.work` committed at the repo root (declares `go 1.25.0` to satisfy the testkit module's higher floor) and lists both `./` and `./pkg/testkit/contract`. `scripts/add-provider.sh` registers new provider modules and writes `go 1.22` into scaffolded provider go.mod files (matching the root floor) — done.
4. `s3common` extraction — DONE pre-tag (lead refactor on 2026-04-28; M3 alibaba landing extended `MapCodeString` with 10 OSS-specific codes). Originally deferred to M3+; pulled forward when the AWS + MinIO duplication surface proved sufficient justification. `pkg/uos/s3common/` now hosts 5 shared helpers: `MapCodeString` (with M3-extended OSS aliases), `MapHTTPStatus`, `MapContextErr`, `IsRetryable`, `LowerMetadataKeys`. Architect's full 5-candidate list reduced to 3 actually-duplicated pieces after hands-on inspection (pointer helpers and `formatRange` are AWS-only because minio-go uses native typed APIs); see `RELEASING.md` §5 Follow-up #4 for details. **M4 (gcs + azure) validated the boundary as correctly drawn**: neither non-S3 vendor extended `MapCodeString` (their JSON / Azure ErrorCode vocabularies live in LOCAL switches inside the per-driver `error_map.go`); the wire-protocol-agnostic helpers (`MapHTTPStatus`, `MapContextErr`, `IsRetryable`, `LowerMetadataKeys`) are reused by all 8 shipped drivers.
5. License + CI scaffolding: Apache-2.0 placeholder committed. CI workflow now also done — `.github/workflows/ci.yml` declares five jobs: `unit-root` (matrix Go 1.22/1.23 × ubuntu/macos), `unit-testkit` (Go 1.25 × ubuntu/macos, `working-directory: pkg/testkit/contract`), `vet-fmt` (covers both modules), `unit-docker` (testkit module, `-tags=docker`), and `surface` (Go 1.22 `TestFrozenSurface` tripwire).
6. **Testkit module hoist (ADR Follow-up #3) — DONE in v0.1.0**: `pkg/testkit/contract` is now its own Go module (`github.com/maqian/object-storage-client/pkg/testkit/contract`) with its own `go.mod`. The testcontainers / Docker / containerd / OTel transitive chain stays inside the testkit module; root `go.mod` reverted to `go 1.22` and root `go.sum` is empty. `go.work` wires the two modules together for local dev; the testkit module carries a `replace` directive that gets removed (and its `require` bumped to the tagged parent) at v0.1.0 release time per `RELEASING.md` §4 post-tag steps.
7. **M4 abstraction validation (gcs + azure) — DONE 2026-04-28**: 8 drivers shipped with **zero `pkg/uos` change**. `Capability` (13), `Code` (14), and `DirectGrantMode` (4) frozen sets all proven sufficient for the non-HMAC milestone (OAuth2 / Service Account / ADC / SharedKey / SAS / Entra). Azure SAS validates `DirectGrantModeToken` as a real-world fit (not just a placeholder). Three v0.2.0-deferred candidates surfaced — see `docs/provider_roadmap.md` Lessons (M4) for full rationale: (a) `Capabilities().MinPartSize` field — Azure Block Blob requires ≥4 MiB blocks while S3 requires ≥5 MiB; needs ≥2 providers per ADR rule (have 1 — Azure); promote when M5 surfaces a second; (b) `MultipartService` "sequential-only" capability flag — GCS resumable upload accepts contiguous parts only; currently inferred from doc; (c) `credential.Provider.Rotate(ctx)` hook — not needed in v0.1 (GCS SDK handles refresh internally; Azure SAS reissue is one-shot via `IssueDirectGrant`); re-evaluate if a vendor surfaces with a credential lifetime shorter than a single contract-suite run.
8. **M5 abstraction validation (qiniu + upyun) — DONE 2026-04-28**: **10 v1-target drivers shipped with zero `pkg/uos` change** — the v1 abstraction is fully validated. All 4 frozen `DirectGrantMode` values exercised in production code: `URL` (S3-family read presign), `Form` (Upyun upload — M5 NEW validation), `Token` (Azure SAS + Qiniu Upload + Qiniu Download), `Headers` (still unused, available for future). Upyun FORM upload absorbed cleanly by the existing `DirectGrant.{Mode, URL, Method, Headers, FormFields, Token}` field set — no widening recommended. The `DirectGrantRequest.Extra map[string]string` escape hatch absorbed all 14 vendor-specific policy keys across qiniu (8 PutPolicy keys: callbackUrl, callbackBody, callbackHost, callbackBodyType, returnBody, returnUrl, saveKey, persistentOps) + upyun (6 policy keys: notify-url, apps, expiration-override, save-key, content-md5, allow-file-type) without growing the request struct. Cumulative v0.2.0 deferred candidates from M3+M4+M5 (none gating any milestone): (1) `MultipartService` sequential-only capability flag (gcs + qiniu both have it); (2) `MinPartSize` capability field (azure 4 MiB ≠ S3 5 MiB; promote to v0.2.0 if M6 surveys ≥1 more provider); (3) `credential.Provider.Rotate(ctx)` hook (still not needed in v1); (4) Azure multipart `Initiate.Metadata` persistence to `Complete` (driver-internal correctness fix); (5) `DirectGrantRequest.Extra` formal documentation of vendor-recognised keys (qiniu + upyun); (6) `DirectGrant.ExtraReturned map[string]string` for vendor-side dispatch info return (qiniu + upyun); (7) Upyun SDK context-cancellation wiring via context-aware `http.Client`; (8) `errors.As(&alreadyMapped)` early-return loses op/bucket/key context (cross-driver pattern). Plus the M3-phase-2 architect notes: `s3common.MapServiceCode`, `s3common.IsRetryableWithStatus`, `credential.MustExtractHMAC`, `s3common.S3FamilyCapabilities()` helper. **M6 (stabilization) is the next milestone**: benchmarks, examples, OTel semantic-conventions alignment, README quickstarts, then v1.0.0 cut.
