import type { ReactNode } from 'react'
import { flexRender, type Renderable } from '@tanstack/react-table'

function isClassComponent(value: object): boolean {
  const prototype = (value as { prototype?: { isReactComponent?: unknown } }).prototype
  return Boolean(prototype?.isReactComponent)
}

/**
 * Render a column's `header` or `cell` template.
 *
 * A function template is called directly rather than mounted as a component.
 * Pages usually rebuild their column definitions on every render, which hands
 * the table a new function each time; mounting it would remount the whole cell
 * on every render, dropping focus and hover state. The trade-off is that a
 * function template must not call hooks — render a component from it instead.
 */
export function renderTemplate<TProps extends object>(
  template: Renderable<TProps> | undefined,
  props: TProps,
): ReactNode {
  if (typeof template === 'function' && !isClassComponent(template)) {
    return (template as (props: TProps) => ReactNode)(props)
  }
  return flexRender(template, props)
}
