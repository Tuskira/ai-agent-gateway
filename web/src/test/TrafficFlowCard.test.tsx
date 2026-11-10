import { describe, expect, it, vi } from 'vitest'
import { fireEvent, render, screen, within } from '@testing-library/react'
import { MemoryRouter, Route, Routes, useLocation } from 'react-router-dom'
import { TrafficFlowCard } from '@/components/app/TrafficFlowCard'
import type { TrafficFlow } from '@/lib/sankey'

const SAMPLE: TrafficFlow = {
  range: '24h',
  metric: 'calls',
  total: 500,
  agents: [{ key: 'cursor', label: 'Cursor', value: 500 }],
  nodes: [
    { id: 'CLIENT:cursor', key: 'cursor', label: 'Cursor', type: 'CLIENT', value: 500 },
    { id: 'PATH:llm', key: 'llm', label: 'LLM calls', type: 'PATH', value: 400 },
    { id: 'PATH:mcp', key: 'mcp', label: 'MCP calls', type: 'PATH', value: 100 },
    {
      id: 'MODEL:claude-sonnet-5',
      key: 'claude-sonnet-5',
      label: 'claude-sonnet-5',
      type: 'MODEL',
      value: 400,
      sublabel: 'anthropic',
    },
    { id: 'CONNECTOR:mesh', key: 'mesh', label: 'mesh', type: 'CONNECTOR', value: 100 },
  ],
  links: [
    {
      source: 'cursor',
      sourceType: 'CLIENT',
      target: 'llm',
      targetType: 'PATH',
      value: 400,
    },
    {
      source: 'cursor',
      sourceType: 'CLIENT',
      target: 'mcp',
      targetType: 'PATH',
      value: 100,
    },
    {
      source: 'llm',
      sourceType: 'PATH',
      target: 'claude-sonnet-5',
      targetType: 'MODEL',
      value: 400,
    },
    {
      source: 'mcp',
      sourceType: 'PATH',
      target: 'mesh',
      targetType: 'CONNECTOR',
      value: 100,
    },
  ],
}

function LocationProbe() {
  const loc = useLocation()
  return <div data-testid="location">{loc.pathname + loc.search}</div>
}

function renderCard(props: Partial<React.ComponentProps<typeof TrafficFlowCard>> = {}) {
  return render(
    <MemoryRouter initialEntries={['/']}>
      <Routes>
        <Route
          path="*"
          element={
            <>
              <TrafficFlowCard
                data={SAMPLE}
                period={{ range: '24h' }}
                groupBy="agent"
                onGroupByChange={() => {}}
                agent=""
                onAgentChange={() => {}}
                {...props}
              />
              <LocationProbe />
            </>
          }
        />
      </Routes>
    </MemoryRouter>,
  )
}

describe('TrafficFlowCard', () => {
  it('renders the title, range subtitle, legend and agent select', () => {
    renderCard()
    expect(screen.getByText('Agent traffic flow')).toBeInTheDocument()
    expect(screen.getByText('who called what · last 24h')).toBeInTheDocument()
    expect(screen.getByText('LLM path')).toBeInTheDocument()
    expect(screen.getByText('MCP path')).toBeInTheDocument()
    const select = screen.getByRole('combobox', { name: 'Agent' })
    expect(select).toHaveValue('')
    expect(screen.getByRole('option', { name: 'All agents' })).toBeInTheDocument()
    // Only agents the window actually saw are offered, not every known family.
    expect(screen.getByRole('option', { name: 'Cursor' })).toBeInTheDocument()
    expect(screen.queryByRole('option', { name: 'Claude Code' })).toBeNull()
  })

  it('keeps a selected agent in the select even when it is missing from the agents list', () => {
    renderCard({ agent: 'codex', data: { ...SAMPLE, agents: [] } })
    expect(screen.getByRole('combobox', { name: 'Agent' })).toHaveValue('codex')
    expect(screen.getByRole('option', { name: 'codex' })).toBeInTheDocument()
  })

  it('reflects the page range in the subtitle', () => {
    renderCard({ period: { range: '7d' } })
    expect(screen.getByText('who called what · last 7d')).toBeInTheDocument()
  })

  it('names custom dates in the subtitle', () => {
    renderCard({ period: { range: 'custom', from: '2026-01-01', to: '2026-01-03' } })
    expect(screen.getByText('who called what · Jan 1 – Jan 3, 2026')).toBeInTheDocument()
  })

  it('calls onAgentChange when an agent is picked from the select', () => {
    const onAgentChange = vi.fn()
    renderCard({ onAgentChange })
    fireEvent.change(screen.getByRole('combobox', { name: 'Agent' }), {
      target: { value: 'cursor' },
    })
    expect(onAgentChange).toHaveBeenCalledWith('cursor')
  })

  it('shows the "enable ClickHouse" empty state when data is null', () => {
    renderCard({ data: null })
    expect(
      screen.getByText('No data yet · enable the ClickHouse sink'),
    ).toBeInTheDocument()
    expect(screen.queryByText(/Band width/)).not.toBeInTheDocument()
  })

  it('shows a custom empty label when provided', () => {
    renderCard({ data: undefined, emptyLabel: 'Custom empty label' })
    expect(screen.getByText('Custom empty label')).toBeInTheDocument()
  })

  it('shows the no-traffic empty state when data has no nodes yet', () => {
    renderCard({ data: { ...SAMPLE, total: 0, nodes: [], links: [] } })
    expect(screen.getByText('No traffic in this range yet')).toBeInTheDocument()
  })

  it('names the selected agent in the empty state when a filter is active', () => {
    renderCard({
      data: { ...SAMPLE, total: 0, nodes: [], links: [] },
      agent: 'codex',
    })
    expect(
      screen.getByText('No traffic from this agent in this range'),
    ).toBeInTheDocument()
  })

  it('renders a loading skeleton while the request is in flight', () => {
    renderCard({ data: undefined, loading: true })
    expect(screen.getByRole('status')).toBeInTheDocument()
    expect(screen.queryByText('No data yet · enable the ClickHouse sink')).toBeNull()
  })

  describe('grouping', () => {
    const MIXED: TrafficFlow = {
      ...SAMPLE,
      agents: [
        {
          key: 'profile:Research Agent',
          label: 'Research Agent',
          value: 300,
          sublabel: 'profile',
        },
        { key: 'key:k1', label: 'ci-key', value: 150, sublabel: 'key' },
        { key: 'cursor', label: 'Cursor', value: 50, sublabel: 'client' },
      ],
      nodes: [
        {
          id: 'CLIENT:profile:Research Agent',
          key: 'profile:Research Agent',
          label: 'Research Agent',
          type: 'CLIENT',
          value: 300,
          sublabel: 'profile',
        },
        {
          id: 'CLIENT:key:k1',
          key: 'key:k1',
          label: 'ci-key',
          type: 'CLIENT',
          value: 150,
          sublabel: 'key',
        },
        {
          id: 'CLIENT:cursor',
          key: 'cursor',
          label: 'Cursor',
          type: 'CLIENT',
          value: 50,
          sublabel: 'client',
        },
        ...SAMPLE.nodes.slice(1),
      ],
      links: [
        {
          source: 'profile:Research Agent',
          sourceType: 'CLIENT',
          target: 'llm',
          targetType: 'PATH',
          value: 300,
        },
        {
          source: 'key:k1',
          sourceType: 'CLIENT',
          target: 'llm',
          targetType: 'PATH',
          value: 100,
        },
        {
          source: 'key:k1',
          sourceType: 'CLIENT',
          target: 'mcp',
          targetType: 'PATH',
          value: 50,
        },
        {
          source: 'cursor',
          sourceType: 'CLIENT',
          target: 'mcp',
          targetType: 'PATH',
          value: 50,
        },
        ...SAMPLE.links.slice(2),
      ],
    }

    it('offers Agent, API key and Harness, with the current grouping pressed', () => {
      renderCard({ groupBy: 'key' })
      const group = screen.getByRole('group', { name: 'Group traffic by' })
      expect(within(group).getByRole('button', { name: 'Agent' })).toHaveAttribute(
        'aria-pressed',
        'false',
      )
      expect(within(group).getByRole('button', { name: 'API key' })).toHaveAttribute(
        'aria-pressed',
        'true',
      )
      expect(within(group).getByRole('button', { name: 'Harness' })).toBeInTheDocument()
    })

    it('reports a picked grouping', () => {
      const onGroupByChange = vi.fn()
      renderCard({ onGroupByChange })
      fireEvent.click(screen.getByRole('button', { name: 'Harness' }))
      expect(onGroupByChange).toHaveBeenCalledWith('client')
    })

    it.each([
      ['agent', 'Agent', 'All agents'],
      ['key', 'API key', 'All API keys'],
      ['client', 'Harness', 'All harnesses'],
    ] as const)('names the select after the %s grouping', (groupBy, name, all) => {
      renderCard({ groupBy })
      expect(screen.getByRole('combobox', { name })).toHaveValue('')
      expect(screen.getByRole('option', { name: all })).toBeInTheDocument()
    })

    it('says in the select which identity named each agent', () => {
      renderCard({ data: MIXED })
      expect(
        screen.getByRole('option', { name: 'Research Agent · profile' }),
      ).toBeInTheDocument()
      expect(screen.getByRole('option', { name: 'ci-key · API key' })).toBeInTheDocument()
      expect(screen.getByRole('option', { name: 'Cursor · harness' })).toBeInTheDocument()
    })
  })
})
