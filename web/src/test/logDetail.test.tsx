import { describe, expect, it } from 'vitest'
import { render, screen } from '@testing-library/react'
import { prettyJson, reindentJson } from '@/lib/logs'
import { HeadersTable } from '@/components/app/logs/HeadersTable'

describe('reindentJson', () => {
  it('matches JSON.stringify indentation for ordinary JSON', () => {
    const text = '{"a":[1,{"b":"x, y: {z}"}],"c":{},"d":[],"e":"q\\"}"}'
    expect(reindentJson(text)).toBe(JSON.stringify(JSON.parse(text), null, 2))
  })

  it('keeps large integers, key order and duplicate keys as captured', () => {
    expect(reindentJson('{"id":12345678901234567891,"b":1,"10":2,"b":3}')).toBe(
      '{\n  "id": 12345678901234567891,\n  "b": 1,\n  "10": 2,\n  "b": 3\n}',
    )
  })
})

describe('prettyJson', () => {
  it('indents JSON that parses', () => {
    expect(prettyJson('{"a":[1,2]}')).toBe('{\n  "a": [\n    1,\n    2\n  ]\n}')
  })

  it('shows a JSON string unquoted', () => {
    expect(prettyJson('"You are a helpful assistant."')).toBe(
      'You are a helpful assistant.',
    )
  })

  it('leaves text that is not JSON as captured', () => {
    const sse = 'data: {"type":"ping"}\n\ndata: [DONE]\n'
    expect(prettyJson(sse)).toBe(sse)
  })

  it('indents a truncated JSON body that no longer parses', () => {
    expect(prettyJson('{"a":[1,{"b":"x', true)).toBe(
      '{\n  "a": [\n    1,\n    {\n      "b": "x',
    )
  })

  it('leaves truncated text that is not JSON as captured', () => {
    const sse = 'data: {"type":"ping"}\n\ndata: {"ty'
    expect(prettyJson(sse, true)).toBe(sse)
  })
})

describe('HeadersTable', () => {
  it('hides only the exact masked markers', () => {
    render(
      <HeadersTable
        headers={{ Authorization: '****', 'X-Api-Key': '[masked]', 'X-Note': 'ends****' }}
      />,
    )
    expect(screen.getByText('X-Note')).toBeInTheDocument()
    expect(screen.queryByText('Authorization')).not.toBeInTheDocument()
    expect(screen.queryByText('X-Api-Key')).not.toBeInTheDocument()
  })

  it('says credential headers were hidden rather than not captured', () => {
    render(<HeadersTable headers={{ Authorization: '****' }} />)
    expect(
      screen.getByText('Only credential headers were sent (values hidden).'),
    ).toBeInTheDocument()
  })
})
