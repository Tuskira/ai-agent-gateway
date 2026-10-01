#!/usr/bin/env bash
#
# sync-skills.sh: pull the SKILLS (kind "skill", not "command" -- a
# command has no meaning as a Claude Code skill directory) attached to
# one agent profile down into .claude/skills/<name>/ files, the layout
# Claude Code reads skills from directly, no MCP round trip involved.
#
# This is the *optional* third delivery path the skills & commands
# feature offers, alongside the gateway-native MCP tool (gateway__skill)
# and the MCP Skills Extension (skills/list, skills/get,
# resources/read) -- see docs/skills.md, "Delivery paths". It talks
# straight to the control-plane REST API:
#
#   GET /api/v1/profiles/{id}/skills               -- what's attached
#   GET /api/v1/skills/{id}/versions/{version}      -- that version's files
#
# Usage:
#   GATEWAY_KEY=gk_... ./sync-skills.sh [profile-name] [output-dir]
#
# profile-name defaults to "skills-demo" (this example's profile);
# output-dir defaults to ".claude/skills" relative to the current
# directory (so it's typically run from a repo root, e.g. as a
# SessionStart hook -- see README.md).
#
# Idempotent: each attached skill's directory is fully replaced (removed,
# then rewritten) from the resolved version's files every run, so a file
# removed from a later version doesn't linger locally, and re-running
# with nothing changed upstream leaves the same files on disk.
#
# Needs a key with skill.read and profile.read (any admin key, or an
# agent-role key -- the built-in agent role's "*.read" grant covers both).
set -euo pipefail

PROFILE_NAME="${1:-skills-demo}"
OUT_DIR="${2:-.claude/skills}"
CONTROL_BASE="${GATEWAY_CONTROL_BASE:-http://localhost:8081/api/v1}"

require_cmd() {
  if ! command -v "$1" >/dev/null 2>&1; then
    echo "FAIL: '$1' is required but was not found in PATH. $2" >&2
    exit 1
  fi
}

require_cmd curl "Install curl."
require_cmd jq "Install jq (e.g. 'brew install jq' or 'apt-get install jq')."

if [[ -z "${GATEWAY_KEY:-}" ]]; then
  echo "FAIL: GATEWAY_KEY is not set (needs skill.read and profile.read -- an admin key, or an agent-role key)." >&2
  exit 1
fi

echo "sync-skills: looking up profile '$PROFILE_NAME'"
PROFILE_ID="$(curl -sSf -H "Authorization: Bearer $GATEWAY_KEY" \
  "$CONTROL_BASE/profiles?limit=500" | jq -r --arg n "$PROFILE_NAME" '.items[] | select(.name==$n) | .id' | head -n1)"
if [[ -z "$PROFILE_ID" ]]; then
  echo "FAIL: no profile named '$PROFILE_NAME' is visible to this key." >&2
  exit 1
fi

items="$(curl -sSf -H "Authorization: Bearer $GATEWAY_KEY" "$CONTROL_BASE/profiles/$PROFILE_ID/skills" | jq -c '.items[] | select(.kind=="skill")')"
if [[ -z "$items" ]]; then
  echo "sync-skills: '$PROFILE_NAME' has no attached skills (commands aren't synced -- they have no meaning as a Claude Code skill directory)."
  exit 0
fi

mkdir -p "$OUT_DIR"
synced=0
while IFS= read -r item; do
  [[ -z "$item" ]] && continue
  name="$(jq -r '.name' <<<"$item")"
  skill_id="$(jq -r '.skill_id' <<<"$item")"
  # version floats to latest_version when the attachment isn't pinned
  # (store.ProfileSkill.Version == nil, "version" omitted from the
  # view) -- see docs/skills.md, "Attachment = grant".
  version="$(jq -r 'if .version then .version else .latest_version end' <<<"$item")"

  echo "sync-skills: $name (version $version)"
  version_resp="$(curl -sSf -H "Authorization: Bearer $GATEWAY_KEY" \
    "$CONTROL_BASE/skills/$skill_id/versions/$version")"

  skill_dir="$OUT_DIR/$name"
  rm -rf "$skill_dir"
  mkdir -p "$skill_dir"

  file_count="$(jq '.files | length' <<<"$version_resp")"
  for ((i = 0; i < file_count; i++)); do
    file="$(jq -c ".files[$i]" <<<"$version_resp")"
    path="$(jq -r '.path' <<<"$file")"
    dest="$skill_dir/$path"
    mkdir -p "$(dirname "$dest")"
    jq -r '.content' <<<"$file" >"$dest"
    echo "  -> $dest"
  done
  synced=$((synced + 1))
done <<<"$items"

echo "sync-skills: synced $synced skill(s) into $OUT_DIR"
