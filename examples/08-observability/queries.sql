-- Sample analytics queries against the ClickHouse tee (database "gateway";
-- columns: see pkg/sink/clickhouse/migrate.go). run.sh pipes this whole
-- file into clickhouse-client after generating traffic, so every query
-- below returns real rows on a fresh run.
--
-- Run by hand the same way run.sh does (ClickHouse 24.8+ runs multiple
-- ';'-separated statements from stdin by default -- no --multiquery flag
-- needed):
--   docker compose -f deploy/docker-compose.yml \
--     -f examples/08-observability/docker-compose.observability.yml \
--     --profile analytics exec -T clickhouse \
--     clickhouse-client --user default --password local \
--     < examples/08-observability/queries.sql

USE gateway;

-- 1. Top 10 MCP tools by call volume, last 24h.
SELECT tool_name, count() AS calls
FROM mcp_access_logs
WHERE timestamp >= now() - INTERVAL 1 DAY AND method = 'tools/call'
GROUP BY tool_name
ORDER BY calls DESC
LIMIT 10;

-- 2. Slowest MCP calls in the last hour. trace_id lets you jump straight
-- to the matching span tree in Jaeger (http://localhost:16686/trace/<id>)
-- if the gateway's OTel sink was enabled for the call.
SELECT timestamp, tool_name, connector_id, duration_ms, trace_id
FROM mcp_access_logs
WHERE timestamp >= now() - INTERVAL 1 HOUR
ORDER BY duration_ms DESC
LIMIT 20;

-- 3. Estimated LLM cost by model, last 7 days. cost_usd is frozen at
-- write time from pkg/pricing's rate card (see docs/observability.md);
-- unpriced models contribute NULL, so ifNull(...) keeps them in the sum
-- at 0 rather than dropping the row.
SELECT model,
       count() AS calls,
       sum(input_tokens + output_tokens + cache_read_tokens + cache_creation_tokens) AS tokens,
       toFloat64(sum(ifNull(cost_usd, 0))) AS usd
FROM llm_calls
WHERE timestamp >= now() - INTERVAL 7 DAY
GROUP BY model
ORDER BY usd DESC;
