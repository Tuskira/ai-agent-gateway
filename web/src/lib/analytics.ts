import { apiFetch } from '@/lib/api'
import type { TimeRange } from '@/lib/overview'
import type { SkillKind } from '@/lib/skills'

/**
 * Data contract for the Skills page's usage columns and "Most used" tile
 * (`GET /api/v1/analytics/skills`, `internal/api/handlers/analytics.go`'s
 * `Skills` method) -- Phase 5 of "skills & commands on agent profiles"
 * (see `SKILLS-CONTRACT.md`). Needs the ClickHouse sink, the same as
 * `/analytics/models`; see `fetchSkillsSummary`.
 *
 * Unlike the Models page, this does not merge with a separate registry
 * fetch: the server itself joins `kind` in from the skill/command
 * registry by name (see `pkg/analytics.SkillSummaryRow`'s doc comment),
 * so every row here already carries everything the page needs. The
 * console still renders the registry's own `Skill[]` (`useSkillsList`)
 * as the table's row set and looks up each one's usage here by name --
 * see `SkillsPage.tsx` -- so a skill with zero traffic still shows up
 * (0 calls), matching how Models merges rows.
 */

/** One skill or command's row of observed gateway traffic. Field names
 * are snake_case, matching `ModelUsageRow`'s own convention (see
 * `web/src/lib/models.ts`). */
export interface SkillUsageRow {
  name: string
  /** "skill" | "command", joined in from the registry by name -- `""`
   * when the name no longer resolves to any registry row (deleted since,
   * or never existed as a registered skill/command). */
  kind: SkillKind | ''
  calls: number
  /** Distinct API keys (`key_id`) that loaded this skill or rendered
   * this command in the window. */
  used_by: number
  last_seen: string
}

/** Names the single highest-`calls` skill/command in a window, for the
 * "Most used" tile. */
export interface SkillsMostUsed {
  name: string
  kind: SkillKind | ''
  calls: number
}

export interface SkillsSummary {
  range: TimeRange
  skills: SkillUsageRow[]
  total_skills: number
  /** `null` when `skills` is empty. */
  most_used: SkillsMostUsed | null
}

/** The range the Skills page always asks for -- same framing as Models'
 * "Observed from gateway traffic over the last 7 days" footer, so there's
 * no range picker on this page either. */
export const SKILLS_TRAFFIC_RANGE: TimeRange = '7d'

/**
 * `GET /api/v1/analytics/skills?range=7d`. Like `fetchModelsSummary`, any
 * failure (404 because ClickHouse isn't enabled, or a network error)
 * resolves to `null` rather than throwing -- the page renders its
 * "analytics are off" state instead of an error screen.
 */
export async function fetchSkillsSummary(
  range: TimeRange = SKILLS_TRAFFIC_RANGE,
): Promise<SkillsSummary | null> {
  try {
    return await apiFetch<SkillsSummary>(`/analytics/skills?range=${range}`)
  } catch {
    return null
  }
}
