import { describe, expect, it, vi } from 'vitest'
import { fireEvent, render, screen } from '@testing-library/react'
import { MemoryRouter } from 'react-router-dom'
import { TrafficFlowCard } from '@/components/app/TrafficFlowCard'
import type { TrafficFlow } from '@/lib/sankey'

// jsdom measures every element as 0 x 0, so the real BalancedSankey draws
// nothing. This stand-in lays each node out as a button, using the card's
// own label renderer and click gate, which is the part under test here.
vi.mock('@/components/app/sankey', () => ({
  BalancedSankey: ({
    data,
    renderNodeLabel,
    onNodeClick,
    isNodeClickable,
  }: {
    data: { nodes: { key: string; label: string; value: number; column: string }[] }
    renderNodeLabel: (n: unknown) => React.ReactNode
    onNodeClick: (n: unknown) => void
    isNodeClickable: (n: unknown) => boolean
  }) => (
    <div>
      {data.nodes.map((n) => {
        const positioned = { ...n, labelWidth: 200 }
        return (
          <div key={n.key}>
            {renderNodeLabel(positioned)}
            {isNodeClickable(positioned) ? (
              <button
                type="button"
                aria-label={`open ${n.key}`}
                onClick={() => onNodeClick(n)}
              />
            ) : null}
          </div>
        )
      })}
    </div>
  ),
}))

const MIXED: TrafficFlow = {
  range: '24h',
  metric: 'calls',
  total: 500,
  agents: [],
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
      value: 40,
      sublabel: 'client',
    },
    { id: 'CLIENT:unknown', key: 'unknown', label: 'Unknown', type: 'CLIENT', value: 5 },
    {
      id: 'CLIENT:other-agents',
      key: 'other-agents',
      label: 'Other agents',
      type: 'CLIENT',
      value: 5,
    },
    { id: 'PATH:llm', key: 'llm', label: 'LLM calls', type: 'PATH', value: 500 },
    {
      id: 'MODEL:claude-sonnet-5',
      key: 'claude-sonnet-5',
      label: 'claude-sonnet-5',
      type: 'MODEL',
      value: 500,
      sublabel: 'anthropic',
    },
  ],
  links: [],
}

function renderCard(props: Partial<React.ComponentProps<typeof TrafficFlowCard>> = {}) {
  return render(
    <MemoryRouter>
      <TrafficFlowCard
        data={MIXED}
        period={{ range: '24h' }}
        groupBy="agent"
        onGroupByChange={() => {}}
        agent=""
        onAgentChange={() => {}}
        {...props}
      />
    </MemoryRouter>,
  )
}

describe('TrafficFlowCard agent nodes', () => {
  it('badges each agent node with the identity that named it', () => {
    renderCard()
    expect(screen.getAllByTestId('agent-kind').map((el) => el.textContent)).toEqual([
      'profile',
      'API key',
      'harness',
    ])
  })

  it('does not badge unknown, the folded bucket, or a model (its sublabel is a provider)', () => {
    renderCard()
    // 3 badges for 5 agent nodes and a model: unknown and other-agents have
    // no sublabel, and "anthropic" is shown as the provider instead.
    expect(screen.getAllByTestId('agent-kind')).toHaveLength(3)
    expect(screen.getByText('Anthropic')).toBeInTheDocument()
  })

  it('shows no badge when the API sends no sublabel (grouped by harness)', () => {
    renderCard({
      groupBy: 'client',
      data: {
        ...MIXED,
        nodes: MIXED.nodes.map((n) => ({
          ...n,
          sublabel: n.type === 'MODEL' ? n.sublabel : undefined,
        })),
      },
    })
    expect(screen.queryByTestId('agent-kind')).toBeNull()
  })

  it('filters by the clicked agent, using its full node key', () => {
    const onAgentChange = vi.fn()
    renderCard({ onAgentChange })
    fireEvent.click(
      screen.getByRole('button', { name: 'open CLIENT:profile:Research Agent' }),
    )
    expect(onAgentChange).toHaveBeenCalledWith('profile:Research Agent')
  })

  it('clears the filter when the selected agent is clicked again', () => {
    const onAgentChange = vi.fn()
    renderCard({ onAgentChange, agent: 'key:k1' })
    fireEvent.click(screen.getByRole('button', { name: 'open CLIENT:key:k1' }))
    expect(onAgentChange).toHaveBeenCalledWith('')
  })

  it('offers no click on the folded "other agents" node', () => {
    renderCard()
    expect(screen.queryByRole('button', { name: 'open CLIENT:other-agents' })).toBeNull()
    expect(
      screen.getByRole('button', { name: 'open CLIENT:unknown' }),
    ).toBeInTheDocument()
  })
})
