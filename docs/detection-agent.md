# Detection agent

The detection agent is a small sidecar program that sits next to the
gateway. After each call completes, the gateway posts it as one turn
([`llm_proxy.detection.agent_url`](llm-plane.md#detection-agent)); the agent
pulls out the new turn, removes secrets from it on the same host, and sends
it to a remote **detection engine** to be judged and recorded. This is
detection only: nothing in the gateway waits for the agent, and the agent
never blocks or changes a call.

This repository ships the agent and the contract between the agent and an
engine. It does **not** ship an engine. Without one, the agent has nothing to
ask, so it is only useful if you run an engine that implements
[the contract below](#the-agent-to-engine-contract) (your own, or one from a
vendor).

The agent is its own Go module, `detection/` (module path
`github.com/Tuskira/tusk-ai-secured-gateway/detection`), with its own
`go.mod`, dependencies, container image and version tags
(`detection/vX.Y.Z`). The gateway does not import it; the two only speak the
JSON contract in [llm-plane.md](llm-plane.md#contract).

## Why a sidecar

- **Redaction stays on your side.** The gateway forwards request bodies
  byte-for-byte and holds the raw text. The agent is the one place that
  decides what part of a call may leave the host, so nothing downstream has
  to be trusted to redact.
- **The gateway stays dependency-light.** Secret scanning pulls in a large
  ruleset and a regex engine; it runs in the agent's process, not the
  gateway's.
- **Failure is contained.** The gateway posts after the call is done and
  drops what the agent does not take; the agent and the engine can be down
  or slow without any call noticing ([failure handling](#failure-handling)).

```
client ──> gateway ──> provider
              │
              └─(complete turn, after the call, loopback)──> detection agent ──(redacted stages, https)──> engine
```

## What leaves the host

For each judgment the agent sends the engine two things, nothing else:

1. **Metadata:** time, tenant id, request id, session id, gateway key id,
   principal, model and request path, as the gateway recorded them. The
   principal is whatever identity string the gateway authenticated, which may
   be an email address.
2. **The redacted new turn.** Not the whole conversation. An agent client
   resends its full history on every call, so the agent extracts only what is
   new in this call:
   - `user_text`: what the user typed in the trailing user/tool messages
     (text the harness injected, such as `<system-reminder>` sections, is
     taken out of it).
   - `user_goal`: the latest text the user typed anywhere in the
     conversation. This can come from an earlier message than the new turn.
     When the request does not carry the whole conversation (an OpenAI
     Responses call that continues a stored one, `history: server_side`, or
     a flat legacy prompt, `history: prompt`), it is only the new turn's own
     `user_text`, and empty when the turn has none: the goal is unknown.
   - `harness_text`: text in the new turn that is not the user's words: the
     `<system-reminder>…</system-reminder>` sections of user messages and the
     turn's mid-conversation `system`/`developer` messages; and, when the
     agent reads the gateway's canonical conversation, the text of an
     attached text document and the wire form of a block the canonical shape
     has no slot for (`opaque`). It is sent, not hidden, because a payload
     can be wrapped in the tag or hidden in a file.
   - `tool_results`: the tool outputs returned in this turn, each with the
     tool name.
   - `prior_tool_calls`: the assistant's tool calls that produced them.
   - For the response stage: `response_text` and `response_tool_calls`.

   A body the agent cannot read as a chat request (malformed JSON, a repeated
   or case-variant key, no `messages`) is sent with **no text**: `unreadable`
   is `true`, `state` and `refs` are empty, and `secrets` lists the kinds of
   secret found in its raw bytes and in each string a lenient decoder reads
   out of them (all at `user_text`). The engine records an error on every
   rule, refuses the call in enforce mode, and the secret rule decides on the
   hits alone.

The raw request and response bodies, earlier history and everything the agent
could not recognise as part of the new turn stay on the host.

### Redaction

Before anything is clipped or sent, every string of the body is scanned for
secrets and each secret is replaced with `[REDACTED:<kind>]`, where `<kind>`
is the rule id that found it (for example `[REDACTED:aws-access-token]`). The
rules are the [gitleaks](https://github.com/gitleaks/gitleaks) default
ruleset plus two the agent adds (`detection/turn/secrets.toml`):

| Rule id | Finds |
|---|---|
| `connection-string-password` | `user:password@` in a database or broker URL (`postgres`, `mysql`, `mongodb`, `redis`, `amqp`, ...). Obvious placeholders such as `${DB_PASSWORD}` or `<password>` are not treated as secrets. |
| `private-key-unterminated` | A private-key block clipped before its `END` line. A complete block is found by the default `private-key` rule. |

Redaction happens before clipping, so a cut can never split a secret in two
and let half of it through. The rule is: no part of any secret value the
scanner finds anywhere in the raw request or response body appears in any
string sent. Values are collected from every string of the body, not only the
new turn (history, system prompt, tool inputs and results, an assistant
prefill, and for a response stage the whole request). Each value of 8 or more
bytes is then removed from every string sent, in every encoding a tool
input's JSON gives it (tool inputs travel in one canonical JSON form), and
overlapping values are removed as one span. The kinds found in strings that
are sent travel with the turn as `secrets: [{kind, field, index}]` (where
each kind was first seen), without the values.

Every byte of every string of the body is scanned, however long the string:
a value stated in the middle of a long tool output from an earlier turn is
found, and a bare copy of it in the new turn is removed. A long string is
scanned in pieces of 256 KiB that overlap by 64 KiB, so a secret (with the
context its rule needs, such as `api_key = "`) up to 64 KiB long is found
whole wherever a piece's edge falls.

That makes preparing a turn linear in the body but not free: on keyword-dense
text it costs roughly 0.2 seconds per MiB of strings (a 10 MiB body about
2.3 seconds on an Apple M5). So it is bounded by a deadline instead of by
what is scanned: past `PrepareTimeout` (4 seconds by default; a field of the
Go `agent.Config`, not an environment variable) the stage is sent as
`not_judged` (reason "...ran past its deadline"), with no text, never partly
redacted, and the agent logs it. The scan stops between two rules of one
piece, so it ends at most about 0.1 seconds after the deadline. This applies
to every stage, including those of `POST /v1/turns`. The agent also caches
scans of strings of 1 KiB or more across calls (an agent client resends its
history every call, so it is scanned once); only a scan that ran to the end
is cached. The cache holds raw values, so it is on only in the agent
(`turn.EnableScanCache`) and never in an engine importing `turn`. It keeps
copies of the values found, never the scanned text, and is bounded by
`AGENT_SCAN_CACHE_BYTES` (64 MiB by default) as well as by 8192 entries;
when either is full it drops entries until both are at half.

The scanner ignores gitleaks' inline allow comment (`gitleaks:allow`): in a
repository it marks a known false positive, but in a call it is only text
the sender wrote, and honouring it would keep the secret on its line.

### Limits you should know about

- **It is pattern matching.** It finds what the rules recognise: known token
  formats, keys with telltale prefixes, high-entropy values next to words
  like `secret` or `api_key`, connection-string passwords, private-key
  blocks. It does not find a password in prose, a secret that is encoded or
  split across fields, or a token format no rule covers.
- **It only removes secrets.** Personal data, source code, customer data and
  anything else in the turn that is not a secret is sent as it is. If that
  must not leave your network, do not point the agent at a remote engine you
  do not control.
- **Fields are clipped.** `user_text`, `user_goal`, `harness_text` and
  `response_text` to 8000 bytes, a tool result to 6000, a tool input to 2000;
  at most 8 tool results or tool calls per list. A clipped string keeps its
  first two thirds and last third around `…[truncated]…`.
- **A very long secret may be found only in part.** A long string is
  scanned in overlapping pieces, so a single "secret" longer than 64 KiB
  (with its context) that a piece's edge cuts may be missed. A private-key
  block is the exception: the piece holding its `BEGIN` line reports it as
  `private-key-unterminated` and the piece holding its `END` line finds the
  block whole, and both are removed.
- **A huge body is not judged.** When the scan does not finish within
  `PrepareTimeout` the stage is sent as `not_judged`, with no text.
- **Deeply nested or very long content is judged on what fits.** Content
  blocks nested more than 8 levels deep (a message's own content is level
  0; a `tool_result` inside a `tool_result` is one level more) are not read
  as blocks: what is past level 8 is added to `harness_text` (to
  `response_text` for a reply) as its JSON, under `[content nested deeper
  than 8 levels, as JSON:]`. Past 65536 content blocks in one body or
  conversation, the rest is left out, oldest message first (the new turn
  is read first), and the stage's text ends with `[content truncated: N
  blocks beyond the cap]`. What is left out is still searched for secrets
  (every string of the raw body is), so a value found there is removed
  from what is sent. Both caps apply the same way to the raw body and the
  canonical conversation. Only an unreadable body or the `PrepareTimeout`
  deadline sends a stage as `not_judged`.
- **The engine should not rely on it.** A turn is whatever the sender
  produced. An engine that stores or forwards turns should re-apply the same
  caps and re-run the same scan on what it receives; the `turn` package
  (`PreparedTurn.Normalized`) does that and can be imported by Go engines.
  `Normalized` also bounds its own work whatever it is sent: every string is
  cut to its cap plus 4 KiB at each side, the `not_judged` reason to 4 KiB,
  the lists to 8 items and `secrets` to 256 hits before anything is scanned.

## How a turn is judged

The gateway's tee posts one complete turn to `POST /v1/turns` and gets `202`
at once. From that one turn the agent derives up to two stages, queues them
as one unit, and a worker prepares (extracts and redacts) and sends them to
the engine in this order:

1. The **request stage**, always. This also holds when the call failed
   (`status_code` is not `2xx`): the prompts were still sent to the provider.
2. The **response stage**, only if the turn carries a `response` (the
   gateway sends one only for a `2xx` that was relayed completely). It is
   built from the request body and the response body together, so it carries
   the request's `user_goal` and `tool_results` for context. A response with
   nothing to judge (no text, no tool calls) sends nothing.

Both stages go out under the gateway's request id. `status_code` is not
passed on: the engine's `meta` has no field for it.

A **batch** (`op: "batch"` with `items`: a Message Batches or Gemini
`batchGenerateContent` creation call) is judged item by item instead: one
request stage per item, in order, each prepared from the item's
`conversation` as if it were a call of its own, and each sent under the
batch call's `request_id` with the item's id in `meta.item`. Secret
scanning covers the whole raw batch body, so a value found in one item's
prompt is removed from every item. There is no response stage (the
response is the batch object, which holds no generation). At most 1000
items are judged; when the turn carries more, or the gateway read only part
of the batch (`normalize_error` set), one `not_judged` request stage with
the reason (no `meta.item`) follows the items. A batch the gateway could
not read at all (no `items`) is judged as one request stage from its raw
body, as any unread turn is.

Judging is always in the background and never inline, whatever the engine
says. The engine's [policy](#get-v1policy) `inline` and `want_response`
flags do not matter for `/v1/turns` (the agent does not even fetch the
policy for it): the gateway's tee cannot act on a verdict, so there is
nothing to hold a call for, and the response stage follows whenever there is
a response. The engine's answer is only recorded, and any block it contains
is a recorded outcome, not something that reaches a client.

The queue holds up to `AGENT_QUEUE_SIZE` turns and `AGENT_QUEUE_BYTES` of raw
turn bytes (request plus response), worked by up to `AGENT_MAX_IN_FLIGHT`
workers. A turn's bytes count against `AGENT_QUEUE_BYTES` until its
judgment is done, not only while it waits: a worker holds the raw turn
while it prepares and sends it. A batch is one turn there, whatever its number of items: its raw
bytes count once and it takes one slot. A turn that does not fit is not judged: the agent still answers
`202` and sends the engine one `not_judged` marker for the request stage (no
state, a short reason) so the gap is visible there; no response stage
follows.

### Inline contract

The older pair `POST /v1/turns/request` and `POST /v1/turns/response` is
still served and tested, but the gateway's current tee does not use them
(see [below](#inline-contract-not-used-by-the-gateways-tee)). There, per
tenant, the policy chooses between judging a request before the call is
forwarded (`inline: true`) and the background queue.

### Failure handling

Nothing the agent or engine does can affect a call: the gateway has already
finished it before the turn is posted.

- **Engine down, slow, non-`200`, or an unreadable answer:** logged as a
  warning, the stage is dropped.
- **Queue full:** `202`, and one `not_judged` marker as above.
- **A bug while preparing a turn (a panic):** recovered, in a worker and on
  the inline path. It is logged at `error` level with its stack (the panic
  value only when the Go runtime raised it, since another may quote the
  turn), the stage is sent as `not_judged` ("the agent failed preparing the
  turn"), and the agent keeps running.
- **Agent down, slow, or answering other than `202`:** the gateway drops
  the turn and counts it in its `/health`; see
  [llm-plane.md](llm-plane.md#queue-limits-and-drops).
- **Shutdown (`SIGTERM`):** the agent stops accepting turns, finishes open
  ones, then drains the queue for up to 30 seconds.
- **Inline contract only:** an engine failure while judging a request inline
  lets the call through, and a failed policy lookup keeps the last policy
  (with none, `inline: false, want_response: true`), cached for one policy
  TTL.

## Running it

Build and run:

```sh
make detection-build        # -> bin/detection-agent
DETECTION_ENGINE_URL=https://engine.example.com \
DETECTION_AGENT_TOKEN="$ENGINE_TOKEN" \
  ./bin/detection-agent
```

Or the container image, built from `Dockerfile.detection-agent` (published
for tags `detection/vX.Y.Z` as
`ghcr.io/tuskira/ai-agent-gateway-detection-agent:<version>`):

```sh
docker build -f Dockerfile.detection-agent -t detection-agent .
```

Point the gateway at it:

```yaml
llm_proxy:
  enabled: true
  detection:
    agent_url: http://127.0.0.1:8090
```

### Kubernetes sidecar

`deploy/k8s/components/detection-agent` is an **example** kustomize
Component that runs the agent as a second container in every `gateway-llm`
pod. It:

- adds the `detection-agent` container with `AGENT_LISTEN=127.0.0.1:8090`
  (pod-local, since the agent's port has no authentication) and
  `DETECTION_ENGINE_URL` / `DETECTION_AGENT_TOKEN` from the Secret
  `detection-agent` (a template with placeholders, like
  `base/secret.yaml`);
- sets `GATEWAY_LLM_PROXY_DETECTION_AGENT_URL=http://127.0.0.1:8090` on the
  gateway container, so its tee posts each completed call to the agent in
  the same pod.

`deploy/k8s/overlays/detection-agent` is the base plus this component; add
`../../components/detection-agent` under `components:` of any other
overlay to combine it with Redis or analytics. Before applying it, fill in
the Secret and set the agent's image tag with an `images:` entry (the
component's tag is a placeholder; the agent is versioned on its own, see
above):

```sh
kubectl kustomize deploy/k8s/overlays/detection-agent   # inspect
```

The agent has no probes (kubelet cannot reach a loopback port, and a down
agent never affects a call). It shares the pod's network, so the base
NetworkPolicy applies to it: its rule for port 443 to public addresses
covers an engine with a public `https` URL; an engine inside the cluster or
on a private address needs an egress rule of its own.

### Configuration

All configuration is environment variables. A value that does not parse, or
is not positive, fails startup.

| Variable | Default | Meaning |
|---|---|---|
| `DETECTION_ENGINE_URL` | required | Base URL of the engine (`/v1/policy` and `/v1/detect` are appended; a trailing `/` is ignored). Use `https`. |
| `DETECTION_AGENT_TOKEN` | required | Bearer token sent to the engine on every call. |
| `AGENT_LISTEN` | `127.0.0.1:8090` | Address the agent serves the gateway on. |
| `AGENT_MAX_IN_FLIGHT` | `256` | Background judgments running at once (also the engine connection pool size). |
| `AGENT_QUEUE_SIZE` | `1024` | Background turns waiting for a worker. |
| `AGENT_QUEUE_BYTES` | `268435456` (256 MiB) | Raw request plus response bytes held by queued turns. |
| `AGENT_SCAN_CACHE_BYTES` | `67108864` (64 MiB) | Memory the secret-scan cache may use for the values it keeps (see [What leaves the host](#what-leaves-the-host)). |
| `AGENT_ENGINE_TIMEOUT` | `8s` | Budget for one inline judgment (a Go duration such as `8s`). With the 1 second policy lookup it fits a gateway's 10 second wait for a verdict with a second to spare. Inline contract only. |
| `AGENT_POLICY_TTL` | `15s` | How long a tenant's policy is cached. Inline contract only. |
| `AGENT_LOG_LEVEL` | `info` | `debug`, `info`, `warn` or `error` (any case). At `debug` the agent logs one line per judged turn: `tenant`, `request_id`, `stages` (e.g. `request,response`), `returned` (judgments the engine answered), `dropped` (judgments that failed), `bytes` (the turn's raw request plus response) and `prepare_ms`. The line carries no text of the turn. |

The agent logs to stderr through `log/slog`, as text. `GET /healthz` answers `200 ok`
and checks nothing else (not the engine).

## The gateway to agent side

### POST /v1/turns

What the gateway's tee calls: one complete turn per call, after the call has
completed. The shape and the canonical fixtures
(`internal/llmplane/testdata/detection/turn.json` and
`turn_no_response.json`) are in [llm-plane.md](llm-plane.md#contract).
Fields (`wire.Turn`): `v`, `id`, `tenant_id`, `session_id`, `key_id`,
`principal`, `model`, `path`, `at`, `status_code`, `request` (base64, the raw
request body, in full) and `response` (base64, the first 1 MiB, present only
when the upstream answered `2xx` and the relay completed).

The gateway also sends `dialect` (the wire format of the route), `op`
(`generate` or `batch`), and, when it could read the bodies, `conversation`
and `answer`: the request and response in one canonical shape whatever the
provider (`wire.Conversation`, `wire.Answer`), with `answer.truncated` set
when the response was cut. When it could not (a route with no reader, such
as a body that does not parse, or a Gemini batch that names an uploaded
file), `normalize_error` says why and only the raw bodies are there. A
batch carries `items` instead of `conversation` and `answer`: each request
of the batch as `{custom_id, conversation}`, at most 1000 (see
[How a turn is judged](#how-a-turn-is-judged)). The per-route table and the
canonical shape are in [llm-plane.md](llm-plane.md#contract).

The agent extracts the new turn from `conversation` and the reply from
`answer` when they are present and `normalize_error` is empty, so every
generation route a gateway with all its readers relays (Anthropic Messages
and legacy Text Completions, OpenAI Chat Completions, Responses and legacy
Completions, Gemini, Bedrock Converse and invoke) is judged the same way.
Otherwise (an older gateway, a batch it could not read, a body the gateway
could not read, a `cv` newer than the agent's) it falls back to the raw `request` and
`response`, which it reads in the Anthropic Messages and OpenAI Chat
Completions shapes. Secret scanning always covers the raw bodies: every
value found anywhere in them (and in the canonical forms) is removed from
what is sent, whichever source the turn was read from.

| Answer | When |
|---|---|
| `202` empty body | The turn is accepted: queued, or shed with a `not_judged` marker when the queue is full. The agent answers before it talks to the engine. |
| `400` | Not JSON or bad base64; `id`, `tenant_id`, `at` or `request` missing; `v` above `1` (body starts with `unsupported wire version`); or a body over 64 MiB. A `v` that is absent or `0` is `1`. |

Both stages are derived from this one turn and are always judged in the
background; see [How a turn is judged](#how-a-turn-is-judged). What leaves
the host is as described [above](#what-leaves-the-host): the request stage,
and the response stage when there is a response, each redacted. The
`status_code` and the raw bodies stay on the host.

`GET /healthz` answers `200 ok`.

### Inline contract (not used by the gateway's tee)

| Route | Answer |
|---|---|
| `POST /v1/turns/request` | `200` with `{"block": ..., "want_response": ...}` once the policy is known (and, for an `inline` tenant, once the engine has judged). |
| `POST /v1/turns/response` | `202` with an empty body; the turn is judged in the background. |

These take a single stage (`wire.TurnRequest`: the same fields without `v`
and `status_code`; `response` only on `/v1/turns/response`) and are the
contract of a gateway that holds the call for a verdict and may refuse it.
The gateway in this repository does not do that: its tee posts to
`/v1/turns` only. They are kept working, tested, and served by the same
agent, but treat them as the legacy inline contract; they are not covered by
the gateway fixtures. On `/v1/turns/request` an `inline` tenant's call waits
for the engine (up to `AGENT_ENGINE_TIMEOUT`, and the engine failing lets
the call through), and a `blocking` rule in the answer is returned as
`block`.

A body larger than 64 MiB is refused with `400` on every route. The agent's
copy of the fixtures, `detection/wire/testdata/gateway/`, must stay
byte-identical to the gateway's; a test in each module pins its own types to
its copy and `make detection-contract` fails if the copies differ.

## The agent to engine contract

Two engine endpoints. Both use `Authorization: Bearer <DETECTION_AGENT_TOKEN>`
and JSON bodies (`Content-Type: application/json`). The agent treats any
status other than `200` as a failure. The Go types are in `detection/wire`
(`wire.DetectRequest`, `wire.DetectResponse`, `wire.PolicyResponse`) and
`detection/turn` (`turn.PreparedTurn`).

### Versioning

Every `POST /v1/detect` carries `"v"`, the contract version (today `1`;
absent means `1`). The rule:

- Fields are only ever **added**. Both sides ignore a field they do not know,
  so a newer agent works with an older engine and the reverse.
- `v` goes up only for a change an older engine would misread. An engine
  that receives a `v` above its own must answer `400` with a body that
  starts with `unsupported wire version`. The agent then logs that the engine
  needs an upgrade (at most once a minute); judgments are dropped until it is.

### GET /v1/policy

`GET /v1/policy?tenant_id=<tenant id>` returns how to route that tenant:

```json
{"inline": false, "want_response": true}
```

- `inline`: judge request-stage turns before the call is forwarded (the
  tenant's configuration says a request can be refused).
- `want_response`: send response-stage turns too (some response rule is on).

Only the [inline contract](#inline-contract-not-used-by-the-gateways-tee)
asks for it; `POST /v1/turns` never does. The agent caches the answer per
tenant for `AGENT_POLICY_TTL`. On that contract it is on the request path of
a call on a cache miss and has a 1 second budget, so answer it from memory.

### POST /v1/detect

Asks the engine to judge one redacted stage. Example (a request-stage turn):

```json
{
  "v": 1,
  "sync": true,
  "meta": {
    "at": "2026-01-02T03:04:05Z",
    "tenant_id": "tenant-1",
    "request_id": "req_0123456789abcdef",
    "session_id": "session-1",
    "key_id": "key-1",
    "principal": "alice@example.com",
    "model": "example-model",
    "path": "/v1/messages"
  },
  "turn": {
    "stage": "request",
    "state": {
      "user_text": "deploy with AWS_ACCESS_KEY_ID=[REDACTED:aws-access-token] please",
      "user_goal": "deploy with AWS_ACCESS_KEY_ID=[REDACTED:aws-access-token] please"
    },
    "refs": {},
    "secrets": [{"kind": "aws-access-token", "field": "user_text", "index": 0}]
  }
}
```

| Field | Notes |
|---|---|
| `v` | Contract version, see above. |
| `sync` | `true` when the agent is waiting for the answer to enforce it (inline contract only; always `false` for turns from the gateway's tee). Informational: record the turn either way. |
| `meta` | `at`, `tenant_id`, `request_id` (the gateway's request id: the same id on both stages of a call, and the id of the call's gateway log row), and when known `session_id`, `key_id`, `principal`, `model`, `path`. For one request of a batch, `item` is its id within the batch (its `custom_id`, at most 128 bytes, any secret in it redacted): every item of a batch shares the batch call's `request_id`, so a record is keyed by `request_id` and `item`. For a turn from the gateway's tee, also `dialect` (the wire format the call was made in, e.g. `anthropic`, `gemini_batch`) and `op` (`generate` or `batch`), and `history` (`full`, `server_side` or `prompt`: how much of the conversation the judged request carries) when the stage was read from the gateway's canonical conversation; absent `history` means `full` or unknown (a stage read from the raw body). The agent reads `history` case-insensitively and sends it in lower case; a value it does not know is sent as it is (lower-cased) and is read as not `full` (the goal is the new turn's own text). All four are optional strings, omitted when empty; an engine that does not know them ignores them, and `v` stays `1`. |
| `turn.stage` | `request` or `response`. |
| `turn.state` | The redacted new turn: `user_text`, `user_goal`, `harness_text`, `tool_results` (`[{tool, content}]`), `prior_tool_calls` (`[{name, input}]`), `response_text`, `response_tool_calls` (`[{name, input}]`). Omitted when empty. A request-stage turn has no `response_*` fields; a response-stage turn carries the request's `user_goal` and `tool_results` for context. |
| `turn.refs` | Per-event ids that are not part of what a judge model reads, so a decision can point at an event: `tool_results` (`[{call_id, call_input}]`), `prior_tool_calls` and `response_tool_calls` (lists of call ids), matched to `state` by index. |
| `turn.secrets` | `[{kind, field, index}]`: the rule id of each kind of secret found, the `state` field it was first seen in (`user_text`, `tool_results`, `harness_text`, `response_text`, `response_tool_calls`) and the index of the event. No values. |
| `turn.unreadable` | `true` when a request body could not be read as a chat request. `state` and `refs` are then empty and `secrets` holds the hits found in its raw bytes (field `user_text`); no text is sent. |
| `turn.not_judged` | Set, with no `state`, when the agent could not hand the call over because its queue was full or preparing it ran past its deadline; the value is the reason. Record it as an error, do not judge it. |

The answer is `200` with a result, or `null` when no rule applied:

```json
{"result": {"stage": "request", "blocking": {"rule_id": "example-rule", "owasp": "LLM01", "title": "Example rule", "probability": 0.97, "threshold": 0.9, "detail": "Example detail"}}}
```

```json
{"result": null}
```

- `result.stage` repeats the stage judged.
- `result.blocking` is set only when a rule says the stage is blocking.
  Its fields are `rule_id`, `owasp`, `title`, `probability?`, `threshold`
  and `detail?`. For a background judgment (everything from the gateway's
  tee) it is only recorded; the agent acts on a `blocking` only for an
  inline-contract judgment (`sync: true`).
- The body must still be valid JSON for a background judgment (`{"result":
  null}` is fine); its content is ignored.

An engine should record every turn it receives before it answers, whether or
not a rule fired. It receives the request stage first and then, when there
is one, the response stage of the same `request_id`; the two come from one
worker, one after the other. For a batch it receives one request stage per
item, all with the same `request_id` and each with its own `meta.item`, in
the batch's order, from one worker.

## Security notes

- **The agent's port has no authentication.** Anyone who can reach it can
  submit turns, fill the queue and spend your engine
  quota. It listens on `127.0.0.1:8090` by default; keep it
  on loopback or pod-local. A sidecar in the same pod, or a process on the
  same host, reaches it there with no other configuration. Do not publish
  the port or bind it to a wider address.
- **The gateway sends the agent unredacted bodies.** The request body and
  the first 1 MiB of the response, raw, over a plain HTTP client, after each
  call. The agent
  is a trusted process on the same host or pod, never a shared or remote
  service. See
  [security-model.md](security-model.md#outbound-requests-ssrf).
- **Use `https` for the engine URL.** The agent does not require it, but the
  bearer token and the redacted turns go over that connection. The agent logs
  the engine URL at startup, so do not put credentials in it; use the token.
- **The token authorises judgments for every tenant** the gateway fronts.
  Keep it in a secret store and rotate it like any service credential.
- **Redaction is best effort.** See [Limits you should know
  about](#limits-you-should-know-about).
