#!/usr/bin/env bash
set -euo pipefail
root="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)"
version="$(awk '$1 == "go" { print $2; exit }' "$root/backend/go.mod")"
export GOROOT="${SUB2API_GOROOT:-$HOME/.local/share/go/$version}"
export GOTOOLCHAIN=local
export GOCACHE="${GOCACHE:-$HOME/Library/Caches/sub2api-go-build}"
if [[ ! -x "$GOROOT/bin/go" || ! -f "$GOROOT/src/runtime/runtime.go" || ! -f "$GOROOT/src/bytes/bytes.go" ]]; then
  printf 'Incomplete Go toolchain: %s; install verified Go %s in this persistent directory.\n' "$GOROOT" "$version" >&2
  exit 1
fi
exec "$GOROOT/bin/go" "$@"
