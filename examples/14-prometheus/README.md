# 14 - Prometheus metrics

## Goal

Scrape the gateway's own operational metrics with Prometheus and look at
them in Grafana, on top of the stack 01-quickstart brings up. You get:

- **The gateway's metrics endpoint.** `GATEWAY_METRICS_DRIVER=prometheus`
  opens a fourth listener, `:9464`, published on `127.0.0.1:9464` only.
  `GET /metrics` serves `gateway_http_*` (requests, latency, in flight per
  plane), `gateway_mcp_*` (requests, tool calls, errors), `gateway_llm_*`
  (calls, tokens, cost, fallbacks, limit denials, detection tee), auth
  failures, database pool, sessions and the Go runtime. The full list is in
  [docs/observability.md](../../docs/observability.md#metric-reference).
- **Prometheus**, always on, at <http://localhost:9090>. It scrapes
  `gateway:9464` every 5 seconds ([prometheus.yml](prometheus.yml)) and
  loads four starter alert rules ([rules.yml](rules.yml)): LLM plane 5xx
  ratio, detection turns dropped, database pool waiting, auth failure
  burst.
- **Grafana**, optional (compose profile `grafana`), at
  <http://localhost:3000>. Anonymous visitors get the Viewer role, so there
  is nothing to log in to. It has the Prometheus datasource and the
  **AI Gateway** dashboard already provisioned: request rate, 5xx rate and
  p95 latency per plane, LLM tokens per model (stacked), LLM cost per
  tenant, tool calls per connector, detection tee results, database
  connections and auth failures.

Metrics are aggregates with deliberately few labels. For per-request
detail (which key, which session, which prompt) use the ClickHouse sink:
see [08-observability](../08-observability/). The admin console does not
read these metrics.

## Prerequisites

- Docker and the `docker compose` plugin
- `curl` and `jq`
- Node.js (`npx` ships with npm) -- only to run the reference "everything"
  MCP server locally
- Free host ports `9464`, `9090` and `3000` (plus the gateway's `8080-8082`)
- No API keys, no cloud account, no LLM provider credentials.

The LLM panels (tokens, cost, detection) stay empty without LLM traffic.
Point a client at the LLM plane as in [07-llm-passthrough](../07-llm-passthrough/)
with a real provider key and they fill in.

## Steps

Run the whole thing with:

```sh
./examples/14-prometheus/run.sh
```

It is safe to re-run. `KEEP_STACK=1 ./examples/14-prometheus/run.sh` leaves
the stack running afterwards so you can look around; on a failure the
stack is always left running.

What `run.sh` does, in order:

1. Starts the local "everything" MCP server (as 01-quickstart does).
2. Brings the stack up with
   `docker compose -f deploy/docker-compose.yml -f examples/14-prometheus/docker-compose.prometheus.yml --profile grafana up -d --build`.
   The overlay sets `GATEWAY_METRICS_DRIVER=prometheus` on the gateway,
   publishes `9464`, and adds the `prometheus` and `grafana` services. (The
   overlay's relative paths start with `../examples/` because Compose
   resolves them against `deploy/`, the directory of the first `-f` file.)
3. Waits for health on the three planes and on the metrics listener,
   bootstraps an admin key, registers the "everything" connector.
4. Sends real MCP traffic: `initialize`, `tools/list`, three `tools/call`
   of `everything__echo`, and one request with an unknown `gk_` key (a
   `401`, so the auth failure counter moves).
5. **Asserts on `GET :9464/metrics`:** `gateway_build_info`,
   `gateway_http_requests_total{plane="mcp",...}`,
   `gateway_mcp_tool_calls_total{connector="everything",tool="everything__echo"}`
   with a count of at least 3, `gateway_auth_failures_total{plane="mcp"}`
   at least 1, `gateway_db_connections`, `go_goroutines`.
6. **Asserts on the Prometheus API:** `up{job="gateway"}` is `1` and
   `gateway_http_requests_total` is non-empty (polling up to 60 seconds for
   the first scrapes); all four alert rules are loaded and healthy; every
   PromQL query in the dashboard runs, and the panels this traffic feeds
   return data.
7. **Asserts on the Grafana API:** `/api/health` is ok, `/api/search?query=Gateway`
   finds the dashboard as an anonymous viewer, `/api/datasources` lists the
   Prometheus datasource, and a query sent through Grafana's datasource
   proxy returns gateway data.
8. Tears the stack down (volumes are kept) unless `KEEP_STACK=1`.

## Look at it yourself

With `KEEP_STACK=1` (or after `docker compose ... up -d` by hand):

```sh
curl -s localhost:9464/metrics | grep '^gateway_'
```

- **Prometheus UI** at <http://localhost:9090>: try
  `sum by (plane, status) (rate(gateway_http_requests_total[1m]))` under
  Graph, `up` under Status > Target health, and Alerts for the four rules.
- **Grafana** at <http://localhost:3000>: opens on the AI Gateway
  dashboard. The admin login is `admin` / `admin`, for editing.

Generate more traffic with the admin key `run.sh` prints, for example
`tools/call` against `http://localhost:8080/mcp` as in 01-quickstart, and
the panels move within a few seconds.

## Use the dashboard in your own Grafana

The dashboard is one file in the repo,
[grafana/dashboards/gateway.json](grafana/dashboards/gateway.json); the
compose stack provisions that same file. To use it elsewhere:

1. In your Grafana, Dashboards > New > Import, upload `gateway.json`.
2. Pick your Prometheus in the "Data source" drop-down at the top (the
   dashboard has a datasource variable, so no editing is needed).
3. Make sure your Prometheus scrapes the gateway's metrics port (`9464`,
   path `/metrics`); on Kubernetes see
   [deploy/README.md](../../deploy/README.md#metrics-prometheus).

The same goes for the alert rules: copy [rules.yml](rules.yml) into your
Prometheus `rule_files` (or convert the expressions to your alerting
system). Thresholds are starting points.

## Security

`/metrics` is **unauthenticated**. It carries no request bodies or
credentials, but it does reveal traffic shape, tenant ids, connector and
model names. This example publishes it on `127.0.0.1` only. In any real
deployment bind it to a private interface or restrict it with a network
policy so that only your Prometheus can reach it (the Kubernetes component
does this). Grafana's anonymous Viewer access and the `admin` / `admin`
login here are for a local demo; do not copy them into a shared
environment.

## Expected output

Abridged:

```
check: gateway /metrics (http://localhost:9464/metrics)
  -> gateway_build_info{go_version="go1.27.1",version="dev"} 1
  -> gateway_http_requests_total{method="GET",plane="mcp",route="/health",status="200"} 1
  -> gateway_mcp_tool_calls_total{connector="everything",outcome="ok",tenant="...",tool="everything__echo"} 3
  -> gateway_auth_failures_total{plane="mcp"} 1
check: Prometheus target up{job="gateway"} == 1
  -> up{job="gateway"} = 1
check: the alert rules are loaded
  -> GatewayLLMServerErrors: ok
  ...
check: every dashboard query runs against Prometheus
  -> 9 dashboard queries ran; panels 1, 3, 6, 8 and 9 have data
check: Grafana finds the dashboard as an anonymous viewer (/api/search?query=Gateway)
  -> [{"title":"AI Gateway","uid":"ai-gateway-overview"}]
================================================================
14-prometheus passed.
```

## Cleanup

`run.sh` removes the containers on success. Otherwise:

```sh
docker compose -f deploy/docker-compose.yml -f examples/14-prometheus/docker-compose.prometheus.yml --profile grafana down      # add -v to drop Postgres data
kill "$(cat examples/14-prometheus/.everything.pid)" 2>/dev/null || true
rm -f examples/14-prometheus/.everything.pid examples/14-prometheus/.everything.log
```
