// Package config loads and validates the gateway's configuration.
//
// Precedence, low to high: built-in defaults, an optional YAML file, then
// environment variables of the form GATEWAY_<SECTION>_<FIELD> (derived from
// the yaml struct tags, e.g. GATEWAY_LLM_PROXY_ENABLED for LLMProxy.Enabled).
package config

import (
	"context"
	"fmt"
	"net"
	"net/netip"
	"net/url"
	"os"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/Tuskira/tusk-ai-secured-gateway/internal/clientip"
	"github.com/Tuskira/tusk-ai-secured-gateway/internal/netguard"
)

// Config is the root configuration for the gateway binary.
type Config struct {
	Service     Service     `yaml:"service"`
	MCP         MCP         `yaml:"mcp"`
	API         API         `yaml:"api"`
	LLMProxy    LLMProxy    `yaml:"llm_proxy"`
	Database    Database    `yaml:"database"`
	Redis       Redis       `yaml:"redis"`
	Auth        Auth        `yaml:"auth"`
	SecretStore SecretStore `yaml:"secret_store"`
	ToolCache   ToolCache   `yaml:"tool_cache"`
	Sessions    Sessions    `yaml:"sessions"`
	Connectors  Connectors  `yaml:"connectors"`
	Egress      Egress      `yaml:"egress"`
	Skills      Skills      `yaml:"skills"`
	MCPCatalog  MCPCatalog  `yaml:"mcp_catalog"`
	Capture     Capture     `yaml:"capture"`
	Sinks       Sinks       `yaml:"sinks"`
	Logging     Logging     `yaml:"logging"`
	Ingest      Ingest      `yaml:"ingest"`
}

// Service identifies the running binary.
type Service struct {
	Name    string `yaml:"name"`
	Version string `yaml:"version"`
}

// MCP configures the MCP tool-server plane.
type MCP struct {
	Enabled     bool          `yaml:"enabled"`
	Address     string        `yaml:"address"`
	ReadTimeout time.Duration `yaml:"read_timeout"`
	// WriteTimeout bounds how long a response may take to write.
	// It defaults to 0 (unlimited) because GET /mcp/stream is a
	// long-lived SSE channel: any non-zero value would sever it on a
	// fixed schedule, since net/http's write deadline covers the whole
	// response, not an individual write. Bound the request side with
	// ReadTimeout and the backend side with connectors.default_timeout_ms
	// instead.
	WriteTimeout time.Duration `yaml:"write_timeout"`

	// RequireProfile rejects any MCP request that carries no
	// X-Agent-Profile-Name header with JSON-RPC -32003. Default false:
	// a caller with no profile header gets every tool its tenant owns
	// (an authenticated, tenant-scoped set). Turn it on to make the
	// profile allow-list mandatory.
	RequireProfile bool `yaml:"require_profile"`

	// MaxUpstreamStreamsPerSession bounds the long-lived
	// server-to-client streams the gateway holds open to connectors on
	// behalf of one agent session (one per connector it subscribed to a
	// resource on). Past it, the request that needed another stream is
	// refused. Default 16.
	MaxUpstreamStreamsPerSession int `yaml:"max_upstream_streams_per_session"`
	// MaxSubscriptionsPerSession bounds the resources/subscribe
	// subscriptions one agent session may hold. Default 256.
	MaxSubscriptionsPerSession int `yaml:"max_subscriptions_per_session"`
	// MaxPendingServerRequestsPerSession bounds the server-initiated
	// requests (sampling, elicitation, roots) relayed to one agent
	// session that may await its answer at once. Past it, the connector
	// is answered -32603. Default 32.
	MaxPendingServerRequestsPerSession int `yaml:"max_pending_server_requests_per_session"`
	// ServerRequestTimeout is how long a server-initiated request relayed
	// to an agent may await its answer before the connector is answered
	// -32603 and the agent is sent notifications/cancelled. It is long
	// by default because an elicitation waits on a human. The time is
	// not charged against the connector's own timeout_ms. Default 5m.
	ServerRequestTimeout time.Duration `yaml:"server_request_timeout"`
}

// ToolCache configures the tool-list cache in front of the connectors.
type ToolCache struct {
	// Enabled turns the Postgres-backed cache on. With it off every
	// tools/list fans out live to every healthy connector.
	Enabled bool `yaml:"enabled"`
	// L2TTL is how long a cached tool row stays fresh.
	L2TTL time.Duration `yaml:"l2_ttl"`
	// RefreshInterval is how often the background refresher re-reads
	// every tenant's connectors and rewrites their cached tools.
	RefreshInterval time.Duration `yaml:"refresh_interval"`
	// CleanupInterval is how often expired rows are deleted.
	CleanupInterval time.Duration `yaml:"cleanup_interval"`
	// ServeStale serves expired rows (and kicks off a refresh) rather
	// than blocking a tools/list on a live fan-out. Default true.
	ServeStale bool `yaml:"serve_stale"`
}

// Sessions configures MCP session lifetime and storage.
type Sessions struct {
	// Store names the pkg/session driver sessions live in: "memory" (the
	// default: in-process, one replica), "redis" (shared, so any mcp
	// replica can serve any session and tools/list_changed reaches every
	// replica's streams; needs redis.enabled and redis.addr), or the name
	// of any other driver the binary has registered via
	// pkg/session.Register. Env override: GATEWAY_SESSIONS_STORE.
	Store string `yaml:"store"`
	// TTL is how long a session survives without being used.
	TTL time.Duration `yaml:"ttl"`
	// CleanupInterval is how often expired sessions are swept, for a
	// driver that needs sweeping (memory). Redis ignores it: a key's own
	// TTL expires it.
	CleanupInterval time.Duration `yaml:"cleanup_interval"`
	// Options is passed verbatim to the driver as pkg/session.Config
	// .Options, for a third-party driver that needs settings beyond the
	// redis section's connection fields. The built-in drivers ignore it.
	// NOT overridable via GATEWAY_* environment variables: overlayEnv
	// walks Config by reflection and only knows how to set scalar leaf
	// fields, not a map. YAML only.
	Options map[string]string `yaml:"options"`
}

// Session store names with built-in meaning to config validation.
const (
	SessionStoreMemory = "memory"
	SessionStoreRedis  = "redis"
)

// StoreName returns the driver name to open, defaulting an empty Store
// to memory.
func (s Sessions) StoreName() string {
	if strings.TrimSpace(s.Store) == "" {
		return SessionStoreMemory
	}
	return strings.TrimSpace(s.Store)
}

// Connectors configures the outbound side of the MCP plane: defaults
// applied to every backend connector unless its own row overrides them.
type Connectors struct {
	// DefaultTimeoutMS bounds one backend call for a connector whose
	// own timeout_ms is unset.
	DefaultTimeoutMS int `yaml:"default_timeout_ms"`
	// TLS configures outbound TLS verification.
	TLS ConnectorTLS `yaml:"tls"`
	// AllowBearerTokenForwarding permits a connector header of type
	// token_field with field bearer_token, which forwards the CALLER's own
	// raw gateway API key to the backend MCP server.
	// Default false: forwarding hands the caller's gateway credential to
	// whoever operates the connector endpoint. Enable only for backends you
	// fully trust. Env: GATEWAY_CONNECTORS_ALLOW_BEARER_TOKEN_FORWARDING.
	AllowBearerTokenForwarding bool `yaml:"allow_bearer_token_forwarding"`
}

// Egress is the outbound-request (SSRF) policy applied to every URL a
// tenant or operator can make the gateway call: connectors, the MCP
// catalog, model registry targets and model catalog providers. By default
// loopback, link-local (cloud metadata), private, CGNAT, ULA, multicast
// and unspecified addresses are refused at dial time, whatever the URL
// spells. List the exceptions here. Both keys accept a comma-separated
// value from the environment (GATEWAY_EGRESS_ALLOWED_CIDRS,
// GATEWAY_EGRESS_ALLOWED_HOSTS).
type Egress struct {
	// AllowedCIDRs are destination ranges exempt from the block, e.g.
	// ["127.0.0.0/8", "::1/128"] for a local lab. A bare IP is a /32 (/128).
	AllowedCIDRs []string `yaml:"allowed_cidrs"`
	// AllowedHosts are exact hostnames exempt from the block (e.g.
	// "host.docker.internal"): a dial to that name is allowed whatever IP it
	// resolves to.
	AllowedHosts []string `yaml:"allowed_hosts"`
}

// Policy compiles the egress allowlist.
func (e Egress) Policy() (*netguard.Policy, error) {
	return netguard.NewPolicy(e.AllowedCIDRs, e.AllowedHosts)
}

// DefaultTimeout returns DefaultTimeoutMS as a duration.
func (c Connectors) DefaultTimeout() time.Duration {
	return time.Duration(c.DefaultTimeoutMS) * time.Millisecond
}

// ConnectorTLS configures outbound TLS for backend connectors.
type ConnectorTLS struct {
	// CABundle is a PEM file of additional roots to trust, added to the
	// system pool rather than replacing it.
	CABundle string `yaml:"ca_bundle"`
	// InsecureSkipVerify disables certificate verification for EVERY
	// connector. Default false, and it should stay false outside a lab:
	// a connector that genuinely needs it can set
	// metadata.tls.insecure_skip_verify on its own row instead.
	InsecureSkipVerify bool `yaml:"insecure_skip_verify"`
}

// Skills configures the skills & commands registry's config-driven seed
// (SeedSkills in internal/skills), which upserts PLATFORM rows (tenant_id
// NULL) from a directory of skill bundles at startup -- the same role
// LLMProxy.Models plays for the model registry, but sourced from a
// directory of SKILL.md bundles instead of inline YAML.
type Skills struct {
	// SeedDir, when set, is walked at startup: every immediate
	// subdirectory containing a SKILL.md is seeded as one platform skill
	// or command (see internal/skills.SeedSkills). Empty disables
	// seeding entirely -- the default, since not every deployment ships
	// bundled skills.
	SeedDir string `yaml:"seed_dir"`
}

// MCPCatalog configures the platform MCP catalog's config-driven seed
// (mcpcatalog.Seed), which upserts entries by slug at startup, the same
// role LLMProxy.Models plays for the model registry. Entries are inert
// until a tenant admin adds one (which creates an ordinary connector).
type MCPCatalog struct {
	// Seed is upserted by slug at startup; entries it does not mention are
	// left alone. NOT overridable via GATEWAY_* environment variables (a
	// list; see API.TrustedProxies).
	Seed []MCPCatalogSeed `yaml:"seed"`
}

// MCPCatalogSeed is one mcp_catalog.seed entry: the YAML shape of a
// catalog entry (pkg/store.MCPCatalogEntry) minus the columns the store
// owns.
type MCPCatalogSeed struct {
	Slug           string            `yaml:"slug"`
	Name           string            `yaml:"name"`
	Description    string            `yaml:"description"`
	Icon           string            `yaml:"icon"`
	Category       string            `yaml:"category"`
	URL            string            `yaml:"url"`
	URLOverridable bool              `yaml:"url_overridable"`
	Transport      string            `yaml:"transport"`
	Auth           MCPCatalogAuth    `yaml:"auth"`
	DefaultHeaders map[string]string `yaml:"default_headers"`
	SuggestedTools []string          `yaml:"suggested_tools"`
	DocsURL        string            `yaml:"docs_url"`
	// Disabled hides the entry from tenants without deleting it.
	Disabled bool `yaml:"disabled"`
}

// MCPCatalogAuth mirrors pkg/store.MCPCatalogAuth.
type MCPCatalogAuth struct {
	Kind           string                `yaml:"kind"`
	Fields         []MCPCatalogField     `yaml:"fields"`
	HeaderTemplate *MCPCatalogHeaderSpec `yaml:"header_template"`
}

// MCPCatalogField mirrors pkg/store.MCPCatalogField.
type MCPCatalogField struct {
	Name        string `yaml:"name"`
	Label       string `yaml:"label"`
	Secret      bool   `yaml:"secret"`
	Required    bool   `yaml:"required"`
	Placeholder string `yaml:"placeholder"`
	Help        string `yaml:"help"`
	Query       string `yaml:"query"`
}

// MCPCatalogHeaderSpec mirrors pkg/store.MCPCatalogHeader.
type MCPCatalogHeaderSpec struct {
	Name   string `yaml:"name"`
	Prefix string `yaml:"prefix"`
}

// Capture configures opt-in request/response payload capture in the
// access log.
type Capture struct {
	// StoreBodies records request and response payloads on every logged
	// call. Default false: usage, latency and identifiers are always
	// recorded, payloads never are unless this is turned on.
	StoreBodies bool `yaml:"store_bodies"`
	// MaxRequestBytes and MaxResponseBytes bound a captured payload; a
	// record cut short is flagged truncated.
	MaxRequestBytes  int `yaml:"max_request_bytes"`
	MaxResponseBytes int `yaml:"max_response_bytes"`
}

// Ingest configures POST /api/v1/ingest (internal/api/handlers.Ingest): the
// control-plane route a companion capture component pushes gateway-shaped
// LLM/access-log records into, so that traffic shows up on the existing
// console pages under source="interceptor" (see docs/api.md and
// pkg/sink.AccessLog/LLMCall's Source/User fields).
type Ingest struct {
	// Enabled turns the route on. Default false: when off, POST
	// /api/v1/ingest answers 404 with the exact body an unmounted route
	// would (see Ingest's handler doc comment) rather than a route-specific
	// message, so an unrolled-out deployment gives no sign the feature
	// exists at all.
	Enabled bool `yaml:"enabled"`
	// MaxBodyBytes bounds the DEcompressed request body (after
	// gzip-decoding, when the caller sent Content-Encoding: gzip). It is
	// deliberately the only size knob exposed here: the compressed size on
	// the wire is capped by the fixed, non-configurable
	// maxIngestCompressedBytes (internal/api/handlers/ingest.go) regardless
	// of this setting, so a misconfigured huge MaxBodyBytes still can't
	// turn the route into a decompression-bomb amplifier past that fixed
	// multiple.
	MaxBodyBytes int `yaml:"max_body_bytes"`
	// MaxRecords bounds the combined number of llm_calls + access_logs
	// entries in one batch.
	MaxRecords int `yaml:"max_records"`
	// RatePerMinute bounds requests per API key (not per IP -- see
	// internal/api/router.go's isIngestPath: this route is exempt from the
	// control plane's per-IP cap, since many interceptor instances behind
	// one office NAT would otherwise throttle each other). Must be
	// positive when Enabled.
	RatePerMinute int `yaml:"rate_per_minute"`
}

// Redis configures the optional Redis connection.
//
// Its one reader is sessions.store: redis, which keeps MCP sessions in
// Redis and relays tools/list_changed between mcp replicas over Redis
// pub/sub, so the mcp plane can run more than one replica. Everything
// else (the tool cache, connector health) stays on Postgres or in
// process. A single node is supported (TLS optional); Cluster is
// rejected by Validate until a cluster client is wired.
type Redis struct {
	Enabled  bool   `yaml:"enabled"`
	Addr     string `yaml:"addr"`
	Password string `yaml:"password"`
	DB       int    `yaml:"db"`
	TLS      bool   `yaml:"tls"`
	Cluster  bool   `yaml:"cluster"`
}

// API configures the control REST + UI plane.
type API struct {
	Enabled bool   `yaml:"enabled"`
	Address string `yaml:"address"`
	ServeUI bool   `yaml:"serve_ui"`

	// TrustedProxies is the set of peers -- single IPs or CIDRs, as seen on
	// the raw TCP connection -- whose X-Forwarded-For header is believed.
	// It applies to EVERY plane (API, MCP, LLM): the rate limiters and the
	// captured client_ip all use one internal/clientip.Resolver built from
	// it. The client is the first address, walking X-Forwarded-For from
	// the right, that is NOT inside this set. Empty (the default) trusts no
	// peer, so every request is keyed on its raw connection address --
	// which, behind a load balancer or ingress, is the proxy's address
	// (one shared identity for all clients). Set it to the CIDR your
	// proxies connect from (the deploy/ overlays do). A malformed entry
	// fails startup. Overridable as a comma-separated list through
	// GATEWAY_API_TRUSTED_PROXIES.
	TrustedProxies []string `yaml:"trusted_proxies"`
}

// LLMProxy configures the LLM proxy plane.
type LLMProxy struct {
	Enabled         bool       `yaml:"enabled"`
	Address         string     `yaml:"address"`
	UpstreamBaseURL string     `yaml:"upstream_base_url"`
	Limits          LLMLimits  `yaml:"limits"`
	Bedrock         Bedrock    `yaml:"bedrock"`
	Providers       Providers  `yaml:"providers"`
	Capture         LLMCapture `yaml:"capture"`
	// Detection tees each relayed call to a detection agent, off the
	// request path; off unless agent_url is set.
	Detection LLMDetection `yaml:"detection"`

	// PricingFile is an optional path to a JSON file, in prices.json's
	// shape (pkg/pricing), that overrides the embedded per-model rate
	// card used to estimate each call's cost_usd. At startup, if set, the
	// file's models are merged OVER the embedded card: a model present in
	// the file replaces the embedded entry for it, and any model absent
	// from the file keeps its embedded price. A malformed file fails
	// startup rather than silently falling back. Empty (the default)
	// means the embedded card alone. Env override:
	// GATEWAY_LLM_PROXY_PRICING_FILE.
	PricingFile string `yaml:"pricing_file"`

	// Models is an optional seed for the LLM plane's model registry (see
	// docs/llm-plane.md "Model registry"): each entry is upserted at
	// startup, by name, as a PLATFORM row (visible to every tenant; a
	// tenant's own row of the same name overrides it). It is a GitOps
	// convenience for deployments that want their default aliases in
	// the config file -- the registry's source of truth stays the
	// database, edited through /api/v1/models. NOT overridable via
	// GATEWAY_* environment variables (a list; see API.TrustedProxies).
	Models []ModelSeed `yaml:"models"`
}

// ModelSeed is one llm_proxy.models entry: the YAML shape of a model
// registry row (pkg/store.Model) minus the columns the store owns.
type ModelSeed struct {
	Name        string            `yaml:"name"`
	Description string            `yaml:"description"`
	Targets     []ModelSeedTarget `yaml:"targets"`
	Price       *ModelSeedPrice   `yaml:"price"`
}

// ModelSeedTarget mirrors pkg/store.ModelTarget. A platform row may not
// name a credential (credentials are tenant-scoped, and a platform row
// belongs to no tenant), so Credential must stay empty here.
type ModelSeedTarget struct {
	Vendor     string `yaml:"vendor"`
	Model      string `yaml:"model"`
	BaseURL    string `yaml:"base_url"`
	Credential string `yaml:"credential"`
	Region     string `yaml:"region"`
	// AllowCallerKey mirrors pkg/store.ModelTarget.AllowCallerKey.
	AllowCallerKey bool `yaml:"allow_caller_key"`
	// Label mirrors pkg/store.ModelTarget.Label.
	Label string `yaml:"label"`
}

// ModelSeedPrice mirrors pkg/store.ModelPrice (USD per million tokens).
type ModelSeedPrice struct {
	Input      float64 `yaml:"input"`
	Output     float64 `yaml:"output"`
	CacheRead  float64 `yaml:"cache_read"`
	CacheWrite float64 `yaml:"cache_write"`
}

// Providers configures the prefix-routed providers reached via /{provider}/…
// (native passthrough). Anthropic uses UpstreamBaseURL and Bedrock its own
// section; these are the additional providers.
type Providers struct {
	OpenAI OpenAIConfig   `yaml:"openai"`
	Gemini ProviderConfig `yaml:"gemini"`
}

// OpenAIConfig is ProviderConfig plus InjectStreamUsage (default FALSE, opt-in):
// add stream_options.include_usage to Chat Completions streams that set none,
// so they are priced. It is the plane's one departure from byte-for-byte
// passthrough — it rewrites the upstream request body — so it stays off unless
// the operator asks for it; an OpenAI-compatible base_url that rejects unknown
// fields would otherwise start failing. Off, such streams record a NULL cost.
type OpenAIConfig struct {
	Enabled           bool   `yaml:"enabled"`
	BaseURL           string `yaml:"base_url"`
	InjectStreamUsage bool   `yaml:"inject_stream_usage"`
}

// ProviderConfig enables a prefix-routed provider and optionally overrides its
// upstream base URL (empty → the provider's built-in default).
type ProviderConfig struct {
	Enabled bool   `yaml:"enabled"`
	BaseURL string `yaml:"base_url"`
}

// LLMLimits bounds each proxied request (abuse / slow-loris guards).
type LLMLimits struct {
	MaxRequestBytes        int64         `yaml:"max_request_bytes"`   // 413 threshold on the inbound request
	MaxStreamDuration      time.Duration `yaml:"max_stream_duration"` // total request deadline
	MaxConcurrentPerTenant int           `yaml:"max_concurrent_per_tenant"`
}

// Bedrock configures the optional Amazon Bedrock provider (SigV4). Disabled →
// the /model/* route is not mounted (falls through to 404).
type Bedrock struct {
	Enabled        bool   `yaml:"enabled"`
	Region         string `yaml:"region"`          // default region for the signed modes (B/C)
	CredentialMode string `yaml:"credential_mode"` // auto | passthrough | client_keys | gateway
	// AllowedRoleAccounts is a comma-separated allowlist of the AWS accounts
	// whose roles a caller may name in X-Bedrock-Role-Arn (the gateway assumes
	// them with its own identity). Each entry is either a bare 12-digit account
	// id, which matches that account in the default "aws" partition ONLY, or a
	// partition-qualified "<partition>:<account>" (aws-us-gov:123456789012,
	// aws-cn:123456789012) for a role outside it; "*" matches every account in
	// every partition. Empty (the default) disables role assumption: an
	// operator must opt in.
	AllowedRoleAccounts string `yaml:"allowed_role_accounts"`
}

// LLMCapture configures how much of each LLM call is persisted. The durable
// (lossless) capture store is selected by Store; a best-effort copy is also teed
// into the shared analytics sinks (cfg.Sinks) for the ClickHouse/OTel views.
type LLMCapture struct {
	// Store selects the durable capture destination: "postgres" keeps everything
	// in the gateway's Postgres (lossless, single-DB); anything else falls back
	// to best-effort stdout.
	Store            string `yaml:"store"`
	StoreBodies      bool   `yaml:"store_bodies"`       // persist request/response bodies + messages/system/tools
	MaxRequestBytes  int    `yaml:"max_request_bytes"`  // per-body request cap (also messages/system/tools); default 1 MiB, 0 = unbounded
	MaxResponseBytes int    `yaml:"max_response_bytes"` // per-body response/stream cap; default 1 MiB, 0 = unbounded
	// BodyStore optionally offloads captured bodies out of the capture row
	// (see sink.BodyStore). Only meaningful with StoreBodies.
	BodyStore BodyStoreConfig `yaml:"body_store"`
}

// LLMDetection is the detection agent the LLM plane tees each relayed call
// to (see docs/llm-plane.md "Detection agent"). Detection only: the turn is
// posted asynchronously after the call completes, nothing on the request
// path waits for the agent, and nothing is ever blocked.
type LLMDetection struct {
	// AgentURL is the agent's base URL (e.g. http://127.0.0.1:8090).
	// Empty (the default) turns detection off.
	AgentURL string `yaml:"agent_url"`
	// Timeout bounds one post of a turn to the agent.
	Timeout time.Duration `yaml:"timeout"`
	// QueueSize caps turns waiting to be posted; past it a turn is dropped.
	QueueSize int `yaml:"queue_size"`
	// QueueBytes caps the request and response bytes the tee holds
	// (waiting or being posted); past it a turn is dropped.
	QueueBytes int64 `yaml:"queue_bytes"`
	// MaxInFlight caps turns being posted at once.
	MaxInFlight int `yaml:"max_in_flight"`
}

// BodyStoreConfig selects where captured LLM request/response bodies live.
// Type "none" (the default) keeps them inline in the capture row; "filesystem"
// or "s3" moves them to that store and records a body_ref on the row instead.
type BodyStoreConfig struct {
	Type string `yaml:"type"` // none | filesystem | s3
	// InlineMaxBytes keeps a call's bodies inline when request+response total
	// at most this many bytes; 0 offloads every call with a body.
	InlineMaxBytes int                       `yaml:"inline_max_bytes"`
	Filesystem     FilesystemBodyStoreConfig `yaml:"filesystem"`
	S3             S3BodyStoreConfig         `yaml:"s3"`
}

// FilesystemBodyStoreConfig configures pkg/sink/bodystore/fs.
type FilesystemBodyStoreConfig struct {
	Root string `yaml:"root"`
}

// S3BodyStoreConfig configures pkg/sink/bodystore/s3. Empty credentials use
// the AWS default chain (env, profile, IRSA / instance role).
type S3BodyStoreConfig struct {
	Bucket          string `yaml:"bucket"`
	Prefix          string `yaml:"prefix"`
	Region          string `yaml:"region"`
	Endpoint        string `yaml:"endpoint"` // S3-compatible endpoint (MinIO, R2); empty = AWS
	ForcePathStyle  bool   `yaml:"force_path_style"`
	AccessKeyID     string `yaml:"access_key_id"`
	SecretAccessKey string `yaml:"secret_access_key"` // never logged
}

// Database configures the store connection.
type Database struct {
	// Driver selects the registered store backend (see pkg/store.Open).
	// "postgres" is the only backend shipped in this module.
	Driver   string `yaml:"driver"`
	Host     string `yaml:"host"`
	Port     int    `yaml:"port"`
	User     string `yaml:"user"`
	Password string `yaml:"password"`
	Database string `yaml:"database"`
	SSLMode  string `yaml:"ssl_mode"`
	Migrate  bool   `yaml:"migrate"`
}

// DefaultPasswordWarning returns a non-empty message when the database uses
// the shipped default password ("gateway") somewhere other than a local
// development setup: a host that is not loopback or a bare single-label name
// (a compose/Kubernetes service name), or TLS turned on or negotiable
// (ssl_mode other than "disable", which signals a real deployment).
// The caller logs it loudly at startup.
func (d Database) DefaultPasswordWarning() string {
	if d.Password != "gateway" {
		return ""
	}
	host := strings.ToLower(d.Host)
	local := host == "localhost" || host == "" || !strings.Contains(host, ".") && !strings.Contains(host, ":")
	if ip, err := netip.ParseAddr(host); err == nil {
		local = ip.IsLoopback()
	}
	if local && d.SSLMode == "disable" {
		return ""
	}
	return fmt.Sprintf("database.password is the shipped default %q on host %q (ssl_mode=%s): anyone who can reach this database can log in. Set a strong password via database.password / GATEWAY_DATABASE_PASSWORD", d.Password, d.Host, d.SSLMode)
}

// DSN returns a pgx-compatible Postgres connection URL.
func (d Database) DSN() string {
	u := url.URL{
		Scheme: "postgres",
		User:   url.UserPassword(d.User, d.Password),
		Host:   fmt.Sprintf("%s:%d", d.Host, d.Port),
		Path:   "/" + d.Database,
	}
	q := url.Values{}
	q.Set("sslmode", d.SSLMode)
	u.RawQuery = q.Encode()
	return u.String()
}

// Auth configures request authentication: which Authenticators the
// gateway's chain includes and how they're parameterized. See
// internal/auth for the chain itself.
type Auth struct {
	// APIKeys configures the api-key Authenticator (internal/auth/apikey).
	APIKeys APIKeysAuth `yaml:"api_keys"`

	// DefaultTenant is the tenant the console login form is prefilled with
	// (GET /api/v1/auth/config "default_tenant"); when exactly one tenant
	// exists, that tenant is reported instead.
	DefaultTenant string `yaml:"default_tenant"`

	// ConsoleAPIKeyLogin lets the admin console offer "sign in with an API
	// key" next to username/password (GET /api/v1/auth/config reports it
	// as api_key_login). Default true in this release so an existing
	// deployment is not locked out; it is an emergency fallback and is
	// planned for removal. It only steers the console: API keys keep
	// authenticating API calls whatever this says.
	ConsoleAPIKeyLogin bool `yaml:"console_api_key_login"`

	// CookieSecure controls the Secure attribute of the console session
	// cookie: "auto" (default) sets it when the request arrived over TLS
	// or carries X-Forwarded-Proto: https; "true" always sets it; "false"
	// never does (plain-HTTP local development only).
	CookieSecure string `yaml:"cookie_secure"`

	// SessionIdle is how long a console session may go unused before it
	// expires. SessionMax is its absolute lifetime from login, however
	// active it is. Defaults 8h and 24h.
	SessionIdle time.Duration `yaml:"session_idle"`
	SessionMax  time.Duration `yaml:"session_max"`

	// DevMode configures the dev-mode gap-filling Authenticator
	// (internal/auth/devmode). Must never be enabled outside local
	// development: it grants an admin Principal to any request that
	// carries no credential at all.
	DevMode DevModeAuth `yaml:"dev_mode"`

	// Roles configures custom role -> permission-pattern grants, merged
	// over pkg/auth.NewRoleAuthorizer's built-ins (admin, agent) at
	// startup: a role name that collides with a built-in REPLACES it
	// entirely (its patterns are not merged in, just overwritten), and
	// any other name adds a new role. Pattern syntax is documented on
	// pkgauth.RoleAuthorizer.Rules, e.g.:
	//
	//	auth:
	//	  roles:
	//	    reader: ["*.read"]
	//	    billing-admin: ["billing.*", "*.read"]
	//
	// API keys may be created with any role name present here (see
	// internal/api/handlers.APIKeys.Create/Rotate's validateRole); an
	// unknown role is rejected with 400.
	//
	// NOT overridable via GATEWAY_* environment variables: like
	// OtelSink.Headers, overlayEnv (below) walks Config by reflection and
	// only knows how to apply string/int/bool/duration leaves, not a
	// map[string][]string -- set this from the YAML config file (or
	// don't use custom roles) in any deployment that needs env-driven
	// config.
	Roles map[string][]string `yaml:"roles"`

	// RateLimit configures the control plane's per-IP rate limiter
	// (internal/auth.RateLimiter): a failure-triggered lockout that slows
	// down credential guessing, plus a general per-IP requests-per-minute
	// cap. See internal/auth/ratelimit.go.
	RateLimit RateLimitAuth `yaml:"rate_limit"`
}

// RateLimitAuth configures internal/auth.RateLimiter.
type RateLimitAuth struct {
	// Enabled turns the limiter on. Default true.
	Enabled bool `yaml:"enabled"`
	// MaxFailures is how many auth failures (401s) from the same IP
	// within Window trip the lockout.
	MaxFailures int `yaml:"max_failures"`
	// Window is the period auth failures are counted over.
	Window time.Duration `yaml:"window"`
	// Lockout is how long a tripped IP is locked out for: every request
	// from it on an authenticated route gets 429 + Retry-After, even one
	// carrying a valid credential.
	Lockout time.Duration `yaml:"lockout"`
	// RequestsPerMinute is the general per-IP request cap on the control
	// plane, independent of authentication outcome. Zero disables it.
	RequestsPerMinute int `yaml:"requests_per_minute"`
	// CredentialMaxFailures is the second dimension: failed lookups of
	// UNKNOWN keys sharing one key prefix ("gk_" + 8 characters), from any
	// IPs, within CredentialWindow lock that prefix for Lockout. A locked
	// prefix serves only keys the process already has cached, so the
	// legitimate holder keeps working while a guesser rotating IPs gets
	// 429. No IP is penalised for it.
	CredentialMaxFailures int `yaml:"credential_max_failures"`
	// CredentialWindow is the period CredentialMaxFailures is counted over.
	CredentialWindow time.Duration `yaml:"credential_window"`
}

// APIKeysAuth configures the api-key Authenticator.
type APIKeysAuth struct {
	// Enabled adds the api-key Authenticator to the chain. Default true;
	// this is the gateway's primary authentication method.
	Enabled bool `yaml:"enabled"`
	// CacheTTL is how long a successful key lookup is cached in memory
	// before being re-fetched from the store. Revocation does not wait for
	// it: a revoke/rotate evicts the entry on every replica at once
	// (Postgres LISTEN/NOTIFY). The TTL is the backstop for a replica
	// whose listener connection is down. Default 30s.
	CacheTTL time.Duration `yaml:"cache_ttl"`
}

// DevModeAuth configures the dev-mode gap-filling Authenticator.
type DevModeAuth struct {
	// Enabled adds the dev-mode Authenticator to the chain. Default
	// false. When true, any request carrying no credential at all is
	// granted an admin Principal for Tenant -- do not enable this
	// outside local development.
	Enabled bool `yaml:"enabled"`
	// Tenant is the tenant slug (or UUID) the manufactured dev-mode
	// Principal is scoped to. Required (non-empty) when Enabled is true;
	// resolved against the store at startup, which fails if it does not
	// exist.
	Tenant string `yaml:"tenant"`
	// AllowRemote lets dev mode run while an enabled plane listens on a
	// non-loopback address. Without it the gateway refuses to start: dev
	// mode serves unauthenticated requests as an admin, so exposing it to
	// the network is almost never intended.
	AllowRemote bool `yaml:"allow_remote"`
	// Platform additionally grants the dev principal platform-admin
	// (tenant enumeration/creation, platform catalog). Default false: the
	// dev principal is a tenant admin only.
	Platform bool `yaml:"platform"`
}

// SecretStore configures the gateway's encrypted credential store
// (internal/secrets): the master key ring used to encrypt/decrypt
// Credential rows, and which external header providers (env, file) are
// permitted to run.
type SecretStore struct {
	// MasterKeyEnv is the name of the environment variable read for the
	// master key ring. See internal/secrets.LoadKeyRing for the value
	// format (a single key, or a comma-separated "id:key" list for
	// rotation). Defaults to GATEWAY_MASTER_KEY.
	MasterKeyEnv string `yaml:"master_key_env"`
	// MasterKeyFile is a file path read for the master key ring instead,
	// used only when the environment variable named by MasterKeyEnv is
	// unset. Generate a value with `openssl rand -base64 32` or
	// `gateway secrets genkey`.
	MasterKeyFile string `yaml:"master_key_file"`
	// ActiveKeyID selects which key in the ring new Encrypt calls use.
	// Rows encrypted under a previous key id stay decryptable for as long
	// as that id remains in the ring; see `gateway secrets rekey`.
	ActiveKeyID string `yaml:"active_key_id"`

	// AllowEnvProvider enables the "env" external header provider, which
	// reads a header's value straight out of the gateway process's own
	// environment. Default false: in a shared/multi-tenant deployment,
	// this would let any tenant admin who can configure a connector's
	// headers read ANY environment variable visible to the process --
	// including GATEWAY_MASTER_KEY itself, or database credentials --
	// simply by pointing a header at it. Only enable this on a
	// single-tenant deployment, or one where every tenant is already
	// trusted with the host environment.
	AllowEnvProvider bool `yaml:"allow_env_provider"`
	// AllowFileProvider enables the "file" external header provider.
	// Default false. When enabled, FileProviderRoot must also be set,
	// and every configured path is required to resolve underneath it.
	AllowFileProvider bool `yaml:"allow_file_provider"`
	// FileProviderRoot is the directory the "file" provider's paths must
	// resolve under; ".." components and symlink escapes are rejected.
	// Required when AllowFileProvider is true.
	FileProviderRoot string `yaml:"file_provider_root"`
}

// Sinks configures the gateway's log sinks (pkg/sink and siblings): where
// every AccessLog/LLMCall record goes, beyond the request path. Every
// enabled sink is combined into one sink.Multi (see cmd/gateway/main.go).
type Sinks struct {
	Stdout     StdoutSink     `yaml:"stdout"`
	Otel       OtelSink       `yaml:"otel"`
	ClickHouse ClickHouseSink `yaml:"clickhouse"`
}

// StdoutSink configures pkg/sink/stdout: JSON lines on stdout.
type StdoutSink struct {
	Enabled bool `yaml:"enabled"`
	// IncludeBodies prints captured request/response bodies (and parsed
	// messages/system/tools) in the stdout lines. Default false: bodies go
	// only to the durable stores (Postgres, ClickHouse, body store), never
	// into container logs, which are usually shipped somewhere less guarded.
	IncludeBodies bool `yaml:"include_bodies"`
}

// OtelSink configures pkg/sink/otel: one OTLP span per record.
type OtelSink struct {
	Enabled bool `yaml:"enabled"`
	// Endpoint is the OTLP collector address, e.g. "localhost:4318"
	// (http) or "localhost:4317" (grpc). Required when Enabled.
	Endpoint string `yaml:"endpoint"`
	// Protocol selects the OTLP transport: "http" (default) or "grpc".
	Protocol string `yaml:"protocol"`
	// Insecure disables TLS on the OTLP connection.
	Insecure bool `yaml:"insecure"`
	// Headers are extra headers sent with every OTLP export (e.g. an
	// ingest API key). NOT overridable via GATEWAY_* environment
	// variables: overlayEnv (below) walks Config by reflection and only
	// knows how to apply string/int/bool/duration leaves, not a nested
	// map -- set this from the YAML file (or don't use it) in any
	// deployment that needs env-driven config.
	Headers map[string]string `yaml:"headers"`
	// ServiceName sets the exported resource's service.name. Defaults to
	// the binary's own name (see pkg/sink/otel) when empty.
	ServiceName string `yaml:"service_name"`
}

// ClickHouseSink configures pkg/sink/clickhouse: batched inserts into
// mcp_access_logs/llm_calls, and the read side backing GET
// /api/v1/analytics/*.
type ClickHouseSink struct {
	Enabled  bool   `yaml:"enabled"`
	Host     string `yaml:"host"`
	Port     int    `yaml:"port"`
	Database string `yaml:"database"`
	Username string `yaml:"username"`
	Password string `yaml:"password"`
	Secure   bool   `yaml:"secure"`

	// BatchSize is the number of buffered records that triggers an
	// immediate flush.
	BatchSize int `yaml:"batch_size"`
	// FlushInterval is the longest a record waits before being flushed.
	FlushInterval time.Duration `yaml:"flush_interval"`
	// BufferSize is the bounded queue's capacity; a record written once
	// it is full is dropped (see the sink's Dropped()) rather than
	// blocking the caller.
	BufferSize int `yaml:"buffer_size"`
}

// Logging configures the process-wide logger.
type Logging struct {
	Level       string `yaml:"level"`
	Development bool   `yaml:"development"`
}

// Default returns the gateway's built-in default configuration.
func Default() *Config {
	return &Config{
		Service: Service{
			Name:    "tusk-ai-secured-gateway",
			Version: "dev",
		},
		MCP: MCP{
			Enabled:     true,
			Address:     "0.0.0.0:8080",
			ReadTimeout: 30 * time.Second,
			// 0 = unlimited: GET /mcp/stream is long-lived. See MCP.WriteTimeout.
			WriteTimeout:                 0,
			RequireProfile:               false,
			MaxUpstreamStreamsPerSession: 16,
			MaxSubscriptionsPerSession:   256,

			MaxPendingServerRequestsPerSession: 32,
			ServerRequestTimeout:               5 * time.Minute,
		},
		API: API{
			Enabled:        true,
			Address:        "0.0.0.0:8081",
			ServeUI:        true,
			TrustedProxies: nil,
		},
		LLMProxy: LLMProxy{
			Enabled:         false,
			Address:         "0.0.0.0:8082",
			UpstreamBaseURL: "https://api.anthropic.com",
			Limits: LLMLimits{
				MaxRequestBytes:        10 << 20, // 10 MiB
				MaxStreamDuration:      15 * time.Minute,
				MaxConcurrentPerTenant: 64,
			},
			Bedrock: Bedrock{
				Enabled:        false,
				Region:         "us-east-1",
				CredentialMode: "auto",
			},
			Providers: Providers{
				OpenAI: OpenAIConfig{Enabled: false, BaseURL: "https://api.openai.com", InjectStreamUsage: false},
				Gemini: ProviderConfig{Enabled: false, BaseURL: "https://generativelanguage.googleapis.com"},
			},
			Capture: LLMCapture{
				Store:            "postgres", // lossless by default; stdout only if explicitly unset
				StoreBodies:      false,      // opt-in (privacy); spec default
				MaxRequestBytes:  1 << 20,    // 1 MiB per body; 0 = unbounded (opt in explicitly)
				MaxResponseBytes: 1 << 20,    // 1 MiB per body; 0 = unbounded (opt in explicitly)
				BodyStore: BodyStoreConfig{
					Type: "none", // bodies inline in the capture row
					S3:   S3BodyStoreConfig{Prefix: "llm-bodies"},
				},
			},
			Detection: LLMDetection{
				AgentURL:    "", // off
				Timeout:     5 * time.Second,
				QueueSize:   1024,
				QueueBytes:  256 << 20, // 256 MiB
				MaxInFlight: 8,
			},
		},
		Database: Database{
			Driver:   "postgres",
			Host:     "localhost",
			Port:     5432,
			User:     "gateway",
			Password: "gateway",
			Database: "gateway",
			SSLMode:  "require",
			Migrate:  true,
		},
		Auth: Auth{
			APIKeys: APIKeysAuth{
				Enabled:  true,
				CacheTTL: 30 * time.Second,
			},
			DefaultTenant:      "default",
			ConsoleAPIKeyLogin: true,
			CookieSecure:       "auto",
			SessionIdle:        8 * time.Hour,
			SessionMax:         24 * time.Hour,
			DevMode: DevModeAuth{
				Enabled: false,
				Tenant:  "default",
			},
			RateLimit: RateLimitAuth{
				Enabled:           true,
				MaxFailures:       10,
				Window:            time.Minute,
				Lockout:           5 * time.Minute,
				RequestsPerMinute: 600,

				CredentialMaxFailures: 20,
				CredentialWindow:      5 * time.Minute,
			},
		},
		SecretStore: SecretStore{
			MasterKeyEnv:      "GATEWAY_MASTER_KEY",
			ActiveKeyID:       "k1",
			AllowEnvProvider:  false,
			AllowFileProvider: false,
		},
		Redis: Redis{
			Enabled: false,
			Addr:    "localhost:6379",
			DB:      0,
		},
		ToolCache: ToolCache{
			Enabled:         true,
			L2TTL:           30 * time.Minute,
			RefreshInterval: 10 * time.Minute,
			CleanupInterval: 15 * time.Minute,
			ServeStale:      true,
		},
		Sessions: Sessions{
			Store:           SessionStoreMemory,
			TTL:             time.Hour,
			CleanupInterval: 5 * time.Minute,
		},
		Connectors: Connectors{
			DefaultTimeoutMS: 30000,
			TLS: ConnectorTLS{
				CABundle:           "",
				InsecureSkipVerify: false,
			},
		},
		Skills: Skills{
			SeedDir: "", // no bundled skills seeded by default
		},
		Capture: Capture{
			StoreBodies:      false,
			MaxRequestBytes:  300000,
			MaxResponseBytes: 300000,
		},
		Sinks: Sinks{
			Stdout: StdoutSink{Enabled: true},
			Otel: OtelSink{
				Enabled:  false,
				Protocol: "http",
			},
			ClickHouse: ClickHouseSink{
				Enabled:       false,
				Port:          9000,
				Database:      "default",
				BatchSize:     100,
				FlushInterval: 5 * time.Second,
				BufferSize:    1000,
			},
		},
		Logging: Logging{
			Level:       "info",
			Development: false,
		},
		Ingest: Ingest{
			Enabled:       false,
			MaxBodyBytes:  32 << 20, // 32 MiB decompressed
			MaxRecords:    1000,
			RatePerMinute: 120,
		},
		MCPCatalog: defaultMCPCatalog(),
	}
}

// Load builds a Config by starting from Default(), overlaying an optional
// YAML file (skipped when path is empty; an explicit path that cannot be
// read is an error), then overlaying environment variables, and finally
// validating the result.
func Load(path string) (*Config, error) {
	cfg := Default()

	if path != "" {
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("read config file %q: %w", path, err)
		}
		if err := DecodeYAMLStrict(data, path, cfg); err != nil {
			return nil, err
		}
	}

	if err := overlayEnv(cfg); err != nil {
		return nil, fmt.Errorf("overlay env vars: %w", err)
	}

	// Install the egress policy before Validate: URL validators consult it
	// to reject literal internal IPs at config time.
	pol, err := cfg.Egress.Policy()
	if err != nil {
		return nil, fmt.Errorf("validate config: %w", err)
	}
	netguard.SetPolicy(pol)

	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("validate config: %w", err)
	}

	return cfg, nil
}

// durationType lets overlayStruct tell a leaf time.Duration field (whose
// reflect.Kind() is Int64) apart from an actual nested config section.
var durationType = reflect.TypeOf(time.Duration(0))

// overlayEnv walks Config's fields recursively by reflection, deriving an
// env var name GATEWAY_<SECTION>_..._<FIELD> from each yaml tag along the
// path, and applies any that are set. Supported leaf field kinds: string,
// int, bool, time.Duration. A struct field other than time.Duration is a
// nested section, not a leaf, and is descended into rather than applied
// directly -- e.g. Auth.APIKeys.Enabled becomes GATEWAY_AUTH_API_KEYS_ENABLED.
func overlayEnv(cfg *Config) error {
	return overlayStruct(reflect.ValueOf(cfg).Elem(), "GATEWAY")
}

func overlayStruct(v reflect.Value, prefix string) error {
	t := v.Type()

	for i := 0; i < t.NumField(); i++ {
		fv := v.Field(i)
		envName := prefix + "_" + strings.ToUpper(yamlTagName(t.Field(i)))

		if fv.Kind() == reflect.Struct && fv.Type() != durationType {
			if err := overlayStruct(fv, envName); err != nil {
				return err
			}
			continue
		}

		raw, ok := os.LookupEnv(envName)
		if !ok {
			continue
		}
		if err := setField(fv, raw); err != nil {
			return fmt.Errorf("env %s: %w", envName, err)
		}
	}

	return nil
}

func yamlTagName(f reflect.StructField) string {
	tag := f.Tag.Get("yaml")
	if tag == "" {
		return strings.ToLower(f.Name)
	}
	return strings.Split(tag, ",")[0]
}

func setField(fv reflect.Value, raw string) error {
	// time.Duration has reflect.Kind() == Int64, so it must be checked
	// before the generic kind switch below.
	if fv.Type() == reflect.TypeOf(time.Duration(0)) {
		d, err := time.ParseDuration(raw)
		if err != nil {
			return err
		}
		fv.SetInt(int64(d))
		return nil
	}

	switch fv.Kind() {
	case reflect.Slice:
		// Only []string is supported: a comma-separated list.
		if fv.Type().Elem().Kind() != reflect.String {
			return fmt.Errorf("unsupported field kind %s of %s", fv.Kind(), fv.Type().Elem())
		}
		var items []string
		for _, part := range strings.Split(raw, ",") {
			if part = strings.TrimSpace(part); part != "" {
				items = append(items, part)
			}
		}
		fv.Set(reflect.ValueOf(items))
	case reflect.String:
		fv.SetString(raw)
	case reflect.Bool:
		b, err := strconv.ParseBool(raw)
		if err != nil {
			return err
		}
		fv.SetBool(b)
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		n, err := strconv.ParseInt(raw, 10, 64)
		if err != nil {
			return err
		}
		fv.SetInt(n)
	default:
		return fmt.Errorf("unsupported field kind %s", fv.Kind())
	}
	return nil
}

// ReAWSRegion matches an AWS region name (us-east-1, us-gov-west-1, ...). It is
// exported because internal/llmplane applies the SAME rule to the region a
// request carries (X-Bedrock-Region, or Flow A's credential scope): the region
// becomes part of the upstream host, so one regex must decide both the startup
// check and the per-request one.
var ReAWSRegion = regexp.MustCompile(`^[a-z]{2}(-[a-z]+)+-\d{1,2}$`)

// reRoleAccount matches one llm_proxy.bedrock.allowed_role_accounts entry: a
// bare 12-digit AWS account id (the default "aws" partition) or a
// partition-qualified "<partition>:<account>" (aws-us-gov:…, aws-cn:…). The
// "*" wildcard is accepted separately.
var reRoleAccount = regexp.MustCompile(`^(?:aws(?:-[a-z]+)*:)?\d{12}$`)

// Validate checks the config for internal consistency: enabled planes must
// have a non-empty address and must not collide with each other, the log
// level must be recognized, and the LLM upstream URL must parse. The AWS
// region and role-account checks mirror the LLM plane's per-request ones, so a
// malformed configured value fails startup instead of every request.
func (c *Config) Validate() error {
	planes := []struct {
		name    string
		enabled bool
		addr    string
	}{
		{"mcp", c.MCP.Enabled, c.MCP.Address},
		{"api", c.API.Enabled, c.API.Address},
		{"llm_proxy", c.LLMProxy.Enabled, c.LLMProxy.Address},
	}

	seen := make(map[string]string, len(planes))
	for _, p := range planes {
		if !p.enabled {
			continue
		}
		if strings.TrimSpace(p.addr) == "" {
			return fmt.Errorf("%s: address is required when enabled", p.name)
		}
		if other, ok := seen[p.addr]; ok {
			return fmt.Errorf("%s and %s cannot both be enabled on address %q", other, p.name, p.addr)
		}
		seen[p.addr] = p.name
	}

	if c.MCP.MaxUpstreamStreamsPerSession < 1 {
		return fmt.Errorf("mcp: max_upstream_streams_per_session must be at least 1 (got %d)", c.MCP.MaxUpstreamStreamsPerSession)
	}
	if c.MCP.MaxSubscriptionsPerSession < 1 {
		return fmt.Errorf("mcp: max_subscriptions_per_session must be at least 1 (got %d)", c.MCP.MaxSubscriptionsPerSession)
	}
	if c.MCP.MaxPendingServerRequestsPerSession < 1 {
		return fmt.Errorf("mcp: max_pending_server_requests_per_session must be at least 1 (got %d)", c.MCP.MaxPendingServerRequestsPerSession)
	}
	if c.MCP.ServerRequestTimeout <= 0 {
		return fmt.Errorf("mcp: server_request_timeout must be positive (got %s)", c.MCP.ServerRequestTimeout)
	}

	switch c.Logging.Level {
	case "debug", "info", "warn", "error":
	default:
		return fmt.Errorf("logging: invalid level %q (want debug, info, warn, or error)", c.Logging.Level)
	}

	if err := validateAbsoluteURL(c.LLMProxy.UpstreamBaseURL); err != nil {
		return fmt.Errorf("llm_proxy: invalid upstream_base_url: %w", err)
	}
	if d := c.LLMProxy.Detection; d.AgentURL != "" {
		if err := validateAgentURL(d.AgentURL); err != nil {
			return fmt.Errorf("llm_proxy.detection: invalid agent_url: %w", err)
		}
		if d.Timeout <= 0 {
			return fmt.Errorf("llm_proxy.detection: timeout must be > 0, got %s", d.Timeout)
		}
		if d.QueueSize <= 0 {
			return fmt.Errorf("llm_proxy.detection: queue_size must be > 0, got %d", d.QueueSize)
		}
		if d.QueueBytes <= 0 {
			return fmt.Errorf("llm_proxy.detection: queue_bytes must be > 0, got %d", d.QueueBytes)
		}
		if d.MaxInFlight <= 0 {
			return fmt.Errorf("llm_proxy.detection: max_in_flight must be > 0, got %d", d.MaxInFlight)
		}
	}

	if _, err := clientip.New(c.API.TrustedProxies); err != nil {
		return fmt.Errorf("api.trusted_proxies: %w", err)
	}

	if c.Auth.DevMode.Enabled && strings.TrimSpace(c.Auth.DevMode.Tenant) == "" {
		return fmt.Errorf("auth.dev_mode: tenant is required when enabled")
	}
	if c.Auth.DevMode.Enabled && !c.Auth.DevMode.AllowRemote {
		for _, p := range planes {
			if p.enabled && !isLoopbackAddr(p.addr) {
				return fmt.Errorf("auth.dev_mode is enabled but %s listens on %q, which is reachable from the network: "+
					"dev mode serves unauthenticated requests as an admin. Bind to 127.0.0.1 or set auth.dev_mode.allow_remote: true",
					p.name, p.addr)
			}
		}
	}

	switch c.Auth.CookieSecure {
	case "auto", "true", "false":
	default:
		return fmt.Errorf("auth: invalid cookie_secure %q (want auto, true, or false)", c.Auth.CookieSecure)
	}
	if c.Auth.SessionIdle <= 0 {
		return fmt.Errorf("auth: session_idle must be positive (got %s)", c.Auth.SessionIdle)
	}
	if c.Auth.SessionMax <= 0 {
		return fmt.Errorf("auth: session_max must be positive (got %s)", c.Auth.SessionMax)
	}
	if c.Auth.SessionIdle > c.Auth.SessionMax {
		return fmt.Errorf("auth: session_idle (%s) must not exceed session_max (%s)", c.Auth.SessionIdle, c.Auth.SessionMax)
	}

	if c.Auth.RateLimit.Enabled {
		if c.Auth.RateLimit.MaxFailures <= 0 {
			return fmt.Errorf("auth.rate_limit: max_failures must be positive when enabled")
		}
		if c.Auth.RateLimit.Window <= 0 {
			return fmt.Errorf("auth.rate_limit: window must be positive when enabled")
		}
		if c.Auth.RateLimit.Lockout <= 0 {
			return fmt.Errorf("auth.rate_limit: lockout must be positive when enabled")
		}
		if c.Auth.RateLimit.RequestsPerMinute < 0 {
			return fmt.Errorf("auth.rate_limit: requests_per_minute must be >= 0 (0 disables the general cap)")
		}
	}

	if c.SecretStore.AllowFileProvider && strings.TrimSpace(c.SecretStore.FileProviderRoot) == "" {
		return fmt.Errorf("secret_store: file_provider_root is required when allow_file_provider is true")
	}

	if c.ToolCache.Enabled {
		if c.ToolCache.L2TTL <= 0 {
			return fmt.Errorf("tool_cache: l2_ttl must be positive when enabled")
		}
		if c.ToolCache.RefreshInterval <= 0 {
			return fmt.Errorf("tool_cache: refresh_interval must be positive when enabled")
		}
		if c.ToolCache.CleanupInterval <= 0 {
			return fmt.Errorf("tool_cache: cleanup_interval must be positive when enabled")
		}
	}

	if c.Sessions.TTL <= 0 {
		return fmt.Errorf("sessions: ttl must be positive")
	}
	if c.Sessions.CleanupInterval <= 0 {
		return fmt.Errorf("sessions: cleanup_interval must be positive")
	}
	switch c.Sessions.StoreName() {
	case SessionStoreMemory:
	case SessionStoreRedis:
		if !c.Redis.Enabled {
			return fmt.Errorf("sessions: store %q requires redis.enabled: true", SessionStoreRedis)
		}
		if strings.TrimSpace(c.Redis.Addr) == "" {
			return fmt.Errorf("sessions: store %q requires redis.addr", SessionStoreRedis)
		}
	default:
		// Any other name is a third-party pkg/session driver; whether the
		// binary registered it is only known at open time, where
		// pkg/session.Open reports the registered names.
	}

	if c.Redis.Enabled && c.Redis.Cluster {
		return fmt.Errorf("redis: cluster mode is not supported yet (point redis.addr at a single node)")
	}

	if c.Connectors.DefaultTimeoutMS < 100 || c.Connectors.DefaultTimeoutMS > 600000 {
		return fmt.Errorf("connectors: default_timeout_ms must be between 100 and 600000 (the range the API accepts for timeout_ms)")
	}

	if b := c.LLMProxy.Bedrock; b.Enabled {
		if b.Region != "" && !ReAWSRegion.MatchString(b.Region) {
			return fmt.Errorf("llm_proxy.bedrock.region %q is not a supported AWS region", b.Region)
		}
		for _, a := range strings.Split(b.AllowedRoleAccounts, ",") {
			if a = strings.TrimSpace(a); a != "" && a != "*" && !reRoleAccount.MatchString(a) {
				return fmt.Errorf("llm_proxy.bedrock.allowed_role_accounts: %q is not a 12-digit AWS account ID, a partition-qualified \"<partition>:<account>\" (e.g. aws-us-gov:123456789012), or \"*\"", a)
			}
		}
	}

	seenModels := map[string]bool{}
	for i, m := range c.LLMProxy.Models {
		if err := ValidateModelName(m.Name); err != nil {
			return fmt.Errorf("llm_proxy.models[%d]: %w", i, err)
		}
		if seenModels[m.Name] {
			return fmt.Errorf("llm_proxy.models[%d]: duplicate name %q", i, m.Name)
		}
		seenModels[m.Name] = true
		if len(m.Targets) == 0 {
			return fmt.Errorf("llm_proxy.models[%d] (%s): at least one target is required", i, m.Name)
		}
		for j, t := range m.Targets {
			if err := ValidateModelTarget(context.Background(), t.Vendor, t.Model, t.BaseURL, t.Region); err != nil {
				return fmt.Errorf("llm_proxy.models[%d].targets[%d]: %w", i, j, err)
			}
			if err := ValidateModelTargetLabel(t.Vendor, t.Label); err != nil {
				return fmt.Errorf("llm_proxy.models[%d].targets[%d]: %w", i, j, err)
			}
			if t.Credential != "" {
				return fmt.Errorf("llm_proxy.models[%d].targets[%d]: credential is not allowed on a platform row (credentials are tenant-scoped)", i, j)
			}
		}
		if m.Price != nil {
			if err := ValidateModelPrice(m.Price.Input, m.Price.Output, m.Price.CacheRead, m.Price.CacheWrite); err != nil {
				return fmt.Errorf("llm_proxy.models[%d].price: %w", i, err)
			}
		}
	}

	// One DNS budget for the whole seed (every gateway command loads the
	// config): a lookup that runs out is accepted, and the dial-time guard
	// stays the authority, so a hanging resolver costs 2s, not 2s per URL.
	seedCtx, cancelSeed := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancelSeed()
	seenCatalog := map[string]bool{}
	for i, e := range c.MCPCatalog.Seed {
		if err := ValidateMCPCatalogSeed(seedCtx, e); err != nil {
			return fmt.Errorf("mcp_catalog.seed[%d]: %w", i, err)
		}
		if seenCatalog[e.Slug] {
			return fmt.Errorf("mcp_catalog.seed[%d]: duplicate slug %q", i, e.Slug)
		}
		seenCatalog[e.Slug] = true
	}

	if c.Capture.StoreBodies {
		if c.Capture.MaxRequestBytes <= 0 || c.Capture.MaxResponseBytes <= 0 {
			return fmt.Errorf("capture: max_request_bytes and max_response_bytes must be positive when store_bodies is true")
		}
	}

	bs := c.LLMProxy.Capture.BodyStore
	switch bs.Type {
	case "", "none":
	case "filesystem":
		if strings.TrimSpace(bs.Filesystem.Root) == "" {
			return fmt.Errorf("llm_proxy.capture.body_store.filesystem: root is required when type is \"filesystem\"")
		}
	case "s3":
		if strings.TrimSpace(bs.S3.Bucket) == "" {
			return fmt.Errorf("llm_proxy.capture.body_store.s3: bucket is required when type is \"s3\"")
		}
		if bs.S3.Endpoint != "" {
			if err := validateAbsoluteURL(bs.S3.Endpoint); err != nil {
				return fmt.Errorf("llm_proxy.capture.body_store.s3: invalid endpoint: %w", err)
			}
		}
	default:
		return fmt.Errorf("llm_proxy.capture.body_store: type must be \"none\", \"filesystem\" or \"s3\", got %q", bs.Type)
	}
	if bs.InlineMaxBytes < 0 {
		return fmt.Errorf("llm_proxy.capture.body_store: inline_max_bytes must be >= 0")
	}

	if c.Sinks.Otel.Enabled {
		if strings.TrimSpace(c.Sinks.Otel.Endpoint) == "" {
			return fmt.Errorf("sinks.otel: endpoint is required when enabled")
		}
		switch c.Sinks.Otel.Protocol {
		case "", "http", "grpc":
		default:
			return fmt.Errorf("sinks.otel: protocol must be \"http\" or \"grpc\", got %q", c.Sinks.Otel.Protocol)
		}
	}

	if c.Ingest.Enabled {
		if c.Ingest.MaxBodyBytes <= 0 {
			return fmt.Errorf("ingest: max_body_bytes must be positive when enabled")
		}
		if c.Ingest.MaxRecords <= 0 {
			return fmt.Errorf("ingest: max_records must be positive when enabled")
		}
		if c.Ingest.RatePerMinute <= 0 {
			return fmt.Errorf("ingest: rate_per_minute must be positive when enabled")
		}
	}

	if c.Sinks.ClickHouse.Enabled {
		if strings.TrimSpace(c.Sinks.ClickHouse.Host) == "" {
			return fmt.Errorf("sinks.clickhouse: host is required when enabled")
		}
		if c.Sinks.ClickHouse.Port <= 0 {
			return fmt.Errorf("sinks.clickhouse: port must be positive when enabled")
		}
		if c.Sinks.ClickHouse.BatchSize <= 0 {
			return fmt.Errorf("sinks.clickhouse: batch_size must be positive when enabled")
		}
		if c.Sinks.ClickHouse.FlushInterval <= 0 {
			return fmt.Errorf("sinks.clickhouse: flush_interval must be positive when enabled")
		}
		if c.Sinks.ClickHouse.BufferSize <= 0 {
			return fmt.Errorf("sinks.clickhouse: buffer_size must be positive when enabled")
		}
	}

	return nil
}

// ReModelName is the shape of a model registry name: what a client puts in
// a request's "model" field to address a registered row. Lowercase so a
// lookup never depends on the caller's casing; the punctuation set covers
// vendor-style ids (claude-sonnet-4.5, gpt-4o_mini).
var ReModelName = regexp.MustCompile(`^[a-z0-9._-]{1,64}$`)

// ModelVendors is the closed set of upstream wire formats a model
// registry target may name (pkg/store.ModelTarget.Vendor).
var ModelVendors = map[string]bool{"anthropic": true, "bedrock": true, "openai_compat": true, "gemini": true}

// ValidateModelName enforces ReModelName. Shared by the config seed and
// the /api/v1/models handlers so both accept exactly the same names.
func ValidateModelName(name string) error {
	if !ReModelName.MatchString(name) {
		return fmt.Errorf("name %q must match %s", name, ReModelName.String())
	}
	return nil
}

// ReCatalogSuggestedName is the shape of a model catalog model's
// suggested_name: like ReModelName, but must start with a letter or digit
// (never "." or "-"), matching model_catalog_models' CHECK constraint
// (migration 000006_model_catalog) -- a suggested_name becomes a real
// models.name once a tenant connects it, so it must also satisfy
// ReModelName, which this stricter pattern always does.
var ReCatalogSuggestedName = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)

// ValidateCatalogSuggestedName enforces ReCatalogSuggestedName.
func ValidateCatalogSuggestedName(name string) error {
	if !ReCatalogSuggestedName.MatchString(name) {
		return fmt.Errorf("suggested_name %q must match %s", name, ReCatalogSuggestedName.String())
	}
	return nil
}

// ReCatalogProviderSlug is the shape of a model catalog provider's slug,
// matching model_catalog_providers' CHECK constraint.
var ReCatalogProviderSlug = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,31}$`)

// ValidateCatalogProviderSlug enforces ReCatalogProviderSlug.
func ValidateCatalogProviderSlug(slug string) error {
	if !ReCatalogProviderSlug.MatchString(slug) {
		return fmt.Errorf("slug %q must match %s", slug, ReCatalogProviderSlug.String())
	}
	return nil
}

// ValidateModelTarget checks one registry target's vendor-specific
// shape: a known vendor and a non-empty vendor model id; openai_compat
// needs a base_url; bedrock needs a well-formed AWS region (and takes no
// base_url); any base_url must be absolute https (http is allowed for
// loopback hosts and host.docker.internal only, so a local development
// upstream works without TLS). The credential reference is checked by the caller, which knows
// the tenant. ctx bounds the DNS lookup validateBaseURLShape performs on
// a hostname base_url (see netguard.CheckHostResolve); pass
// context.Background() when no request-scoped context is available
// (e.g. validating a YAML-seeded target at startup).
func ValidateModelTarget(ctx context.Context, vendor, model, baseURL, region string) error {
	if !ModelVendors[vendor] {
		return fmt.Errorf("vendor %q must be one of anthropic, bedrock, openai_compat, gemini", vendor)
	}
	if strings.TrimSpace(model) == "" {
		return fmt.Errorf("model is required")
	}
	switch vendor {
	case "openai_compat":
		if baseURL == "" {
			return fmt.Errorf("base_url is required for vendor openai_compat")
		}
	case "bedrock":
		if !ReAWSRegion.MatchString(region) {
			return fmt.Errorf("region %q is required for vendor bedrock and must be a well-formed AWS region", region)
		}
		if baseURL != "" {
			return fmt.Errorf("base_url is not used by vendor bedrock")
		}
	}
	if vendor != "bedrock" && region != "" {
		return fmt.Errorf("region is only used by vendor bedrock")
	}
	if baseURL != "" {
		if err := validateBaseURLShape(ctx, baseURL); err != nil {
			return err
		}
	}
	return nil
}

// validateBaseURLShape enforces the restriction every openai_compat base
// URL must satisfy, whether it is a registry target's base_url (above) or
// a model catalog provider's (ValidateCatalogBaseURL): absolute http(s),
// https unless the host is loopback or host.docker.internal (a gateway in
// Docker reaching a model server on the same machine), and no query,
// fragment or userinfo. Shared so the two surfaces can never drift apart.
func validateBaseURLShape(ctx context.Context, raw string) error {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") {
		return fmt.Errorf("base_url %q must be an absolute http(s) URL", raw)
	}
	if u.Scheme == "http" && !isLoopbackHost(u.Hostname()) {
		return fmt.Errorf("base_url %q must use https (http is allowed for loopback hosts and host.docker.internal only)", raw)
	}
	if u.RawQuery != "" || u.Fragment != "" || u.User != nil {
		return fmt.Errorf("base_url %q must not carry a query, fragment or userinfo", raw)
	}
	if err := netguard.CheckHostResolve(ctx, u.Hostname()); err != nil {
		return fmt.Errorf("base_url %q: %w", raw, err)
	}
	return nil
}

// reVersionSegment matches an API-version-like path segment: v<digits>,
// v<digits>beta (e.g. /v1, /v2beta), or the literal "openai" (Gemini's
// OpenAI-compatible endpoint lives under .../v1beta/openai). Used only for
// ValidateCatalogBaseURL's non-fatal warning below.
var reVersionSegment = regexp.MustCompile(`(^|/)v\d+(beta)?(/|$)|(^|/)openai(/|$)`)

// ValidateCatalogBaseURL validates a model catalog provider's base_url:
// the same shape rule validateBaseURLShape enforces for a registry
// openai_compat target, plus two catalog-specific conventions. It returns
// the value to persist (trailing slash trimmed -- not an error) and a
// non-fatal warning the caller may surface to the admin when the path has
// no API-version-like segment: most OpenAI-compatible APIs version their
// base path (see docs/llm-plane.md's base_url convention), so a bare host
// is very likely missing it, though the gateway will still send requests
// there.
func ValidateCatalogBaseURL(ctx context.Context, raw string) (trimmed, warning string, err error) {
	trimmed = strings.TrimRight(raw, "/")
	if err := validateBaseURLShape(ctx, trimmed); err != nil {
		return "", "", err
	}
	u, _ := url.Parse(trimmed)
	if !reVersionSegment.MatchString(u.Path) {
		warning = fmt.Sprintf("base_url %q does not look like it includes an API version segment (e.g. /v1); the provider's models may not respond at this path", trimmed)
	}
	return trimmed, warning, nil
}

// ReModelLabel is the shape of a registry target's label.
var ReModelLabel = regexp.MustCompile(`^[a-z0-9._-]{1,32}$`)

// reservedModelLabels are names a label may not take: the gateway's native
// Anthropic and Bedrock wires own them, and their capture rows are priced
// and region-tagged by wire (another vendor's tokens would be billed as
// Claude's).
var reservedModelLabels = map[string]bool{"anthropic": true, "bedrock": true}

// ValidateModelTargetLabel checks a registry target's optional label: the
// name of the vendor behind an openai_compat target ("groq", "deepseek",
// "ollama", ...). Empty is valid. Only openai_compat takes one -- it is the
// one vendor type that stands for many vendors; the others name theirs.
func ValidateModelTargetLabel(vendor, label string) error {
	if label == "" {
		return nil
	}
	if vendor != "openai_compat" {
		return fmt.Errorf("label is only used by vendor openai_compat")
	}
	if !ReModelLabel.MatchString(label) {
		return fmt.Errorf("label %q must match %s", label, ReModelLabel.String())
	}
	if reservedModelLabels[label] {
		return fmt.Errorf("label %q is reserved for the native %s wire", label, label)
	}
	return nil
}

// ValidateModelPrice rejects negative rates.
func ValidateModelPrice(input, output, cacheRead, cacheWrite float64) error {
	for name, v := range map[string]float64{"input": input, "output": output, "cache_read": cacheRead, "cache_write": cacheWrite} {
		if v < 0 {
			return fmt.Errorf("%s must be >= 0", name)
		}
	}
	return nil
}

// isLoopbackHost reports whether plain http to host stays on the machine
// running the gateway: localhost, a loopback IP, or host.docker.internal
// -- the name Docker gives the host a container runs on, which is how a
// gateway in Compose reaches a model server on the same laptop. Matched
// exactly (no suffixes), so "host.docker.internal.evil.com" is not it.
func isLoopbackHost(host string) bool {
	if host == "localhost" || host == "host.docker.internal" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func validateAbsoluteURL(raw string) error {
	if raw == "" {
		return fmt.Errorf("must not be empty")
	}
	u, err := url.Parse(raw)
	if err != nil {
		return err
	}
	if u.Scheme == "" || u.Host == "" {
		return fmt.Errorf("must be an absolute URL, got %q", raw)
	}
	return nil
}

// validateAgentURL checks llm_proxy.detection.agent_url: an absolute http(s)
// URL with no userinfo. Errors never echo the value, so a credential put in
// the URL by mistake does not reach the startup log.
func validateAgentURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("not a URL")
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("must be an absolute http or https URL")
	}
	if u.Host == "" {
		return fmt.Errorf("must be an absolute URL with a host")
	}
	if u.User != nil {
		return fmt.Errorf("must not carry userinfo (user:password@)")
	}
	return nil
}

// isLoopbackAddr reports whether a listen address ("host:port") binds only
// the loopback interface. An empty host (":8080"), 0.0.0.0 and [::] listen
// on every interface and are not loopback; "localhost" is.
func isLoopbackAddr(addr string) bool {
	host, _, err := net.SplitHostPort(strings.TrimSpace(addr))
	if err != nil {
		return false
	}
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
