# 03 - Cursor, VS Code, and Codex CLI

## Goal

Point three more MCP clients at the same gateway, with the same two
headers as [02-claude-code](../02-claude-code/): Cursor, VS Code's native
MCP support, and Codex CLI. Same URL, same `Authorization` and
`X-Agent-Profile-Name`, three different config files.

## Prerequisites

- One of: [Cursor](https://cursor.com/), VS Code 1.102+ (native MCP
  support went GA in that release -- no extension needed), or the
  [Codex CLI](https://developers.openai.com/codex/) with a release that
  supports streamable-HTTP MCP servers (`url` + `bearer_token_env_var` /
  `http_headers`, not just `command`-based stdio servers).
- A gateway running with a connector registered and a profile granted its
  tools -- see [01-quickstart](../01-quickstart/) and
  [05-profiles](../05-profiles/), or the two inline `curl` commands in
  [02-claude-code](../02-claude-code/)'s Prerequisites.
- An agent-role `gk_...` key (`$GATEWAY_KEY` below).

## Steps

Each tool's file below is a minimal, ready-to-copy config for this
directory's own snippets (`cursor/mcp.json`, `vscode/mcp.json`,
`codex/config.toml`). All three carry the same URL and the same two
headers Claude Code uses: `Authorization: Bearer $GATEWAY_KEY` and
`X-Agent-Profile-Name: Full Access`.

### Cursor

[`cursor/mcp.json`](cursor/mcp.json) -- project-scoped at `.cursor/mcp.json`
(or global at `~/.cursor/mcp.json`):

```json
{
  "mcpServers": {
    "tusk-ai-gateway": {
      "url": "http://localhost:8080/mcp",
      "headers": {
        "Authorization": "Bearer gk_REPLACE_ME",
        "X-Agent-Profile-Name": "Full Access"
      }
    }
  }
}
```

Cursor supports `${env:VAR}` interpolation in `headers` (its own syntax --
not Claude Code's `${VAR}`), so `"Bearer ${env:GATEWAY_KEY}"` also works if
you'd rather not paste the plaintext key into the file.

**What you should see:** open the chat, click **@** (or `Cmd/Ctrl+L` then
**@**) -- `tusk-ai-gateway`'s tools appear, scoped to the profile.

### VS Code

VS Code's own native MCP support (not a third-party extension) reads
[`vscode/mcp.json`](vscode/mcp.json) from `.vscode/mcp.json` in your
workspace. Its shape differs from Claude Code's and Cursor's in one place:
the top-level key is `servers`, not `mcpServers`, and `"type": "http"` is
required -- omitting it makes VS Code assume a stdio server and try to
launch the URL as a command, which fails immediately.

```json
{
  "servers": {
    "tusk-ai-gateway": {
      "type": "http",
      "url": "http://localhost:8080/mcp",
      "headers": {
        "Authorization": "Bearer gk_REPLACE_ME",
        "X-Agent-Profile-Name": "Full Access"
      }
    }
  }
}
```

**What you should see:** the Chat view's tools picker lists
`tusk-ai-gateway`'s tools, scoped to the profile.

### Codex CLI

[`codex/config.toml`](codex/config.toml), at `~/.codex/config.toml`:

```toml
[mcp_servers.tusk-ai-gateway]
url = "http://localhost:8080/mcp"
bearer_token_env_var = "GATEWAY_KEY"

[mcp_servers.tusk-ai-gateway.http_headers]
X-Agent-Profile-Name = "Full Access"
```

`bearer_token_env_var` names an environment variable Codex reads at
connect time -- `export GATEWAY_KEY=gk_...` first, rather than writing the
key into `config.toml`. This is Codex's own `Authorization: Bearer ...`
mechanism, kept separate from `http_headers` (static values) the same way
the gateway keeps `X-Gateway-Key` separate from `Authorization` on the LLM
plane -- see [02-claude-code](../02-claude-code/) step 2. Codex's HTTP MCP support (`url`
instead of `command`) is comparatively new; if your installed version only
accepts `command`-based (stdio) servers, run `npx mcp-remote
http://localhost:8080/mcp --header "Authorization: Bearer $GATEWAY_KEY"
--header "X-Agent-Profile-Name: Full Access"` as a `command` entry
instead, bridging stdio to this same HTTP endpoint (`mcp-remote` is a
third-party package, not part of this gateway -- verify its flags against
its own docs before relying on it).

**What you should see:** `codex mcp list` shows `tusk-ai-gateway`; a
prompt that needs one of the profile's tools calls it successfully.

## Expected output

- All three clients see exactly the tools `X-Agent-Profile-Name` grants --
  not everything the tenant owns. An unrecognized or misspelled profile
  name grants nothing, not "everything" (see
  [05-profiles](../05-profiles/)); leaving the header off entirely serves
  every tool the key can reach, unless the gateway has
  `mcp.require_profile: true` set.
- Every tool call from any of the three lands in **Access Logs**
  (`/access-logs`), grouped by the `Mcp-Session-Id` the gateway minted for
  that client's session -- same mechanism as 02-claude-code, just a
  different calling process.

## Cleanup

Remove or revert the config file for whichever tool you edited:

```sh
rm -f .cursor/mcp.json .vscode/mcp.json   # or restore them from version control
# Codex CLI: remove the [mcp_servers.tusk-ai-gateway] table from ~/.codex/config.toml
```

The agent key and profile these snippets use live in the gateway's
Postgres, not in this directory -- see
[05-profiles](../05-profiles/)'s Cleanup section if you want to remove
them.
