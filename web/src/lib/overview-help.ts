import {
  Activity,
  Bell,
  Bot,
  CircleAlert,
  CircleCheck,
  CircleDollarSign,
  CircleHelp,
  Clock,
  Coins,
  Gauge,
  IdCard,
  List,
  Sigma,
  Timer,
  TriangleAlert,
  Wrench,
} from 'lucide-react'
import type { HelpContent } from '@/components/app/WidgetHelp'

/*
 * Help text for the Overview page: what each tile and widget counts, and
 * how. Every statement here describes what the analytics endpoint actually
 * computes (pkg/sink/clickhouse/reader.go) — keep the two in step.
 */

const BEFORE_ANALYTICS = 'Before analytics is enabled'

export const KPI_HELP = {
  llm: {
    short:
      'Every request the gateway sent to a language model in this period, whether it succeeded or failed. The change is measured against the period of the same length just before it.',
  },
  mcp: {
    short:
      'Tool invocations (tools/call) sent through the gateway to MCP servers. Other MCP traffic, such as listing tools or opening a session, is not counted here.',
  },
  tokens: {
    short:
      'Input, output, cache-read and cache-write tokens, added up across every LLM call in this period.',
  },
  cost: {
    short:
      'Estimated spend on LLM calls. Each call is priced from the gateway’s rate card when it is made, and calls to a model with no price are left out.',
  },
  success: {
    short:
      'The share of responses that completed without an error, across LLM and MCP traffic. Notifications (204) carry no result, so they are left out of the calculation.',
  },
} satisfies Record<string, HelpContent>

export const WIDGET_HELP = {
  llmUsage: {
    short:
      'Calls, tokens and estimated cost for each model requested through the gateway, ordered by tokens used.',
    blocks: [
      {
        kind: 'callout',
        body: 'Use this to see which models carry your traffic and where the spend goes.',
      },
      {
        kind: 'card',
        title: 'Calls',
        icon: Activity,
        tone: 'info',
        body: 'The number beside each model, and the length of its bar. Bars are scaled to the model with the most calls.',
      },
      {
        kind: 'card',
        title: 'Tokens',
        icon: Coins,
        tone: 'brand',
        body: 'Input, output, cache-read and cache-write tokens for that model, added together.',
      },
      {
        kind: 'card',
        title: 'Cost (est.)',
        icon: CircleDollarSign,
        tone: 'warning',
        body: 'Priced from the gateway’s rate card when each call was made, so it can differ from your provider’s invoice. A model with no price shows $0.00.',
      },
      {
        kind: 'card',
        title: 'What’s listed',
        icon: List,
        tone: 'muted',
        body: 'Up to 20 models, grouped by the model name the client asked for.',
      },
    ],
  },
  trafficDistribution: {
    short:
      'How requests through the gateway split between LLM calls and MCP requests in this period.',
    blocks: [
      {
        kind: 'callout',
        body: 'A quick read on what your agents use the gateway for: talking to models, or reaching tools.',
      },
      {
        kind: 'card',
        title: 'LLM Agent',
        icon: Bot,
        tone: 'warning',
        body: 'Every request sent to a language model.',
      },
      {
        kind: 'card',
        title: 'MCP Tool',
        icon: Wrench,
        tone: 'brand',
        body: 'Every MCP request: tool calls, and also listing tools and opening a session. That is why this number is higher than the MCP Tool Calls tile, which counts tool calls only.',
      },
      {
        kind: 'card',
        title: 'Total',
        icon: Sigma,
        tone: 'muted',
        body: 'The two added together, shown in the centre of the ring.',
      },
    ],
  },
  topAgents: {
    short:
      'Agent profiles ranked by the number of MCP requests they sent in this period.',
    blocks: [
      {
        kind: 'callout',
        body: 'Shows which agents are most active through the gateway.',
      },
      {
        kind: 'card',
        title: 'How agents are identified',
        icon: IdCard,
        tone: 'info',
        body: 'By the X-Agent-Profile-Name header on each request. Requests without it are not counted here.',
      },
      {
        kind: 'card',
        title: 'What’s listed',
        icon: List,
        tone: 'muted',
        body: 'The ten most active profiles. Bars are scaled to the busiest one.',
      },
      {
        kind: 'card',
        title: BEFORE_ANALYTICS,
        icon: CircleHelp,
        tone: 'muted',
        body: 'Without the ClickHouse sink there is no traffic to rank, so the list shows your configured profiles and how many tools each one exposes.',
      },
    ],
  },
  health: {
    short:
      'Responses grouped by outcome: succeeded, failed, or a notification that carries no result.',
    blocks: [
      {
        kind: 'callout',
        body: 'Responses are grouped by outcome rather than HTTP status, because an MCP call answers HTTP 200 even when the call inside it failed.',
      },
      {
        kind: 'card',
        title: '200 · Success',
        icon: CircleCheck,
        tone: 'success',
        body: 'The call completed without an error.',
      },
      {
        kind: 'card',
        title: '204 · Notification',
        icon: Bell,
        tone: 'info',
        body: 'A one-way message that carries no result, so it is neither a success nor a failure.',
      },
      {
        kind: 'card',
        title: 'Error',
        icon: CircleAlert,
        tone: 'danger',
        body: 'The call reported an error, or the response status was 400 or above.',
      },
      {
        kind: 'card',
        title: 'Why the centre differs from the 200 slice',
        icon: CircleHelp,
        tone: 'muted',
        body: 'Each slice is a share of every response. The success rate in the centre leaves notifications out, so it is higher than the 200 slice whenever there are any.',
      },
    ],
  },
  latency: {
    short:
      'How long calls through the gateway took, across LLM and MCP traffic together.',
    blocks: [
      {
        kind: 'card',
        title: 'Median',
        icon: Gauge,
        tone: 'info',
        body: 'Half of all calls finished faster than this. It describes the typical call.',
      },
      {
        kind: 'card',
        title: 'p95',
        icon: Timer,
        tone: 'warning',
        body: '95% of calls finished faster than this. It shows how slow the slowest calls get.',
      },
      {
        kind: 'card',
        title: 'Slowest calls',
        icon: TriangleAlert,
        tone: 'danger',
        body: 'The five longest single calls in the period, each named by its tool, MCP method or model.',
      },
    ],
  },
  requestsByClient: {
    short: 'MCP requests grouped by the client application that sent them.',
    blocks: [
      {
        kind: 'card',
        title: 'How clients are identified',
        icon: IdCard,
        tone: 'info',
        body: 'From the User-Agent header. The gateway recognises claude-code, cursor, vscode and codex.',
      },
      {
        kind: 'card',
        title: 'Other',
        icon: CircleHelp,
        tone: 'muted',
        body: 'Everything else: SDKs, scripts, and clients that send no User-Agent.',
      },
    ],
  },
  mcpTools: {
    short: 'The tools your agents used most in this period.',
    blocks: [
      {
        kind: 'card',
        title: 'What’s counted',
        icon: Wrench,
        tone: 'brand',
        body: 'MCP requests that name a tool, counted per tool.',
      },
      {
        kind: 'card',
        title: 'What’s listed',
        icon: List,
        tone: 'muted',
        body: 'The ten most used tools. Bars are scaled to the busiest one.',
      },
    ],
  },
  topConnectors: {
    short: 'MCPs ranked by the number of requests routed to them.',
    blocks: [
      {
        kind: 'card',
        title: 'What’s listed',
        icon: List,
        tone: 'muted',
        body: 'The ten busiest MCPs. Bars are scaled to the busiest one.',
      },
      {
        kind: 'card',
        title: BEFORE_ANALYTICS,
        icon: CircleHelp,
        tone: 'muted',
        body: 'Without the ClickHouse sink the list shows your configured MCPs and how many tools each one exposes. The dot is green for a healthy MCP, red for an unhealthy one and grey when its state is unknown.',
      },
    ],
  },
  trafficOverTime: {
    short: 'LLM calls and MCP requests over the selected period.',
    blocks: [
      {
        kind: 'card',
        title: 'Time buckets',
        icon: Clock,
        tone: 'info',
        body: 'One point per hour for periods up to 48 hours (Last 24h, or one or two custom days), and one per day for longer ones. A bucket with no traffic is drawn as zero.',
      },
      {
        kind: 'card',
        title: 'LLM calls',
        icon: Bot,
        tone: 'warning',
        body: 'Every request sent to a language model.',
      },
      {
        kind: 'card',
        title: 'MCP calls',
        icon: Wrench,
        tone: 'brand',
        body: 'Every MCP request, not only tool calls.',
      },
    ],
  },
  trafficFlow: {
    short:
      "Who called what over the selected period: each agent's calls split by plane, then by the model or MCP that served them.",
    blocks: [
      {
        kind: 'card',
        title: 'Agent',
        icon: IdCard,
        tone: 'info',
        body: 'The calling client family, from the User-Agent header on either plane (e.g. Claude Code, Cursor, VS Code). Pick one in the top-right to focus the flow.',
      },
      {
        kind: 'card',
        title: 'LLM path / MCP path',
        icon: Sigma,
        tone: 'brand',
        body: 'Band width is call volume. LLM calls end at the model that served them; MCP tool calls end at the MCP that handled them.',
      },
    ],
  },
} satisfies Record<string, HelpContent>
