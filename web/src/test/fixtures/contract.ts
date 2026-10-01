import raw from './models-api.json?raw'
import type { ModelInput, ModelsSummary, RegisteredModel } from '@/lib/models'
import type { ApiErrorBody } from '@/lib/types'

/**
 * `models-api.json` holds the requests the console sends to the model
 * registry and the responses the REAL handlers give to them. It is not
 * written by hand: `internal/api/handlers/models_contract_test.go` replays
 * each request through `handlers.Models` / `handlers.Analytics` and fails
 * when an answer differs from the file (and regenerates the file with
 * `UPDATE_CONSOLE_FIXTURES=1`). The console's tests use these bodies in
 * place of hand-written ones, so a field the API renames, drops or adds
 * shows up here.
 */
interface Exchange<Req, Res> {
  request: Req
  status: number
  response: Res
}

interface Page<T> {
  items: T[]
  total: number
}

export interface ModelsContract {
  /** What the Add dialog sends for a new model, and the 201 answer. */
  create: Exchange<ModelInput, RegisteredModel>
  /** The model the Edit dialog is opened on. */
  seed: Exchange<ModelInput, RegisteredModel>
  /** `GET /models`: one platform row and the two above. */
  list: Exchange<undefined, Page<RegisteredModel>>
  /** What the Edit dialog sends for the seeded model, and the 200 answer. */
  update: Exchange<ModelInput, RegisteredModel>
  /** A request the API refuses, and its 400 envelope. */
  invalid: Exchange<ModelInput, ApiErrorBody>
  /** `GET /analytics/models?range=7d`. */
  analytics_models: Exchange<undefined, ModelsSummary>
}

export const contract = JSON.parse(raw) as ModelsContract
