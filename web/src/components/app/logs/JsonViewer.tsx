import { useEffect, useRef, useState } from 'react'
import type { KeyboardEvent } from 'react'
import {
  CaseSensitive,
  ChevronDown,
  ChevronUp,
  ChevronsDownUp,
  ChevronsUpDown,
  Search,
  WrapText,
} from 'lucide-react'
import { json } from '@codemirror/lang-json'
import {
  HighlightStyle,
  codeFolding,
  foldEffect,
  foldGutter,
  foldKeymap,
  syntaxHighlighting,
  unfoldAll,
} from '@codemirror/language'
import {
  SearchQuery,
  findNext,
  findPrevious,
  getSearchQuery,
  openSearchPanel,
  search,
  setSearchQuery,
} from '@codemirror/search'
import { Compartment, EditorState } from '@codemirror/state'
import { EditorView, keymap, lineNumbers } from '@codemirror/view'
import { tags } from '@lezer/highlight'
import { cn } from '@/lib/utils'
import { Button } from '@/components/ui/button'
import { Input } from '@/components/ui/input'
import { nestedFoldRanges } from '@/components/app/logs/json-folding'
import { matchPosition, type MatchPosition } from '@/components/app/logs/json-search'

const wrapping = new Compartment()

/** Editor chrome from the design tokens, so it follows light/dark with the
 * rest of the console. Syntax colours are tokens too, swapped under .dark. */
const theme = EditorView.theme({
  '&': {
    height: '100%',
    fontSize: '12.5px',
    color: 'var(--foreground)',
    backgroundColor: 'var(--bg-subtle)',
    '--jv-key': 'var(--text-link)',
    '--jv-string': 'var(--sev-low-fg)',
    '--jv-number': 'var(--sev-critical-fg)',
    '--jv-atom': 'var(--sev-medium-fg)',
    '--jv-selection': 'color-mix(in oklab, var(--primary) 25%, transparent)',
  },
  '.dark &': {
    '--jv-string': 'var(--sev-low)',
    '--jv-number': 'var(--brand-bright)',
    '--jv-atom': 'var(--sev-medium)',
    '--jv-selection': 'color-mix(in oklab, var(--primary) 40%, transparent)',
  },
  '&.cm-focused': { outline: 'none' },
  '.cm-scroller': {
    overflow: 'auto',
    fontFamily: 'var(--font-mono)',
    lineHeight: '1.65',
  },
  '.cm-content': { padding: '10px 0' },
  '.cm-line': { padding: '0 14px 0 6px' },
  '& ::selection': { backgroundColor: 'var(--jv-selection)' },
  '.cm-gutters': {
    color: 'var(--text-subtle)',
    backgroundColor: 'var(--bg-subtle)',
    border: 'none',
    borderRight: '1px solid var(--border)',
  },
  '.cm-lineNumbers .cm-gutterElement': {
    minWidth: '30px',
    padding: '0 4px 0 12px',
    fontVariantNumeric: 'tabular-nums',
  },
  '.cm-foldGutter .cm-gutterElement': {
    display: 'flex',
    alignItems: 'center',
    justifyContent: 'center',
    width: '18px',
    padding: '0 4px 0 0',
    cursor: 'pointer',
    color: 'var(--text-subtle)',
  },
  '.cm-foldGutter .cm-gutterElement:hover': { color: 'var(--foreground)' },
  '.cm-foldPlaceholder': {
    margin: '0 3px',
    padding: '0 6px',
    color: 'var(--text-muted)',
    backgroundColor: 'var(--bg-muted)',
    border: '1px solid var(--border)',
    borderRadius: 'var(--r-pill)',
  },
  '.cm-searchMatch': {
    backgroundColor: 'color-mix(in srgb, var(--sev-low) 40%, transparent)',
    borderRadius: 'var(--r-1)',
  },
  '.cm-searchMatch.cm-searchMatch-selected': {
    backgroundColor: 'color-mix(in srgb, var(--sev-medium) 55%, transparent)',
  },
  // Find lives in the toolbar; the editor's own panel only exists to keep
  // match highlighting on (see `search` below).
  '.cm-panels': { display: 'none' },
})

const highlighting = HighlightStyle.define([
  { tag: tags.propertyName, color: 'var(--jv-key)' },
  { tag: tags.string, color: 'var(--jv-string)' },
  { tag: tags.number, color: 'var(--jv-number)' },
  { tag: [tags.bool, tags.null], color: 'var(--jv-atom)' },
  { tag: tags.punctuation, color: 'var(--text-muted)' },
])

/** Chevron for the fold gutter, drawn to match the console's icons. */
function foldMarker(open: boolean): HTMLElement {
  const ns = 'http://www.w3.org/2000/svg'
  const svg = document.createElementNS(ns, 'svg')
  svg.setAttribute('viewBox', '0 0 24 24')
  svg.setAttribute('width', '12')
  svg.setAttribute('height', '12')
  svg.setAttribute('fill', 'none')
  svg.setAttribute('stroke', 'currentColor')
  svg.setAttribute('stroke-width', '2.5')
  svg.setAttribute('stroke-linecap', 'round')
  svg.setAttribute('stroke-linejoin', 'round')
  const path = document.createElementNS(ns, 'path')
  path.setAttribute('d', open ? 'm6 9 6 6 6-6' : 'm9 18 6-6-6-6')
  svg.append(path)
  const marker = document.createElement('span')
  marker.title = open ? 'Fold' : 'Unfold'
  marker.append(svg)
  return marker
}

function matchLabel({ total, current, capped }: MatchPosition): string {
  if (total === 0) return 'No results'
  const count = capped ? `${total}+` : `${total}`
  return current > 0 ? `${current} of ${count}` : `${count} matches`
}

const toggleClassName =
  'aria-pressed:border-primary/40 aria-pressed:bg-primary/10 aria-pressed:text-text-link'

interface JsonViewerProps {
  /** Text to show, already indented. Need not be valid JSON. */
  value: string
  /** Accessible name for the text region, e.g. "Request body". */
  label: string
}

/** Read-only viewer for a captured body: highlighting, folding, find and
 * line numbers. Only the visible lines are in the DOM, so a multi-MB body
 * scrolls like a small one. It works on the text as captured rather than a
 * parsed value, which keeps large integers and duplicate keys exact and
 * still renders truncated or non-JSON bodies. */
export default function JsonViewer({ value, label }: JsonViewerProps) {
  const hostRef = useRef<HTMLDivElement>(null)
  const viewRef = useRef<EditorView | null>(null)
  const findRef = useRef<HTMLInputElement>(null)
  const [wrap, setWrap] = useState(true)
  const [find, setFind] = useState('')
  const [matchCase, setMatchCase] = useState(false)
  const [matches, setMatches] = useState<MatchPosition | null>(null)

  useEffect(() => {
    if (!hostRef.current) return
    const view = new EditorView({
      parent: hostRef.current,
      state: EditorState.create({
        doc: value,
        extensions: [
          EditorState.readOnly.of(true),
          EditorView.editable.of(false),
          // Not editable means not focusable by default; the fold
          // shortcuts need focus to land in the text.
          EditorView.contentAttributes.of({ tabindex: '0', 'aria-label': label }),
          lineNumbers(),
          codeFolding(),
          foldGutter({ markerDOM: foldMarker }),
          json(),
          syntaxHighlighting(highlighting),
          search({
            // Hidden, and at the top: a hidden panel at the bottom reports
            // the editor's whole height as a scroll margin, which throws
            // matches off screen.
            createPanel: () => ({ dom: document.createElement('div'), top: true }),
            scrollToMatch: (range) => EditorView.scrollIntoView(range, { y: 'center' }),
          }),
          keymap.of(foldKeymap),
          wrapping.of(EditorView.lineWrapping),
          theme,
        ],
      }),
    })
    // Matches are only highlighted while the editor's panel counts as open.
    openSearchPanel(view)
    viewRef.current = view
    return () => {
      view.destroy()
      viewRef.current = null
    }
  }, [value, label])

  useEffect(() => {
    viewRef.current?.dispatch({
      effects: wrapping.reconfigure(wrap ? EditorView.lineWrapping : []),
    })
  }, [wrap, value, label])

  function collapse() {
    const view = viewRef.current
    if (!view) return
    view.dispatch({
      effects: nestedFoldRanges(view.state).map((range) => foldEffect.of(range)),
    })
  }

  /** Searches from the top, so typing lands on the first match. */
  function runSearch(text: string, caseSensitive: boolean) {
    setFind(text)
    setMatchCase(caseSensitive)
    const view = viewRef.current
    if (!view) return
    const query = new SearchQuery({ search: text, caseSensitive, literal: true })
    view.dispatch({ effects: setSearchQuery.of(query), selection: { anchor: 0 } })
    if (!query.valid) return setMatches(null)
    findNext(view)
    setMatches(matchPosition(view.state, query))
  }

  function step(forward: boolean) {
    const view = viewRef.current
    if (!view || !matches?.total) return
    if (forward) findNext(view)
    else findPrevious(view)
    setMatches(matchPosition(view.state, getSearchQuery(view.state)))
  }

  function onFindKeyDown(event: KeyboardEvent<HTMLInputElement>) {
    if (event.key === 'Enter') {
      event.preventDefault()
      step(!event.shiftKey)
    } else if (event.key === 'Escape' && find) {
      runSearch('', matchCase)
    }
  }

  // The browser's own find can't see lines that aren't rendered.
  function onKeyDown(event: KeyboardEvent<HTMLDivElement>) {
    if ((event.metaKey || event.ctrlKey) && event.key.toLowerCase() === 'f') {
      event.preventDefault()
      findRef.current?.focus()
      findRef.current?.select()
    }
  }

  return (
    <div className="flex min-h-0 flex-1 flex-col gap-2.5" onKeyDown={onKeyDown}>
      <div className="flex flex-wrap items-center gap-x-3 gap-y-2">
        <div className="flex min-w-0 flex-1 items-center gap-1 sm:max-w-sm">
          <div className="relative min-w-0 flex-1">
            <Search
              className="pointer-events-none absolute top-1/2 left-2.5 size-3.5 -translate-y-1/2 text-text-subtle"
              aria-hidden="true"
            />
            <Input
              ref={findRef}
              data-json-find=""
              aria-label="Find"
              placeholder="Find"
              autoComplete="off"
              spellCheck={false}
              value={find}
              onChange={(event) => runSearch(event.target.value, matchCase)}
              onKeyDown={onFindKeyDown}
              className="h-7 pr-24 pl-8 text-xs md:text-xs"
            />
            {matches ? (
              <span
                aria-live="polite"
                className={cn(
                  'pointer-events-none absolute top-1/2 right-2.5 -translate-y-1/2 text-xs tabular-nums',
                  matches.total === 0 ? 'text-sev-high' : 'text-text-subtle',
                )}
              >
                {matchLabel(matches)}
              </span>
            ) : null}
          </div>
          <Button
            type="button"
            variant="ghost"
            size="icon-sm"
            aria-label="Previous match"
            title="Previous match (Shift+Enter)"
            disabled={!matches?.total}
            onClick={() => step(false)}
          >
            <ChevronUp aria-hidden="true" />
          </Button>
          <Button
            type="button"
            variant="ghost"
            size="icon-sm"
            aria-label="Next match"
            title="Next match (Enter)"
            disabled={!matches?.total}
            onClick={() => step(true)}
          >
            <ChevronDown aria-hidden="true" />
          </Button>
          <Button
            type="button"
            variant="outline"
            size="icon-sm"
            aria-label="Match case"
            title="Match case"
            aria-pressed={matchCase}
            className={toggleClassName}
            onClick={() => runSearch(find, !matchCase)}
          >
            <CaseSensitive aria-hidden="true" />
          </Button>
        </div>
        <div className="ml-auto flex items-center gap-1.5">
          <Button type="button" variant="outline" size="sm" onClick={collapse}>
            <ChevronsDownUp aria-hidden="true" />
            Collapse
          </Button>
          <Button
            type="button"
            variant="outline"
            size="sm"
            onClick={() => viewRef.current && unfoldAll(viewRef.current)}
          >
            <ChevronsUpDown aria-hidden="true" />
            Expand
          </Button>
          <Button
            type="button"
            variant="outline"
            size="sm"
            aria-pressed={wrap}
            className={toggleClassName}
            onClick={() => setWrap((on) => !on)}
          >
            <WrapText aria-hidden="true" />
            Wrap lines
          </Button>
        </div>
      </div>
      <div
        ref={hostRef}
        className="min-h-0 flex-1 overflow-hidden rounded-r-4 border border-border"
      />
    </div>
  )
}
