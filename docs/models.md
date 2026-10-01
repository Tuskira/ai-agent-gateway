# Model catalog and the Models page

This page covers the **Model Catalog** — a platform-wide list of known
LLM providers and their models that a tenant connects with one click —
and how it shows up on the console's **Models** page alongside the
tenant-scoped [model registry](llm-plane.md#model-registry). For the
registry itself (targets, labels, translation, fallback) see
[llm-plane.md](llm-plane.md#model-registry); this page is about the
catalog layered on top of it.

## Catalog vs. registry

- **Registry** (`models` table, `/api/v1/models`) — tenant-scoped rows a
  tenant creates by hand (or a catalog "connects" on its behalf). Only a
  registered model is routed; an unregistered name is forwarded unchanged
  to the provider its route names.
- **Catalog** (`model_catalog_providers` / `model_catalog_models` tables,
  `/api/v1/model-catalog`) — a platform-wide, not-tenant-scoped list of
  known providers (Nebius, Together AI, OpenAI, Google Gemini by default)
  and the models each offers. A catalog row is a **template**: it is never
  itself callable. "Connecting" a provider reads a catalog row and creates
  an ordinary registry row from it, so everything downstream (routing,
  labels, translation, budgets) works exactly as it does for a model
  someone typed into the model form by hand.

Editing a catalog provider or model after a tenant has already connected
from it does **not** change that tenant's existing registry row — the
connect step copies `base_url`, `model_id`, `price` and `capabilities` at
that moment (and names the row after the model's suggested name, with the
provider slug as the target's `label`); it does not keep them in sync
afterwards.

## The five states on the Models page

The Models page merges three sources — the tenant's registry, its traffic
summary, and (if `model.read` allows it) the catalog — into one row set,
each row in exactly one of five states:

| State | Meaning |
|---|---|
| **Active** | Registered and seen in traffic. |
| **Registered** | Registered, enabled, not yet seen in traffic. |
| **Disabled** | Registered, disabled, not seen in traffic. (A disabled model that *is* seen in traffic still shows "Active" — the disabled check happens at call time, not in this summary.) |
| **Discovered** | Seen in traffic under a name with no registry record at all — the gateway forwarded it byte-for-byte, untranslated, unrouted. No row actions, except "Set up" (below) when its name happens to match a catalog model. |
| **Available** | A catalog model this tenant has not connected — no registry record, no traffic. Shown with its provider's display name and a single "Set up" action. |

An "Available" row and a matched "Discovered" row both offer a **Set up**
row action, which opens the Connect dialog described below. Once
connected, the row becomes an ordinary Registered/Active row — its
provider column shows the catalog provider's friendly name (e.g. "Nebius
AI Studio") instead of the raw `openai_compat` vendor label.

## Connect flow

From the Models page or the admin catalog page, "Set up" opens **Connect
`<Provider>`**:

1. **Pick models.** Checkboxes for every model the provider offers in the
   catalog; a model already connected by this tenant shows
   checked-and-disabled ("Already set up as `<registry name>`"). The model
   that was clicked is preselected.
2. **Supply a credential.** Either pick an existing credential (the dialog
   starts on the one named `<slug>-api-key`, e.g. `nebius-api-key`, when it
   exists) or enter a new API key, stored as `<slug>-api-key` unless you
   name it. Connect never overwrites a credential: a new key under a name
   already taken is refused with `409`. To change a stored key, rotate it
   on the Credentials page.
3. **Test connection** (optional but recommended) calls the provider's own
   `/models` endpoint with the given key and reports, per selected model,
   whether the provider actually lists it.
4. Each selected model shows any capability/price gaps before you commit:
   - `tools: false` → "No tool calling: will not work with Claude Code or
     other agents that use tools."
   - `tools: null` → "Tool support unknown."
   - `price: null` → "No price set: cost and dollar budgets won't apply
     until an admin sets one."
   - A catalog `notes` field, when the admin has set one, shows alongside
     these — e.g. explaining *why* tool calling is unsupported for a
     particular model.
5. **Connect** creates the credential (if new) and one registry model per
   selected catalog model, in a single transaction. Re-running Connect for
   a model already set up is a no-op (`status: "exists"`), not an error.

Deleting a credential that a registered model's target still references
warns and lists the affected models on the Credentials page. Deleting a
model on the Models page shows whether its credential is still used
elsewhere, and offers to delete it too when nothing else uses it.

## Base URL convention

A catalog provider's `base_url` is OpenAI-compatible and **includes the
API version segment**, the same convention the OpenAI SDKs use for their
own base URL:

| Provider | `base_url` |
|---|---|
| Nebius AI Studio | `https://api.tokenfactory.eu-west2.nebius.com/v1` |
| Together AI | `https://api.together.ai/v1` |
| OpenAI | `https://api.openai.com/v1` |
| Google Gemini | `https://generativelanguage.googleapis.com/v1beta/openai` |

This is the same rule the registry applies to every `openai_compat`
target — see [llm-plane.md's `base_url`
convention](llm-plane.md#model-registry) for how the gateway strips a
client's leading `/v1` before joining so a same-dialect OpenAI passthrough
doesn't double the version segment. The catalog API warns (a non-fatal
`warnings` field on create/update) when a `base_url` looks like it's
missing a version-like segment (`/v1`, `/v1beta`, `/openai`, …).

## Permissions

- **Catalog reads** (`GET /model-catalog`) — the ordinary `model.read`
  permission. Every tenant with model read access sees the full catalog,
  merged with its own registrations.
- **Catalog writes** (create/edit/delete a provider or model) —
  `platform.catalog.manage`. A `platform-admin` API key has it (the first
  key `gateway bootstrap-key` creates, or one made with `--platform`); a
  tenant `admin` key does not. A console
  `admin` user only has it when the gateway is running **single-tenant**
  (`GET /auth/config` reports `single_tenant: true`); in a multi-tenant
  deployment the catalog is shared platform-wide, so managing it stays
  with platform-admin API keys, and the admin catalog page shows a read-only banner
  explaining why. See [security-model.md](security-model.md)'s
  "Identity: keys, roles, permissions" section (`platform.catalog.manage`)
  for the full rule.
- **Connect** — `model.create` plus `credential.create` (and
  `credential.read` when reusing an existing credential). The same
  permissions a tenant already needs to create a model and a credential by
  hand.

## Price, capabilities and notes

`price` (`{input, output}`, USD per 1M tokens) and `capabilities`
(`{tools, vision, streaming, max_context}`, each `true`/`false`/`null`)
mirror the same fields on a registry model. The default catalog ships with
**no prices** (never a guessed number); every seeded model sets `tools` and
`streaming`, and leaves `vision` and `max_context` unknown.
Connecting a model copies its catalog price and capabilities onto the new
registry row as a starting point; an admin can change either afterwards on
the registry model itself without touching the catalog.

`notes` is a free-text field an admin can set on a catalog model to
explain a limited or unknown capability above it — shown to a tenant in
the Connect dialog and in the admin catalog page's model table. The
default catalog sets it on the models known not to support multi-turn
tool use through the Chat Completions translation path (`gpt-6-astra`,
`gpt-6.1-sol`, `gpt-6-luna`, `gemini-3.8-flash`, `gemini-3.7-flash`; plain
chat still works): "Multi-turn tool use through Chat Completions
translation is not supported yet; plain chat works."
`moonshotai/Kimi-K3` and `zai-org/GLM-5.3` have no such note — both tool
calling and streaming are supported.

## Admin catalog page

`/model-catalog`, under **Configure** in the console nav (hidden for a
principal without `model.read`, read-only with a banner for one without
`platform.catalog.manage`):

- A **providers** table — add, edit or delete a provider (slug, display
  name, base URL, docs URL, enabled), with base-URL warnings shown inline.
- Per provider, expand the row for its **models** table — add, edit or
  delete a model (vendor model id, display name, suggested registry name,
  price, capabilities, notes, enabled).
- Deleting a provider or model still referenced by a tenant's registry
  model is refused (`409` with a usage count) unless confirmed with
  `?force=true`; force-deleting the catalog row never touches the tenant's
  existing registry model, which keeps working exactly as connected.

## Refreshing catalog prices

Catalog prices (typed by an admin, or written by an earlier refresh)
drift as vendors change their rate cards. **Refresh prices**, a provider
row action on the admin catalog page shown only for providers whose slug has a price source wired up (`nebius` and
`together`), fetches a provider's current prices straight from that
provider's own pricing API and lets an admin review the diff before
writing anything:

1. **Fetch.** For `nebius` the fetch needs no key — its pricing feed
   (`https://tokenfactory.nebius.com/api/public/models_info`, distinct
   from the OpenAI-compatible inference host a `nebius` provider's
   `base_url` points at) is public. For `together` it calls
   `GET {base_url}/models` with a key — the dialog offers an existing
   credential or a one-off key typed in just for this call; the key is
   never stored.
2. **Review.** A table lists every one of the provider's catalog models,
   old price next to the fetched one, with a changed row highlighted. A
   model the feed didn't mention previews with no new price (nothing to
   apply for it) and is never force-cleared. A count of vendor models the
   feed priced that aren't in the catalog at all (`unmatched_provider_models`)
   is shown too — a nudge to add them, not something this flow does by
   itself.
3. **Apply.** Writing the reviewed prices takes one write per model,
   wrapped in a single transaction. A checkbox, **on by default**, "Also
   update tenant models that still use the catalog price": when checked,
   a tenant's own registry model connected from a refreshed catalog model
   is updated too, but **only if its price still exactly equals that
   catalog model's price before this refresh** — the instant a tenant
   admin overrides a connected model's price by hand, a later catalog
   refresh leaves it alone.

For Nebius, a model's price can vary by deployment region
(`model_catalog_providers.base_url`'s region segment, e.g. `eu-west2` in
`https://api.tokenfactory.eu-west2.nebius.com/v1`); the refresh picks the
price for the region that provider's `base_url` names, falling back to
the feed's first listed price when a model's feed entry doesn't
distinguish regions.

This is a one-shot, admin-triggered action — the gateway never polls a
vendor's pricing API on its own.
