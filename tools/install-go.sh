#!/usr/bin/env bash
set -euo pipefail
repo="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)"
version="$(awk '$1 == "go" { print $2; exit }' "$repo/backend/go.mod")"
os="$(uname -s | tr '[:upper:]' '[:lower:]')"
case "$(uname -m)" in
  arm64|aarch64) arch=arm64 ;;
  x86_64) arch=amd64 ;;
  *) printf 'Unsupported architecture\n' >&2; exit 1 ;;
esac
target="$HOME/.local/share/go/$version"
if [[ -e "$target" ]]; then
  SUB2API_GOROOT="$target" "$repo/tools/go.sh" version
  SUB2API_GOROOT="$target" "$repo/tools/go.sh" test bytes archive/tar
  exit
fi
mkdir -p "$HOME/.local/share/go"
stage="$(mktemp -d "$HOME/.local/share/go/.install.XXXXXXXX")"
trap 'rm -rf -- "$stage"' EXIT
file="go$version.$os-$arch.tar.gz"
curl -fsSL 'https://go.dev/dl/?mode=json&include=all' -o "$stage/releases.json"
expected="$(jq -r --arg file "$file" '.[] | .files[] | select(.filename == $file) | .sha256' "$stage/releases.json")"
[[ "$expected" =~ ^[0-9a-f]{64}$ ]] || { printf 'Official checksum unavailable\n' >&2; exit 1; }
curl -fL "https://go.dev/dl/$file" -o "$stage/$file"
actual="$(shasum -a 256 "$stage/$file" | awk '{print $1}')"
[[ "$actual" == "$expected" ]] || { printf 'Go archive checksum mismatch\n' >&2; exit 1; }
tar -xzf "$stage/$file" -C "$stage"
SUB2API_GOROOT="$stage/go" "$repo/tools/go.sh" test bytes archive/tar
mv "$stage/go" "$target"
SUB2API_GOROOT="$target" "$repo/tools/go.sh" version
printf 'Verified archive SHA256: %s\nInstalled: %s\n' "$actual" "$target"
