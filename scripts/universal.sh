#!/usr/bin/env bash
# Joins the two macOS binaries of a version into one universal binary and
# its archive. Run it on macOS (it needs lipo), after scripts/build.sh for
# darwin arm64 and darwin amd64:
#   scripts/universal.sh <version> [<dist folder>]
# reads <dist>/plugin-json_<version>_darwin_arm64.tar.gz and
# <dist>/plugin-json_<version>_darwin_amd64.tar.gz, and gives
# <dist>/plugin-json-<version>-darwin-universal.tar.gz with the folder
# plugin-json-<version>-darwin-universal/ in it, and plugin-json (arm64 and
# x86_64) and README.md in that folder, as in the other archives.
set -euo pipefail
version=$1
root=$(cd "$(dirname "$0")/.." && pwd)
dist=$(cd "${2:-$root/dist}" && pwd)
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
for arch in arm64 amd64; do
  tar -C "$work" -xzf "$dist/plugin-json_${version}_darwin_${arch}.tar.gz"
done
name="plugin-json-${version}-darwin-universal"
out="$dist/$name"
rm -rf "$out"
mkdir -p "$out"
lipo -create -output "$out/plugin-json" \
  "$work/plugin-json_${version}_darwin_arm64/plugin-json" \
  "$work/plugin-json_${version}_darwin_amd64/plugin-json"
lipo "$out/plugin-json" -verify_arch arm64 x86_64
cp "$work/plugin-json_${version}_darwin_arm64/README.md" "$out/"
tar -C "$dist" -czf "$dist/$name.tar.gz" "$name"
echo "$dist/$name.tar.gz"
