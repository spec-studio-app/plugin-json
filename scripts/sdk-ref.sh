#!/usr/bin/env bash
# Prints the git ref of plugin-sdk that go.mod uses: the commit of a
# pseudo-version, or the tag go/vX.Y.Z of a release.
set -euo pipefail
cd "$(dirname "$0")/.."
version=$(go list -m -f '{{.Version}}' github.com/spec-studio-app/plugin-sdk/go)
if [[ "$version" =~ -([0-9a-f]{12})$ ]]; then
  echo "${BASH_REMATCH[1]}"
else
  echo "go/${version}"
fi
