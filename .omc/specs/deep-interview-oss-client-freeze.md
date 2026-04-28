# Deep Interview Spec: object-storage-client v1 Scope Freeze

## Metadata
- **Interview ID**: oss-client-freeze-2026-04-27
- **Rounds**: 5
- **Final Ambiguity Score**: 3.3% (threshold 20%)
- **Type**: brownfield (light — repo has docs + AGENTS.md but no Go code)
- **Generated**: 2026-04-27
- **Threshold**: 0.2
- **Status**: PASSED

## Clarity Breakdown (final)

| Dimension | Score | Weight | Weighted |
| --- | --- | --- | --- |
| Goal Clarity | 0.97 | 0.35 | 0.3395 |
| Constraint Clarity | 0.97 | 0.25 | 0.2425 |
| Success Criteria | 0.97 | 0.25 | 0.2425 |
| Context Clarity | 0.95 | 0.15 | 0.1425 |
| **Total Clarity** | | | **0.967** |
| **Ambiguity** | | | **0.033** |

## Goal

Freeze the v1 scope of a Go universal object storage **client SDK only** (no proxy / gateway / SaaS) into three binding documents (`docs/architecture_plan.md`, `docs/provider_roadmap.md`, `docs/provider_matrix.md`) that cover the user's seven explicit freeze items: core abstractions, non-goals, package boundaries, milestone plan, provider rollout order, contract test strategy, and error/capability model boundaries. No Go implementation code is added in this pass.

## Constraints (binding)

- **Repo scope**: SDK / library only; never a proxy server, gateway, control plane, or credential-hosting service. (Hard rule from `AGENTS.md`.)
- **Doc canon**: the 3 freeze docs supersede `docs/prd.md` on any conflict (Round 1).
- **Module layout**: multi-module Go repo; root `go.mod` for `pkg/uos`, per-provider `providers/<name>/go.mod` (Round 2).
- **Test helpers**: live at `pkg/testkit/contract` (public path), not `internal/testkit`; multi-module forbids cross-module `internal/` import.
- **Provider rollout**: PRD's order is locked — M2 AWS+MinIO → M3 Alibaba+Tencent+Huawei+Volcengine → M4 GCS+Azure → M5 Qiniu+Upyun → M6 stabilize (Round 3).
- **Test gate**: PR gate = MinIO via `testcontainers-go`; cloud nightly = opt-in via repo secret (SKIP without secret, never FAIL) (Round 4).
- **Public API surface**: `Code` enum frozen at 14 entries; `Capability` enum frozen at 13 entries (Round 5). Vendor-specific richness escapes via `Error.Cause` / `Error.SecondaryID` and via `Capability=ExtensionOnly` + `Client.As(target)`.
- **Go version target**: 1.22+ (carried over from PRD; not re-litigated this round).

## Non-Goals

- Proxy / gateway / SaaS / credential-hosting service.
- CDN domain management, accelerated-domain signing, refresh / preheat.
- Image / video / audio processing, content moderation, transcoding.
- Bucket lifecycle, cross-region replication, object lock, inventory, events, website hosting.
- Cross-provider server-side copy guarantees.
- A first-class `Directory` abstraction (only `List(prefix, delimiter)`).
- `Rename` / `Move` core primitives (default helper = `Copy + Delete`).
- Object key normalization (no `path.Clean`, no leading-slash trim, no UTF-8 transcoding).

## Acceptance Criteria

- [ ] `docs/architecture_plan.md` exists and explicitly addresses the seven user-listed freeze items.
- [ ] `docs/provider_roadmap.md` exists with M2-M6 per-provider scope, validation focus, and per-milestone exit checklist.
- [ ] `docs/provider_matrix.md` exists with the provider × capability matrix, footnotes, and an auth-scheme table.
- [ ] All three docs declare themselves binding and supersede `docs/prd.md` on conflict.
- [ ] `architecture_plan.md` Appendix A lists deferred follow-ups (AGENTS.md alignment, `go mod init`, `go.work` bootstrap).
- [ ] No Go source files are added by this freeze pass.
- [ ] `.omc/specs/deep-interview-oss-client-freeze.md` (this file) preserves the 5-round transcript and decisions.

## Assumptions Exposed & Resolved

| Assumption | Challenge | Resolution |
| --- | --- | --- |
| "PRD is the spec; freeze docs are summaries." | Round 1: are freeze docs binding or advisory? | Binding; freeze docs supersede PRD on conflict. |
| "One Go module is fine; `internal/testkit` works." | Round 2: NFR-008 (`core 不直接依赖所有 driver`) is in tension with single-module — every consumer would pull all 10 SDKs into `go.sum`. | Multi-module; `internal/testkit` becomes `pkg/testkit/contract`. |
| "Provider order is whatever the team feels like." | Round 3: follow PRD or reorder for business priority? | Lock PRD order; the abstraction-validation logic (S3 → HMAC → 异构 → 特殊) is the right pedagogy. |
| "Contract tests just need MinIO somewhere." | Round 4: who runs MinIO; what gates a PR; cloud creds? | `testcontainers-go` for PR gate; cloud nightly is opt-in per-provider secret. |
| "Drivers can grow new error codes / capabilities as needed." | Round 5: are the 14 codes / 13 caps frozen? | Both frozen for v1; vendor richness escapes via `Cause` / `SecondaryID` (errors) and `ExtensionOnly` + `As(target)` (capabilities). |

## Technical Context (brownfield observations)

- Repo state: `docs/prd.md` (~750 lines, very detailed), `AGENTS.md` (engineering rules + hard "no proxy" boundary), `providers/AGENTS.md` (provider rules). No Go code, no `go.mod`. Single bootstrap commit.
- Detected conflicts (all resolved in favor of freeze docs):
  - `AGENTS.md` line 16 references `docs/requirements_and_design.md` which does not exist (actual file is `docs/prd.md`). Logged as Appendix A.1 cleanup.
  - `AGENTS.md` says `internal/testkit`; PRD §4.1 says `tests/contract`; multi-module forces `pkg/testkit/contract` (public). Logged.
  - PRD §4.1 mentions `/internal/httpx`; multi-module forces `pkg/uos/httpx` (public). Logged.

## Ontology (Key Entities)

| Entity | Type | Fields | Relationships |
| --- | --- | --- | --- |
| Client | core domain | Provider, Capabilities, Buckets, Objects, Multipart, Signer, As, Close | composes BucketService / ObjectService / MultipartService / Signer; produced by Factory.Open |
| Bucket | core domain | Name, Region, CreatedAt, StorageClass, Extra | owned by Client; lists Objects |
| Object | core domain | Key, Size, ETag, Checksums, Metadata, LastModified, VersionID, StorageClass | belongs to Bucket; identified by opaque key |
| Provider/Driver | supporting | Provider (string id), Factory, Registry, DriverConfig | one per vendor; registered with Registry |
| Capability | supporting | Capability enum (13), Availability, Status, Report | declared by each driver; queried by Client |
| Error | supporting | Provider, Operation, Bucket, Key, Code (14), Message, HTTPStatus, RequestID, SecondaryID, Retryable, Capability, Cause | returned by every operation; supports errors.Is / errors.As |
| Signer | supporting | SignURL, IssueDirectGrant | per-bucket service on Client |
| DirectGrant | supporting | Mode, URL, Method, Headers, FormFields, Token, ExpiresAt | unified grant for SAS / Upload Token / FORM |
| TransferManager | supporting | Config, Manager, StateStore, UnknownSizePolicy | orchestrates multipart / resume; lives in `pkg/uos/transfer` |
| CredentialProvider | supporting | Provider (interface), Credential, AuthScheme | pluggable chain; per-driver consumption |
| Registry/Factory | supporting | Register, Open | dispatch from `Config.Provider` to driver |

## Ontology Convergence

| Round | Entity Count | New | Changed | Stable | Stability Ratio |
| --- | --- | --- | --- | --- | --- |
| 1 | 11 | 11 | — | — | N/A |
| 2 | 11 | 0 | 0 | 11 | 100% |
| 3 | 11 | 0 | 0 | 11 | 100% |
| 4 | 11 | 0 | 0 | 11 | 100% |
| 5 | 11 | 0 | 0 | 11 | 100% |

The ontology converged at Round 1 and held for the remaining 4 rounds — the PRD already established a stable noun set; the interview only resolved the *meta* questions (binding role, module layout, rollout, tests, frozen surface).

## Interview Transcript

### Round 1 — Doc canonicity
- **Targeting**: Success Criteria (0.55) — exit criteria for the freeze itself were undefined.
- **Q**: What role should the 3 freeze docs play relative to `docs/prd.md`?
- **A**: Binding; supersede PRD on conflict.
- **Resulting ambiguity**: 28% → 20%.

### Round 2 — Go module strategy
- **Targeting**: Constraint Clarity (0.65) — NFR-008 in tension with AGENTS.md's `internal/testkit` if single-module.
- **Q**: How should the Go module(s) be structured to satisfy NFR-008 while keeping shared test helpers reachable from each `providers/<name>` package?
- **A**: Multi-module: root + per-provider go.mod (recommended option).
- **Resulting ambiguity**: 20% → 12%. Cascading effects: `internal/testkit` → `pkg/testkit/contract`; `internal/httpx` → `pkg/uos/httpx`; `go.work` joins the layout; release tagging becomes per-module.

### Round 3 — Provider rollout order
- **Targeting**: Goal/Success Criteria — explicit user freeze item #5.
- **Q**: Follow PRD's M2-M5 order or reorder for a different business priority?
- **A**: Follow PRD's order (S3 family → HMAC国云 → 异构 → 特殊授权).
- **Resulting ambiguity**: 12% → 10%.

### Round 4 — Contract test strategy
- **Targeting**: Success Criteria — M2 exit criterion was unwritable without test-runtime answer.
- **Q**: How are contract tests run? PR gate vs nightly vs cloud creds?
- **A**: MinIO via `testcontainers-go` for the PR gate; cloud nightly is opt-in per provider secret (SKIP without secret, never FAIL).
- **Resulting ambiguity**: 10% → 6%.

### Round 5 — Error / Capability boundary
- **Targeting**: Constraint Clarity — public API stability for `errors.Is` and capability gating.
- **Q1**: Are PRD's 14 error codes frozen for v1 or extensible per driver?
- **A1**: Frozen; drivers map only (vendor richness via `Cause` / `Message` / `SecondaryID`).
- **Q2**: Are PRD's 13 capabilities frozen for v1 or extensible per driver?
- **A2**: Frozen + the `ExtensionOnly` `Availability` slot for vendor-specific abilities (escape via `As(target)`).
- **Resulting ambiguity**: 6% → 3.3%. Crystallized.

## Output Artifacts

- `docs/architecture_plan.md` — binding architecture spec.
- `docs/provider_roadmap.md` — per-milestone rollout.
- `docs/provider_matrix.md` — capability × provider matrix.

## Recommended next step

The three freeze docs are the binding contract for M1. Before starting M1 implementation:

1. Realign `AGENTS.md` per `architecture_plan.md` Appendix A.1 (mechanical edit; corrects doc path and `internal/testkit` → `pkg/testkit/contract`).
2. Run `go mod init` for the root module with the canonical owner path.
3. Bootstrap `go.work`.

M1's first implementation commit should be the `pkg/uos` interface skeleton plus the empty `pkg/testkit/contract` suite. No provider lands until M2.
