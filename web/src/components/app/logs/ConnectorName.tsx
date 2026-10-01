import {
  Tooltip,
  TooltipContent,
  TooltipProvider,
  TooltipTrigger,
} from '@/components/ui/tooltip'

interface ConnectorNameProps {
  connectorId: string
  name: string | undefined
}

/** Shows the resolved connector name with its raw id in a tooltip — falls
 * back to the id itself while the connector list is still loading or the
 * connector no longer exists. Wraps its own `TooltipProvider` so it works
 * standalone in tests that render a page without the app-level one. */
export function ConnectorName({ connectorId, name }: ConnectorNameProps) {
  if (!connectorId) return <span className="text-text-subtle">—</span>

  return (
    <TooltipProvider>
      <Tooltip>
        <TooltipTrigger render={<span className="cursor-default text-text-muted" />}>
          {name ?? connectorId}
        </TooltipTrigger>
        <TooltipContent className="font-mono">{connectorId}</TooltipContent>
      </Tooltip>
    </TooltipProvider>
  )
}
