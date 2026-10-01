import { describe, expect, it } from 'vitest'
import { render, screen } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { EditorState } from '@codemirror/state'
import { json } from '@codemirror/lang-json'
import { SearchQuery } from '@codemirror/search'
import { prettyJson } from '@/lib/logs'
import { nestedFoldRanges } from '@/components/app/logs/json-folding'
import { matchPosition } from '@/components/app/logs/json-search'
import JsonViewer from '@/components/app/logs/JsonViewer'

const BODY = prettyJson(
  '{"model":"m","messages":[{"role":"user","content":[{"type":"text"}]}],"meta":{}}',
)

/** The folded text of each range, whitespace collapsed, in document order. */
function foldedText(doc: string, maxRanges?: number): string[] {
  const state = EditorState.create({ doc, extensions: [json()] })
  return nestedFoldRanges(state, maxRanges).map(({ from, to }) =>
    doc.slice(from, to).replace(/\s+/g, ''),
  )
}

describe('nestedFoldRanges', () => {
  it('folds every nested object and array, leaving the root open', () => {
    expect(foldedText(BODY)).toEqual([
      '{"role":"user","content":[{"type":"text"}]}',
      '"role":"user","content":[{"type":"text"}]',
      '{"type":"text"}',
      '"type":"text"',
    ])
  })

  it('keeps only the shallowest levels when there are too many to fold', () => {
    expect(foldedText(BODY, 2)).toEqual([
      '{"role":"user","content":[{"type":"text"}]}',
      '"role":"user","content":[{"type":"text"}]',
    ])
  })

  it('folds an unclosed array in a truncated body through to the end', () => {
    expect(foldedText('{\n  "a": [\n    1,\n    2')).toEqual(['1,2'])
  })
})

describe('matchPosition', () => {
  const doc = 'alpha beta alpha gamma alpha'
  const query = new SearchQuery({ search: 'alpha', literal: true })

  it('counts the matches and finds which one is selected', () => {
    const state = EditorState.create({ doc, selection: { anchor: 11, head: 16 } })
    expect(matchPosition(state, query)).toEqual({ total: 3, current: 2, capped: false })
  })

  it('reports no current match when the selection is elsewhere', () => {
    const state = EditorState.create({ doc, selection: { anchor: 6 } })
    expect(matchPosition(state, query)).toEqual({ total: 3, current: 0, capped: false })
  })

  it('stops counting at the cap', () => {
    const state = EditorState.create({ doc })
    expect(matchPosition(state, query, 2)).toEqual({ total: 2, current: 0, capped: true })
  })
})

describe('JsonViewer', () => {
  it('shows the body text', () => {
    render(<JsonViewer value={BODY} label="Request body" />)
    expect(screen.getByRole('textbox', { name: 'Request body' })).toHaveTextContent(
      '"role": "user"',
    )
  })

  it('collapses nested values and expands them again', async () => {
    const user = userEvent.setup()
    render(<JsonViewer value={BODY} label="Request body" />)
    const content = screen.getByRole('textbox', { name: 'Request body' })

    await user.click(screen.getByRole('button', { name: 'Collapse' }))
    expect(content).not.toHaveTextContent('"role": "user"')
    expect(content).toHaveTextContent('"model": "m"')

    await user.click(screen.getByRole('button', { name: 'Expand' }))
    expect(content).toHaveTextContent('"role": "user"')
  })

  it('wraps long lines until wrapping is switched off', async () => {
    const user = userEvent.setup()
    render(<JsonViewer value={BODY} label="Request body" />)
    const content = screen.getByRole('textbox', { name: 'Request body' })
    const wrap = screen.getByRole('button', { name: 'Wrap lines' })
    expect(wrap).toHaveAttribute('aria-pressed', 'true')
    expect(content).toHaveClass('cm-lineWrapping')

    await user.click(wrap)
    expect(wrap).toHaveAttribute('aria-pressed', 'false')
    expect(content).not.toHaveClass('cm-lineWrapping')
  })

  it('jumps to the first match as you type and steps through the rest', async () => {
    const user = userEvent.setup()
    render(<JsonViewer value={BODY} label="Request body" />)
    const find = screen.getByRole('textbox', { name: 'Find' })

    await user.type(find, 'me')
    expect(screen.getByText('1 of 2')).toBeInTheDocument()

    await user.keyboard('{Enter}')
    expect(screen.getByText('2 of 2')).toBeInTheDocument()
    await user.keyboard('{Enter}')
    expect(screen.getByText('1 of 2')).toBeInTheDocument()

    await user.click(screen.getByRole('button', { name: 'Previous match' }))
    expect(screen.getByText('2 of 2')).toBeInTheDocument()
  })

  // A panel docked at the bottom adds its height to the scroll margin. The
  // search panel is hidden, so there it would measure as the whole editor
  // and push every match off screen.
  it('docks the hidden search panel at the top', () => {
    const { container } = render(<JsonViewer value={BODY} label="Request body" />)
    expect(container.querySelector('.cm-panels-top')).toBeInTheDocument()
    expect(container.querySelector('.cm-panels-bottom')).not.toBeInTheDocument()
  })

  it('says so when nothing matches', async () => {
    const user = userEvent.setup()
    render(<JsonViewer value={BODY} label="Request body" />)

    await user.type(screen.getByRole('textbox', { name: 'Find' }), 'zzz')
    expect(screen.getByText('No results')).toBeInTheDocument()
    expect(screen.getByRole('button', { name: 'Next match' })).toBeDisabled()
  })

  it('ignores case until match case is switched on', async () => {
    const user = userEvent.setup()
    render(<JsonViewer value={BODY} label="Request body" />)

    await user.type(screen.getByRole('textbox', { name: 'Find' }), 'ME')
    expect(screen.getByText('1 of 2')).toBeInTheDocument()

    await user.click(screen.getByRole('button', { name: 'Match case' }))
    expect(screen.getByText('No results')).toBeInTheDocument()
  })

  it('clears the search on Escape', async () => {
    const user = userEvent.setup()
    render(<JsonViewer value={BODY} label="Request body" />)
    const find = screen.getByRole('textbox', { name: 'Find' })

    await user.type(find, 'me')
    await user.keyboard('{Escape}')
    expect(find).toHaveValue('')
    expect(screen.queryByText('1 of 2')).not.toBeInTheDocument()
  })

  it('moves focus to the find field on the find shortcut', async () => {
    const user = userEvent.setup()
    render(<JsonViewer value={BODY} label="Request body" />)

    await user.click(screen.getByRole('textbox', { name: 'Request body' }))
    await user.keyboard('{Control>}f{/Control}')
    expect(screen.getByRole('textbox', { name: 'Find' })).toHaveFocus()
  })
})
