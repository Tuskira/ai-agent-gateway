#!/usr/bin/env bash
#
# 13-claude-desktop/check.sh: static checks only -- no Docker, no network, no
# Mac. run.sh is the live gateway-side test; this is what to run when you only
# want to know the example's files are consistent:
#
#   - README.md, run.sh and docker-compose.ingest.yml are present
#   - the overlay turns ingest on and the README names the same setting
#   - every relative link in README.md resolves
#   - run.sh parses as bash and (when shellcheck is installed) is clean
#   - the README gives the API-plane gateway URL for a local gateway
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

fail() {
  echo "FAIL: $1" >&2
  exit 1
}

for f in README.md run.sh check.sh docker-compose.ingest.yml; do
  echo "check: $f exists"
  [[ -f "$SCRIPT_DIR/$f" ]] || fail "$SCRIPT_DIR/$f not found"
done

echo "check: the overlay sets GATEWAY_INGEST_ENABLED to true"
grep -q 'GATEWAY_INGEST_ENABLED: "true"' "$SCRIPT_DIR/docker-compose.ingest.yml" \
  || fail "docker-compose.ingest.yml does not set GATEWAY_INGEST_ENABLED"
grep -q 'GATEWAY_INGEST_ENABLED=true' "$SCRIPT_DIR/README.md" \
  || fail "README.md does not mention GATEWAY_INGEST_ENABLED=true"

echo "check: README points a local gateway at the API plane (:8081)"
grep -q 'GATEWAY_URL=http://localhost:8081' "$SCRIPT_DIR/README.md" \
  || fail "README.md is missing 'GATEWAY_URL=http://localhost:8081'"

echo "check: README links the utility"
grep -q 'github.com/Tuskira/claude-desktop-utility' "$SCRIPT_DIR/README.md" \
  || fail "README.md does not link claude-desktop-utility"

echo "check: relative links in README.md resolve"
# Markdown links "](target)" that are neither URLs nor in-page anchors.
while IFS= read -r target; do
  path="${target%%#*}"
  [[ -n "$path" ]] || continue
  [[ -e "$SCRIPT_DIR/$path" ]] || fail "README.md links '$target', which does not exist"
done < <(grep -o '](\([^)]*\))' "$SCRIPT_DIR/README.md" | sed 's/^](//; s/)$//' | grep -v -e '^https\?://' -e '^#' || true)

echo "check: run.sh and check.sh parse as bash"
bash -n "$SCRIPT_DIR/run.sh" || fail "run.sh has a syntax error"
bash -n "$SCRIPT_DIR/check.sh" || fail "check.sh has a syntax error"

if command -v shellcheck >/dev/null 2>&1; then
  echo "check: shellcheck run.sh check.sh"
  shellcheck "$SCRIPT_DIR/run.sh" "$SCRIPT_DIR/check.sh" || fail "shellcheck reported problems"
else
  echo "skip: shellcheck is not installed"
fi

echo "OK: 13-claude-desktop static checks passed."
