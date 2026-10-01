import { Construction } from 'lucide-react'
import { Card, CardContent, CardHeader } from '@/components/ui/card'

interface ComingSoonProps {
  title: string
  description: string
}

export function ComingSoon({ title, description }: ComingSoonProps) {
  return (
    <div className="flex flex-col gap-6">
      <div>
        <h1 className="text-2xl font-bold tracking-tight text-foreground">{title}</h1>
      </div>
      <Card>
        <CardHeader className="items-center gap-3 text-center">
          <Construction className="size-8 text-text-subtle" aria-hidden="true" />
        </CardHeader>
        <CardContent className="text-center">
          <p className="text-sm text-text-subtle">{description}</p>
        </CardContent>
      </Card>
    </div>
  )
}
