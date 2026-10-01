# Security Policy

## Supported versions

AI Agent Gateway is pre-1.0. Security fixes land on the latest
minor line; there is no long-term-support branch yet.

| Version | Supported |
|---|---|
| 0.3.x | yes |
| 0.2.x | best effort until 0.4.0 ships, then unsupported |
| < 0.2.0 | no |

## Reporting a vulnerability

Email **security@tuskira.ai** with a description of the issue, the
affected version or commit, and reproduction steps if you have them. Do
not open a public GitHub issue for a security report.

Once this repository is public, you can instead use GitHub's private
vulnerability reporting (Security -> Report a vulnerability). It is not
available while the repository is private, so email remains the channel
until then.

## Response targets

- **Acknowledgement:** within 3 business days of your report.
- **Triage and severity assessment:** within 10 business days, with a plan
  and an expected timeline.
- **Fix target:** 30 days for a high-severity issue, from confirmation to
  a released fix or documented mitigation. Lower-severity issues are
  scheduled case by case; we'll tell you the plan once we've assessed
  impact.

We will keep you updated at least every 2 weeks until the report is closed.

## Hardening documentation

Deployment-relevant security settings are documented in
[docs/security-model.md](docs/security-model.md) (identity, tenant isolation,
secrets and logs, off-by-default features, known limits)
and [docs/configuration.md](docs/configuration.md). Notably: unknown config
keys fail startup, captured request/response bodies are never printed to
stdout unless `sinks.stdout.include_bodies` is set, and LLM capture bodies
are capped at 1 MiB by default.

## Coordinated disclosure

We ask that you give us the chance to investigate and release a fix (or a
mitigation) before any public disclosure. We'll credit reporters who want
credit, in the release notes of the fix, once it ships.

## Scope

In scope: the gateway binary (`cmd/gateway`, `internal/`, `pkg/`), the
embedded admin console (`web/`), and the Docker image build
(`Dockerfile`). Known, already-documented limitations of the current
design — e.g. no OIDC support yet, no per-connector rate limiting, cost
figures being estimates — are tracked as ordinary issues, not security
reports; see [docs/security-model.md](docs/security-model.md#known-limits)
for the current list before reporting one of those.

Out of scope: vulnerabilities in a third-party MCP server or LLM provider
you've configured the gateway to talk to, and social-engineering or
physical-access attacks against a deployment's own infrastructure.
