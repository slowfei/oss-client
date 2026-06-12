#!/usr/bin/env bash
# scripts/release.sh — synchronized v0.2.0+ release tooling.
#
# Bumps every in-repo go.mod's in-repo require lines to the new version,
# updates the go.work workspace replace block to match, renames CHANGELOG
# [Unreleased] → [vX.Y.Z] and prepends a fresh [Unreleased] block,
# commits as a release-prep, then creates 12 git tags (1 bare root +
# 1 testkit + 10 providers) all at the same version.
#
# Per RELEASING.md §3 (synchronized-bump rule, introduced in v0.2.0):
# every release bumps ALL modules to the same vX.Y.Z. The bare `vX.Y.Z`
# tag is the canonical root reference; the path-prefixed tags exist
# because Go's module proxy requires them.
#
# Usage:
#   scripts/release.sh v0.2.0
#
# Optional env:
#   SKIP_TESTS=1   skip the pre-flight test run (use only in CI rerun)
#
# After this script finishes, the maintainer:
#   1. git push origin main
#   2. git push origin --tags
#   3. gh release create vX.Y.Z --target main --title "vX.Y.Z" \
#         --notes-file <(scripts/release-notes.sh vX.Y.Z)
#
# A SINGLE GitHub Release covers all 12 tags so the Releases page stays
# clean (1 entry per version), even though `git tag -l` lists 12 tags.

set -euo pipefail

VERSION="${1:-}"
if [[ -z "$VERSION" ]]; then
  echo "usage: $0 vX.Y.Z" >&2
  exit 2
fi
if [[ ! "$VERSION" =~ ^v[0-9]+\.[0-9]+\.[0-9]+$ ]]; then
  echo "version must match vX.Y.Z (got: $VERSION)" >&2
  exit 2
fi

cd "$(git rev-parse --show-toplevel)"

# Pre-flight: clean tree on main, tests + frozen surface green.
if [[ -n "$(git status --porcelain)" ]]; then
  echo "ERROR: working tree dirty; commit or stash first" >&2
  exit 1
fi
BRANCH="$(git symbolic-ref --short HEAD)"
if [[ "$BRANCH" != "main" ]]; then
  echo "ERROR: must be on main branch (currently: $BRANCH)" >&2
  exit 1
fi
if git rev-parse "$VERSION" >/dev/null 2>&1; then
  echo "ERROR: tag $VERSION already exists locally" >&2
  exit 1
fi

if [[ "${SKIP_TESTS:-}" != "1" ]]; then
  echo "==> running tests (set SKIP_TESTS=1 to skip)"
  go test -short -race -count=1 ./... >/dev/null \
    || { echo "ERROR: tests FAILED" >&2; exit 1; }
  go test ./pkg/uos -run TestFrozenSurface -count=1 >/dev/null \
    || { echo "ERROR: TestFrozenSurface FAILED" >&2; exit 1; }
fi

# Modules to TAG: root + testkit + 10 providers (12 total). These are
# the public modules whose tags ship to the proxy.
PROVIDERS=(alibaba aws azure gcs huawei minio qiniu tencent upyun volcengine)
TAGGED_DIRS=(. ./pkg/testkit/contract)
for p in "${PROVIDERS[@]}"; do
  TAGGED_DIRS+=("./providers/$p")
done

# Modules whose go.mod requires also need bumping but which are NOT
# tagged (workspace consumers — examples + benchmarks). They must stay
# in sync with TAGGED_DIRS so go.work workspace resolution doesn't fall
# through to proxy lookups for stale v0.1.x references.
UNTAGGED_DIRS=(
  ./examples/quickstart
  ./examples/multipart
  ./examples/direct_grant_qiniu
  ./examples/direct_grant_upyun
  ./examples/streaming_write
  ./benchmarks
)

# All go.mod dirs that need version bumps.
GOMOD_DIRS=("${TAGGED_DIRS[@]}" "${UNTAGGED_DIRS[@]}")

# Bump in-repo require lines via `go mod edit` (more robust than sed).
# Walks every potential in-repo dep: root module + testkit + each of the
# 10 providers. Examples + benchmarks may depend on any subset of the 10
# providers, so the loop must consider all of them.
echo "==> bumping go.mod require lines to $VERSION across ${#GOMOD_DIRS[@]} modules"
for d in "${GOMOD_DIRS[@]}"; do
  if [[ ! -f "$d/go.mod" ]]; then continue; fi
  if grep -q "^	github.com/slowfei/oss-client v" "$d/go.mod"; then
    (cd "$d" && go mod edit -require="github.com/slowfei/oss-client@$VERSION")
  fi
  if grep -q "^	github.com/slowfei/oss-client/pkg/testkit/contract v" "$d/go.mod"; then
    (cd "$d" && go mod edit -require="github.com/slowfei/oss-client/pkg/testkit/contract@$VERSION")
  fi
  for p in "${PROVIDERS[@]}"; do
    if grep -q "^	github.com/slowfei/oss-client/providers/$p v" "$d/go.mod"; then
      (cd "$d" && go mod edit -require="github.com/slowfei/oss-client/providers/$p@$VERSION")
    fi
  done
done

# Update go.work workspace replace block versions in place.
echo "==> updating go.work workspace replace versions"
sed -i '' -E "s|(github.com/slowfei/oss-client[^[:space:]]*) v[0-9]+\.[0-9]+\.[0-9]+|\1 $VERSION|g" go.work

# Rename CHANGELOG [Unreleased] → [VERSION] + prepend new [Unreleased].
echo "==> updating CHANGELOG.md"
awk -v ver="$VERSION" '
  /^## \[Unreleased\]/ && !seen {
    print "## [Unreleased]"
    print ""
    print "## [" ver "]"
    seen = 1
    next
  }
  { print }
' CHANGELOG.md > CHANGELOG.md.tmp && mv CHANGELOG.md.tmp CHANGELOG.md

# Verify build under workspace mode (the workspace replace block makes
# the not-yet-published-tag references resolve to local paths).
echo "==> verifying root build under workspace mode"
go build ./... >/dev/null \
  || { echo "ERROR: root build FAILED post-bump" >&2; exit 1; }

# Commit release-prep.
echo "==> creating release-prep commit"
git add -A
git commit -m "chore(release): $VERSION release-prep — synchronized bump

Bumps every in-repo go.mod require line to $VERSION and updates the
go.work workspace replace block to match. Per RELEASING.md §3 (the
synchronized-bump rule introduced in v0.2.0), every release moves ALL
12 modules to the same vX.Y.Z together so root, testkit, and the 10
provider modules share one version going forward.

CHANGELOG.md [Unreleased] section renamed to [$VERSION]; a fresh empty
[Unreleased] block prepended for the next cycle.

Tags created at this commit (12 total; pushed separately by maintainer):
- $VERSION (bare; root module github.com/slowfei/oss-client)
- pkg/testkit/contract/$VERSION
- providers/{alibaba,aws,azure,gcs,huawei,minio,qiniu,tencent,upyun,volcengine}/$VERSION

Generated by scripts/release.sh."

PREP_COMMIT=$(git rev-parse HEAD)

# Create 12 tags at the prep commit.
echo "==> creating 12 tags at $PREP_COMMIT"
git tag -a "$VERSION" -m "$VERSION — root module github.com/slowfei/oss-client.

Synchronized release; all 12 in-repo modules tagged at $VERSION at
commit $PREP_COMMIT. The bare root tag is the canonical reference for
go module proxy resolution; the GitHub Release page collects all 12
tags into a single Release object titled \"$VERSION\"."

git tag -a "pkg/testkit/contract/$VERSION" \
  -m "pkg/testkit/contract $VERSION — same commit as bare $VERSION (synchronized)."

for p in "${PROVIDERS[@]}"; do
  git tag -a "providers/$p/$VERSION" \
    -m "providers/$p $VERSION — same commit as bare $VERSION (synchronized)."
done

# Done.
cat <<EOF

==> done. 12 tags created locally at commit $PREP_COMMIT.

Next steps (maintainer action):

1. Push commit + tags to origin:
     git push origin main
     git push origin --tags

2. Create ONE GitHub Release object that covers all 12 module tags:
     gh release create $VERSION \\
       --target main \\
       --title "$VERSION" \\
       --notes-file <(scripts/release-notes.sh $VERSION)

   This produces a single entry on the GitHub Releases page (per
   RELEASING.md §6 GitHub-Release-consolidation pattern). The 12
   underlying tags exist in git for go module proxy resolution but
   only the bare $VERSION tag has a GitHub Release object attached.

3. Verify proxy resolves the new tags (after push):
     go list -m github.com/slowfei/oss-client@$VERSION
     go list -m github.com/slowfei/oss-client/providers/aws@$VERSION

To roll back BEFORE pushing:
     git tag -d $VERSION pkg/testkit/contract/$VERSION \\
$(printf '       providers/%s/'"$VERSION"' \\\n' "${PROVIDERS[@]}" | sed '$ s/ \\$//')
     git reset --hard HEAD~1
EOF
