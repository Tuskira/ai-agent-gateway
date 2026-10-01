#!/usr/bin/env bash
#
# 03-cursor-and-vscode: static validation only -- no network, no IDE or
# CLI install required. Checks that cursor/mcp.json, vscode/mcp.json, and
# codex/config.toml each parse and carry the gateway's actual header names
# (Authorization, X-Agent-Profile-Name). Exits non-zero on the first
# failure.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

fail() {
  echo "FAIL: $1" >&2
  exit 1
}

echo "check: required tools (jq, python3)"
command -v jq >/dev/null 2>&1 || fail "'jq' is required but was not found in PATH."
command -v python3 >/dev/null 2>&1 || fail "'python3' is required but was not found in PATH."

# ---------------------------------------------------------------------------
# cursor/mcp.json -- top-level "mcpServers", per-server url + headers
# ---------------------------------------------------------------------------

CURSOR_JSON="$SCRIPT_DIR/cursor/mcp.json"
echo "check: $CURSOR_JSON is valid JSON"
[[ -f "$CURSOR_JSON" ]] || fail "$CURSOR_JSON not found"
jq empty "$CURSOR_JSON" 2>/dev/null || fail "$CURSOR_JSON is not valid JSON"

cursor_name="$(jq -r '.mcpServers | keys[0] // empty' "$CURSOR_JSON")"
[[ -n "$cursor_name" ]] || fail "$CURSOR_JSON: .mcpServers is empty"
cursor_url="$(jq -r --arg n "$cursor_name" '.mcpServers[$n].url // empty' "$CURSOR_JSON")"
[[ "$cursor_url" == "http://localhost:8080/mcp" ]] || fail "$CURSOR_JSON: .mcpServers.$cursor_name.url is '$cursor_url', expected http://localhost:8080/mcp"

echo "check: $CURSOR_JSON headers carry Authorization and X-Agent-Profile-Name"
for h in Authorization X-Agent-Profile-Name; do
  has="$(jq -r --arg n "$cursor_name" --arg h "$h" '.mcpServers[$n].headers | has($h)' "$CURSOR_JSON")"
  [[ "$has" == "true" ]] || fail "$CURSOR_JSON: headers.$h is missing"
done
cursor_auth="$(jq -r --arg n "$cursor_name" '.mcpServers[$n].headers.Authorization' "$CURSOR_JSON")"
[[ "$cursor_auth" == Bearer\ * ]] || fail "$CURSOR_JSON: headers.Authorization does not start with 'Bearer '"

# ---------------------------------------------------------------------------
# vscode/mcp.json -- top-level "servers" (NOT "mcpServers"), type required
# ---------------------------------------------------------------------------

VSCODE_JSON="$SCRIPT_DIR/vscode/mcp.json"
echo "check: $VSCODE_JSON is valid JSON"
[[ -f "$VSCODE_JSON" ]] || fail "$VSCODE_JSON not found"
jq empty "$VSCODE_JSON" 2>/dev/null || fail "$VSCODE_JSON is not valid JSON"

vscode_name="$(jq -r '.servers | keys[0] // empty' "$VSCODE_JSON")"
[[ -n "$vscode_name" ]] || fail "$VSCODE_JSON: .servers is empty (VS Code's native MCP support keys on 'servers', not 'mcpServers')"

vscode_type="$(jq -r --arg n "$vscode_name" '.servers[$n].type // empty' "$VSCODE_JSON")"
[[ "$vscode_type" == "http" ]] || fail "$VSCODE_JSON: .servers.$vscode_name.type is '$vscode_type', expected 'http' (omitting it makes VS Code try to launch the url as a stdio command)"

vscode_url="$(jq -r --arg n "$vscode_name" '.servers[$n].url // empty' "$VSCODE_JSON")"
[[ "$vscode_url" == "http://localhost:8080/mcp" ]] || fail "$VSCODE_JSON: .servers.$vscode_name.url is '$vscode_url', expected http://localhost:8080/mcp"

echo "check: $VSCODE_JSON headers carry Authorization and X-Agent-Profile-Name"
for h in Authorization X-Agent-Profile-Name; do
  has="$(jq -r --arg n "$vscode_name" --arg h "$h" '.servers[$n].headers | has($h)' "$VSCODE_JSON")"
  [[ "$has" == "true" ]] || fail "$VSCODE_JSON: headers.$h is missing"
done
vscode_auth="$(jq -r --arg n "$vscode_name" '.servers[$n].headers.Authorization' "$VSCODE_JSON")"
[[ "$vscode_auth" == Bearer\ * ]] || fail "$VSCODE_JSON: headers.Authorization does not start with 'Bearer '"

# ---------------------------------------------------------------------------
# codex/config.toml -- [mcp_servers.<name>] with url + bearer_token_env_var
# and/or http_headers
# ---------------------------------------------------------------------------

CODEX_TOML="$SCRIPT_DIR/codex/config.toml"
echo "check: $CODEX_TOML exists and is valid TOML"
[[ -f "$CODEX_TOML" ]] || fail "$CODEX_TOML not found"

python3 - "$CODEX_TOML" <<'PY' || exit 1
import sys
import tomllib

path = sys.argv[1]

def fail(msg):
    print(f"FAIL: {msg}", file=sys.stderr)
    sys.exit(1)

try:
    with open(path, "rb") as f:
        data = tomllib.load(f)
except tomllib.TOMLDecodeError as e:
    fail(f"{path} is not valid TOML: {e}")

servers = data.get("mcp_servers", {})
if not servers:
    fail(f"{path}: no [mcp_servers.<name>] table found")

name, server = next(iter(servers.items()))

if server.get("url") != "http://localhost:8080/mcp":
    fail(f"{path}: mcp_servers.{name}.url is {server.get('url')!r}, expected http://localhost:8080/mcp")

# The gateway key can arrive as a bearer token (Codex's own
# bearer_token_env_var, resolved to "Authorization: Bearer ...") or as a
# static/env header named Authorization -- accept either shape.
has_bearer_var = bool(server.get("bearer_token_env_var"))
headers = server.get("http_headers", {}) or {}
env_headers = server.get("env_http_headers", {}) or {}
has_auth_header = "Authorization" in headers or "Authorization" in env_headers
if not (has_bearer_var or has_auth_header):
    fail(f"{path}: mcp_servers.{name} sets neither bearer_token_env_var nor an Authorization header")

if "X-Agent-Profile-Name" not in headers and "X-Agent-Profile-Name" not in env_headers:
    fail(f"{path}: mcp_servers.{name} is missing an X-Agent-Profile-Name header")

print(f"  -> mcp_servers.{name} OK (bearer_token_env_var={server.get('bearer_token_env_var')!r})")
PY

echo
echo "================================================================"
echo "03-cursor-and-vscode check.sh passed (static checks only, no network)."
echo "================================================================"
