#!/usr/bin/env bash
# add-provider.sh — scaffold a new provider module and register it in go.work.
#
# Usage: scripts/add-provider.sh <name>
#
# Creates providers/<name>/ with a minimal go.mod that depends on the root
# module via the workspace, then runs `go work use ./providers/<name>` so
# `go test ./...` from the repo root picks it up.

set -euo pipefail

if [[ $# -ne 1 ]]; then
  echo "usage: $0 <name>" >&2
  exit 2
fi

NAME="$1"
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
DIR="${ROOT}/providers/${NAME}"

if [[ -e "${DIR}" ]]; then
  echo "error: ${DIR} already exists" >&2
  exit 1
fi

ROOT_MODULE="$(awk '/^module /{print $2}' "${ROOT}/go.mod")"
if [[ -z "${ROOT_MODULE}" ]]; then
  echo "error: failed to parse module path from ${ROOT}/go.mod" >&2
  exit 1
fi

PROVIDER_MODULE="${ROOT_MODULE}/providers/${NAME}"

mkdir -p "${DIR}"
cat > "${DIR}/go.mod" <<EOF
module ${PROVIDER_MODULE}

go 1.25

require ${ROOT_MODULE} v0.0.0
EOF

cd "${ROOT}"
go work use "./providers/${NAME}"

echo "scaffolded ${DIR}"
echo "module path: ${PROVIDER_MODULE}"
echo "remember to add a factory.go that implements pkg/uos.Factory"
