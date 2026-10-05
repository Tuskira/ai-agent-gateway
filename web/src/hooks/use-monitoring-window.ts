import { useSearchParams } from 'react-router-dom'
import { MONITORING_WINDOWS, type MonitoringWindow } from '@/lib/token-monitoring'

/** The Token Monitoring window kept in the URL (?window=), defaulting to
 * Today, so a reload, a drill-down and Back all keep the selection. */
export function useMonitoringWindow(): [MonitoringWindow, (w: MonitoringWindow) => void] {
  const [params, setParams] = useSearchParams()
  const raw = params.get('window')
  const window = MONITORING_WINDOWS.some((w) => w.value === raw)
    ? (raw as MonitoringWindow)
    : 'today'
  const setWindow = (w: MonitoringWindow) => {
    const next = new URLSearchParams(params)
    next.set('window', w)
    setParams(next)
  }
  return [window, setWindow]
}
