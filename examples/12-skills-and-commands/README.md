# 12 - Skills and commands on agent profiles

## Goal

A **skill** (reference text an agent loads on demand) and a **command**
(a reusable prompt template with named arguments), registered in the
skills & commands registry and attached to one agent profile, delivered
three ways. Show that:

- A skill/command is invisible until it's **attached** to a profile --
  attachment is a grant, the same rule an agent profile's tool allow-list
  already follows (see [docs/profiles.md](../../docs/profiles.md)).
- `initialize`'s `instructions` carry the profile's own free text, then a
  capped index of its attached skills; the `prompts` capability is
  advertised because the profile has a command attached.
- The gateway-native path: `tools/list` carries one native tool,
  `gateway__skill`, that loads a skill's file content by name;
  `prompts/list`/`prompts/get` serve the command as a plain-named prompt,
  substituting `{{path}}` from the caller's arguments.
- The **MCP Skills Extension** ([SEP-2640](https://modelcontextprotocol.io/extensions/skills/overview))
  path: `skills/list` and `resources/read` describe and serve the same
  skill under a `skill://code-review/SKILL.md` URI, with a `sha256`
  digest this example verifies against the actual bytes served.
- **No profile header means no skills at all** -- unlike a tool
  allow-list's `mcp.require_profile`-gated fallback to "every tool in the
  tenant", `gateway__skill` never appears without a resolved profile that
  has at least one attached skill.

See [docs/skills.md](../../docs/skills.md) for the full registry and
delivery-path reference this example exercises.

## Prerequisites

- Docker and the `docker compose` plugin
- `curl`, `jq`, and `shasum` (ships with macOS and most Linux perl installs)
- Nothing else: no API keys, no cloud account, no LLM provider
  credentials, and -- unlike 01/04/05/06/08/11 -- no local MCP server to run:
  skills and commands are served entirely by the gateway itself, never
  routed to a connector.

This example is self-contained: it registers its own skill, command and
profile rather than reusing anything another example left running.

## Steps

Run the whole thing with:

```sh
./examples/12-skills-and-commands/run.sh
```

It's safe to re-run: the skill, command and profile are looked up by name
first and reused if already there (the same pattern
[05-profiles](../05-profiles/) uses for its two profiles) -- which also
keeps `skills-demo` sitting on the stack afterwards for the
[Claude Code walkthrough](#try-it-from-claude-code) below. The one
exception is the agent API key, minted fresh every run, since its
plaintext is only ever returned once (see 05-profiles' README for the
same rationale).

**1. Register the `code-review` skill and the `fix-lint` command**
(`POST /api/v1/skills`):

```sh
curl -X POST http://localhost:8081/api/v1/skills \
  -H "Authorization: Bearer $GATEWAY_ADMIN_KEY" -H "Content-Type: application/json" \
  -d '{"name": "code-review", "kind": "skill",
       "files": [
         {"path": "SKILL.md", "content": "---\nname: code-review\ndescription: ...\n---\n..."},
         {"path": "references/checklist.md", "content": "..."}
       ]}'

curl -X POST http://localhost:8081/api/v1/skills \
  -H "Authorization: Bearer $GATEWAY_ADMIN_KEY" -H "Content-Type: application/json" \
  -d '{"name": "fix-lint", "kind": "command",
       "arguments": [{"name": "path", "description": "The file to fix.", "required": true}],
       "files": [{"path": "SKILL.md", "content": "Fix every lint error in `{{path}}`. ..."}]}'
```

`code-review`'s two files are exactly what a portable
[Agent Skill](https://modelcontextprotocol.io/extensions/skills/overview)
looks like: `SKILL.md` at the root plus a supporting reference file.
`fix-lint` is a command: its `SKILL.md` body (the text after the
frontmatter) is a prompt template, and every `{{path}}` placeholder in it
must be a declared argument, or the registry rejects it with `400`.

**2. Create the `skills-demo` profile, set its instructions, attach both**
(`PUT /api/v1/profiles/{id}` and `PUT /api/v1/profiles/{id}/skills`):

```sh
curl -X PUT http://localhost:8081/api/v1/profiles/$PROFILE_ID \
  -H "Authorization: Bearer $GATEWAY_ADMIN_KEY" -H "Content-Type: application/json" \
  -d '{"name": "skills-demo", "instructions": "This profile is for code review and lint-fixing tasks. Load the code-review skill before approving a diff."}'

curl -X PUT http://localhost:8081/api/v1/profiles/$PROFILE_ID/skills \
  -H "Authorization: Bearer $GATEWAY_ADMIN_KEY" -H "Content-Type: application/json" \
  -d '{"items": [{"skill_id": "'$CODE_REVIEW_ID'"}, {"skill_id": "'$FIX_LINT_ID'"}]}'
```

Both are full-replace routes (like `/tools`), so they're called
unconditionally on every run -- safe whether or not the profile already
existed. Omitting `version` on an attachment floats it to the skill's
current `latest_version`.

**3. Mint an agent-role API key**, same as 05-profiles' step 3.

**4. The gateway-native MCP path**, scoped to `skills-demo`:

```sh
# initialize: instructions carry the profile's text + a skill index;
# capabilities.prompts is advertised because a command is attached
curl -s http://localhost:8080/mcp \
  -H "Authorization: Bearer $AGENT_KEY" -H "X-Agent-Profile-Name: skills-demo" \
  -H "Content-Type: application/json" \
  -d '{"jsonrpc":"2.0","id":1,"method":"initialize",
       "params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"curl","version":"0"}}}'
# -> "instructions": "This profile is for code review...\n\nTool gateway. ...\n\nSkills available to this profile. Load one with the tool `gateway__skill` (argument `name`, optional `path`, default SKILL.md):\n- code-review: How to review a pull request for code style and safety in this repository."

# tools/list: the native gateway__skill tool
curl -s http://localhost:8080/mcp -H "Authorization: Bearer $AGENT_KEY" \
  -H "X-Agent-Profile-Name: skills-demo" -H "Mcp-Session-Id: $SESSION_ID" -H "Content-Type: application/json" \
  -d '{"jsonrpc":"2.0","id":2,"method":"tools/list"}'
# -> tools include {"name": "gateway__skill", "inputSchema": {...}}

# tools/call gateway__skill: loads code-review's SKILL.md content
curl -s http://localhost:8080/mcp -H "Authorization: Bearer $AGENT_KEY" \
  -H "X-Agent-Profile-Name: skills-demo" -H "Mcp-Session-Id: $SESSION_ID" -H "Content-Type: application/json" \
  -d '{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"gateway__skill","arguments":{"name":"code-review"}}}'
# -> {"content": [{"type": "text", "text": "---\nname: code-review\n...\n---\n\n# Code review\n..."}]}

# prompts/list: fix-lint as a native prompt (plain name, no "__")
curl -s http://localhost:8080/mcp -H "Authorization: Bearer $AGENT_KEY" \
  -H "X-Agent-Profile-Name: skills-demo" -H "Mcp-Session-Id: $SESSION_ID" -H "Content-Type: application/json" \
  -d '{"jsonrpc":"2.0","id":4,"method":"prompts/list"}'
# -> prompts include {"name": "fix-lint", "arguments": [{"name": "path", "required": true}]}

# prompts/get fix-lint: {{path}} rendered from the caller's arguments
curl -s http://localhost:8080/mcp -H "Authorization: Bearer $AGENT_KEY" \
  -H "X-Agent-Profile-Name: skills-demo" -H "Mcp-Session-Id: $SESSION_ID" -H "Content-Type: application/json" \
  -d '{"jsonrpc":"2.0","id":5,"method":"prompts/get","params":{"name":"fix-lint","arguments":{"path":"internal/foo.go"}}}'
# -> {"messages": [{"role": "user", "content": {"type": "text", "text": "Fix every lint error in `internal/foo.go`. ..."}}]}
```

**5. The MCP Skills Extension path** ([SEP-2640](https://modelcontextprotocol.io/extensions/skills/overview)):

```sh
# skills/list: describes every attached skill, with a verifiable manifest
curl -s http://localhost:8080/mcp -H "Authorization: Bearer $AGENT_KEY" \
  -H "X-Agent-Profile-Name: skills-demo" -H "Mcp-Session-Id: $SESSION_ID" -H "Content-Type: application/json" \
  -d '{"jsonrpc":"2.0","id":6,"method":"skills/list"}'
# -> {"skills": [{"uri": "skill://code-review/SKILL.md", "frontmatter": {...},
#                 "resources": [{"uri": "skill://code-review/SKILL.md", "digest": "sha256:...", "size": ...}, ...]}]}

# resources/read: serves the file's actual text -- run.sh hashes this
# response with `shasum -a 256` and checks it against the digest above
curl -s http://localhost:8080/mcp -H "Authorization: Bearer $AGENT_KEY" \
  -H "X-Agent-Profile-Name: skills-demo" -H "Mcp-Session-Id: $SESSION_ID" -H "Content-Type: application/json" \
  -d '{"jsonrpc":"2.0","id":7,"method":"resources/read","params":{"uri":"skill://code-review/SKILL.md"}}'
# -> {"contents": [{"uri": "skill://code-review/SKILL.md", "mimeType": "text/markdown", "text": "---\nname: code-review\n..."}]}
```

**6. The no-profile-header negative:**

```sh
curl -s http://localhost:8080/mcp -H "Authorization: Bearer $AGENT_KEY" -H "Mcp-Session-Id: $SESSION_ID" \
  -H "Content-Type: application/json" -d '{"jsonrpc":"2.0","id":8,"method":"tools/list"}'
# -> tools list every tenant tool mcp.require_profile's default allows, but NEVER gateway__skill
```

## Expected output

- `initialize`'s instructions contain both the profile's own text and a
  skill index line for `code-review`; `capabilities.prompts` is present.
- `tools/list` (with the header) includes `gateway__skill`; without the
  header, it never does, regardless of what other tenant tools are
  visible.
- `tools/call gateway__skill` returns `code-review`'s `SKILL.md` content.
- `prompts/list` includes `fix-lint` with a required `path` argument;
  `prompts/get` renders `{{path}}` into the template text.
- `skills/list` describes `code-review` with a `sha256:` digest for
  `SKILL.md`; `resources/read` on the same URI returns content that
  hashes to that exact digest.
- `run.sh` finishes by printing the console URL, the profile name, and
  the agent key once, the same closing block as 05-profiles.

## Try it from Claude Code

With `run.sh` having left `skills-demo` on the stack (and `$KEY` set to
an agent-role key -- reuse the one `run.sh` printed, or mint a new one
the same way step 3 above does):

```sh
claude mcp add --transport http gateway http://localhost:8080/mcp \
  --header "Authorization: Bearer $KEY" \
  --header "X-Agent-Profile-Name: skills-demo"
```

Then, inside Claude Code:

1. `/mcp__gateway__fix-lint` -- Claude Code discovers `fix-lint` as a
   native prompt (no `__` in its own name, unlike a connector's tools)
   and prompts you for its `path` argument.
2. Ask Claude something that should make it reach for the skill, e.g.
   *"Use the gateway's code-review skill to review this diff."* The model
   calls `gateway__skill` (or, if your client speaks the Skills
   Extension, `skills/list` + `resources/read`) on its own, the same
   round trip step 4/5 above drove by hand.

What to look at afterwards, in the console at `http://localhost:8081`:

- **Skills** page -- `code-review` and `fix-lint`, their kind badges and
  version.
- **Profiles** -> `skills-demo` -> **Profile Studio** -> "Skills &
  commands" tab -- the same two attachments, editable from here instead
  of the API.
- **Session Timeline** (needs ClickHouse, the Compose `analytics`
  profile) -- the `gateway__skill` call (or `skills/get`/
  `resources/read`) and the `fix-lint` `prompts/get`, each logged with
  `skill_name` set (`sink.AccessLog.SkillName`).

### Optional: syncing skills to `.claude/skills/`

A third delivery path needs no MCP round trip at all:
[`sync-skills.sh`](sync-skills.sh) pulls a profile's attached *skills*
(not commands -- a command has no meaning as a skill directory) straight
from the control-plane API into `.claude/skills/<name>/`, the layout
Claude Code reads from directly:

```sh
GATEWAY_KEY=$GATEWAY_ADMIN_KEY ./examples/12-skills-and-commands/sync-skills.sh skills-demo .claude/skills
# sync-skills: code-review (version 1)
#   -> .claude/skills/code-review/SKILL.md
#   -> .claude/skills/code-review/references/checklist.md
```

It's idempotent -- each skill's directory is fully replaced from the
resolved version every run, so a file dropped from a later version
doesn't linger locally. Wire it up to run automatically at the start of
every session with a `SessionStart` hook in `.claude/settings.json`:

```json
{
  "hooks": {
    "SessionStart": [
      {
        "hooks": [
          {
            "type": "command",
            "command": "GATEWAY_KEY=$GATEWAY_ADMIN_KEY examples/12-skills-and-commands/sync-skills.sh skills-demo .claude/skills"
          }
        ]
      }
    ]
  }
}
```

(Point `GATEWAY_KEY` at a real key some other way in a shared repo --
hardcoding one in `settings.json` is a secret in version control.)

## Cleanup

```sh
docker compose -f deploy/docker-compose.yml down
```

The skill, command, profile and every agent key this example mints live
in the stack's Postgres, not in this directory -- `down` alone leaves
them in the volume for the next run to reuse (the point of the
lookup-or-create pattern above). `down -v` also drops that volume, which
wipes them for good.

Every re-run mints one more `12-skills-and-commands-agent` API key
without revoking the previous one, exactly like 05-profiles' step 3.
They're harmless to leave, but to tidy them up:

```sh
curl -sS -H "Authorization: Bearer $GATEWAY_ADMIN_KEY" \
  "http://localhost:8081/api/v1/api-keys?limit=500" \
  | jq -r '.items[] | select(.name=="12-skills-and-commands-agent") | .id' \
  | while read -r id; do
      curl -sS -X DELETE "http://localhost:8081/api/v1/api-keys/$id" \
        -H "Authorization: Bearer $GATEWAY_ADMIN_KEY"
    done
```
