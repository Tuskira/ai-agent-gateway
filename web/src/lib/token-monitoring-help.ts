import {
  ArrowDownUp,
  CircleDollarSign,
  Clock,
  Layers,
  ListFilter,
  Users,
} from 'lucide-react'
import type { HelpContent } from '@/components/app/WidgetHelp'

/*
 * Help text for Token Monitoring: what each tile and widget counts. Every
 * figure comes from the ClickHouse view llm_usage_canonical
 * (pkg/sink/clickhouse/migrate.go) through pkg/sink/clickhouse/monitoring.go
 * — keep the two in step.
 */

const COUNTED =
  'Only successful calls that produced usage are counted. Failed calls, calls refused by a budget or rate limit, and the free token-count and batch endpoints are left out.'

export const MONITORING_HELP = {
  totalTokens: {
    short:
      'Input, output, cache-read and cache-write tokens in this period, the same total as the Overview page. The change compares it with the period of the same length just before; "New" means that period had none.',
    blocks: [
      { kind: 'callout', body: COUNTED },
      {
        kind: 'card',
        title: 'Not the same as per-model tokens',
        icon: Layers,
        tone: 'info',
        body: 'Cards, callers, the charts and their changes count input + output only. Cache tokens are usually most of the total, so they are shown on their own tile.',
      },
    ],
  },
  inputOutput: {
    short:
      'Input (prompt) and output (completion) tokens. Together they are the "tokens" shown per model, per caller and in the charts.',
    blocks: [
      {
        kind: 'card',
        title: 'Input excludes cache reads',
        icon: ArrowDownUp,
        tone: 'info',
        body: 'OpenAI and Gemini count cached tokens inside input; they are subtracted back out, the same way pricing does, so every provider is counted alike.',
      },
    ],
  },
  cache: {
    short:
      'Tokens read from and written to the provider’s prompt cache. They are part of Total tokens and of cost, but not of the per-model and per-caller tokens.',
  },
  cost: {
    short:
      'Estimated spend: each call is priced from the gateway’s rate card when it is made. Calls to a model with no price are counted as unpriced, never as $0.',
    blocks: [
      {
        kind: 'card',
        title: 'Cache is priced separately',
        icon: CircleDollarSign,
        tone: 'success',
        body: 'Cache reads and writes have their own rates, so cost follows real spend even when cache tokens dominate the total.',
      },
    ],
  },
  calls: {
    short: `LLM calls with usage in this period. ${COUNTED} That is why this can be lower than LLM Agent Calls on the Overview page, which counts every request.`,
  },
  byRole: {
    short:
      'Input + output tokens by the role of the API key that made the call: agent, admin, interceptor or other.',
    blocks: [
      {
        kind: 'card',
        title: 'Roles',
        icon: Users,
        tone: 'info',
        body: 'Admin covers admin and platform-admin keys. Interceptor covers interceptor keys and any ingested call. Other covers viewer keys, keys since deleted and calls with no key.',
      },
    ],
  },
  byModel: {
    short:
      'Input + output tokens per model, ordered by tokens. A model is the name callers asked for, so a registry alias is one row whatever target answered.',
  },
  byCaller: {
    short: 'Input + output tokens per API key, ordered by tokens.',
  },
  usageOverTime: {
    short: 'Input + output tokens over the selected period, in UTC.',
    blocks: [
      {
        kind: 'card',
        title: 'Time buckets',
        icon: Clock,
        tone: 'info',
        body: 'One point per hour for periods up to 48 hours, one per day for longer ones. A bucket with no usage is drawn as zero.',
      },
    ],
  },
  costByModel: {
    short:
      'Each model’s estimated cost and its share of the total, most expensive first; unpriced models come last. Click a model for its callers and sessions.',
    blocks: [
      {
        kind: 'card',
        title: 'Token split',
        icon: Layers,
        tone: 'info',
        body: 'The bar splits the model’s tokens into input, output, cache read and cache write. The change compares input + output tokens with the previous period.',
      },
      {
        kind: 'card',
        title: 'Cards or list',
        icon: ListFilter,
        tone: 'muted',
        body: 'The list view shows every model in a sortable table. "Show all" opens every model, with a search.',
      },
    ],
  },
  callers: {
    short:
      'One row per API key: its role, input + output tokens and their share, estimated cost, calls, models used and the change against the previous period. Click a row for its models and sessions.',
  },
  sessions: {
    short:
      'The sessions behind this usage, with their tokens, cost and models. Open one to follow it on the Session Timeline.',
  },
} satisfies Record<string, HelpContent>
