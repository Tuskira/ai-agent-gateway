import { useSearchParams } from 'react-router-dom'
import {
  periodFromParams,
  periodParams,
  type Period,
  type TimeRange,
} from '@/lib/overview'

/** A dashboard's period kept in the URL (`?range=` or `?from=&to=`), so a
 * reload, a drill-down and Back all keep the selection. */
export function usePeriod(def: TimeRange): [Period, (p: Period) => void] {
  const [params, setParams] = useSearchParams()
  const setPeriod = (p: Period) => {
    const next = new URLSearchParams(params)
    for (const k of ['range', 'from', 'to']) next.delete(k)
    for (const [k, v] of Object.entries(periodParams(p))) next.set(k, v)
    setParams(next)
  }
  return [periodFromParams(params, def), setPeriod]
}
