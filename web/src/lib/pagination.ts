import type { PaginationModel } from '@/components/app/data-table'

/** Page sizes the log tables offer. The first is the default. */
export const LOG_PAGE_SIZES = [25, 50, 100]

const DEFAULT_PAGE_SIZE = 25

/**
 * Read `limit` / `offset` search params as a page model.
 *
 * The URL can be edited by hand or come from an old link, so nothing in it is
 * trusted: a limit that isn't one of the offered sizes falls back to the
 * default, and an offset that is missing, negative or not a number becomes the
 * first page. An offset that isn't a multiple of the limit is rounded down to
 * the start of its page, so the rows shown always match the range displayed.
 */
export function paginationFromParams(params: URLSearchParams): PaginationModel {
  const limit = Number(params.get('limit'))
  const pageSize = LOG_PAGE_SIZES.includes(limit) ? limit : DEFAULT_PAGE_SIZE
  const offset = Number(params.get('offset'))
  const page = Number.isFinite(offset) && offset > 0 ? Math.floor(offset / pageSize) : 0
  return { page, pageSize }
}

/**
 * A copy of `params` with `limit` / `offset` set from a page model. Values
 * equal to the defaults are removed, keeping shared links short.
 */
export function withPagination(
  params: URLSearchParams,
  { page, pageSize }: PaginationModel,
): URLSearchParams {
  const next = new URLSearchParams(params)
  if (pageSize === DEFAULT_PAGE_SIZE) next.delete('limit')
  else next.set('limit', String(pageSize))
  if (page > 0) next.set('offset', String(page * pageSize))
  else next.delete('offset')
  return next
}
