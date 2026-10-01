# Documentation index

This is the documentation for AI Agent Gateway: one Go binary
that fronts MCP tool servers and LLM providers behind authentication,
per-tenant credential injection, agent-profile scoping, and request logging.

Start here:

- **[getting-started.md](getting-started.md)** — after the README
  Quickstart: register an MCP server as a connector and make a first tool
  call.
- **[architecture.md](architecture.md)** — the three planes, the shared
  core, the seams a plugin would extend, request flows, and the
  data model.
- **[configuration.md](configuration.md)** — every key in
  `configs/base/default.yml`, its type, default, and environment variable.
- **[security-model.md](security-model.md)** — identity, tenant isolation,
  secrets handling, what the gateway can and cannot see, and known limits.
- **[authentication.md](authentication.md)** — API keys vs. console users,
  roles, sessions/cookies/CSRF, lockout, password reset (including the
  break-glass CLI), and the emergency API-key login flag.

Operating the gateway:

- **[connectors-and-credentials.md](connectors-and-credentials.md)** —
  registering an MCP backend, credentials and rotation, header types,
  `tool_arg_overrides`, the tool cache, and troubleshooting.
- **[profiles.md](profiles.md)** — agent profiles: what they scope, the
  `X-Agent-Profile-Name` header, and Profile Studio.
- **[models.md](models.md)** — the model catalog: catalog vs. registry,
  the five states on the Models page, the Connect flow, base URL
  convention, permissions, price/capabilities/notes, and the admin catalog
  page.
- **[skills.md](skills.md)** — the skills & commands registry: skill vs.
  command vs. instructions, versions, attachment, and the three ways a
  profile's skills/commands reach an MCP client.
- **[llm-plane.md](llm-plane.md)** — the LLM proxy: provider routes, BYOK
  setup, Bedrock credential modes, limits, streaming, and capture.
- **[observability.md](observability.md)** — sinks, the analytics API, the
  console, trace propagation, and sample ClickHouse queries.
- **[api.md](api.md)** — the control-plane REST API: auth, error shape,
  pagination, and the full route table.
- **[third-party-notices.md](third-party-notices.md)** — where every
  third-party license notice lives (release archive, container image,
  console, SBOM), and this docs site's own third-party components.

Project:

- **[SECURITY.md](https://github.com/Tuskira/tusk-ai-secured-gateway/blob/main/SECURITY.md)** — supported versions and how to
  report a vulnerability.
- **[CONTRIBUTING.md](https://github.com/Tuskira/tusk-ai-secured-gateway/blob/main/CONTRIBUTING.md)** — dev setup, `make` targets,
  and conventions for a pull request.
- **[CODEOWNERS](https://github.com/Tuskira/tusk-ai-secured-gateway/blob/main/CODEOWNERS)** — who reviews what.

## Status

The gateway is pre-1.0. The current release ships all three
planes, the encrypted credential store, agent profiles, the control-plane
API, OTel/ClickHouse sinks with analytics, and the embedded admin console.
CI, tagged releases to GHCR, and Kubernetes manifests (`deploy/k8s`,
see [deploy/README.md](https://github.com/Tuskira/tusk-ai-secured-gateway/blob/main/deploy/README.md)) are in place.
