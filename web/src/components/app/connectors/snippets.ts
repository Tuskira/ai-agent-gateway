import { getMcpUrl } from '@/lib/utils'

export interface SnippetTab {
  id: string
  label: string
  language: string
  code: string
}

/** "Get started" code snippets for a connector's detail modal — how to
 * reach OUR gateway's MCP endpoint with a gateway API key and an agent
 * profile, not the upstream connector directly. `<your gk_ key>` and
 * `<profile>` are literal placeholders for the person to fill in. */
export function buildGetStartedSnippets(connectorSlug: string): SnippetTab[] {
  const mcpUrl = getMcpUrl()
  const auth = `Authorization: Bearer <your gk_ key>`
  const profileHeader = `X-Agent-Profile-Name: <profile>`

  return [
    {
      id: 'curl',
      label: 'cURL',
      language: 'bash',
      code: `curl -X POST "${mcpUrl}" \\
  -H "${auth}" \\
  -H "${profileHeader}" \\
  -H "Content-Type: application/json" \\
  -d '{"jsonrpc":"2.0","id":1,"method":"tools/list"}'`,
    },
    {
      id: 'python',
      label: 'Python',
      language: 'python',
      code: `import httpx

resp = httpx.post(
    "${mcpUrl}",
    headers={
        "Authorization": "Bearer <your gk_ key>",
        "X-Agent-Profile-Name": "<profile>",
        "Content-Type": "application/json",
    },
    json={"jsonrpc": "2.0", "id": 1, "method": "tools/list"},
)
print(resp.json())`,
    },
    {
      id: 'sdk',
      label: 'SDK',
      language: 'typescript',
      code: `import { Client } from "@modelcontextprotocol/sdk/client/index.js"
import { StreamableHTTPClientTransport } from "@modelcontextprotocol/sdk/client/streamableHttp.js"

const transport = new StreamableHTTPClientTransport(new URL("${mcpUrl}"), {
  requestInit: {
    headers: {
      Authorization: "Bearer <your gk_ key>",
      "X-Agent-Profile-Name": "<profile>",
    },
  },
})

const client = new Client({ name: "${connectorSlug}-client", version: "1.0.0" })
await client.connect(transport)
const tools = await client.listTools()`,
    },
    {
      id: 'claude-code',
      label: 'Claude Code',
      language: 'bash',
      code: `claude mcp add --transport http ${connectorSlug} "${mcpUrl}" \\
  --header "${auth}" \\
  --header "${profileHeader}"`,
    },
    {
      id: 'codex',
      label: 'Codex',
      language: 'toml',
      code: `[mcp_servers.${connectorSlug}]
url = "${mcpUrl}"
headers = { Authorization = "Bearer <your gk_ key>", X-Agent-Profile-Name = "<profile>" }`,
    },
    {
      id: 'cursor',
      label: 'Cursor',
      language: 'json',
      code: `{
  "mcpServers": {
    "${connectorSlug}": {
      "url": "${mcpUrl}",
      "headers": {
        "Authorization": "Bearer <your gk_ key>",
        "X-Agent-Profile-Name": "<profile>"
      }
    }
  }
}`,
    },
  ]
}
