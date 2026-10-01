import { describe, expect, it } from 'vitest'
import { paginationFromParams, withPagination } from '@/lib/pagination'

function read(query: string) {
  return paginationFromParams(new URLSearchParams(query))
}

describe('paginationFromParams', () => {
  it('defaults to the first page of 25', () => {
    expect(read('')).toEqual({ page: 0, pageSize: 25 })
  })

  it('reads an offered limit and an aligned offset', () => {
    expect(read('limit=50&offset=100')).toEqual({ page: 2, pageSize: 50 })
  })

  it('falls back to the default for a limit that is not offered', () => {
    for (const limit of ['0', '-25', '7', '100000', 'abc', '']) {
      expect(read(`limit=${limit}`)).toEqual({ page: 0, pageSize: 25 })
    }
  })

  it('treats a missing, negative or non-numeric offset as the first page', () => {
    for (const offset of ['-50', 'abc', '', 'NaN', 'Infinity']) {
      expect(read(`offset=${offset}`).page).toBe(0)
    }
  })

  it('rounds an unaligned offset down to the start of its page', () => {
    expect(read('limit=25&offset=30')).toEqual({ page: 1, pageSize: 25 })
  })
})

describe('withPagination', () => {
  it('writes limit and offset, keeping other params', () => {
    const next = withPagination(new URLSearchParams('model=x'), { page: 2, pageSize: 50 })
    expect(next.toString()).toBe('model=x&limit=50&offset=100')
  })

  it('drops values equal to the defaults', () => {
    const next = withPagination(new URLSearchParams('limit=50&offset=100'), {
      page: 0,
      pageSize: 25,
    })
    expect(next.toString()).toBe('')
  })

  it('does not change the params it was given', () => {
    const params = new URLSearchParams('offset=25')
    withPagination(params, { page: 0, pageSize: 25 })
    expect(params.toString()).toBe('offset=25')
  })
})
