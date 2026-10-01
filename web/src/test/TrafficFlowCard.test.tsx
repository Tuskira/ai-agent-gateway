import { describe, expect, it, vi } from 'vitest'
import { fireEvent, render, screen } from '@testing-library/react'
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
                range="24h"
                clientName=""
                onClientNameChange={() => {}}
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
    renderCard({ clientName: 'codex', data: { ...SAMPLE, agents: [] } })
    expect(screen.getByRole('combobox', { name: 'Agent' })).toHaveValue('codex')
    expect(screen.getByRole('option', { name: 'codex' })).toBeInTheDocument()
  })

  it('reflects the page range in the subtitle', () => {
    renderCard({ range: '7d' })
    expect(screen.getByText('who called what · last 7d')).toBeInTheDocument()
  })

  it('calls onClientNameChange when an agent is picked from the select', () => {
    const onClientNameChange = vi.fn()
    renderCard({ onClientNameChange })
    fireEvent.change(screen.getByRole('combobox', { name: 'Agent' }), {
      target: { value: 'cursor' },
    })
    expect(onClientNameChange).toHaveBeenCalledWith('cursor')
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
      clientName: 'codex',
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
})
