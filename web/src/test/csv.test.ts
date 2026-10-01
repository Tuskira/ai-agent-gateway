import { describe, expect, it } from 'vitest'
import { csvCell, rowsToCsv } from '@/components/app/data-table/csv'

describe('csvCell', () => {
  it('leaves plain text and numbers alone', () => {
    expect(csvCell('hello')).toBe('hello')
    expect(csvCell(42)).toBe('42')
    expect(csvCell(-5)).toBe('-5')
  })

  it('writes null and undefined as empty', () => {
    expect(csvCell(null)).toBe('')
    expect(csvCell(undefined)).toBe('')
  })

  it('quotes cells holding a comma, quote or line break', () => {
    expect(csvCell('a,b')).toBe('"a,b"')
    expect(csvCell('say "hi"')).toBe('"say ""hi"""')
    expect(csvCell('line1\nline2')).toBe('"line1\nline2"')
    expect(csvCell('line1\r\nline2')).toBe('"line1\r\nline2"')
  })

  it('neutralises text a spreadsheet would run as a formula', () => {
    expect(csvCell('=HYPERLINK("http://x")')).toBe(`"'=HYPERLINK(""http://x"")"`)
    expect(csvCell('+1+1')).toBe("'+1+1")
    expect(csvCell('-2+3')).toBe("'-2+3")
    expect(csvCell('@SUM(A1)')).toBe("'@SUM(A1)")
  })
})

describe('rowsToCsv', () => {
  it('joins a header row and body rows', () => {
    expect(
      rowsToCsv(
        ['Name', 'Count'],
        [
          ['a', 1],
          ['b,c', 2],
        ],
      ),
    ).toBe('Name,Count\na,1\n"b,c",2')
  })

  it('returns only the header row when there are no rows', () => {
    expect(rowsToCsv(['Name'], [])).toBe('Name')
  })
})
