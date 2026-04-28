# Releasing

This repository is a Go multi-module workspace. Each module is
versioned and tagged independently. This document defines:

1. The module map.
2. The tag scheme.
3. The semver-bump rules per module type.
4. The release checklist for `pkg/uos/v0.1.0` (M1).
5. How the ADR Follow-ups list interacts with releases.

## 1. Module map

| Module path                                   | Tag prefix               | Status (v0.1) | Notes |
| --------------------------------------------- | ------------------------ | ------------- | ----- |
| `github.com/maqian/object-storage-client`     | `pkg/uos/`               | ACTIVE        | Root module. Houses `pkg/uos`, `pkg/uos/{capability,credential,transfer,middleware,httpx}`, and `pkg/testkit/contract`. |
| `github.com/maqian/object-storage-client/providers/<name>` | `providers/<name>/` | EMPTY         | One module per provider, scaffolded by `scripts/add-provider.sh`. First provider lands in M2 (`providers/aws`, `providers/minio`). |

`pkg/testkit/contract` lives inside the root module in v0.1, so it
shares the `pkg/uos` tag. ADR Follow-up #3 (see §5) tracks the
decision to hoist it into its own module at v0.2.0.

## 2. Tag scheme

Go module tooling requires that each module's tag be prefixed with
the module's directory path relative to the repo root. We use:

- Root module (`pkg/uos`): `pkg/uos/vX.Y.Z`
  - Example: `pkg/uos/v0.1.0`
- Provider module (`providers/<name>`): `providers/<name>/vX.Y.Z`
  - Example: `providers/aws/v0.1.0`

Each module follows [SemVer 2.0.0](https://semver.org/spec/v2.0.0.html)
independently. There is no umbrella repo-wide version; `pkg/uos` and
each provider drift on their own cadence.

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

## 4. Release checklist — `pkg/uos/v0.1.0`

Run through this list in order. The `git tag` and `git push` at the
bottom are **maintainer actions**, gated on user approval; the
release executor (or this checklist) does not run them.

### Pre-flight

- [ ] CHANGELOG.md `[pkg/uos/v0.1.0]` entry merged onto `main`.
- [ ] All four CI jobs green on the v0.1 PR:
  - `unit` (matrix: `ubuntu-latest` / `macos-latest` × Go `1.25`).
  - `vet-fmt`.
  - `unit-docker` (`-tags=docker` contract suite).
  - `surface` (the freezing tripwire — `TestFrozenSurface`).
- [ ] Local `go test -short -race ./...` is green.
- [ ] Local `go test ./pkg/uos -run TestFrozenSurface -count=1 -v`
  is green; all three subtests (`codes_frozen_14`,
  `capabilities_frozen_13`, `direct_grant_modes_frozen_4`) pass.
- [ ] `gofmt -l .` prints nothing; `go vet ./...` is clean.
- [ ] A maintainer has confirmed the canonical module path
  `github.com/maqian/object-storage-client` is correct (this is
  also baked into all the example imports in `pkg/uos/doc.go`).

### Tag (maintainer action — DO NOT auto-execute)

The tag command is documented here so the executor leaves a clear
breadcrumb; a human must run it after the pre-flight checklist is
green and approval is given:

```bash
git tag pkg/uos/v0.1.0
git push origin pkg/uos/v0.1.0
```

After tagging, verify the tag is fetchable:

```bash
go list -m github.com/maqian/object-storage-client@v0.1.0
```

(This validates that Go module proxy can serve the tagged version.)

### Post-tag

- [ ] Open `[Unreleased]` section in CHANGELOG.md for ongoing
  v0.2.0 work.
- [ ] Bump the AGENTS.md Appendix A status table if any items
  graduated from "deferred" to "released."

## 5. ADR Follow-ups (informational)

The full Follow-ups list is in `.omc/plans/v0.1-implementation-plan.md`
under the `ADR` section's `Follow-ups (post-v0.1, ranked by
importance)` heading (items 1-11). This file does not duplicate it.

One follow-up has been re-prioritised since the plan was approved:

- **Follow-up #3 — `pkg/testkit/contract` module hoist**: planned
  for "M6, conditional on testkit evolving faster than pkg/uos."
  **Promoted to v0.2.0 mandatory.** Reason: the M1 implementation
  pulled `testcontainers-go` into the root `go.mod`, dragging Go
  1.25 in as a transitive requirement and inflating root
  `go.sum`. Hoisting `pkg/testkit/contract` into its own module
  removes that cost from consumers who only want `pkg/uos`. The
  v0.2.0 release will perform the hoist and tag both
  `pkg/uos/v0.2.0` and `pkg/testkit/contract/v0.1.0` together.

All other Follow-ups remain at the priority captured in the ADR.
