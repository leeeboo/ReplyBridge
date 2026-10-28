#!/bin/sh
# Run checks before packaging; archives contain only the explicit public file list.
set -eu
root=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
cd "$root"
version=$(sed -n 's/^[[:space:]]*version[[:space:]]*= "\([^"]*\)"/\1/p' cmd/shadowbrain-comment-bridge/main.go)
test -n "$version"
out="dist/$version"
if [ -e "$out" ]; then
  printf '%s\n' "$out already exists; preserve it and choose a new version or move it aside" >&2
  exit 1
fi
mkdir -p "$out"
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
trap 'exit 1' HUP INT TERM
for platform in linux/amd64 linux/arm64 darwin/amd64 darwin/arm64; do
  os=${platform%/*}
  arch=${platform#*/}
  name="replybridge_${version}_${os}_${arch}"
  dir="$work/$name"
  mkdir -p "$dir/deploy/comment-bridge" "$dir/docs"
  CGO_ENABLED=0 GOOS="$os" GOARCH="$arch" go build -trimpath -ldflags='-s -w' -o "$dir/shadowbrain-comment-bridge" ./cmd/shadowbrain-comment-bridge
  cp LICENSE THIRD_PARTY_NOTICES.md README.md CHANGELOG.md .env.example "$dir/"
  cp docs/comment-bridge.md docs/quick-start.md "$dir/docs/"
  cp deploy/comment-bridge/README.md deploy/comment-bridge/compose*.yaml deploy/comment-bridge/compose.env.example "$dir/deploy/comment-bridge/"
  COPYFILE_DISABLE=1 tar -czf "$out/$name.tar.gz" -C "$work" "$name"
done
(cd "$out" && shasum -a 256 ./*.tar.gz > SHA256SUMS)
printf '%s\n' "Packages and SHA256SUMS: $out"
