# Skills and commands

The **skills & commands registry** (`/api/v1/skills`) holds reusable text
an agent can load on demand, or run against arguments you fill in. It is
one registry for two `kind`s of row:

- **`skill`** — reference text an agent reads when it decides it's
  relevant: a runbook, a checklist, a style guide. Nothing about it takes
  arguments; it is loaded verbatim.
- **`command`** — a reusable prompt **template** with named arguments
  (`{{name}}` placeholders), rendered on demand into one message, the same
  role a slash command plays in Claude Code.

Neither is visible or usable on its own — see
[Attachment = grant](#attachment--grant) below. A skill or command is
never forwarded to a connector (an MCP server registered with the gateway;
the console's **MCPs** page); it is served entirely by the gateway
itself. See [example 12](https://github.com/Tuskira/tusk-ai-secured-gateway/tree/main/examples/12-skills-and-commands) for a
worked, runnable walkthrough of everything on this page.

## Skill vs. command vs. instructions

An [agent profile](profiles.md) can carry all three, and they answer
different questions:

| | Answers | Set via |
|---|---|---|
| **`instructions`** | "What should this agent always know?" | `PUT /profiles/{id}` (free text, ≤ 8 KiB) |
| **skill** (`kind: "skill"`) | "What might this agent need to look up?" | attach a registry row via `PUT /profiles/{id}/skills` |
| **command** (`kind: "command"`) | "What repeatable task, with what inputs, should this agent be able to run on request?" | attach a registry row, same route |

`instructions` is always present in `initialize`'s own instructions text;
a skill is loaded only when the agent decides to (`gateway__skill`, or
`skills/get`/`resources/read`); a command is invoked explicitly, by name,
with arguments (`prompts/get`).

## The registry: versions, platform vs. tenant

Every skill/command row is a **versioned bundle of text files** — never
scripts, never anything executable (see
[Security](#security) below). `POST /api/v1/skills` creates version 1;
`POST /api/v1/skills/{id}/versions` adds a new one. A version is
immutable once created: editing a skill's content always means adding a
new version, never rewriting an old one, so an attachment pinned to a
specific version (below) never moves under it.

```sh
curl -X POST http://localhost:8081/api/v1/skills \
  -H "Authorization: Bearer $GATEWAY_KEY" -H "Content-Type: application/json" \
  -d '{"name": "code-review", "kind": "skill",
       "files": [
         {"path": "SKILL.md", "content": "---\nname: code-review\ndescription: How to review a diff.\n---\n\n# Code review\n..."},
         {"path": "references/checklist.md", "content": "..."}
       ]}'
# -> {"id": "...", "name": "code-review", "kind": "skill", "description": "How to review a diff.",
#     "latest_version": 1, "enabled": true, "scope": "tenant", ...}
```

`GET /api/v1/skills/{id}` returns the row plus its `latest` version's
files (`{version, files: [{path, content, sha256, size}]}`);
`GET .../versions` lists every version without file bodies, newest first;
`GET .../versions/{v}` gets one version's files.

**Platform vs. tenant**, the same convention as the [model
registry](llm-plane.md#model-registry): a row with `tenant_id` set is a
**tenant** row, visible only to that tenant; a row with no tenant (only
the [seed](#seed) creates these) is a **platform** row, visible to
**every** tenant and read-only through the API (`PUT`, `DELETE` or `POST …/versions` on
one is `403`) — a tenant overrides a platform default by creating its own
row of the same name, which then wins for that tenant on every lookup
(`GET /skills`'s `scope` field says which is which). Names are unique per
tenant, and among platform rows, counting live rows only: a
**soft-deleted** row's name is free again immediately, the same rule
connector and profile slugs follow, so create-delete-recreate under the
same name always works.

## Validation rules and limits

The API and the [seed](#seed) apply the same checks:

| Rule | Limit |
|---|---|
| Name | `^[a-z0-9][a-z0-9._-]{0,63}$`, must not contain `__` (reserved for `<connector>__<tool>` naming), must not be `gateway` (reserved for the gateway's own native tools) |
| Files per version | 1 – 20, must include `SKILL.md` at the root |
| File path | `^[A-Za-z0-9][A-Za-z0-9._/-]*$`, no `..` segment, no leading/trailing `/`, extension one of `.md .txt .json .yaml .yml .csv .xml .toml` |
| File content | valid UTF-8, no NUL bytes |
| Size | `SKILL.md` ≤ 64 KiB; any other file ≤ 256 KiB; every file combined ≤ 512 KiB |
| `description` | 1 – 1024 characters (from `SKILL.md`'s frontmatter — see below; never settable directly) |
| Command `arguments` | 0 – 10 entries, each `{name: ^[a-z][a-z0-9_]{0,31}$, description ≤ 256 chars, required}`, no duplicate names |
| Command template | every `{{name}}` placeholder in `SKILL.md`'s body must be a declared argument, or the write is rejected |

A `kind: "skill"` row must declare no `arguments` at all — those exist
only for commands.

### `SKILL.md`'s frontmatter

Every `SKILL.md` must start with a YAML frontmatter block:

```markdown
---
name: code-review
description: How to review a diff against this gateway's own style rules.
---

The body — a skill's reference text, or a command's `{{argument}}` template.
```

`name` must equal the row's own `name`; `description` (1–1024 characters)
becomes the row's `description` and is **read-only through the API** — a
`description` in a create/update request body is accepted (so echoing a
`GET` response back doesn't 400 on an "unknown field") but always
ignored. Only `SKILL.md` itself, via a new version, changes it.

Allowed frontmatter keys: the portable [Agent
Skills](https://modelcontextprotocol.io/extensions/skills/overview)
fields `name, description, license, compatibility, metadata,
allowed-tools`, plus the Claude Code fields `user-invocable,
disable-model-invocation, context, agent, background, model, effort,
paths, shell, arguments, argument-hint, when_to_use`. Any other key is a
`400`. **`hooks` is explicitly rejected** — see
[Security](#security). The parsed frontmatter is stored verbatim
(`skills.frontmatter`, a JSON object) and returned on every read
(`GET /skills/{id}`, and in `skills/list`/`skills/get`'s manifest, below).

## Attachment = grant

A skill or command that exists in the registry is invisible and
unusable — no `tools/list` entry, no `prompts/list` entry, no
`skills/list` entry — until it is **attached** to an [agent
profile](profiles.md), the same rule the profile's tool allow-list
already follows. Attachment is a **replace**, like `PUT
/profiles/{id}/tools`:

```sh
curl -X PUT http://localhost:8081/api/v1/profiles/$PROFILE_ID/skills \
  -H "Authorization: Bearer $GATEWAY_KEY" -H "Content-Type: application/json" \
  -d '{"items": [{"skill_id": "'$SKILL_ID'"}, {"skill_id": "'$COMMAND_ID'", "version": 2}]}'
```

Every `skill_id` must be visible to the caller's tenant (a tenant row or
a platform row); when `version` is set, that version must already exist.
Both are checked before the write, the same way `PUT /profiles/{id}/tools` checks
`connector_id` ownership.

An attached skill or command that is later disabled or deleted (or whose
pinned version no longer exists) grants nothing. Attachment and registry
changes apply at once where the API and MCP plane share a process;
elsewhere within 30 seconds, how long an MCP replica caches a profile.

### Outdated

Omitting `version` **floats** the attachment to the skill's current
`latest_version` — the common case, and what you want for a runbook that
should always serve the newest edit. Setting `version` **pins** it to
that one version forever, even after a newer version is added: the
attachment is "outdated" whenever its pinned `version` is behind the
skill's `latest_version` (`GET /profiles/{id}/skills`'s response carries
both fields, so a client can compute this itself — `PUT`/`GET
/profiles/{id}/skills` never render an "outdated" label; the console's
Profile Studio does, see [profiles.md](profiles.md#profile-studio)).
Pinning is for a command whose exact wording matters for reproducibility
(a runbook a regulator has signed off on, say); floating is the default
for everything else.

## Delivery paths

A profile's attached skills and commands reach an MCP client three ways.
The first two are always on; the third is opt-in tooling, not a gateway
feature.

### 1. Gateway-native tool and prompts

Always available, no client-side support needed beyond `tools/call` and
`prompts/get`, which every MCP client already has:

- **`gateway__skill`** (a `tools/call`), listed in `tools/list` only when
  the resolved profile has ≥ 1 attached skill: `{name, path?}` →
  `{content: [{type: "text", text: <file content>}]}`. `path` defaults to
  `SKILL.md`. Never routed to a connector — `"gateway"` is a reserved
  connector name/slug precisely so a real connector's tools can never
  collide with it.
- Attached **commands** are served as native **`prompts/get`** entries,
  merged into `prompts/list` under their own plain name (no `__`, unlike
  a connector's `<connector>__<prompt>`): `{{name}}` placeholders in the
  template are substituted from the caller's `arguments`.
- `initialize`'s `instructions` carry a capped index of attached skills
  ("Skills available to this profile. Load one with the tool
  `gateway__skill`…"), and the server's `prompts` capability is
  advertised whenever the profile has a command attached.
- Both deny with `-32003` (`{skill, profile}`) when the name isn't
  attached; `gateway__skill` denies an unknown `path` with `-32602`, and
  a command denies a missing required argument the same way.

Full wire shapes and the `initialize` instructions format:
[profiles.md, "Skills and commands"](profiles.md#skills-and-commands).

### 2. MCP Skills Extension (SEP-2640)

For a client that speaks the [MCP Skills
Extension](https://modelcontextprotocol.io/extensions/skills/overview)
natively: `skills/list` and `skills/get` describe a profile's attached
**skills** (never commands — a command has no meaning as a discoverable
resource; it's invoked, not read), and `resources/read` on a
`skill://<name>/<path>` URI serves a file's content. Every response
carries a complete file manifest with `sha256` digests, so a client can
verify what it fetched matches what it was told to expect:

```json
{"uri": "skill://code-review/SKILL.md",
 "frontmatter": {"name": "code-review", "description": "..."},
 "resources": [
   {"uri": "skill://code-review/SKILL.md", "digest": "sha256:...", "size": 512},
   {"uri": "skill://code-review/references/checklist.md", "digest": "sha256:...", "size": 340}
 ]}
```

`initialize` advertises `resources` and `extensions:
{"io.modelcontextprotocol/skills": {}}` only when the profile has ≥ 1
attached skill. Full shapes, pagination, caching and error codes:
[architecture.md, "MCP Skills Extension
(SEP-2640)"](architecture.md#mcp-skills-extension-sep-2640).

### 3. Optional: sync to files

Neither of the above requires a client to keep anything on disk. Some
tools — Claude Code among them — read skills from plain files under
`.claude/skills/<name>/` instead of over MCP. For that case,
[`examples/12-skills-and-commands/sync-skills.sh`](https://github.com/Tuskira/tusk-ai-secured-gateway/blob/main/examples/12-skills-and-commands/sync-skills.sh)
pulls a profile's attached skills straight from the control-plane API
(`GET /profiles/{id}/skills` + `GET /skills/{id}/versions/{v}`) into that
layout — no MCP round trip, and safe to wire into a `SessionStart` hook
so it runs before every session (the example's README shows the hook
snippet). This is example tooling, not a gateway endpoint or a supported
integration surface; the two MCP-native paths above are what the gateway
itself guarantees.

## Seed

`skills.seed_dir` (env `GATEWAY_SKILLS_SEED_DIR`, see
[configuration.md](configuration.md#skills-skills--commands-registry-seed))
names a directory walked once at startup, after the model-registry
(`llm_proxy.models`) and MCP-catalog (`mcp_catalog.seed`) seeds: every immediate subdirectory
containing a `SKILL.md` becomes one **platform** row, named after the subdirectory.
`SKILL.md`'s frontmatter supplies the name (must equal the subdirectory
name) and description; `metadata.kind` (`"skill"` by default, or
`"command"`) and, for a command, `metadata.arguments` supply what the API
takes as separate top-level fields — the seed's directory format has no
`kind` field of its own to put them in. Every other file directly inside
the subdirectory (not walked recursively) becomes a supporting
file, with the same validation the API applies.

A platform row that doesn't exist yet is created; one that exists is left
alone unless the directory's files differ from its current latest version
(compared by path set + `sha256` digest), in which case the directory's
files are added as a new version (and the row is re-enabled). The
directory is a seed, not the source of truth for which rows exist:
deleting a subdirectory doesn't delete the row. Runs on every replica at
boot, and concurrent boots are safe: at worst each racing replica records one
extra, identical version.

## Console

The **Skills** page (`/skills`, under Configure) lists every skill and
command visible to the tenant — tiles for skills, commands and the most
used one in the selected range, a table (name, status, kind, description,
version, calls, used by, last seen, scope, updated; status is the
lifecycle state described in
[observability.md](observability.md#console-pages), including Discovered
rows for skills seen in LLM traffic but not registered), and an add dialog
with a monospace `SKILL.md` editor prefilled with a valid
frontmatter template, an extra-files editor, and — for a command — an
arguments editor. A skill's detail view lists its versions and can add a
new one; platform rows show a read-only "Platform" badge.

Attaching a skill/command to a profile happens in **Profile Studio**'s
"Skills & commands" tab: a checkbox list of the tenant's + platform's skills and commands, a
version selector (latest, or pin one), saving via `PUT
/profiles/{id}/skills`. The Studio's "Instructions" tab saves via `PUT
/profiles/{id}`. See [profiles.md, "Profile
Studio"](profiles.md#profile-studio).

## Security

- **Text-only, never executable.** The file-extension allow-list
  (`.md .txt .json .yaml .yml .csv .xml .toml`) admits no script, binary,
  or shell file; `SKILL.md`'s frontmatter explicitly rejects the `hooks`
  key, the one field in the portable Agent Skills / Claude Code
  frontmatter vocabulary that could otherwise make a "skill" run
  something. Nothing in this registry is ever executed by the gateway —
  a skill is text handed to an agent to read, a command is text rendered
  into a prompt.
- **Secret scan on every write.** Every file, on create and on every new
  version, is scanned for common credential shapes (a gateway API key, an
  AWS access key id, a PEM private-key header, a GitHub PAT, a Slack
  token, an Anthropic key, a generic `sk-` key, a Google API key) and
  rejected with `400 validation_error` ("secret-like content detected in
  `<path>`") on any match. This catches an obvious accident, not a
  determined attempt to obfuscate one — it is not a substitute for
  reviewing what gets registered.
- **A skill/command's content is untrusted input to whatever reads it.**
  Registering one requires `skill.create`/`skill.update`, gated like
  every other write; but once attached, its text is served to an agent
  and, from there, likely into an LLM's context, the same trust boundary
  a connector's tool description or resource content already crosses.
  Treat what you register the way you'd treat a tool description you
  didn't write yourself.
- **Attachment is the only access control.** A tenant row is visible to
  create/read/update/delete within its own tenant; a platform row is
  read-only to every tenant. Neither alone grants an agent anything — see
  [Attachment = grant](#attachment--grant). There is no per-user or
  per-key scoping within a tenant: every agent profile in the tenant can
  be granted any of the tenant's own skills/commands, plus every platform
  one.
- **Logging.** A `gateway__skill` call, a native command's `prompts/get`,
  and the Skills Extension's `skills/get`/`resources/read` on a
  `skill://` URI are all logged like any other `tools/call`/`prompts/get`/
  `resources/read`, with an additional `skill_name` field
  (ClickHouse `mcp_access_logs.skill_name`)
  naming the skill or command invoked. `skills/list` is not logged (list
  methods never are).

## Permissions

`skill.create`, `skill.read`, `skill.update`, `skill.delete` — covered by
the built-in admin role's `*` grant, and `skill.read` by the agent role's
`*.read` grant. Attaching a skill/command to a profile uses the existing
`profile.update`/`profile.read` permissions, not a skill-specific one —
attaching is an edit to the profile, not to the registry.

## API reference

See [api.md](api.md#route-table) for the full route table
(`/skills`, `/skills/{id}`, `/skills/{id}/versions[/{v}]`,
`/profiles/{id}/skills`) with exact request/response shapes.
