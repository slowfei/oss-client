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
| `github.com/maqian/oss-client`                  | `vX.Y.Z` (bare)             | ACTIVE        | Root module — `go.mod` lives at the repo root. Houses `pkg/uos` and its subpackages (`capability`, `credential`, `transfer`, `middleware`, `httpx`). Stdlib-only; no third-party transitive deps. **Tag form is bare `vX.Y.Z`** (no path prefix) per Go's tag-discovery rule for root modules. A vanity `pkg/uos/vX.Y.Z` tag MAY also be created at the same commit for human-readability + CHANGELOG cross-reference, but downstream `go get github.com/maqian/oss-client@vX.Y.Z` ONLY resolves the bare tag — see §5 footnote on the v0.1.0 tag-naming incident. |
| `github.com/maqian/oss-client/pkg/testkit/contract` | `pkg/testkit/contract/` | ACTIVE        | Independent module hosting the cross-provider contract test suite. Pulls `testcontainers-go` and its transitive Docker / containerd / OTel chain so that `pkg/uos` consumers do not pay that cost. Pinned at Go 1.25 because `testcontainers-go` requires it. Local development resolves the parent module via `go.work`; the `replace` directive in its `go.mod` keeps `go mod tidy` runnable until the parent ships a published tag. |
| `github.com/maqian/oss-client/providers/aws`    | `providers/aws/`            | ACTIVE        | M2 native driver (`aws-sdk-go-v2 + service/s3`). Pinned at Go 1.25.0 because `aws-sdk-go-v2 v1.41+` requires it. Replace directives for parent + testkit (cleared at release time per §4 Post-tag). |
| `github.com/maqian/oss-client/providers/minio`  | `providers/minio/`          | ACTIVE        | M2 native driver (`minio-go/v7`). `go 1.22` (same floor as root). Replace directives for parent + testkit. |
| `github.com/maqian/oss-client/providers/<name>` | `providers/<name>/`         | PLANNED       | Future provider modules (M3+: alibaba, tencent, huawei, volcengine; M4: gcs, azure; M5: qiniu, upyun). Scaffolded by `scripts/add-provider.sh`. |

The contract testkit was hoisted out of the root module in v0.1.0
itself, ahead of its originally-planned slot — see §5.

## 2. Tag scheme

Go's module tag-discovery rule:
- A module whose `go.mod` lives at the **repo root** uses BARE
  `vX.Y.Z` tags (no path prefix).
- A module whose `go.mod` lives at a **subpath** uses
  `<subpath-relative-to-repo-root>/vX.Y.Z` tags.

In this repo:

- **Root module** (`github.com/maqian/oss-client`, `go.mod` at repo root):
  `vX.Y.Z` — example: `v0.1.0`. The `pkg/uos` directory is a sub-package
  of the root module, **not** a separate Go module. A vanity
  `pkg/uos/vX.Y.Z` tag may be created at the same commit for human-
  readability (CHANGELOG entries reference it as a label), but Go's
  module proxy ONLY resolves the bare `vX.Y.Z` tag for the root.
- **Contract testkit module** (`go.mod` at `pkg/testkit/contract/`):
  `pkg/testkit/contract/vX.Y.Z` — example: `pkg/testkit/contract/v0.1.0`.
- **Provider module** (`go.mod` at `providers/<name>/`):
  `providers/<name>/vX.Y.Z` — example: `providers/aws/v0.1.0`.

Each module follows [SemVer 2.0.0](https://semver.org/spec/v2.0.0.html)
independently. There is no umbrella repo-wide version; `github.com/maqian/oss-client`,
`pkg/testkit/contract`, and each provider drift on their own cadence.

> **Lessons-learned (v0.1.0 tag pass)**: the original §4 release
> commands tagged the root module as `pkg/uos/v0.1.0`, which Go's
> module proxy does NOT consult for `github.com/maqian/oss-client@v0.1.0`
> resolution (it expects a bare `v0.1.0` tag at the repo root). The
> bare `v0.1.0` tag was added retroactively at the same commit; both
> tags now point at the same release and `pkg/uos/v0.1.0` is retained
> as a vanity label. For v0.2.0+, follow the corrected scheme above:
> tag the root with the bare `vX.Y.Z` form FIRST; the path-prefixed
> vanity tag is optional and exists purely for CHANGELOG / README
> cross-reference convenience.

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
  `github.com/maqian/oss-client` is correct (this is
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
# Tag root first so providers can pin a real parent version.
# CANONICAL form is the bare semver — Go's module proxy resolves
# `github.com/maqian/oss-client@v0.1.0` ONLY against the bare tag.
git tag v0.1.0
git push origin v0.1.0

# (Optional) Vanity tag at the same commit for human-readability +
# CHANGELOG cross-reference. Downstream Go tooling does NOT need this;
# only the bare `v0.1.0` above is functional. Skip if you don't want
# the duplicate label.
git tag pkg/uos/v0.1.0
git push origin pkg/uos/v0.1.0

# Tag testkit module (its replace directive must be removed and its
# require bumped to pkg/uos/v0.1.0 in the same commit before tagging)
git tag pkg/testkit/contract/v0.1.0
git push origin pkg/testkit/contract/v0.1.0

# Tag providers — mixed v0.1.0 / v0.1.1 strategy.
#
# After M5 ship, a v0.1.1 patch landed (commit d936e1c) addressing 3
# architect-flagged correctness items: (1) Azure multipart Initiate.Metadata
# persistence, (2) cross-driver errors.As(&alreadyMapped) context augmentation
# (touched 9 of 10 drivers), (3) Qiniu Download Mode=Token → Mode=URL.
#
# providers/aws was the ONLY driver untouched by the v0.1.1 patch (its
# mapError uses a different shape with no errors.As pre-check, and it has
# no multipart Initiate.Metadata path of its own). It tags at v0.1.0.
# The other 9 providers carry the v0.1.1 patch and tag at v0.1.1.
#
# All replace directives have been removed and requires bumped to v0.1.0
# in the release-prep commit (atomic — first tag pass, no incremental
# per-tag commits needed). For subsequent releases (v0.2.0+), the per-tag
# replace-cleanup-commit protocol resumes (each provider tag its own commit).

git tag providers/aws/v0.1.0
git push origin providers/aws/v0.1.0

git tag providers/minio/v0.1.1
git push origin providers/minio/v0.1.1

# M3 provider modules (alibaba shipped first as the s3common validation
# case; tencent/huawei/volcengine landed in parallel afterwards).
git tag providers/alibaba/v0.1.1
git push origin providers/alibaba/v0.1.1

git tag providers/tencent/v0.1.1
git push origin providers/tencent/v0.1.1

git tag providers/huawei/v0.1.1
git push origin providers/huawei/v0.1.1

git tag providers/volcengine/v0.1.1
git push origin providers/volcengine/v0.1.1

# M4 provider modules (the non-HMAC milestone — gcs uses
# OAuth2/Service Account/ADC, azure uses SharedKey/SAS/Entra).
git tag providers/gcs/v0.1.1
git push origin providers/gcs/v0.1.1

git tag providers/azure/v0.1.1
git push origin providers/azure/v0.1.1

# M5 provider modules (the DirectGrant non-URL milestone — qiniu uses
# Upload Token (Mode=Token), upyun uses FORM authorization (Mode=Form).
git tag providers/qiniu/v0.1.1
git push origin providers/qiniu/v0.1.1

git tag providers/upyun/v0.1.1
git push origin providers/upyun/v0.1.1
```

After tagging, verify both tags are fetchable:

```bash
go list -m github.com/maqian/oss-client@v0.1.0
go list -m github.com/maqian/oss-client/pkg/testkit/contract@v0.1.0
```

(This validates that Go module proxy can serve the tagged versions.)

### Post-tag

- [ ] Open `[Unreleased]` section in CHANGELOG.md for ongoing
  v0.2.0 work.
- [ ] In `pkg/testkit/contract/go.mod`, replace
  `github.com/maqian/oss-client v0.0.0` with the
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
