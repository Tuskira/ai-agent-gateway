import { useSearchParams } from 'react-router-dom'
import {
  parseGroupBy,
  TRAFFIC_FLOW_GROUP_BY_DEFAULT,
  type TrafficFlowGroupBy,
} from '@/lib/sankey'

/** The Agent traffic flow card's grouping, kept in the URL (`?flow_by=`) so
 * a reload, a shared link and Back keep it. The default (`agent`) is left
 * out of the URL. */
export function useFlowGroupBy(): [TrafficFlowGroupBy, (g: TrafficFlowGroupBy) => void] {
  const [params, setParams] = useSearchParams()
  const setGroupBy = (g: TrafficFlowGroupBy) => {
    const next = new URLSearchParams(params)
    next.delete('flow_agent')
    if (g === TRAFFIC_FLOW_GROUP_BY_DEFAULT) next.delete('flow_by')
    else next.set('flow_by', g)
    setParams(next)
  }
  return [parseGroupBy(params.get('flow_by')), setGroupBy]
}

/** The picked node of the traffic flow card (`?flow_agent=`), kept in the
 * URL next to the grouping. Empty means "all". */
export function useFlowAgent(): [string, (a: string) => void] {
  const [params, setParams] = useSearchParams()
  const setAgent = (a: string) => {
    const next = new URLSearchParams(params)
    if (a) next.set('flow_agent', a)
    else next.delete('flow_agent')
    setParams(next)
  }
  return [params.get('flow_agent') ?? '', setAgent]
}
