#!/usr/bin/env bash
# Runs the plugin-sdk conformance runner on plugin-json.
#   scripts/conformance.sh stdio   the runner starts the plugin over stdio
#   scripts/conformance.sh http    the plugin serves HTTP on 127.0.0.1:7070
# The runner is built from the plugin-sdk commit in go.mod, with the schemas
# of the same commit. Set SDK_DIR to use a plugin-sdk checkout that you have.
set -euo pipefail
mode=${1:-stdio}
root=$(cd "$(dirname "$0")/.." && pwd)
work=$(mktemp -d)
trap 'kill "${plugin_pid:-}" 2>/dev/null || true; rm -rf "$work"' EXIT

sdk=${SDK_DIR:-}
if [ -z "$sdk" ]; then
  ref=$("$root/scripts/sdk-ref.sh")
  sdk="$work/plugin-sdk"
  git clone --quiet https://github.com/spec-studio-app/plugin-sdk "$sdk"
  git -C "$sdk" checkout --quiet "$ref"
fi
(cd "$sdk/conformance" && go build -o "$work/plugin-conformance" ./cmd/plugin-conformance)
(cd "$root" && go build -o "$work/plugin-json" .)

data="$work/data"
case "$mode" in
  stdio)
    "$work/plugin-conformance" --schemas "$sdk/schema" --setting "data_dir=$data" -- "$work/plugin-json"
    ;;
  http)
    "$work/plugin-json" --listen 127.0.0.1:7070 2>"$work/plugin.log" &
    plugin_pid=$!
    for _ in $(seq 1 50); do
      if curl -s -o /dev/null http://127.0.0.1:7070/rpc 2>/dev/null; then break; fi
      sleep 0.1
    done
    "$work/plugin-conformance" --schemas "$sdk/schema" --setting "data_dir=$data" --url http://127.0.0.1:7070/rpc
    ;;
  *)
    echo "usage: scripts/conformance.sh stdio|http" >&2
    exit 2
    ;;
esac
