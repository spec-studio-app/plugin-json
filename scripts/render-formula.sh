#!/usr/bin/env bash
# Writes the Homebrew formula for a release to stdout:
#   scripts/render-formula.sh <version> <checksums.txt>
# checksums.txt has one "<sha256>  plugin-json_<version>_<os>_<arch>.tar.gz"
# line for each archive, as sha256sum writes it.
set -euo pipefail
version=$1 sums=$2
root=$(cd "$(dirname "$0")/.." && pwd)
sum() {
  local s
  s=$(awk -v f="plugin-json_${version}_$1.tar.gz" '$2 == f { print $1 }' "$sums")
  if [ -z "$s" ]; then
    echo "render-formula: no checksum for plugin-json_${version}_$1.tar.gz in $sums" >&2
    exit 1
  fi
  echo "$s"
}
darwin_arm64=$(sum darwin_arm64)
darwin_amd64=$(sum darwin_amd64)
linux_arm64=$(sum linux_arm64)
linux_amd64=$(sum linux_amd64)
sed -e "s/{{VERSION}}/${version}/g" \
  -e "s/{{SHA_DARWIN_ARM64}}/${darwin_arm64}/" \
  -e "s/{{SHA_DARWIN_AMD64}}/${darwin_amd64}/" \
  -e "s/{{SHA_LINUX_ARM64}}/${linux_arm64}/" \
  -e "s/{{SHA_LINUX_AMD64}}/${linux_amd64}/" \
  "$root/packaging/plugin-json.rb.tmpl"
