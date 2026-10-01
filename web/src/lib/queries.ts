import { useMutation, useQueries, useQuery, useQueryClient } from '@tanstack/react-query'
import { apiFetch } from '@/lib/api'
import type { HealthResponse } from '@/lib/types'
import {
  fetchApiKeys,
  fetchConnectorToolCount,
  fetchConnectors,
  fetchOverviewMetrics,
  fetchProfileToolCount,
  fetchProfiles,
  MAX_TOOL_COUNT_REQUESTS,
  type ConnectorSummary,
  type ProfileSummary,
  type TimeRange,
} from '@/lib/overview'
import { fetchMcpsUsage, fetchSkillsUsage } from '@/lib/discovery'
import { changePassword, getAuthConfig, type ChangePasswordInput } from '@/lib/auth-api'
import {
  createUser,
  deleteUser,
  listAudit,
  listUsers,
  resetUserPassword,
  revokeUserSessions,
  updateUser,
  type CreateUserInput,
  type UpdateUserInput,
} from '@/lib/users'
import {
  createApiKey,
  deleteApiKey,
  listApiKeys,
  rotateApiKey,
  type CreateApiKeyInput,
} from '@/lib/api-keys'
import {
  createCredential,
  deleteCredential,
  getCredential,
  listCredentials,
  updateCredential,
  type CreateCredentialInput,
  type UpdateCredentialInput,
} from '@/lib/credentials'
import {
  checkConnectorHealth,
  createConnector,
  deleteConnector,
  discoverConnectorTools,
  getConnector,
  listConnectors,
  listConnectorTools,
  updateConnector,
  type Connector,
  type ConnectorInput,
} from '@/lib/connectors'
import {
  addCatalogEntry,
  createCatalogEntry,
  deleteCatalogEntry,
  listCatalog,
  updateCatalogEntry,
  type AddCatalogInput,
  type CatalogEntryInput,
} from '@/lib/mcpCatalog'
import {
  createProfile,
  deleteProfile,
  getProfile,
  listProfiles,
  listProfileSkills,
  listProfileTools,
  setProfileSkills,
  setProfileTools,
  updateProfile,
  type ProfileInput,
  type ProfileSkillInput,
  type ProfileTool,
} from '@/lib/profiles'
import {
  clearAllCache,
  clearConnectorCache,
  fetchCacheStats,
  refreshAllCache,
  refreshConnectorCache,
  searchCache,
} from '@/lib/cache'
import { fetchHeaderProviders } from '@/lib/headers'
import { fetchAccessLogDetail, fetchAccessLogs, type AccessLogFilters } from '@/lib/logs'
import { fetchLlmLogDetail, fetchLlmLogs, type LlmLogFilters } from '@/lib/llm-logs'
import { fetchSessionTimeline, type TimelineOrder } from '@/lib/session-timeline'
import { fetchSkillsSummary, SKILLS_TRAFFIC_RANGE } from '@/lib/analytics'
import { fetchTrafficFlow } from '@/lib/sankey'
import {
  createModel,
  deleteModel,
  fetchModelsSummary,
  listRegisteredModels,
  updateModel,
  MODELS_TRAFFIC_RANGE,
  type ModelInput,
} from '@/lib/models'
import {
  applyCatalogPrices,
  connectProvider,
  createCatalogModel,
  createCatalogProvider,
  deleteCatalogModel,
  deleteCatalogProvider,
  fetchModelCatalogSafe,
  getCatalogModelUsage,
  listModelCatalog,
  previewCatalogPrices,
  testProviderConnection,
  updateCatalogModel,
  updateCatalogProvider,
  type ApplyPricesInput,
  type CatalogModelInput,
  type CatalogProviderInput,
  type ConnectProviderInput,
  type PreviewPricesInput,
  type TestConnectionInput,
} from '@/lib/model-catalog'
import {
  createSkill,
  createSkillVersion,
  deleteSkill,
  getSkill,
  listSkills,
  listSkillVersions,
  updateSkill,
  type CreateSkillInput,
  type SkillFileInput,
  type SkillKind,
  type UpdateSkillInput,
} from '@/lib/skills'

/** `GET /api/v1/health` — no auth required. */
export function useHealth() {
  return useQuery({
    queryKey: ['health'],
    queryFn: () => apiFetch<HealthResponse>('/health'),
    staleTime: 60_000,
    retry: false,
  })
}

/** `GET /api/v1/connectors` */
export function useConnectors() {
  return useQuery({
    queryKey: ['connectors'],
    queryFn: fetchConnectors,
    staleTime: 30_000,
  })
}

/** `GET /api/v1/profiles` */
export function useProfiles() {
  return useQuery({
    queryKey: ['profiles'],
    queryFn: fetchProfiles,
    staleTime: 30_000,
  })
}

/** `GET /api/v1/api-keys` — admin-only, resolves to `null` on a 403. */
export function useApiKeys() {
  return useQuery({
    queryKey: ['api-keys'],
    queryFn: fetchApiKeys,
    staleTime: 30_000,
  })
}

/** `GET /api/v1/analytics/overview?range=…` — resolves to `null` until the
 * analytics service ships. */
export function useOverviewMetrics(range: TimeRange) {
  return useQuery({
    queryKey: ['overview-metrics', range],
    queryFn: () => fetchOverviewMetrics(range),
    staleTime: 30_000,
  })
}

/** `GET /api/v1/analytics/traffic-flow?range=…&client_name=…` — resolves
 * to `null` on any failure (404 because ClickHouse isn't enabled, or a
 * network error), same convention as `useOverviewMetrics`. */
export function useTrafficFlow(range: TimeRange, clientName: string) {
  return useQuery({
    queryKey: ['traffic-flow', range, clientName],
    queryFn: () => fetchTrafficFlow(range, clientName),
    staleTime: 30_000,
  })
}

/** Cached tool count per connector, capped at `MAX_TOOL_COUNT_REQUESTS`
 * connectors so an Overview render never fires an unbounded fan-out. */
export function useConnectorToolCounts(connectors: ConnectorSummary[] | undefined) {
  const targets = (connectors ?? []).slice(0, MAX_TOOL_COUNT_REQUESTS)
  return useQueries({
    queries: targets.map((c) => ({
      queryKey: ['connector-tools', c.id],
      queryFn: () => fetchConnectorToolCount(c.id),
      staleTime: 30_000,
    })),
  })
}

/** Tool count per profile, same cap as `useConnectorToolCounts`. */
export function useProfileToolCounts(profiles: ProfileSummary[] | undefined) {
  const targets = (profiles ?? []).slice(0, MAX_TOOL_COUNT_REQUESTS)
  return useQueries({
    queries: targets.map((p) => ({
      queryKey: ['profile-tools', p.id],
      queryFn: () => fetchProfileToolCount(p.id),
      staleTime: 30_000,
    })),
  })
}

/* ------------------------------------------------------------------------ */
/* Connectors page                                                          */
/* ------------------------------------------------------------------------ */

export function useConnectorsList() {
  return useQuery({ queryKey: ['connectors', 'list'], queryFn: listConnectors })
}

export function useConnector(id: string | undefined) {
  return useQuery({
    queryKey: ['connectors', 'detail', id],
    queryFn: () => getConnector(id!),
    enabled: !!id,
  })
}

export function useConnectorTools(id: string | undefined) {
  return useQuery({
    queryKey: ['connectors', 'tools', id],
    queryFn: () => listConnectorTools(id!),
    enabled: !!id,
  })
}

/** All connectors' cached tool lists at once — feeds the Profile Studio
 * tool manager's left pane. Not capped: the tool manager is a dedicated
 * page, unlike the Overview's fan-out. */
export function useAllConnectorTools(connectors: { id: string }[] | undefined) {
  const targets = connectors ?? []
  return useQueries({
    queries: targets.map((c) => ({
      queryKey: ['connectors', 'tools', c.id],
      queryFn: () => listConnectorTools(c.id),
      staleTime: 30_000,
    })),
  })
}

export function useConnectorHealth(id: string | undefined, enabled: boolean) {
  return useQuery({
    queryKey: ['connectors', 'health', id],
    queryFn: () => checkConnectorHealth(id!),
    enabled: !!id && enabled,
    staleTime: 0,
    retry: false,
  })
}

/** `GET /api/v1/mcp-catalog`: fetched for the Catalog tab, and for the MCPs tab
 * only when a Discovered server needs matching against it (`enabled`). */
export function useMcpCatalog(enabled = true) {
  return useQuery({ queryKey: ['mcp-catalog'], queryFn: listCatalog, enabled })
}

/** `POST /api/v1/mcp-catalog/{slug}/add`: creates a tenant connector. */
export function useAddCatalogEntry() {
  const queryClient = useQueryClient()
  return useMutation({
    mutationFn: ({ slug, input }: { slug: string; input: AddCatalogInput }) =>
      addCatalogEntry(slug, input),
    onSuccess: () => {
      queryClient.invalidateQueries({ queryKey: ['connectors'] })
      queryClient.invalidateQueries({ queryKey: ['mcp-catalog'] })
      queryClient.invalidateQueries({ queryKey: ['mcps', 'usage'] })
    },
  })
}

/** Create (no `slug` given to update) or replace a tenant catalog entry. */
export function useSaveCatalogEntry() {
  const queryClient = useQueryClient()
  return useMutation({
    mutationFn: ({ editSlug, input }: { editSlug?: string; input: CatalogEntryInput }) =>
      editSlug ? updateCatalogEntry(editSlug, input) : createCatalogEntry(input),
    onSuccess: () => {
      queryClient.invalidateQueries({ queryKey: ['mcp-catalog'] })
    },
  })
}

export function useDeleteCatalogEntry() {
  const queryClient = useQueryClient()
  return useMutation({
    mutationFn: (slug: string) => deleteCatalogEntry(slug),
    onSuccess: () => {
      queryClient.invalidateQueries({ queryKey: ['mcp-catalog'] })
    },
  })
}

export function useCreateConnector() {
  const queryClient = useQueryClient()
  return useMutation({
    mutationFn: (input: ConnectorInput) => createConnector(input),
    onSuccess: () => {
      queryClient.invalidateQueries({ queryKey: ['connectors'] })
      queryClient.invalidateQueries({ queryKey: ['mcps', 'usage'] })
    },
  })
}

export function useUpdateConnector(id: string) {
  const queryClient = useQueryClient()
  return useMutation({
    mutationFn: (input: ConnectorInput) => updateConnector(id, input),
    onSuccess: () => {
      queryClient.invalidateQueries({ queryKey: ['connectors'] })
      queryClient.invalidateQueries({ queryKey: ['mcps', 'usage'] })
    },
  })
}

/** Turns a connector on or off by rewriting `metadata.enabled`. The PUT replaces
 * metadata wholesale, so the rest of it is sent back as read (masked header
 * values round-trip untouched). */
export function useSetConnectorEnabled() {
  const queryClient = useQueryClient()
  return useMutation({
    mutationFn: ({ connector, enabled }: { connector: Connector; enabled: boolean }) => {
      const { enabled: _prev, ...rest } = connector.metadata ?? {}
      void _prev
      return updateConnector(connector.id, {
        name: connector.name,
        slug: connector.slug,
        endpoint: connector.endpoint,
        timeout_ms: connector.timeout_ms,
        metadata: enabled ? rest : { ...rest, enabled: false },
      })
    },
    onSuccess: () => {
      queryClient.invalidateQueries({ queryKey: ['connectors'] })
      queryClient.invalidateQueries({ queryKey: ['mcp-catalog'] })
      queryClient.invalidateQueries({ queryKey: ['mcps', 'usage'] })
    },
  })
}

export function useDeleteConnector() {
  const queryClient = useQueryClient()
  return useMutation({
    mutationFn: (id: string) => deleteConnector(id),
    onSuccess: () => {
      queryClient.invalidateQueries({ queryKey: ['connectors'] })
      queryClient.invalidateQueries({ queryKey: ['mcps', 'usage'] })
    },
  })
}

export function useDiscoverConnectorTools(id: string) {
  const queryClient = useQueryClient()
  return useMutation({
    mutationFn: () => discoverConnectorTools(id),
    onSuccess: () => {
      queryClient.invalidateQueries({ queryKey: ['connectors', 'tools', id] })
    },
  })
}

/* ------------------------------------------------------------------------ */
/* Profiles page + Profile Studio tool manager                              */
/* ------------------------------------------------------------------------ */

export function useProfilesList() {
  return useQuery({ queryKey: ['profiles', 'list'], queryFn: listProfiles })
}

export function useProfile(id: string | undefined) {
  return useQuery({
    queryKey: ['profiles', 'detail', id],
    queryFn: () => getProfile(id!),
    enabled: !!id,
  })
}

export function useProfileTools(id: string | undefined) {
  return useQuery({
    queryKey: ['profiles', 'tools', id],
    queryFn: () => listProfileTools(id!),
    enabled: !!id,
  })
}

/** Every profile's tool set at once — feeds the Profiles table's "Tools"
 * count column. Not capped, same reasoning as `useAllConnectorTools`. */
export function useAllProfileTools(profiles: { id: string }[] | undefined) {
  const targets = profiles ?? []
  return useQueries({
    queries: targets.map((p) => ({
      queryKey: ['profiles', 'tools', p.id],
      queryFn: () => listProfileTools(p.id),
      staleTime: 30_000,
    })),
  })
}

export function useCreateProfile() {
  const queryClient = useQueryClient()
  return useMutation({
    mutationFn: (input: ProfileInput) => createProfile(input),
    onSuccess: () => {
      queryClient.invalidateQueries({ queryKey: ['profiles'] })
    },
  })
}

export function useUpdateProfile(id: string) {
  const queryClient = useQueryClient()
  return useMutation({
    mutationFn: (input: ProfileInput) => updateProfile(id, input),
    onSuccess: () => {
      queryClient.invalidateQueries({ queryKey: ['profiles'] })
    },
  })
}

export function useDeleteProfile() {
  const queryClient = useQueryClient()
  return useMutation({
    mutationFn: (id: string) => deleteProfile(id),
    onSuccess: () => {
      queryClient.invalidateQueries({ queryKey: ['profiles'] })
    },
  })
}

export function useSetProfileTools(id: string) {
  const queryClient = useQueryClient()
  return useMutation({
    mutationFn: (tools: ProfileTool[]) => setProfileTools(id, tools),
    onSuccess: () => {
      queryClient.invalidateQueries({ queryKey: ['profiles', 'tools', id] })
      queryClient.invalidateQueries({ queryKey: ['profiles', 'list'] })
    },
  })
}

/** `GET /api/v1/profiles/{id}/skills` — the profile's attached skills and
 * commands, feeding Profile Studio's "Skills & commands" tab. */
export function useProfileSkills(id: string | undefined) {
  return useQuery({
    queryKey: ['profiles', 'skills', id],
    queryFn: () => listProfileSkills(id!),
    enabled: !!id,
  })
}

/** `PUT /api/v1/profiles/{id}/skills` — replace semantics, same pattern as
 * `useSetProfileTools`. */
export function useSetProfileSkills(id: string) {
  const queryClient = useQueryClient()
  return useMutation({
    mutationFn: (items: ProfileSkillInput[]) => setProfileSkills(id, items),
    onSuccess: () => {
      queryClient.invalidateQueries({ queryKey: ['profiles', 'skills', id] })
      queryClient.invalidateQueries({ queryKey: ['profiles', 'list'] })
    },
  })
}

/* ------------------------------------------------------------------------ */
/* API Keys page                                                            */
/* ------------------------------------------------------------------------ */

export function useApiKeysList() {
  return useQuery({ queryKey: ['api-keys', 'list'], queryFn: listApiKeys })
}

export function useCreateApiKey() {
  const queryClient = useQueryClient()
  return useMutation({
    mutationFn: (input: CreateApiKeyInput) => createApiKey(input),
    onSuccess: () => {
      queryClient.invalidateQueries({ queryKey: ['api-keys'] })
    },
  })
}

export function useDeleteApiKey() {
  const queryClient = useQueryClient()
  return useMutation({
    mutationFn: (id: string) => deleteApiKey(id),
    onSuccess: () => {
      queryClient.invalidateQueries({ queryKey: ['api-keys'] })
    },
  })
}

export function useRotateApiKey() {
  const queryClient = useQueryClient()
  return useMutation({
    mutationFn: (id: string) => rotateApiKey(id),
    onSuccess: () => {
      queryClient.invalidateQueries({ queryKey: ['api-keys'] })
    },
  })
}

/* ------------------------------------------------------------------------ */
/* Credentials page                                                         */
/* ------------------------------------------------------------------------ */

export function useCredentialsList() {
  return useQuery({ queryKey: ['credentials', 'list'], queryFn: listCredentials })
}

export function useCredential(name: string | undefined) {
  return useQuery({
    queryKey: ['credentials', 'detail', name],
    queryFn: () => getCredential(name!),
    enabled: !!name,
  })
}

export function useCreateCredential() {
  const queryClient = useQueryClient()
  return useMutation({
    mutationFn: (input: CreateCredentialInput) => createCredential(input),
    onSuccess: () => {
      queryClient.invalidateQueries({ queryKey: ['credentials'] })
    },
  })
}

export function useUpdateCredential(name: string) {
  const queryClient = useQueryClient()
  return useMutation({
    mutationFn: (input: UpdateCredentialInput) => updateCredential(name, input),
    onSuccess: () => {
      queryClient.invalidateQueries({ queryKey: ['credentials'] })
    },
  })
}

export function useDeleteCredential() {
  const queryClient = useQueryClient()
  return useMutation({
    mutationFn: (name: string) => deleteCredential(name),
    onSuccess: () => {
      queryClient.invalidateQueries({ queryKey: ['credentials'] })
    },
  })
}

/* ------------------------------------------------------------------------ */
/* Tool Search page                                                         */
/* ------------------------------------------------------------------------ */

export function useCacheSearch(q: string, includeStale = false) {
  return useQuery({
    queryKey: ['cache', 'search', q, includeStale],
    queryFn: () => searchCache(q, undefined, includeStale),
    enabled: q.trim().length > 0,
  })
}

/* ------------------------------------------------------------------------ */
/* Cache page                                                               */
/* ------------------------------------------------------------------------ */

export function useCacheStats() {
  return useQuery({ queryKey: ['cache', 'stats'], queryFn: fetchCacheStats })
}

export function useRefreshCache() {
  const queryClient = useQueryClient()
  return useMutation({
    mutationFn: () => refreshAllCache(),
    onSuccess: () => {
      queryClient.invalidateQueries({ queryKey: ['cache'] })
    },
  })
}

export function useRefreshConnectorCache() {
  const queryClient = useQueryClient()
  return useMutation({
    mutationFn: (connectorId: string) => refreshConnectorCache(connectorId),
    onSuccess: () => {
      queryClient.invalidateQueries({ queryKey: ['cache'] })
    },
  })
}

export function useClearCache() {
  const queryClient = useQueryClient()
  return useMutation({
    mutationFn: () => clearAllCache(),
    onSuccess: () => {
      queryClient.invalidateQueries({ queryKey: ['cache'] })
    },
  })
}

export function useClearConnectorCache() {
  const queryClient = useQueryClient()
  return useMutation({
    mutationFn: (connectorId: string) => clearConnectorCache(connectorId),
    onSuccess: () => {
      queryClient.invalidateQueries({ queryKey: ['cache'] })
    },
  })
}

/* ------------------------------------------------------------------------ */
/* Access Logs page                                                         */
/* ------------------------------------------------------------------------ */

export function useAccessLogsList(
  filters: AccessLogFilters,
  limit: number,
  offset: number,
) {
  return useQuery({
    queryKey: ['access-logs', 'list', filters, limit, offset],
    queryFn: () => fetchAccessLogs(filters, limit, offset),
    placeholderData: (prev) => prev,
  })
}

/** Admin-only detail (403 for agents) — `enabled` gates the fetch on the
 * drawer being open AND the signed-in principal being an admin, so an
 * agent's session never even attempts the call. */
export function useAccessLogDetail(requestId: string | undefined, enabled: boolean) {
  return useQuery({
    queryKey: ['access-logs', 'detail', requestId],
    queryFn: () => fetchAccessLogDetail(requestId!),
    enabled: !!requestId && enabled,
    retry: false,
  })
}

/* ------------------------------------------------------------------------ */
/* LLM Logs page                                                            */
/* ------------------------------------------------------------------------ */

export function useLlmLogsList(filters: LlmLogFilters, limit: number, offset: number) {
  return useQuery({
    queryKey: ['llm-logs', 'list', filters, limit, offset],
    queryFn: () => fetchLlmLogs(filters, limit, offset),
    placeholderData: (prev) => prev,
  })
}

export function useLlmLogDetail(requestId: string | undefined, enabled: boolean) {
  return useQuery({
    queryKey: ['llm-logs', 'detail', requestId],
    queryFn: () => fetchLlmLogDetail(requestId!),
    enabled: !!requestId && enabled,
    retry: false,
  })
}

/* ------------------------------------------------------------------------ */
/* Session Timeline page                                                    */
/* ------------------------------------------------------------------------ */

export function useSessionTimeline(sessionId: string, order: TimelineOrder = 'asc') {
  return useQuery({
    queryKey: ['session-timeline', sessionId, order],
    queryFn: () => fetchSessionTimeline(sessionId, order),
    enabled: !!sessionId,
  })
}

/* ------------------------------------------------------------------------ */
/* Header providers (Connectors create/edit dialog)                         */
/* ------------------------------------------------------------------------ */

export function useHeaderProviders() {
  return useQuery({
    queryKey: ['headers', 'providers'],
    queryFn: fetchHeaderProviders,
    staleTime: 5 * 60_000,
  })
}

/* ------------------------------------------------------------------------ */
/* Models page                                                              */
/* ------------------------------------------------------------------------ */

/** `GET /api/v1/models` — the model registry (Phase 1, landing alongside
 * this). Not wrapped in a try/catch like `fetchModelsSummary`: a 403 here
 * is a real "you need an admin key" signal the page acts on
 * (`ApiError`), not a "not deployed yet" one to swallow. */
export function useModelsRegistryList() {
  return useQuery({
    queryKey: ['models', 'registry', 'list'],
    queryFn: listRegisteredModels,
  })
}

/** `GET /api/v1/analytics/models?range=7d` — resolves to `null` when
 * ClickHouse isn't enabled (see `fetchModelsSummary`). */
export function useModelsSummary() {
  return useQuery({
    queryKey: ['models', 'summary', MODELS_TRAFFIC_RANGE],
    queryFn: () => fetchModelsSummary(MODELS_TRAFFIC_RANGE),
    staleTime: 30_000,
  })
}

export function useCreateModel() {
  const queryClient = useQueryClient()
  return useMutation({
    mutationFn: (input: ModelInput) => createModel(input),
    onSuccess: () => {
      queryClient.invalidateQueries({ queryKey: ['models'] })
    },
  })
}

export function useUpdateModel(id: string) {
  const queryClient = useQueryClient()
  return useMutation({
    mutationFn: (input: ModelInput) => updateModel(id, input),
    onSuccess: () => {
      queryClient.invalidateQueries({ queryKey: ['models'] })
    },
  })
}

export function useDeleteModel() {
  const queryClient = useQueryClient()
  return useMutation({
    mutationFn: (id: string) => deleteModel(id),
    onSuccess: () => {
      queryClient.invalidateQueries({ queryKey: ['models'] })
    },
  })
}

/* ------------------------------------------------------------------------ */
/* Model Catalog (`/model-catalog` admin page, Connect dialog off Models)   */
/* ------------------------------------------------------------------------ */

/** `GET /api/v1/model-catalog`, raw — throws on a 403/network failure. Used
 * by `ModelCatalogPage`, which needs to tell "not permitted" apart from
 * "empty catalog" (see `AdminOnlyNotice`-style pages elsewhere). */
export function useModelCatalogList() {
  return useQuery({ queryKey: ['model-catalog', 'list'], queryFn: listModelCatalog })
}

/** Safe variant for the Models page: resolves to `null` on any failure
 * instead of throwing, so a principal without `model.read` (or the
 * catalog being unreachable) just means no "Available" rows — not a
 * broken page. See `fetchModelCatalogSafe`. */
export function useModelCatalogSummary() {
  return useQuery({
    queryKey: ['model-catalog', 'summary'],
    queryFn: fetchModelCatalogSafe,
    staleTime: 30_000,
  })
}

export function useCreateCatalogProvider() {
  const queryClient = useQueryClient()
  return useMutation({
    mutationFn: (input: CatalogProviderInput) => createCatalogProvider(input),
    onSuccess: () => {
      queryClient.invalidateQueries({ queryKey: ['model-catalog'] })
    },
  })
}

export function useUpdateCatalogProvider(id: string) {
  const queryClient = useQueryClient()
  return useMutation({
    mutationFn: (input: CatalogProviderInput) => updateCatalogProvider(id, input),
    onSuccess: () => {
      queryClient.invalidateQueries({ queryKey: ['model-catalog'] })
    },
  })
}

export function useDeleteCatalogProvider() {
  const queryClient = useQueryClient()
  return useMutation({
    mutationFn: ({ id, force }: { id: string; force?: boolean }) =>
      deleteCatalogProvider(id, { force }),
    onSuccess: () => {
      queryClient.invalidateQueries({ queryKey: ['model-catalog'] })
    },
  })
}

export function useCreateCatalogModel(providerId: string) {
  const queryClient = useQueryClient()
  return useMutation({
    mutationFn: (input: CatalogModelInput) => createCatalogModel(providerId, input),
    onSuccess: () => {
      queryClient.invalidateQueries({ queryKey: ['model-catalog'] })
    },
  })
}

export function useUpdateCatalogModel(id: string) {
  const queryClient = useQueryClient()
  return useMutation({
    mutationFn: (input: CatalogModelInput) => updateCatalogModel(id, input),
    onSuccess: () => {
      queryClient.invalidateQueries({ queryKey: ['model-catalog'] })
    },
  })
}

export function useDeleteCatalogModel() {
  const queryClient = useQueryClient()
  return useMutation({
    mutationFn: ({ id, force }: { id: string; force?: boolean }) =>
      deleteCatalogModel(id, { force }),
    onSuccess: () => {
      queryClient.invalidateQueries({ queryKey: ['model-catalog'] })
    },
  })
}

export function useCatalogModelUsage(id: string | undefined) {
  return useQuery({
    queryKey: ['model-catalog', 'usage', id],
    queryFn: () => getCatalogModelUsage(id!),
    enabled: !!id,
  })
}

/** `POST /model-catalog/providers/{id}/test` — stateless; not cached. */
export function useTestProviderConnection(providerId: string) {
  return useMutation({
    mutationFn: (input: TestConnectionInput) => testProviderConnection(providerId, input),
  })
}

/** `POST /model-catalog/providers/{id}/connect` — invalidates models,
 * credentials AND the catalog itself (the connected models now carry
 * `tenant_model_id`, which flips their rows from "available" to
 * "registered"/"active" on both the Models page and the admin catalog
 * page's per-provider table). */
export function useConnectProvider(providerId: string) {
  const queryClient = useQueryClient()
  return useMutation({
    mutationFn: (input: ConnectProviderInput) => connectProvider(providerId, input),
    onSuccess: () => {
      queryClient.invalidateQueries({ queryKey: ['models'] })
      queryClient.invalidateQueries({ queryKey: ['credentials'] })
      queryClient.invalidateQueries({ queryKey: ['model-catalog'] })
    },
  })
}

/** `POST /model-catalog/providers/{id}/prices/preview` — read-only, not
 * cached (same convention as `useTestProviderConnection`). */
export function usePreviewCatalogPrices(providerId: string) {
  return useMutation({
    mutationFn: (input: PreviewPricesInput) => previewCatalogPrices(providerId, input),
  })
}

/** `POST /model-catalog/providers/{id}/prices/apply` — invalidates the
 * catalog (new prices) and, since `update_tenant_models` may have touched
 * tenant rows too, the models page's own list. */
export function useApplyCatalogPrices(providerId: string) {
  const queryClient = useQueryClient()
  return useMutation({
    mutationFn: (input: ApplyPricesInput) => applyCatalogPrices(providerId, input),
    onSuccess: () => {
      queryClient.invalidateQueries({ queryKey: ['model-catalog'] })
      queryClient.invalidateQueries({ queryKey: ['models'] })
    },
  })
}

/* ------------------------------------------------------------------------ */
/* Skills page                                                              */
/* ------------------------------------------------------------------------ */

/** `GET /api/v1/skills?kind=…` — `skill.read` (an agent's built-in
 * `*.read` role covers it too, unlike the model registry). */
export function useSkillsList(kind?: SkillKind) {
  return useQuery({
    queryKey: ['skills', 'list', kind ?? 'all'],
    queryFn: () => listSkills(kind),
  })
}

/** `GET /api/v1/analytics/skills?range=7d` — resolves to `null` when
 * ClickHouse isn't enabled (see `fetchSkillsSummary`). */
export function useSkillsSummary(range: TimeRange = SKILLS_TRAFFIC_RANGE) {
  return useQuery({
    queryKey: ['skills', 'summary', range],
    queryFn: () => fetchSkillsSummary(range),
    staleTime: 30_000,
  })
}

/** `GET /api/v1/analytics/skills/usage?range=…` — skills the model was seen
 * using, registered or not; `null` when analytics is off. */
export function useSkillsUsage(range: TimeRange) {
  return useQuery({
    queryKey: ['skills', 'usage', range],
    queryFn: () => fetchSkillsUsage(range),
    staleTime: 30_000,
  })
}

/** `GET /api/v1/analytics/mcps/usage?range=…`; `null` when analytics is off. */
export function useMcpsUsage(range: TimeRange) {
  return useQuery({
    queryKey: ['mcps', 'usage', range],
    queryFn: () => fetchMcpsUsage(range),
    staleTime: 30_000,
  })
}

/** `GET /api/v1/skills/{id}` — the row plus its latest version's files. */
export function useSkill(id: string | undefined) {
  return useQuery({
    queryKey: ['skills', 'detail', id],
    queryFn: () => getSkill(id!),
    enabled: !!id,
  })
}

/** `GET /api/v1/skills/{id}/versions` */
export function useSkillVersions(id: string | undefined) {
  return useQuery({
    queryKey: ['skills', 'versions', id],
    queryFn: () => listSkillVersions(id!),
    enabled: !!id,
  })
}

export function useCreateSkill() {
  const queryClient = useQueryClient()
  return useMutation({
    mutationFn: (input: CreateSkillInput) => createSkill(input),
    onSuccess: () => {
      queryClient.invalidateQueries({ queryKey: ['skills'] })
    },
  })
}

/** `PUT /api/v1/skills/{id}` — description/enabled/metadata/arguments
 * only; platform rows 403. */
export function useUpdateSkill(id: string) {
  const queryClient = useQueryClient()
  return useMutation({
    mutationFn: (input: UpdateSkillInput) => updateSkill(id, input),
    onSuccess: () => {
      queryClient.invalidateQueries({ queryKey: ['skills', 'detail', id] })
      queryClient.invalidateQueries({ queryKey: ['skills', 'list'] })
    },
  })
}

export function useDeleteSkill() {
  const queryClient = useQueryClient()
  return useMutation({
    mutationFn: (id: string) => deleteSkill(id),
    onSuccess: () => {
      queryClient.invalidateQueries({ queryKey: ['skills'] })
    },
  })
}

/** `POST /api/v1/skills/{id}/versions` — files only; the detail sheet's
 * "New version" editor. */
export function useCreateSkillVersion(id: string) {
  const queryClient = useQueryClient()
  return useMutation({
    mutationFn: (files: SkillFileInput[]) => createSkillVersion(id, files),
    onSuccess: () => {
      queryClient.invalidateQueries({ queryKey: ['skills', 'detail', id] })
      queryClient.invalidateQueries({ queryKey: ['skills', 'versions', id] })
      queryClient.invalidateQueries({ queryKey: ['skills', 'list'] })
    },
  })
}

/* ------------------------------------------------------------------------ */
/* Password login, Users page                                               */
/* ------------------------------------------------------------------------ */

/** `GET /api/v1/auth/config` (public): which login methods the login page shows. */
export function useAuthConfig() {
  return useQuery({
    queryKey: ['auth', 'config'],
    queryFn: getAuthConfig,
    retry: false,
    staleTime: 60_000,
  })
}

export function useChangePassword() {
  return useMutation({
    mutationFn: (input: ChangePasswordInput) => changePassword(input),
  })
}

export function useUsersList() {
  return useQuery({ queryKey: ['users', 'list'], queryFn: listUsers, retry: false })
}

export function useCreateUser() {
  const queryClient = useQueryClient()
  return useMutation({
    mutationFn: (input: CreateUserInput) => createUser(input),
    onSuccess: () => {
      queryClient.invalidateQueries({ queryKey: ['users'] })
      queryClient.invalidateQueries({ queryKey: ['auth-audit'] })
    },
  })
}

export function useUpdateUser() {
  const queryClient = useQueryClient()
  return useMutation({
    mutationFn: ({ id, input }: { id: string; input: UpdateUserInput }) =>
      updateUser(id, input),
    onSuccess: () => {
      queryClient.invalidateQueries({ queryKey: ['users'] })
      queryClient.invalidateQueries({ queryKey: ['auth-audit'] })
    },
  })
}

export function useDeleteUser() {
  const queryClient = useQueryClient()
  return useMutation({
    mutationFn: (id: string) => deleteUser(id),
    onSuccess: () => {
      queryClient.invalidateQueries({ queryKey: ['users'] })
      queryClient.invalidateQueries({ queryKey: ['auth-audit'] })
    },
  })
}

export function useResetUserPassword() {
  const queryClient = useQueryClient()
  return useMutation({
    mutationFn: (id: string) => resetUserPassword(id),
    onSuccess: () => {
      queryClient.invalidateQueries({ queryKey: ['users'] })
      queryClient.invalidateQueries({ queryKey: ['auth-audit'] })
    },
  })
}

export function useRevokeUserSessions() {
  const queryClient = useQueryClient()
  return useMutation({
    mutationFn: (id: string) => revokeUserSessions(id),
    onSuccess: () => {
      queryClient.invalidateQueries({ queryKey: ['auth-audit'] })
    },
  })
}

export function useAuthAudit() {
  return useQuery({ queryKey: ['auth-audit', 'list'], queryFn: listAudit, retry: false })
}
