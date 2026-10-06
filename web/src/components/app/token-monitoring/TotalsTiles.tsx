import { Link } from 'react-router-dom'
import { ArrowDownUp, Coins, Database, Layers, PhoneCall, Sigma } from 'lucide-react'
import { KpiCard } from '@/components/app/KpiCard'
import { WidgetHelp } from '@/components/app/WidgetHelp'
import { MONITORING_HELP } from '@/lib/token-monitoring-help'
import { NumberTicker } from '@/components/app/NumberTicker'
import { DeltaText } from '@/components/app/token-monitoring/parts'
import { Skeleton } from '@/components/ui/skeleton'
import { formatCompactNumber } from '@/lib/overview'
import { formatTokenCost, type PageTotals } from '@/lib/token-monitoring'

export function AnalyticsOffNotice() {
  return (
    <div className="flex flex-col items-center gap-3 rounded-r-5 border border-border bg-card px-6 py-14 text-center">
      <span className="flex size-[52px] items-center justify-center rounded-r-5 bg-sev-medium-bg text-sev-medium-fg">
        <Database className="size-[22px]" aria-hidden="true" />
      </span>
      <div className="text-base font-semibold text-foreground">
        Analytics are turned off
      </div>
      <p className="max-w-[52ch] text-[13.5px] text-text-subtle">
        Token monitoring reads the ClickHouse sink. Enable it to see token usage by model
        and caller.
      </p>
      <Link to="/docs" className="text-[13px] font-semibold text-text-link">
        View docs →
      </Link>
    </div>
  )
}

/** The headline tiles shared by the page and its drill-downs. */
export function TotalsTiles({
  totals,
  loading,
}: {
  totals: PageTotals | undefined
  loading: boolean
}) {
  const ready = !loading && !!totals
  // Counts up like the Overview tiles; '—' when there is no value (unpriced).
  const value = (
    v: number | null | undefined,
    format: (n: number) => string,
    testId?: string,
  ) =>
    !ready ? (
      <Skeleton className="h-7 w-24" />
    ) : (
      <span data-testid={testId}>
        {v === null || v === undefined ? '—' : <NumberTicker value={v} format={format} />}
      </span>
    )
  // "163.39K / 248.97K" is too wide for a narrow tile at the card's 28px:
  // scale paired values with the tile's own width (KpiCard is a @container).
  const pair = (a: number, b: number) =>
    ready ? (
      <span
        title={`${a.toLocaleString()} / ${b.toLocaleString()}`}
        className="text-[18px] @min-[250px]:text-[22px] @min-[320px]:text-[28px]"
      >
        <NumberTicker value={a} format={formatCompactNumber} /> /{' '}
        <NumberTicker value={b} format={formatCompactNumber} />
      </span>
    ) : (
      <Skeleton className="h-7 w-24" />
    )
  return (
    <div className="grid grid-cols-[repeat(auto-fit,minmax(200px,1fr))] gap-4">
      <KpiCard
        title="Total tokens"
        help={
          <WidgetHelp
            title="Total tokens"
            content={MONITORING_HELP.totalTokens}
            triggerClassName="size-5"
          />
        }
        titleNote={<span className="text-text-subtle">input + output + cache</span>}
        icon={Sigma}
        chipClassName="bg-sev-medium/15 text-sev-medium"
        value={value(totals?.tokens_with_cache, formatCompactNumber, 'total-tokens')}
        footer={
          totals ? (
            <span data-testid="total-delta">
              <DeltaText
                delta={totals.tokens_with_cache_delta_pct}
                tokens={totals.tokens_with_cache}
              />{' '}
              vs previous period
            </span>
          ) : null
        }
      />
      <KpiCard
        title="Input / Output"
        help={
          <WidgetHelp
            title="Input / Output"
            content={MONITORING_HELP.inputOutput}
            triggerClassName="size-5"
          />
        }
        icon={ArrowDownUp}
        chipClassName="bg-brand-cyan-to/15 text-brand-cyan-to"
        value={pair(totals?.prompt_tokens ?? 0, totals?.completion_tokens ?? 0)}
        footer="Per-model and caller tokens"
      />
      <KpiCard
        title="Cache read / write"
        help={
          <WidgetHelp
            title="Cache read / write"
            content={MONITORING_HELP.cache}
            triggerClassName="size-5"
          />
        }
        icon={Layers}
        chipClassName="bg-primary/15 text-primary"
        value={pair(totals?.cache_read_tokens ?? 0, totals?.cache_write_tokens ?? 0)}
        footer="Counted in Total tokens only"
      />
      <KpiCard
        title="Cost"
        help={
          <WidgetHelp
            title="Cost"
            content={MONITORING_HELP.cost}
            triggerClassName="size-5"
          />
        }
        icon={Coins}
        chipClassName="bg-status-resolved/15 text-status-resolved"
        value={value(totals?.cost_usd, formatTokenCost, 'total-cost-value')}
        footer={
          <span data-testid="total-cost">
            Estimate
            {totals && totals.unpriced_calls > 0
              ? ` · ${totals.unpriced_calls} unpriced calls`
              : ''}
          </span>
        }
      />
      <KpiCard
        title="Calls"
        help={
          <WidgetHelp
            title="Calls"
            content={MONITORING_HELP.calls}
            triggerClassName="size-5"
          />
        }
        icon={PhoneCall}
        chipClassName="bg-sev-high/15 text-sev-high"
        value={value(totals?.calls, formatCompactNumber)}
        footer="Successful LLM calls with usage"
      />
    </div>
  )
}
