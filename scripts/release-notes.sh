#!/usr/bin/env bash
# scripts/release-notes.sh — emit GitHub Release body markdown for vX.Y.Z.
#
# Designed to be piped into `gh release create --notes-file`. Includes:
#   - one-line release headline
#   - module table (12 rows, with go get one-liners)
#   - link to CHANGELOG.md at the release ref
#   - explanation of "why one Release for 12 tags"
#
# Usage:
#   scripts/release-notes.sh v0.2.0 > /tmp/notes.md
#   gh release create v0.2.0 --notes-file /tmp/notes.md ...

set -euo pipefail

VERSION="${1:-}"
if [[ -z "$VERSION" ]]; then
  echo "usage: $0 vX.Y.Z" >&2
  exit 2
fi

cat <<EOF
# $VERSION — synchronized release

All 12 in-repo modules tagged at \`$VERSION\` at the same commit.

## Modules

| Module path | Install |
|---|---|
| \`github.com/slowfei/oss-client\` (root, pkg/uos) | \`go get github.com/slowfei/oss-client@$VERSION\` |
| \`.../pkg/testkit/contract\` | \`go get github.com/slowfei/oss-client/pkg/testkit/contract@$VERSION\` |
| \`.../providers/aws\` | \`go get github.com/slowfei/oss-client/providers/aws@$VERSION\` |
| \`.../providers/minio\` | \`go get github.com/slowfei/oss-client/providers/minio@$VERSION\` |
| \`.../providers/alibaba\` | \`go get github.com/slowfei/oss-client/providers/alibaba@$VERSION\` |
| \`.../providers/tencent\` | \`go get github.com/slowfei/oss-client/providers/tencent@$VERSION\` |
| \`.../providers/huawei\` | \`go get github.com/slowfei/oss-client/providers/huawei@$VERSION\` |
| \`.../providers/volcengine\` | \`go get github.com/slowfei/oss-client/providers/volcengine@$VERSION\` |
| \`.../providers/gcs\` | \`go get github.com/slowfei/oss-client/providers/gcs@$VERSION\` |
| \`.../providers/azure\` | \`go get github.com/slowfei/oss-client/providers/azure@$VERSION\` |
| \`.../providers/qiniu\` | \`go get github.com/slowfei/oss-client/providers/qiniu@$VERSION\` |
| \`.../providers/upyun\` | \`go get github.com/slowfei/oss-client/providers/upyun@$VERSION\` |

Each provider module imports only its own vendor SDK + \`pkg/uos\`.
Pulling \`providers/aws\` does **not** drag Azure / GCS / qiniu / etc
into your transitive dep chain. The root \`pkg/uos\` ships
**stdlib-only**.

## Changes

See [\`CHANGELOG.md\`](https://github.com/slowfei/oss-client/blob/$VERSION/CHANGELOG.md)
for the full per-version changelog (look for the \`[$VERSION]\` section).

## Why one Release for 12 tags

Each Go module has its own tag namespace. The Go module proxy resolves
\`<module-path>@<version>\` against tags named \`<subpath>/<version>\`,
so this repo MUST publish 12 git tags per release (one per module).

This repo synchronises every release: all 12 modules bump together to
the same \`vX.Y.Z\`. The GitHub Releases page collects them under a
single Release object (this one) so the visual cadence stays clean —
even though \`git tag -l\` shows 12 tags per release.

See [\`RELEASING.md\`](https://github.com/slowfei/oss-client/blob/$VERSION/RELEASING.md)
§3 (synchronized-bump rule) and §6 (GitHub-Release-consolidation
pattern) for the binding policy.
EOF
