# Security model

## Identity: keys, roles, permissions

Every request that reaches a plane handler carries a `Principal`
(`pkg/auth.Principal`): a subject, a tenant id, zero or more roles, and an
auth method. Two `Authenticator`s issue one from real credentials: the
API-key authenticator (`internal/auth/apikey`, every plane) and the console
session authenticator (`internal/auth/session`, the `gw_session` cookie,
control plane only). A dev-mode gap-filler exists for local use only (see
below).

**API keys.** A key is `gk_` followed by the base62 encoding of 32
`crypto/rand` bytes (`internal/auth/apikey.Generate`). Only the SHA-256
hash of the plaintext (`apikey.Hash`) is ever persisted, in
`api_keys.key_hash`; the first 8 characters after the prefix are kept
separately as `key_prefix` for display (e.g. `gk_A1b2C3d4`) — not enough
entropy to reconstruct the key. A key is presented as
`Authorization: Bearer gk_...` or `X-Gateway-Key: gk_...` (the latter
takes precedence, and does not shadow a BYOK `Authorization` header on the
LLM plane — see below). A successful lookup is cached in memory for
`auth.api_keys.cache_ttl` (default 30s). A **revoke or rotate** evicts the
key from the cache of the process that handled it at once, and — with the
Postgres store — from **every other process** (API, MCP and LLM replicas
alike) within milliseconds: the revoke publishes the key's hash with
`pg_notify` in the same statement as the `UPDATE`, and each process holds
a `LISTEN` connection that evicts it (see
[Known limits](#known-limits) for the exact guarantee). Unknown keys are
cached negatively for 5 s (bounded), so a flood of bad keys costs one
database lookup, not one each.

**Roles and permissions.** Permissions are dot-separated strings (e.g.
`connector.create`, `mcp.access`, `platform.admin`), matched against a
role's glob patterns by `pkg/auth.RoleAuthorizer`. Five roles ship built
in:

| Role | Grants |
|---|---|
| `admin` | `*` — every **tenant-scoped** permission, and nothing in the `platform.` namespace |
| `platform-admin` | `platform.*` (`platform.admin`, `platform.catalog.manage`). API keys only. A platform-admin key is stored with this role and carries `admin` as well, so it also does everything a tenant admin can |
| `agent` | `mcp.*`, `llm.*`, `*.read` |
| `viewer` | `*.read` only (the read-only role for console users; also valid for a key) |
| `interceptor` | `ingest.write` only |

A console **user** (username and password, see
[authentication.md](authentication.md)) with role `admin` is a tenant
admin like an `admin` key: tenant creation stays with platform-admin API
keys and the CLI, and no user ever holds `platform-admin`. `platform.catalog.manage` is a partial exception — a console
admin holds it only in a single-tenant deployment (see below). Only
`admin` matches `users.manage`, the permission behind the `/users` routes
and the auth audit.

`interceptor` exists for one purpose: a key minted with it can call
`POST /api/v1/ingest` ([api.md, "Ingest"](api.md#ingest)) and nothing
else — not even `*.read`. It's the role to mint for the capture component
that feeds that route, so a leaked interceptor key can write
ingest records but can't read connectors, credentials, or any other
tenant data. `agent`'s `*.read` grant does not satisfy `ingest.write`
(a read-only pattern never matches a write permission), so an `agent`
key cannot call `/ingest` either.

The admin console accepts only API keys whose role is `admin` for its
(emergency) API-key sign-in; users sign in with a password instead
([authentication.md](authentication.md)). Any other key, `agent` included,
is refused at the console's key login. This is a console check, not an
API one: `agent`'s `*.read` grant still reads the API directly. To close
that, redefine `agent` without `*.read` in `auth.roles`.

A trailing `*` matches everything from that point on (`mcp.*` matches
`mcp`, `mcp.tools`, `mcp.tools.call`); a bare `*` segment elsewhere matches
exactly one segment (`*.read` matches `connector.read`, not
`connector.tools.read`). Custom roles can be added, and any built-in
replaced outright, via `auth.roles` in the YAML config (no env-var
equivalent — see [configuration.md](configuration.md)).

**`platform.admin`.** One permission is carved out from the ordinary
wildcard rules: a permission in the `platform.` namespace can only be
satisfied by a pattern that itself starts with the literal segment
`platform` — a bare `*` or `*.read` grant does **not** imply it. It
gates `GET`/`POST /api/v1/tenants` (tenant enumeration and
creation, which are inherently cross-tenant operations) and writes of
**platform** MCP catalog entries. The built-in `admin` role is a *tenant*
admin and does **not** hold it; only `platform-admin` does.

Who gets a `platform-admin` key: the **first** key `gateway bootstrap-key`
ever creates (no API key exists in the database yet) is
`admin` + `platform-admin`, so a fresh install can create more tenants.
Every later `bootstrap-key` yields a tenant-admin key unless
`--platform` is passed (CLI only, so it needs shell access to the
deployment). Over the HTTP API, `POST /api-keys` (and rotating such a key)
with `role: "platform-admin"` — or any custom role with a `platform.*`
pattern — requires the caller to hold `platform.admin` itself, so a tenant
admin cannot mint a platform key.

**`platform.catalog.manage`.** The other permission in the `platform.`
namespace, gating every [model catalog](llm-plane.md#model-registry) write
route (`api.md`'s `/model-catalog/*` rows) — the platform-wide list of
providers and models a tenant can "connect." Only a
`platform-admin` **API key** holds it unconditionally. For a console **user** with role
`admin`, it is granted only while the deployment is **single-tenant** (the
same "exactly one tenant exists" test `GET /auth/config`'s `single_tenant`
field uses, wired up as `RoleAuthorizer.SingleTenant`) — in a multi-tenant
deployment the catalog is shared platform-wide, so managing it stays
API-key-admin only, the same reasoning as `platform.admin`. This is a
dynamic check (re-evaluated per request, not baked into a role at
startup): a deployment that starts multi-tenant and is later reduced to
one tenant picks up the grant for its console admins without a restart.
Catalog **reads** (`GET /model-catalog`) are gated on the ordinary
`model.read`, so every tenant can see the catalog and what it has
connected regardless of this permission.

**`mcp.access`.** The one permission the MCP plane itself checks
(`internal/dataplane/transport.PermissionMCPAccess`), on top of
authentication. `agent` holds it via `mcp.*`; a custom role with only
`*.read` cannot reach the plane.

**`llm.access`.** The LLM plane's equivalent
(`internal/llmplane.PermissionLLMAccess`): every LLM-plane request
(except `/health`) must hold it, `403` otherwise. `admin` and `agent`
hold it; a custom role must grant `llm.access` (or `llm.*`) to call
providers — including Bedrock Flow C, which spends the gateway's own AWS
identity.

## Tenant isolation

There is no per-request tenant header a client can set to select its
tenant. `Principal.TenantID` comes only from the server-side credential:
the API key's `api_keys.tenant_id`, the console user's tenant, or the
dev-mode tenant. Every store method is tenant-scoped by that
value — see `pkg/store`'s interfaces, every one of which takes
`tenantID` explicitly. The one gateway-owned exception is the outbound
`X-Tenant-Id` header the MCP plane's backend client sets on every call to
a connector (`internal/dataplane/client.HeaderTenantID`): it is written
*after* the connector's own configured headers, so no connector
`metadata.headers` entry can override it. An MCP session id is also
tenant-checked on resolve (`session.Manager.Resolve`): a session id
leaked from tenant A presented with tenant B's key resolves to "not
found," not to tenant A's backend handles.

## Secrets at rest and in logs

**Strict configuration.** The config file is decoded with unknown keys
rejected (`config <file>: unknown key "mcp.require_profle" at line 3`), so a
mistyped security setting stops startup instead of silently staying at its
default. Unknown `GATEWAY_*` environment variables are logged as a warning.

**Captured bodies.** With `store_bodies` on, request/response bodies (prompts,
completions, tool arguments) are stored in Postgres/ClickHouse/the body store
and capped (LLM plane: 1 MiB each by default). They are not printed to stdout
unless `sinks.stdout.include_bodies` is set; the gateway warns at startup when
both are on. See [observability.md](observability.md#sinks).

**At rest.** Connector credentials are AES-256-GCM-encrypted
(`internal/secrets.KeyRing.Encrypt`) with the key id as additional
authenticated data, so ciphertext produced under one key id can never be
decrypted under another. The key material is a ring —
`GATEWAY_MASTER_KEY` (or `secret_store.master_key_file`) holds either a
single base64/hex 32-byte key, or a comma-separated `id:key` list for
rotation (e.g. `k1:<old>,k2:<new>`), with `secret_store.active_key_id`
selecting which one new writes use. `gateway secrets genkey` mints a key;
`gateway secrets rekey --tenant <slug|all>` re-encrypts every credential
of a tenant (or all tenants) under the ring's current active key — run it
after adding a new key alongside the old one and flipping
`active_key_id`, then drop the old key id from the ring once done.
Neither the plaintext key nor any credential payload is ever logged — only
a non-reversible 8-character fingerprint (`secrets.Fingerprint`) of each
key id, at startup.

**In logs.** Three independent allowlists gate what's recorded, all
deny-by-default (an allowlist, not a denylist, because a connector's
header config is operator-authored and can mint a header of *any* name
carrying a live credential):

- Outbound headers to a backend connector: debug logs mask every value
  except a small fixed set (`Accept`, `Content-Type`, `Mcp-Protocol-Version`,
  `Traceparent`, `X-Tenant-Id`, `X-Agent-Profile-Name`, …) — a masked
  value is rendered as `sha256:<8 hex chars>****`, not even a length or
  prefix survives (`internal/dataplane/client.maskHeaders`/`maskValue`).
- Captured MCP access-log headers (only when `capture.store_bodies` is
  on): an allowlist (`internal/dataplane/transport.maskedHeaders`); any
  other header, `Authorization` included, is recorded by name with the
  value `****`.
- LLM-plane captured headers (recorded on every call, whatever
  `llm_proxy.capture.store_bodies` is set to): a separate allowlist
  (`content-type`, `x-request-id`, the rate-limit families, …); any other
  header, credentials included, is recorded by name with the value
  `"[masked]"` (`internal/llmplane.maskInto`).
- On either plane, the console leaves masked headers out of its log
  views; they remain in the stored record and the API response.
- Captured bodies (either plane, only with `store_bodies` on) are stored as
  sent and are not scanned: a prompt, tool argument, or tool result that
  contains a secret — including a credential the gateway injected and a
  backend echoes back — is stored with it. Leave `store_bodies` off where
  that is not acceptable. Bodies reach stdout only with
  `sinks.stdout.include_bodies: true`, and then access to the container's
  logs equals access to the bodies.

Connector metadata returned by `GET /api/v1/connectors*` is masked the
same way: a `static` header's `value` becomes `"***"`, and every value
inside an `external` header's `config` object is masked uniformly. The
API's `PUT /connectors/{id}` treats an unchanged `"***"` value as "keep
the stored value," so a UI read-modify-write round trip can never
overwrite a real secret with the placeholder.

## What the gateway can and cannot see

**LLM plane (BYOK).** The client's own provider credential
(`x-api-key`/`Authorization: Bearer sk-...` for Anthropic/OpenAI, SigV4
`Authorization` for Bedrock passthrough, `x-goog-api-key` for Gemini)
rides through unmodified — the gateway forwards it, it never stores or
inspects it as a secret. The gateway strips its **own** credential,
`X-Gateway-Key` or an `Authorization: Bearer gk_...` header
(`isGatewayBearer`), so a gateway key never reaches a provider. It also
strips `X-Provider-Key`/`X-Provider-Key-<label>`, which it reads itself for
a translated registry target. A
[model registry](llm-plane.md#model-registry) target may instead name a
stored tenant credential, which the gateway decrypts and sends to that
target. Bedrock's client-supplied
temporary credentials/role headers (`X-Bedrock-Access-Key-Id`,
`X-Bedrock-Secret-Access-Key`, `X-Bedrock-Session-Token`,
`X-Bedrock-Role-Arn`, `X-Bedrock-External-Id`) are consumed to sign the
request and are never forwarded either. Role assumption is off unless the
operator allowlists AWS accounts (`llm_proxy.bedrock.allowed_role_accounts`)
and always uses the caller's tenant ID as the STS external ID (never a
caller-chosen value), so a role whose trust policy requires
`sts:ExternalId = <tenant id>` can only be assumed for that tenant, and
the caller-supplied Bedrock region is validated before it becomes part
of the upstream host. Upstream redirects are relayed to the client, never
followed, so a provider key is never re-sent to another host; and a
transport error is recorded with the URL's query string removed (Gemini
accepts `?key=`).

**MCP plane.** The gateway resolves and injects connector credentials
itself (via the header-resolver framework); a backend only ever sees what
a configured header resolves to, plus the gateway-owned `X-Tenant-Id` and
`Mcp-Session-Id`. The `incoming_field` resolver denylists forwarding
`Authorization`, `X-Tenant-Id`, `Tenantid`, `X-Gateway-Key`, the hop-by-hop
headers, and every `Mcp-*` header, so a connector config cannot be used to
forge the caller's identity or leak the caller's own gateway credential to
a third party.

Every header config is validated when the connector is saved: an unknown
type, an invalid header name (outbound, or an `incoming_field` header), a
denylisted `incoming_field` header, an invalid `token_field`, a line break
in a `static` value or a `prefix`, a disabled `env`/`file` provider, or a
`metadata.headers` that is not an object is `400 validation_error` on POST
and PUT. On PUT the check runs after `***` values are unmasked and covers
only the headers the request adds or changes, so a connector saved before
this check existed can still be edited, enabled and disabled. A resolved
value is never allowed to split a header: a multi-line `external` value (a
stored credential, `env` or `file`, such as a PEM key) has its trailing
newline trimmed and each remaining CR/LF escaped to the two characters
`\n`, and a `static`, `token_field` or `incoming_field` value containing
CR or LF is refused at call time (the header is skipped and logged).

## Outbound requests (SSRF)

Tenants and admins choose URLs the gateway then calls: connector endpoints,
MCP catalog URLs (and a tenant's override), model registry `base_url`s and
model catalog providers. Without a guard these reach whatever the gateway
can reach: the cloud metadata service (`169.254.169.254`), the node,
databases and other in-cluster workloads.

The gateway therefore dials every such URL through `internal/netguard`:

- **Checked at connect time, on the resolved IP.** The dialer's `Control`
  hook sees the address about to be connected, so `2130706433`,
  `0x7f000001`, `[::ffff:127.0.0.1]`, a hostname that resolves to `127.0.0.1`
  (or flips to it after validation) and a `302` to an internal address are
  all refused. Redirects are capped at 5 hops; each hop is dialed through the
  same guard.
- **Blocked by default:** loopback, link-local (`169.254/16`, `fe80::/10`),
  private (`10/8`, `172.16/12`, `192.168/16`), CGNAT (`100.64/10`), unique
  local (`fc00::/7`), unspecified, multicast and broadcast, including their
  IPv4-mapped, IPv4-compatible and NAT64 forms.
- **Fast feedback:** a literal blocked IP in any spelling is rejected when the
  connector, catalog entry, model target or provider is created, with
  `400 {"error":{"type":"egress_blocked",...}}`. A hostname is resolved too
  (2s budget; at startup the MCP catalog seed shares one 2s budget) and rejected the same way when every address it resolves to is
  blocked and not allowlisted -- `localhost` and any `*.localhost` name are
  judged as loopback without a DNS round trip, since some resolvers never
  forward that TLD upstream. A name that times out, doesn't resolve yet, or
  resolves to at least one non-blocked address is accepted at create time;
  the dial-time `Control` hook above remains the actual security boundary
  either way (a lookup failure here is not a rejection, since the dialer's
  own resolution at connect time is what's authoritative).
- **Opt-in exceptions:** `egress.allowed_cidrs` and `egress.allowed_hosts`
  (see [configuration.md](configuration.md#egress-outbound-request-policy)).
  The default is empty. The shipped compose stack allows
  `host.docker.internal` and loopback so a local demo can reach services on
  your laptop; do not copy that to a shared deployment.
- **Network layer:** `deploy/k8s/base/networkpolicy.yaml` allows egress on
  443/80 to public addresses only (RFC 1918 and link-local excluded), so the
  cluster blocks the same ranges even if the application check were bypassed.

Caveats: the guard covers the gateway's own upstream clients. Anything that
makes the gateway call a URL through other code (a third-party
`ExternalProvider` plugin, for instance) must use `netguard.DialContext`
itself. A proxy configured with `HTTP(S)_PROXY` is dialed through the guard
too, so a proxy on a private address must be allowlisted.

### Forwarding the caller's credential to a connector

A connector header of `{"type": "token_field", "field": "bearer_token"}`
sends the caller's own gateway API key to the connector's backend. The
backend operator then holds a credential valid against this gateway. It is
off unless `connectors.allow_bearer_token_forwarding: true`; creating such a
connector is `400 bearer_forwarding_disabled`, and a call through a connector
that still has it fails with a clear error rather than forwarding.

## Dev mode

`auth.dev_mode.enabled: true` adds an `Authenticator`
(`internal/auth/devmode`) that grants a **tenant-admin** `Principal` (never
`platform-admin`, unless `auth.dev_mode.platform: true`) to any
request carrying no credential at all (no `Authorization`, no
`X-Gateway-Key`) — it only fills the gap left by every other
`Authenticator`; a request with *some* credential, even an invalid one, is
still rejected normally. Every time it fires it logs a `WARN`. This exists
for local development only and must never be enabled on a shared or
production deployment: with it on, an unauthenticated request to any
plane is served as a tenant admin. Guards: the gateway logs one loud
`WARN` at startup; it **refuses to start** when an enabled plane listens
on a non-loopback address (`0.0.0.0`, `:port`, `[::]`, a LAN IP) unless
`auth.dev_mode.allow_remote: true`; and `auth.dev_mode.tenant` is a tenant
slug (or UUID) that must exist at startup — the gateway fails with a clear
error otherwise.

## `env`/`file` header providers: off by default, and why

Two of the four outbound-header resolvers are `ExternalProvider`s gated
behind explicit config, off by default:

- `secret_store.allow_env_provider` (default `false`) — the `env`
  provider reads a header's value from the **gateway process's own
  environment**. On a shared/multi-tenant deployment, enabling it would
  let any tenant admin who can configure a connector's headers read *any*
  environment variable visible to the process — including
  `GATEWAY_MASTER_KEY` itself, or the database password — simply by
  pointing a header at it. Only enable it on a single-tenant deployment,
  or one where every tenant is already trusted with the host environment.
- `secret_store.allow_file_provider` (default `false`) — the `file`
  provider reads a header's value from a file on the gateway's local
  filesystem. When enabled, `secret_store.file_provider_root` is required
  and every configured path must resolve underneath it: `..` components
  are rejected lexically, and the fully-resolved path (after following
  symlinks) is re-checked against the root, so a symlink planted under
  the root cannot be used to read a file outside it.

## Server-initiated MCP requests: off by default, and why

A connector (backend MCP server) can ask the gateway to relay
`sampling/createMessage`, `elicitation/create`, and `roots/list` back to
the calling agent (see
[connectors-and-credentials.md](connectors-and-credentials.md#server-initiated-requests-sampling-elicitation-roots)
for the mechanics). All three default to `false` per connector, because
each hands a third party — code you configured but don't control at
runtime — something it can use against the caller, not just answer a
tool call:

- **`sampling`** spends the *caller's* LLM budget on the backend's
  behalf. A connector allowed to sample can turn every tool call it
  handles into an arbitrary number of additional model calls, billed to
  the tenant that configured it, with no tool-level rate limit of its
  own.
- **`elicitation`** lets the backend put its own text in front of the
  human operating the agent, mid-session, framed as a legitimate prompt
  from the tool the person already chose to trust — the same shape as a
  phishing prompt, but delivered through a channel the person has no
  reason to be suspicious of.
- **`roots`** tells the backend which local directories/filesystem paths
  the agent is working in — information that, once leaked, doesn't just
  aid a phishing attempt but on its own reveals project names, usernames,
  and directory layouts.

Turning any of these on is a deliberate, per-connector trust decision an
admin makes once they've decided a specific backend deserves it — never
a gateway-wide default.

Where it is enforced (`internal/dataplane/orchestrator/relay.go`):

- **At the connector's handshake.** The gateway declares to the connector,
  as its MCP client, only the capabilities the agent declared at its own
  `initialize` **and** the connector's policy allows. A connector is never
  invited to send a request the relay would refuse. Before this, the
  gateway declared `roots` to every backend unconditionally; it no longer
  does.
- **On every request, again.** Whatever the handshake declared, each
  request a connector sends — inside a `tools/call` reply or on its
  long-lived stream — is checked against the connector's policy as stored
  *now* (turning a flag off applies to sessions already open) and the
  session's declared capabilities, and refused with `-32601` otherwise. A
  refused request never reaches the agent, and a method other than the
  three is refused the same way.
- **Only the session the request went to can answer it.** The agent sees
  a gateway-minted id (`gw-` + 64 random bits), never the connector's,
  and an answer is matched against the requests pending for the session
  it was POSTed on (tenant and session id both); one POSTed on any other
  session is ignored. Across replicas the answer travels on the session
  channel carrying the tenant and session id the receiving replica
  resolved, and is matched the same way.
- **Bounded.** At most `mcp.max_pending_server_requests_per_session`
  requests await one session's answers, each for at most
  `mcp.server_request_timeout`; ending the session fails them all. A
  connector cannot pile up work on an agent without limit.
- **Content stays out of the logs.** Each relayed request is
  access-logged by method, connector, session, outcome and duration;
  `params` and results — prompts, completions, what a human typed — are
  never written there.

## Rate limiting and lockout

`internal/auth.RateLimiter` throttles credential guessing on **all three
planes** (API, MCP, LLM) with two failure dimensions, plus a general cap on
the control plane. All of it is configured under `auth.rate_limit`, and the
state is per process (in memory).

**Lockouts are per plane.** Each plane has its own limiter instance, with
independent counters and lockouts for both dimensions below; the same
thresholds apply to each. Failures on one plane never count against, or lock
the client out of, another. What a developer sees: an IDE or agent with a
wrong MCP key locks that machine's IP out of the **MCP plane only** — its
model calls on the LLM plane, and console/API use, keep working — and the
reverse holds too. The shared parts are deliberately not lockouts: the
API-key lookup cache and the 5 s negative cache are shared by all planes, so
a key known to be bad is rejected cheaply everywhere. `rate_limit.locked_ips`
on `GET /api/v1/health` counts the API plane's lockouts only; each plane logs
its own `lockout started` WARN with a `plane` attribute.

- A **per-IP failure lockout**: `max_failures` auth failures (default 10)
  from the same client IP within `window` (default 1m) locks that IP out
  for `lockout` (default 5m) — every request from it, even one carrying a
  valid credential, gets `429` + `Retry-After` until the lockout expires.
  The 429 is in each plane's own error format: the REST envelope on the
  API, a JSON-RPC error (`-32029`) on MCP, and the client dialect's error
  envelope on the LLM plane (Anthropic, OpenAI, Gemini, Bedrock), chosen
  from the request path. `GET /api/v1/health` reports the live
  `rate_limit.locked_ips` count. Health, docs, `openapi.json`, and the
  embedded UI's static files are exempt from the lockout (but not the
  general cap).
- A **per-credential lockout**: `credential_max_failures` failed lookups
  of **unknown** keys sharing one key prefix (`gk_` + 8 characters) —
  from any IPs — within `credential_window` (default 20 per 5m) lock that
  prefix for `lockout`. This bounds guessing of one key from a rotating
  pool of IPs, and charges nobody's IP for it. A locked prefix refuses
  only keys the process does not already have cached, so the legitimate
  holder (whose key is cached) keeps working while a guesser gets `429`
  without a database lookup per guess. A revoked or expired key is not a
  guess and is not counted. The counters are per plane too, for consistency
  and so that one plane's mistyped key cannot lock a legitimate cold-cache
  holder out of the others (a wrong key sharing the real key's prefix would
  otherwise lock that prefix everywhere). The price is that a guesser can
  make `credential_max_failures` guesses per plane per window instead of per
  process (three times as many) — immaterial against a 256-bit-class random
  key, and each plane is still bounded.
- A **general cap** on the control plane (`requests_per_minute`, default
  600): a token bucket with a small burst
  (`clamp(requests_per_minute/20, 5, 50)`), applied to every API request
  regardless of auth outcome — except `POST /api/v1/ingest`
  ([api.md, "Ingest"](api.md#ingest)), which is exempt from this per-IP
  cap entirely (many interceptor installs can share one office NAT) and
  instead enforces its own per-API-key cap (`ingest.rate_per_minute`,
  `internal/auth.KeyRateLimiter`).

**Client IP and `api.trusted_proxies`.** One resolver
(`internal/clientip`) decides the client IP for every limiter and for the
`client_ip` in access and LLM logs. By default (`api.trusted_proxies`
empty) it is the TCP peer's address and `X-Forwarded-For` is **ignored**.
Behind a load balancer or ingress that means every client shares the
proxy's address — one bad client then locks out everyone — so set
`api.trusted_proxies` to the CIDRs (or single IPs) your proxies connect
from. When the peer is trusted, `X-Forwarded-For` is walked **from the
right**, skipping entries inside a trusted range; the first untrusted
entry is the client. A client cannot choose its own identity by prepending
entries, because it can only write to the left of what the proxy appends.
If every entry is trusted the left-most is used; a malformed entry stops
the walk (the hop to its right is used). IPv4-mapped IPv6 addresses are
normalised. A malformed `trusted_proxies` entry fails startup. The
`deploy/` manifests ship the right range for each environment:
the kind overlay trusts the kind pod CIDR, the EKS overlay trusts
`${GW_VPC_CIDR}` (the ALB's addresses, `target-type: ip`), and the Docker
Compose file trusts the Docker bridge range (a dev convenience — direct
clients on the host then appear as the bridge gateway, which is trusted,
so do not publish a compose deployment to untrusted networks). For plain
`kubectl apply -k deploy/k8s/base` behind ingress-nginx, set
`GATEWAY_API_TRUSTED_PROXIES` in the `gateway-env` ConfigMap to your
cluster's pod CIDR.

## LLM budgets and per-key limits

An API key can carry `limits` (daily/monthly USD budgets, requests per
minute, a `max_tokens` ceiling), set only by `admin.manage` holders and
enforced by the LLM plane before a request is forwarded
([llm-plane.md, "Budgets and limits"](llm-plane.md#budgets-and-limits)).
They bound a leaked or runaway key's cost, but they are a **soft** control:
spend is summed from captured `cost_usd` (unknown cost counts as $0),
cached for up to 10 s per replica, and calls in flight when the budget is
crossed still complete; `rpm` is counted per replica. They fail closed —
a key with a USD budget is refused when spend cannot be read and nothing
is cached, or when no Postgres capture store is configured — and every
refusal is captured in `llm_calls` with its error.

## Browser security headers

Every response on the control-plane origin — the JSON API, the public
docs page, and the embedded admin console at `/` — carries a fixed set of
hardening headers (`internal/api/security_headers.go`, mounted ahead of
everything else `NewRouter` serves): `X-Content-Type-Options: nosniff`,
`Referrer-Policy: no-referrer`, `X-Frame-Options: DENY`,
`Permissions-Policy: camera=(), microphone=(), geolocation=()`, and a
`Content-Security-Policy`. This matters because a script running on this
origin acts as the signed-in admin: it can read the API key the console
keeps in `sessionStorage` after an API-key sign-in, and it can make
requests with a password user's `HttpOnly` session cookie.

The default CSP is `script-src 'self'`, plus the specific external
origins the console's own build actually needs: `style-src`/`font-src`
allow `fonts.googleapis.com`/`fonts.gstatic.com` (the console loads Inter
and JetBrains Mono from Google Fonts — see `web/index.html`), and
`style-src` keeps `'unsafe-inline'` because Radix/shadcn (the console's
component library) set `style=` attributes on elements at runtime. There
is no inline `<script>` anywhere in the built console — the one inline
script index.html used to carry (dark-mode flash prevention, run before
React mounts) ships as `web/public/theme-init.js`, served same-origin
instead, specifically so `script-src` never needs a per-script hash kept
in sync with that file's contents.

`GET /api/v1/docs` overrides the default CSP with its own
(`handlers.cspDocs` in `internal/api/handlers/docs.go`), because it's the
one route on this origin that loads a script from a CDN: Scalar's API
reference viewer, from jsDelivr. That policy adds
`https://cdn.jsdelivr.net` to `script-src`/`style-src`/`font-src` (Scalar
is a Vue app that injects `<style>` tags at runtime, hence
`'unsafe-inline'` there too) and widens `img-src` to `https:` (an OpenAPI
document can point Scalar's viewer at example images hosted anywhere),
but deliberately leaves `connect-src 'self'` — so the docs page can't be
turned into an open proxy to whatever a spec's `servers` list names, or to
Scalar's own telemetry.

**The pinned Scalar build.** `docs.go`'s `scalarVersion`/`scalarIntegrity`
constants pin the exact `@scalar/api-reference` file jsDelivr serves,
instead of the unversioned `.../npm/@scalar/api-reference` URL this page
used before — that URL resolves to whatever the latest release happens to
be at request time, with no integrity check, so a compromised or simply
newly-broken release starts running in every admin's browser the moment
it's published. The Scalar configuration itself (`data-url=` pointing at
`/api/v1/openapi.json`) lives as a data attribute on the same `<script>`
tag that loads the pinned bundle, not as a separate executed inline
script — so the docs page's CSP needs no script hash either. Bumping the
pin is a maintainer task; see
[CONTRIBUTING.md](https://github.com/Tuskira/ai-agent-gateway/blob/main/CONTRIBUTING.md#bumping-the-pinned-scalar-build).

## Known limits

- **Revocation reaches every replica within milliseconds, backed by a
  30-second TTL.** API-key lookups cache in memory for
  `auth.api_keys.cache_ttl` (30s default). With the Postgres store, a
  revoke/rotate publishes `pg_notify('gateway_key_revoked', <key hash>)` in
  the same statement as the write, and every gateway process (API, MCP and
  LLM) evicts the key from its cache on receipt — measured at ~12 ms
  across two processes. `LISTEN/NOTIFY` is at-most-once: a process whose
  listener connection is down misses events, so it flushes its whole key
  cache each time the listener (re)connects (with exponential backoff),
  and until then a revoked key can still be served for up to
  `cache_ttl`. A store without the optional `store.RevocationNotifier`
  interface has the TTL only. Console sessions are not cached (read per
  request), so a session revoke is immediate everywhere.
- **No OIDC/SSO.** Programs authenticate with API keys and console users
  with username/password sessions ([authentication.md](authentication.md)).
  `pkg/auth.Authenticator` is an interface a plugin could add another
  implementation against, but none ships here.
- **A key is only confined to a profile if you bind it.** Without a
  binding, profile enforcement is driven by the caller-supplied
  `X-Agent-Profile-Name` header, checked against the tenant's profiles —
  such a key can present any profile name its tenant owns, and
  `mcp.require_profile` only forces the header to be present. Create the
  key with `profile_id` (or `PATCH /api-keys/{id}`) to bind it: the MCP
  plane then enforces that profile, ignores an absent header, and rejects
  a header naming a different profile with `-32003`. (The LLM plane does
  not use profiles.)
- **A single MCP replica without Redis.** With the default
  `sessions.store: memory` (`pkg/session/memory`) sessions are
  process-local; running more than one MCP replica on it means a client
  whose requests land on a different replica gets JSON-RPC `-32000` and
  has to re-initialize — correct behavior, but it redoes the backend
  handshake. `sessions.store: redis` (`pkg/session/redis`) shares them.
  Note what that puts in Redis: each session's per-connector backend
  session ids, which are bearer handles for those backends, so the Redis
  instance must be treated as sensitive (auth + TLS in production, and no
  other tenant of the same Redis with access to the `gw:` keyspace). The
  gateway still checks the tenant on every Resolve, so a Redis-level
  read of another tenant's session id is not usable with your API key.
- **Sinks drop on overflow.** `sink.LogSink` writes are contractually
  non-blocking; the ClickHouse sink's bounded buffer
  (`sinks.clickhouse.buffer_size`) drops a record rather than blocking the
  request path when full. `GET /health`'s `sinks.*.dropped` counter is the
  only signal of this. The LLM plane's durable Postgres write is
  synchronous and does not drop; only the analytics tee can.
- **Cost is an estimate.** Every dollar figure the gateway reports
  (`cost_usd`, the console's cost tiles) is computed at capture time from
  parsed token counts and a static per-model rate card
  (`pkg/pricing`), never from a provider-reported invoice line — see
  [llm-plane.md](llm-plane.md). `costEstimated: true` is stamped on every
  `GET /api/v1/analytics/overview` response.
- **MCP permission denial** is JSON-RPC `-32006` (HTTP 403); connector unhealthy is `-32005`.
- **No per-connector or per-tool quota.** The MCP and LLM planes limit
  failed authentication (per-IP lockout, per-key-prefix limiter, negative
  cache; see [Rate limiting and lockout](#rate-limiting-and-lockout)), but
  neither caps requests per tool or per connector. On the MCP and LLM
  planes the only quotas are the LLM plane's per-tenant concurrency cap and per-key limits; the
  MCP plane has no request-rate quota.

## Disclosure process

See [SECURITY.md](https://github.com/Tuskira/ai-agent-gateway/blob/main/SECURITY.md) for how to report a vulnerability,
response targets, and what's in scope.
