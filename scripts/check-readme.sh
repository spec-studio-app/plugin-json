#!/usr/bin/env bash
# Checks that the README has its sections and the link to plugin-sdk, and
# that the repo has no license file (no license yet).
set -euo pipefail
cd "$(dirname "$0")/.."
fail=0
for heading in "## What the plugin does" "## Settings" "## The data folder" "## The import of the relay's old store" "## Conformance" "## Release"; do
  if ! grep -qF "$heading" README.md; then
    echo "README.md has no section: $heading" >&2
    fail=1
  fi
done
if ! grep -q "github.com/spec-studio-app/plugin-sdk" README.md; then
  echo "README.md has no link to plugin-sdk" >&2
  fail=1
fi
if ls LICENSE* COPYING* >/dev/null 2>&1; then
  echo "the repo has a license file; it must have none yet" >&2
  fail=1
fi
exit $fail
