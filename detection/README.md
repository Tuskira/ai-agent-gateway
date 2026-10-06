# detection

The detection agent: a sidecar that receives each completed call from the
gateway's detection tee (`llm_proxy.detection`, `POST /v1/turns`), extracts
the new turn, redacts secrets on the local host, and sends it to a remote
detection engine to be judged and recorded. Detection only: it answers `202`
at once and works from a background queue. This module ships the agent and
the agent-to-engine contract; it does not include an engine.

It is a separate Go module (`github.com/Tuskira/tusk-ai-secured-gateway/detection`)
with no dependency on the gateway's, versioned with tags `detection/vX.Y.Z`.

| Path | What |
|---|---|
| `cmd/detection-agent` | The binary (configured by environment variables). |
| `internal/agent` | HTTP handlers (`/v1/turns`, plus the older inline `/v1/turns/request` and `/v1/turns/response`), policy cache, background queue, engine client. |
| `turn` | Extracts the new turn from a request/response body and redacts secrets. Importable by an engine. |
| `wire` | The JSON types of the gateway-to-agent and agent-to-engine contracts. `wire/testdata/gateway` is a byte-identical copy of the gateway's fixtures. |

From the repository root:

```sh
make detection-build       # -> bin/detection-agent
make detection-test        # go test -race
make detection-vet detection-lint detection-vuln
make detection-contract    # the fixture copies match the gateway's
```

Or from this directory: `go build ./... && go test -race ./...`.

Everything else (data flow, redaction and its limits, settings, the engine
contract, security notes) is in [docs/detection-agent.md](../docs/detection-agent.md).
