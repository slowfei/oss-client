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
2. `go mod init`: root module initialized as `github.com/maqian/object-storage-client`. Go directive is **`1.25.0`** in root `go.mod` (raised from the planned 1.22 because `testcontainers-go` transitive deps require it; accepted as the new v0.1 floor) — done.
3. `go.work` bootstrap: `go.work` committed at the repo root and used by `scripts/add-provider.sh` to register new provider modules (script writes `go 1.25` into scaffolded provider modules) — done.
4. `s3common` extraction: deferred to M3+ once two S3-family drivers have shipped (unchanged).
5. License + CI scaffolding: Apache-2.0 placeholder committed. CI workflow now also done — `.github/workflows/ci.yml` declares `unit`, `vet-fmt`, `unit-docker`, and `surface` jobs, all running on Go 1.25.
6. **Testkit module hoist (new — promoted from ADR Follow-up #3)**: `pkg/testkit/contract` lives in the root module in v0.1, which forced `testcontainers-go` (and Go 1.25) into the root `go.mod`/`go.sum`. The ADR originally listed the hoist decision as M6-conditional; it is now scheduled as **v0.2.0 mandatory** so consumers who only want `pkg/uos` do not pay the testcontainers cost. See `RELEASING.md` §5 for the release-side handling.
