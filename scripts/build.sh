#!/usr/bin/env bash
# Builds one static binary and its archive:
#   scripts/build.sh <version> <goos> <goarch>
# gives dist/plugin-json_<version>_<goos>_<goarch>.tar.gz with plugin-json and
# README.md in it.
set -euo pipefail
version=$1 goos=$2 goarch=$3
root=$(cd "$(dirname "$0")/.." && pwd)
name="plugin-json_${version}_${goos}_${goarch}"
out="$root/dist/$name"
rm -rf "$out"
mkdir -p "$out"
(cd "$root" && CGO_ENABLED=0 GOOS="$goos" GOARCH="$goarch" \
  go build -trimpath -ldflags "-s -w -X main.version=${version}" -o "$out/plugin-json" .)
cp "$root/README.md" "$out/"
tar -C "$root/dist" -czf "$root/dist/$name.tar.gz" "$name"
echo "$root/dist/$name.tar.gz"
