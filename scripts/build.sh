#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT_DIR"

VERSION="$(cat VERSION | tr -d '\r\n')"
COMMIT="$(git rev-parse --short HEAD 2>/dev/null | tr -d '\r\n' || true)"
if [[ -z "${COMMIT}" ]]; then
  COMMIT="unknown"
elif ! git diff --quiet 2>/dev/null; then
  COMMIT="${COMMIT}-dirty"
fi
BINARY="sever-provider"
if [[ "${OS:-}" == "Windows_NT" ]] || [[ "$(uname -s 2>/dev/null)" =~ MINGW|MSYS|CYGWIN ]]; then
  BINARY="sever-provider.exe"
fi

go build -ldflags "-X sever_provider/internal/buildinfo.Version=${VERSION} -X sever_provider/internal/buildinfo.Commit=${COMMIT}" -o "${BINARY}" ./cmd/server
