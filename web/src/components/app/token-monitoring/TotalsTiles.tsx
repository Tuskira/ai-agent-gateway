import { Link } from 'react-router-dom'
import { ArrowDownUp, Coins, Database, Layers, PhoneCall, Sigma } from 'lucide-react'
import { KpiCard } from '@/components/app/KpiCard'
import { DeltaText } from '@/components/app/token-monitoring/parts'
import { Skeleton } from '@/components/ui/skeleton'
import { formatCompactNumber } from '@/lib/overview'
import { formatTokenCost, type TokenTotals } from '@/lib/token-monitoring'

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
  totals: TokenTotals | undefined
  loading: boolean
}) {
  const ready = !loading && !!totals
  const value = (v: string, testId?: string) =>
    ready ? <span data-testid={testId}>{v}</span> : <Skeleton className="h-7 w-24" />
  // "163.39K / 248.97K" is too wide for a narrow tile at the card's 28px:
  // scale paired values with the tile's own width (KpiCard is a @container).
  const pair = (a: number, b: number) =>
    ready ? (
      <span
        title={`${a.toLocaleString()} / ${b.toLocaleString()}`}
        className="text-[18px] @min-[250px]:text-[22px] @min-[320px]:text-[28px]"
      >
        {formatCompactNumber(a)} / {formatCompactNumber(b)}
      </span>
    ) : (
      <Skeleton className="h-7 w-24" />
    )
  return (
    <div className="grid grid-cols-[repeat(auto-fit,minmax(200px,1fr))] gap-4">
      <KpiCard
        title="Total tokens"
        icon={Sigma}
        chipClassName="bg-sev-medium/15 text-sev-medium"
        value={value(totals ? formatCompactNumber(totals.tokens) : '', 'total-tokens')}
        footer={
          totals ? (
            <span data-testid="total-delta">
              <DeltaText delta={totals.delta_pct} tokens={totals.tokens} /> vs previous
              period
            </span>
          ) : null
        }
      />
      <KpiCard
        title="Input / Output"
        icon={ArrowDownUp}
        chipClassName="bg-brand-cyan-to/15 text-brand-cyan-to"
        value={pair(totals?.prompt_tokens ?? 0, totals?.completion_tokens ?? 0)}
        footer="Tokens = input + output"
      />
      <KpiCard
        title="Cache read / write"
        icon={Layers}
        chipClassName="bg-primary/15 text-primary"
        value={pair(totals?.cache_read_tokens ?? 0, totals?.cache_write_tokens ?? 0)}
        footer="Reported separately, not in tokens"
      />
      <KpiCard
        title="Cost"
        icon={Coins}
        chipClassName="bg-status-resolved/15 text-status-resolved"
        value={value(totals ? formatTokenCost(totals.cost_usd) : '', 'total-cost-value')}
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
        icon={PhoneCall}
        chipClassName="bg-sev-high/15 text-sev-high"
        value={value(totals ? totals.calls.toLocaleString() : '')}
        footer="Successful LLM calls with usage"
      />
    </div>
  )
}
