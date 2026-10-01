import { Link } from 'react-router-dom'
import { ArrowLeft, BookOpen } from 'lucide-react'

export default function DocsPage() {
  return (
    <div className="flex min-h-svh flex-col items-center justify-center gap-3 px-4 text-center">
      <BookOpen className="size-8 text-muted-foreground" aria-hidden="true" />
      <h1 className="text-xl font-semibold text-foreground">Docs</h1>
      <p className="max-w-md text-sm text-muted-foreground">
        Documentation for the Tuskira AI Agent Gateway is coming soon.
      </p>
      <Link
        to="/"
        className="mt-2 inline-flex items-center gap-1.5 text-sm font-medium text-primary underline underline-offset-2"
      >
        <ArrowLeft className="size-4" aria-hidden="true" />
        Back to the console
      </Link>
    </div>
  )
}
