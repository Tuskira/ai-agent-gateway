# 08 - Observability

## Goal

Turn on the ClickHouse analytics tee and the OTel span exporter on top of
the stack 01-quickstart brings up, then read the same traffic back three
ways -- the console, `GET /api/v1/analytics/*`, and raw ClickHouse SQL --
plus ship the same spans to a real tracing backend (Jaeger).

Three sinks, three jobs (`docs/observability.md`):

- **stdout** -- one JSON line per record. Always on by default; this is
  the only sink 01-quickstart runs with.
- **ClickHouse tee** -- adds durable, queryable analytics: the Overview
  page's KPIs, Access Logs and Session Timeline read from it -- with it
  off, those `GET /api/v1/analytics/*` routes answer `404` rather than
  erroring. (LLM Logs reads it too when it's on, and otherwise falls back
  to the Postgres capture table.)
- **OTel export** -- adds spans in your own tracing backend (Jaeger,
  here): one span per record (`mcp.<method>` / `llm.<provider>`),
  correlated with the caller's own `traceparent` on the MCP plane (not
  the LLM plane -- see "Traceparent behaviour, honestly" below).

Postgres stays the source of truth throughout: nothing above changes what
gets written there, only what else also gets a copy.

## Prerequisites

- 01-quickstart
- Docker and the `docker compose` plugin
- `curl` and `jq`
- Node.js (`npx` ships with npm) -- only needed to run the reference
  "everything" MCP server locally
- Nothing else: no API keys, no cloud account, no LLM provider
  credentials.

This example is self-contained: it does not assume 01-quickstart has been
run first, but reuses whatever it already left running (the "everything"
server, the compose stack, the "everything" connector) instead of
duplicating it.

## Steps

Run the whole thing with:

```sh
./examples/08-observability/run.sh
```

It's safe to re-run: every step checks what already exists and reuses it
instead of failing or duplicating work.

What `run.sh` does, in order:

**1. Start the local "everything" MCP server.** Identical to
01-quickstart's step 1 -- reused if it's already listening on `:23014`.

**2. Bring up the compose stack with the analytics profile and the
OTel/Jaeger overlay.** `deploy/docker-compose.yml` alone never starts
ClickHouse (gated behind the `analytics` Compose profile) and never sets
any `GATEWAY_SINKS_OTEL_*` env var at all -- `internal/config/config.go`'s
`OtelSink` has no default there to override, unlike
`GATEWAY_SINKS_CLICKHOUSE_ENABLED`, which the base file already forwards
from the shell. So this example needs a second `-f` file on every
command, exactly like `docs/observability.md`'s own compose invocation
plus one more file:

```sh
GATEWAY_SINKS_CLICKHOUSE_ENABLED=true docker compose \
  -f deploy/docker-compose.yml \
  -f examples/08-observability/docker-compose.observability.yml \
  --profile analytics up -d
```

[`docker-compose.observability.yml`](docker-compose.observability.yml)
sets the four OTel env vars on `gateway` and adds two services:

```yaml
services:
  gateway:
    environment:
      GATEWAY_SINKS_OTEL_ENABLED: "true"
      GATEWAY_SINKS_OTEL_ENDPOINT: "otel-collector:4318"   # host:port, no scheme
      GATEWAY_SINKS_OTEL_INSECURE: "true"                # plaintext inside the compose network
      GATEWAY_SINKS_OTEL_PROTOCOL: "http"
  otel-collector: # OTLP in (http :4318, grpc :4317) -> debug log + Jaeger
  jaeger:          # UI on :16686 (Jaeger v2 -- OTLP-native, no config needed)
```

One gotcha worth calling out (already hit and documented in
[`examples/09-roles-keys-and-rate-limits/docker-compose.roles.yml`](../09-roles-keys-and-rate-limits/docker-compose.roles.yml)):
a relative path inside an overlay file resolves against the **project**
directory, which Compose takes from the *first* `-f` file
(`deploy/docker-compose.yml`, i.e. `deploy/`) -- not against this overlay
file's own directory. So the collector's config is mounted from
`../examples/08-observability/otel-collector.yaml`, not
`./otel-collector.yaml`. Verified with `docker compose ... config`.

**3. Wait for health on all three gateway planes plus the otel-collector,
then confirm both sinks actually turned on:**

```sh
curl -sSf http://localhost:8080/health          # MCP plane
curl -sSf http://localhost:8081/api/v1/health   # control plane
curl -sSf http://localhost:8082/health          # LLM plane
curl -sSf http://localhost:13133/               # otel-collector's health_check extension
```

`run.sh` asserts `.sinks.clickhouse.enabled` and `.sinks.otel.enabled` are
both `true` on the control-plane response before going any further.

**4-6. Bootstrap an admin key; register/reuse the "everything" connector;
probe its health and discover its tools.** Identical to 01-quickstart's
steps 4-6 -- see [01-quickstart/README.md](../01-quickstart/README.md)
for the full detail.

**7. Send traffic under one shared W3C trace id.** One trace id is minted
locally --

```sh
TRACE_ID=$(od -An -tx1 -N16 /dev/urandom | tr -d ' \n')   # 32 lowercase hex chars
```

-- with no external dependency (no Python, matching the rest of this
repo's examples). It has to be exactly right: `pkg/trace.ParseTraceparent`
validates strictly (32/16 lowercase hex characters, never all-zero) since
a `traceparent` is echoed into logs and forwarded to backends, and a
*rejected* header isn't an error at the gateway -- `trace.FromHeader`
silently falls back to a fresh, different trace id instead (best-effort
correlation), which would make every assertion below fail quietly against
the wrong id.

3 `tools/list` and 20 `tools/call everything__echo` calls each then carry
their own `traceparent` header -- the same trace id, a fresh span id per
call:

```
traceparent: 00-<32-hex trace id>-<16-hex span id>-01
```

```sh
curl -sS http://localhost:8080/mcp \
  -H "Authorization: Bearer $GATEWAY_ADMIN_KEY" \
  -H "Mcp-Session-Id: $SESSION_ID" \
  -H "traceparent: 00-$TRACE_ID-$(od -An -tx1 -N8 /dev/urandom | tr -d ' \n')-01" \
  -H "Content-Type: application/json" \
  -d '{"jsonrpc":"2.0","id":21,"method":"tools/call","params":{"name":"everything__echo","arguments":{"message":"08-observability-1"}}}'
```

The gateway reads `traceparent` fresh off every HTTP request
(`internal/dataplane/transport/capture.go`'s `traceFromRequest`), not once
per MCP session, so this works even though every call in the loop shares
one `Mcp-Session-Id`.

**8. Wait for the ClickHouse sink to flush, then assert via the analytics
API.** ClickHouse batches on 100 records or a 5s timer, whichever comes
first (`pkg/sink/clickhouse/config.go`); this run's ~25 records never
reaches 100, so `run.sh` sleeps 7s (5s timer + margin), then:

- `GET /api/v1/analytics/overview?range=24h` -- `.kpis.mcpToolCalls.value`
  (a count of `method = "tools/call"` rows only; `tools/list` doesn't
  count towards it -- `pkg/sink/clickhouse/reader.go`) rises by at least
  20, and `everything__echo` shows up in `.mcpTools`.
- `GET /api/v1/analytics/logs?tool=everything__echo&limit=50` -- rows
  carry a `trace_id` field (`pkg/sink.AccessLog`), but
  `analytics.AccessLogFilter` (`pkg/analytics/analytics.go`) has no
  server-side trace-id filter, so `run.sh` matches it client-side with
  `jq`. Exactly 20 rows carry the shared trace id: one per `tools/call`
  above -- the 3 `tools/list` calls carry the same trace id too, but have
  no `tool_name`, so the `tool=` filter excludes them.

**9. Assert the same thing straight from ClickHouse SQL.** ClickHouse's
host ports are commented out by default in `deploy/docker-compose.yml`,
so this goes through `docker compose exec`, not a host port:

```sh
docker compose -f deploy/docker-compose.yml \
  -f examples/08-observability/docker-compose.observability.yml \
  --profile analytics exec -T clickhouse \
  clickhouse-client --user default --password local --database gateway \
  -q "SELECT count() FROM mcp_access_logs WHERE trace_id = '<id>'"
```

This one counts every method, not just `tools/call`, so it comes back at
least 20 (echo calls) plus the 3 `tools/list` calls sharing the same
trace id. [`queries.sql`](queries.sql) has three more: top tools by call
volume, slowest calls (with `trace_id`, so you can jump to the matching
trace in Jaeger), and LLM cost by model -- `run.sh` pipes the whole file
into `clickhouse-client` (ClickHouse 24.8+ runs multiple `;`-separated
statements from stdin by default; no `--multiquery` flag needed).

**10. Assert via the otel-collector's own log.** The collector's `debug`
exporter (`verbosity: detailed` -- see [`otel-collector.yaml`](otel-collector.yaml))
prints every span's attributes to its container log, so `run.sh` polls
`docker compose ... logs otel-collector` for the shared trace id, up to
30s.

**11. Print the console URLs, a direct Jaeger trace link, and the admin
key.**

### Traceparent behaviour, honestly

- **MCP plane** -- continues the caller's trace. A valid inbound
  `traceparent` is parsed and its trace id carried through: the
  access-log row's `trace_id`, the outbound call to the connector
  (`internal/dataplane/client`'s `newRequest` sets its own `traceparent`
  from `call.Trace.Traceparent()`), and the OTel span
  (`pkg/sink/otel/otel.go`'s `WriteAccess` parents the span onto that
  trace id -- via a *synthetic* parent span id, since `AccessLog` only
  carries a trace id, not the caller's actual span id; a backend that
  resolves the parent reference will show this span as the trace's root).
- **LLM plane** -- does **not**. `sink.LLMCall` carries no trace id at all
  (`pkg/sink/otel/otel.go`'s doc comment), so every LLM span always starts a brand-new trace, even if the
  request itself carried a `traceparent`. This example only exercises the
  MCP plane (no LLM credentials needed); see
  [`examples/07-llm-passthrough`](../07-llm-passthrough/) for the LLM
  plane.

## Expected output

- Steps identical to 01-quickstart's connector setup succeed the same
  way.
- `GET /api/v1/health` reports all three sinks on:

  ```json
  {
    "status": "ok", "plane": "api",
    "sinks": {
      "stdout":     {"enabled": true, "dropped": 0},
      "clickhouse": {"enabled": true, "dropped": 0},
      "otel":       {"enabled": true, "endpoint": "otel-collector:4318", "protocol": "http"}
    }
  }
  ```

  A non-zero ClickHouse `dropped` means its buffer is backing up --
  usually because the collector or ClickHouse is unreachable; `otel`
  reports no drop counter of its own (its SDK batches internally) -- see
  `docs/observability.md`.
- 20/20 `tools/call` echo back their own message.
- `analytics/overview`'s `kpis.mcpToolCalls.value` rises by at least 20;
  `everything__echo` appears in `mcpTools`.
- `analytics/logs?tool=everything__echo` returns exactly 20 rows carrying
  the run's trace id.
- ClickHouse's own `SELECT count()` on that trace id is at least 20.
- `queries.sql` runs cleanly against ClickHouse.
- The otel-collector's log contains the trace id within 30s.
- `run.sh` finishes by printing:

  ```
  ================================================================
  08-observability passed.
  Console:        http://localhost:8081
  Overview:       http://localhost:8081/
  Access Logs:    http://localhost:8081/access-logs
  LLM Logs:       http://localhost:8081/llm-logs
  Jaeger trace:   http://localhost:16686/trace/<trace id>
  Admin key:      gk_...
  ================================================================
  ```

## Cleanup

```sh
# Stop the local "everything" MCP server, if this run started it
kill "$(cat examples/08-observability/.everything.pid)" 2>/dev/null || true
rm -f examples/08-observability/.everything.pid examples/08-observability/.everything.log

# Stop the whole stack -- both -f files and the profile, or ClickHouse,
# the collector, and Jaeger are left running
docker compose \
  -f deploy/docker-compose.yml \
  -f examples/08-observability/docker-compose.observability.yml \
  --profile analytics down
```

The `analytics` profile is additive: dropping just the overlay
(`docker compose -f deploy/docker-compose.yml up -d gateway`, no second
`-f`) reverts the gateway to stdout-only and leaves ClickHouse, the
collector, and Jaeger running until you `down` them explicitly. Data in
ClickHouse and Postgres survives a plain `down`; add `-v` to also drop
both volumes.
