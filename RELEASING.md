# Releasing

This repository is a Go multi-module workspace. Each module is
versioned and tagged independently. This document defines:

1. The module map.
2. The tag scheme.
3. The semver-bump rules per module type.
4. The release checklist for `pkg/uos/v0.1.0` (M1).
5. How the ADR Follow-ups list interacts with releases.

## 1. Module map

| Module path                                                | Tag prefix                  | Status (v0.1) | Notes |
| ---------------------------------------------------------- | --------------------------- | ------------- | ----- |
| `github.com/maqian/object-storage-client`                  | `pkg/uos/`                  | ACTIVE        | Root module. Houses `pkg/uos` and its subpackages (`capability`, `credential`, `transfer`, `middleware`, `httpx`). Stdlib-only; no third-party transitive deps. |
| `github.com/maqian/object-storage-client/pkg/testkit/contract` | `pkg/testkit/contract/` | ACTIVE        | Independent module hosting the cross-provider contract test suite. Pulls `testcontainers-go` and its transitive Docker / containerd / OTel chain so that `pkg/uos` consumers do not pay that cost. Pinned at Go 1.25 because `testcontainers-go` requires it. Local development resolves the parent module via `go.work`; the `replace` directive in its `go.mod` keeps `go mod tidy` runnable until the parent ships a published tag. |
| `github.com/maqian/object-storage-client/providers/aws`    | `providers/aws/`            | ACTIVE        | M2 native driver (`aws-sdk-go-v2 + service/s3`). Pinned at Go 1.25.0 because `aws-sdk-go-v2 v1.41+` requires it. Replace directives for parent + testkit (cleared at release time per §4 Post-tag). |
| `github.com/maqian/object-storage-client/providers/minio`  | `providers/minio/`          | ACTIVE        | M2 native driver (`minio-go/v7`). `go 1.22` (same floor as root). Replace directives for parent + testkit. |
| `github.com/maqian/object-storage-client/providers/<name>` | `providers/<name>/`         | PLANNED       | Future provider modules (M3+: alibaba, tencent, huawei, volcengine; M4: gcs, azure; M5: qiniu, upyun). Scaffolded by `scripts/add-provider.sh`. |

The contract testkit was hoisted out of the root module in v0.1.0
itself, ahead of its originally-planned slot — see §5.

## 2. Tag scheme

Go module tooling requires that each module's tag be prefixed with
the module's directory path relative to the repo root. We use:

- Root module (`pkg/uos`): `pkg/uos/vX.Y.Z`
  - Example: `pkg/uos/v0.1.0`
- Contract testkit module: `pkg/testkit/contract/vX.Y.Z`
  - Example: `pkg/testkit/contract/v0.1.0`
- Provider module (`providers/<name>`): `providers/<name>/vX.Y.Z`
  - Example: `providers/aws/v0.1.0`

Each module follows [SemVer 2.0.0](https://semver.org/spec/v2.0.0.html)
independently. There is no umbrella repo-wide version; `pkg/uos`,
`pkg/testkit/contract`, and each provider drift on their own cadence.

## 3. Semver-bump rules

### Patch (`vX.Y.Z+1`)
Bug fixes, doc-only changes, internal refactors that do not change
any exported identifier.

### Minor (`vX.Y+1.0`)
Additive changes:
- New exported function, type, method, struct field, or constant on
  an existing module surface.
- New driver capability that was previously absent from the
  driver's `Report` (provider modules only).

Per the binding ADR (`.omc/plans/v0.1-implementation-plan.md` ADR
section), additions to **any** of the three frozen sets in `pkg/uos`
require a minor bump *and* satisfy the "≥2 providers needing the
same semantic" rule:

- New `Code` constant in `pkg/uos`.
- New `Capability` constant in `pkg/uos/capability`.
- New `DirectGrantMode` value in `pkg/uos`.

The fence is `pkg/uos/surface_test.go` / `TestFrozenSurface`:
adding to a frozen set requires updating that test in the same PR
that bumps the minor version. A PR that grows a frozen set without
updating the surface test is a CI failure (the `surface` job).

### Major (`vX+1.0.0`)
Removing or renaming any exported identifier; changing any frozen
constant's string value (wire-breaking); changing the `errors.Is`
matching contract on `*Error`.

For `pkg/uos`, a major bump should be exceedingly rare: the v1
freeze (architecture_plan §7) is designed so that abstraction
defects discovered at M2+ land as additive minors against
`pkg/uos`, not as v2.0.0. See the §6.5 abstraction-validation gate
in the architecture plan.

## 4. Release checklist — `pkg/uos/v0.1.0` and `pkg/testkit/contract/v0.1.0`

Run through this list in order. The `git tag` and `git push` at the
bottom are **maintainer actions**, gated on user approval; the
release executor (or this checklist) does not run them.

### Pre-flight

- [ ] CHANGELOG.md `[pkg/uos/v0.1.0]` entry merged onto `main`
  (the v0.1.0 entry covers both modules' first cut, since the
  testkit hoist landed pre-tag).
- [ ] All five CI jobs green on the v0.1 PR:
  - `unit-root` (matrix: `ubuntu-latest` / `macos-latest` × Go `1.22`/`1.23`).
  - `unit-testkit` (same matrix at Go `1.25`).
  - `vet-fmt` (root + testkit).
  - `unit-docker` (`-tags=docker` contract suite from the testkit module).
  - `surface` (the freezing tripwire — `TestFrozenSurface`).
- [ ] Local `go test -short -race ./...` is green (root).
- [ ] Local `cd pkg/testkit/contract && go test -short -race ./...`
  is green; `cd pkg/testkit/contract && go build -tags=docker ./...`
  succeeds.
- [ ] Local `go test ./pkg/uos -run TestFrozenSurface -count=1 -v`
  is green; all three subtests (`codes_frozen_14`,
  `capabilities_frozen_13`, `direct_grant_modes_frozen_4`) pass.
- [ ] `gofmt -l .` prints nothing (root and testkit); `go vet ./...`
  is clean (both modules).
- [ ] A maintainer has confirmed the canonical module path
  `github.com/maqian/object-storage-client` is correct (this is
  also baked into all the example imports in `pkg/uos/doc.go` and
  the `replace` directive in `pkg/testkit/contract/go.mod`).

### Tag (maintainer action — DO NOT auto-execute)

The tag commands are documented here so the executor leaves a clear
breadcrumb; a human must run them after the pre-flight checklist is
green and approval is given. Tag the root first, then the testkit
module, so the testkit's `replace` directive can be removed and its
`require` line bumped to the published parent tag in the same PR
that flips it from local-dev to a real release.

```bash
# Tag root first so providers can pin a real parent version
git tag pkg/uos/v0.1.0
git push origin pkg/uos/v0.1.0

# Tag testkit module (its replace directive must be removed and its
# require bumped to pkg/uos/v0.1.0 in the same commit before tagging)
git tag pkg/testkit/contract/v0.1.0
git push origin pkg/testkit/contract/v0.1.0

# Tag M2 provider modules (each module's replace directives for parent
# AND testkit must be removed, and the requires bumped to the freshly-
# tagged versions, in the same commit before tagging)
git tag providers/aws/v0.1.0
git push origin providers/aws/v0.1.0

git tag providers/minio/v0.1.0
git push origin providers/minio/v0.1.0

# Tag M3 provider modules (alibaba shipped first as the s3common
# validation case; tencent/huawei/volcengine landed in parallel
# afterwards). Same replace-cleanup-per-commit pattern as M2.
git tag providers/alibaba/v0.1.0
git push origin providers/alibaba/v0.1.0

git tag providers/tencent/v0.1.0
git push origin providers/tencent/v0.1.0

git tag providers/huawei/v0.1.0
git push origin providers/huawei/v0.1.0

git tag providers/volcengine/v0.1.0
git push origin providers/volcengine/v0.1.0
```

After tagging, verify both tags are fetchable:

```bash
go list -m github.com/maqian/object-storage-client@v0.1.0
go list -m github.com/maqian/object-storage-client/pkg/testkit/contract@v0.1.0
```

(This validates that Go module proxy can serve the tagged versions.)

### Post-tag

- [ ] Open `[Unreleased]` section in CHANGELOG.md for ongoing
  v0.2.0 work.
- [ ] In `pkg/testkit/contract/go.mod`, replace
  `github.com/maqian/object-storage-client v0.0.0` with the
  freshly-tagged version and remove the `replace` directive.
- [ ] Bump the AGENTS.md Appendix A status table if any items
  graduated from "deferred" to "released."

## 5. ADR Follow-ups (informational)

The full Follow-ups list is in `.omc/plans/v0.1-implementation-plan.md`
under the `ADR` section's `Follow-ups (post-v0.1, ranked by
importance)` heading (items 1-11). This file does not duplicate it.

One follow-up was resolved during the v0.1.0 cycle:

- **Follow-up #3 — `pkg/testkit/contract` module hoist**: planned
  for "M6, conditional on testkit evolving faster than pkg/uos,"
  provisionally promoted to "v0.2.0 mandatory" during M1 when the
  testcontainers transitive cost surfaced, and **resolved inside
  the v0.1.0 release** — done before tagging. `pkg/testkit/contract`
  now lives at its own module path with its own `go.mod`. Root
  `go.sum` is empty; `pkg/uos` consumers no longer carry the
  Docker / containerd / OTel transitive chain.

M2 surfaced the answer to two more Follow-ups:

- **Follow-up #1 — M2 transfer.Manager / AWS multipart answer — RESOLVED in v0.1.0 pre-tag**:
  the original ADR asked "did `transfer.Manager` orchestrate AWS
  multipart correctly?" Answer (recorded during M2): **bypass — both the AWS and MinIO
  M2 drivers do NOT route uploads through `pkg/uos/transfer.Manager`**.
  Both delegate to the vendor SDK's native multipart implementation
  (`s3.UploadPart` and `minio.Client.PutObject` respectively),
  which already handles size-based dispatch, parallel part uploads,
  abort-on-failure, and progress reporting. Wrapping
  `transfer.Manager` around either would double the
  orchestration logic.
  **Resolution (2026-04-28, pre-tag)**: shipped the Architect's
  proposed `Uploader` interface (and a symmetric `Downloader`)
  inside `pkg/uos/uploader.go` — see `architecture_plan.md` §1.1
  rows 12 and 13 / `CHANGELOG.md` `[pkg/uos/v0.1.0]` entry.
  Both interfaces are structural one-method interfaces satisfied
  implicitly by the existing `ObjectService`; both M2 drivers
  (`providers/aws`, `providers/minio`) needed zero code change.
  The bypass is now first-class via `ObjectService.Put`-as-Uploader;
  M4 (GCS) and M5 (Upyun) drivers will be free to satisfy
  `Uploader` via a `transfer.Manager`-backed wrapper in their own
  provider package without contorting their request shapes into the
  unified `MultipartService` API.
- **Follow-up #4 — `s3common` extraction — RESOLVED in pre-tag
  refactor**: originally planned for "M3+ once two S3-family
  drivers have shipped"; the M2 architect review recommended
  pulling it forward to the FIRST M3 driver landing rather than
  waiting for the second. We ultimately did the extraction
  pre-tag, while only AWS + MinIO were shipped, because the
  duplication surface was already visible and the cost of doing
  it later (4 国云 drivers also paying the duplication tax) was
  larger than the value of waiting.

  What landed in `pkg/uos/s3common`:
  - `MapCodeString` — S3-compat wire code string → `uos.Code`.
  - `MapHTTPStatus` — HTTP status fallback table.
  - `MapContextErr` — `context.Canceled`/`DeadlineExceeded`
    → `uos.ErrTimeout`.
  - `IsRetryable` — marks the three retryable Codes.
  - `LowerMetadataKeys` — metadata key case-folding (with
    nil/empty collapse).

  What did NOT extract: the architect's review listed five
  candidates including pointer-flatten helpers and HTTP Range
  header formatting. Hands-on inspection of providers/aws +
  providers/minio showed only 3 of the 5 were actually
  duplicated — pointer helpers and `formatRange` are AWS-only
  (MinIO uses native typed APIs). Re-evaluate if M3+ vendors
  surface the same duplication.

All other Follow-ups remain at the priority captured in the ADR.
