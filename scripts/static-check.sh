#!/usr/bin/env bash
# Runs the Linux binary in an image with no libc (FROM scratch) and checks
# that it answers plugin.describe over stdio:
#   scripts/static-check.sh <path to the linux binary>
set -euo pipefail
bin=$1
ctx=$(mktemp -d)
trap 'rm -rf "$ctx"' EXIT
cp "$bin" "$ctx/plugin-json"
printf 'FROM scratch\nCOPY plugin-json /plugin-json\nENTRYPOINT ["/plugin-json"]\n' > "$ctx/Dockerfile"
docker build -q -t plugin-json-static-check "$ctx" >/dev/null
out=$(printf '{"jsonrpc":"2.0","id":1,"method":"plugin.describe","params":{}}\n' | docker run --rm -i plugin-json-static-check)
echo "$out"
grep -q '"name":"json"' <<<"$out"
