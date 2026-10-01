// Package store defines the gateway's persistence contract: the eight
// tenant-scoped data models, the interfaces a backend must implement for
// each, and a driver registry so alternative backends can be added without
// modifying this package.
//
// Every model and method here is document-shaped: no SQL, no driver types
// leak into this package. internal/store/postgres is the reference
// implementation; pkg/store/storetest is the conformance suite every
// backend (including postgres) must pass.
package store

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/Tuskira/tusk-ai-secured-gateway/internal/config"
)

// Sentinel errors returned by every Store method that can fail this way.
// Backends must map their driver-specific errors (e.g. a unique constraint
// violation, a zero-row UPDATE/SELECT) onto these.
var (
	// ErrNotFound is returned when a Get/GetBySlug/GetByHash/Update/
	// Revoke/Delete/SoftDelete target does not exist (or is not visible
	// to the caller, e.g. wrong tenant or already soft-deleted).
	ErrNotFound = errors.New("store: not found")

	// ErrConflict is returned when a Create/Rotate would violate a
	// uniqueness constraint (e.g. a duplicate tenant slug, API key hash,
	// or (tenant, name)/(tenant, slug) pair).
	ErrConflict = errors.New("store: conflict")
)

// ---------------------------------------------------------------------------
// Models
// ---------------------------------------------------------------------------

// Tenant is the top-level isolation boundary. Every other model is scoped
// to a TenantID. The tenant "default" is created at first boot.
type Tenant struct {
	ID        string
	Slug      string
	Name      string
	Settings  map[string]any
	CreatedAt time.Time
}

// APIKey is a hashed, tenant-bound, role-scoped bearer credential. Only
// KeyHash (never the plaintext key) is persisted; KeyPrefix is kept for
// display in the admin UI.
type APIKey struct {
	ID        string
	TenantID  string
	Name      string
	Role      string
	KeyHash   string
	KeyPrefix string

	ExpiresAt  *time.Time
	RevokedAt  *time.Time
	LastUsedAt *time.Time

	// Limits caps what this key may spend or send through the LLM plane
	// (enforced by internal/llmplane). nil = unlimited.
	Limits *Limits

	// ProfileID binds the key to one agent profile of its tenant. When
	// set, the MCP plane enforces that profile for every request the key
	// makes and rejects a different X-Agent-Profile-Name. nil = unbound.
	ProfileID *string

	CreatedBy string
	CreatedAt time.Time
}

// Limits is a set of LLM-plane budgets and caps, carried by an APIKey or a
// Model. Every field is optional;
// a nil field is not enforced. DailyUSD / MonthlyUSD bound the captured
// cost_usd summed over the current UTC day / calendar month; RPM bounds
// requests per rolling minute; MaxTokens bounds the max_tokens a request
// may ask for.
type Limits struct {
	DailyUSD   *float64 `json:"daily_usd,omitempty"`
	MonthlyUSD *float64 `json:"monthly_usd,omitempty"`
	RPM        *int     `json:"rpm,omitempty"`
	MaxTokens  *int     `json:"max_tokens,omitempty"`
}

// Empty reports whether l sets no limit at all (nil counts as empty).
func (l *Limits) Empty() bool {
	return l == nil || (l.DailyUSD == nil && l.MonthlyUSD == nil && l.RPM == nil && l.MaxTokens == nil)
}

// User roles a console user may hold. They are deliberately a closed set
// (the users.role CHECK constraint mirrors it): a user's role maps onto
// the gateway's role->permission table in pkg/auth (RoleAdmin gets every
// permission except platform.admin, RoleViewer gets "*.read" only).
const (
	UserRoleAdmin  = "admin"
	UserRoleViewer = "viewer"
)

// User is a human console account. A user belongs to exactly one tenant.
// PasswordHash is an argon2id PHC string (internal/auth/password); the
// plaintext password is never stored.
type User struct {
	ID       string
	TenantID string
	// Username is lowercase and immutable once created (it may be an
	// email address); unique per tenant among live (non-deleted) users.
	Username    string
	DisplayName string

	PasswordHash string
	Role         string // UserRoleAdmin | UserRoleViewer

	MustChangePassword bool
	Disabled           bool

	// FailedLogins counts consecutive failed logins; LockedUntil, when in
	// the future, makes every login attempt fail (see
	// UserStore.RecordLoginFailure).
	FailedLogins int
	LockedUntil  *time.Time
	LastLoginAt  *time.Time

	PasswordChangedAt time.Time
	CreatedAt         time.Time
	UpdatedAt         time.Time
	DeletedAt         *time.Time
}

// UserSession is one logged-in browser session. ID is the hex SHA-256 of
// the opaque session token; the raw token lives only in the client's
// cookie, so a database read never yields a usable session.
type UserSession struct {
	ID        string
	UserID    string
	TenantID  string
	CSRFToken string

	CreatedAt  time.Time
	LastSeenAt time.Time
	// ExpiresAt is the absolute expiry (created + the configured maximum
	// session lifetime); the idle timeout is enforced by the session
	// authenticator from LastSeenAt.
	ExpiresAt time.Time
	RevokedAt *time.Time

	UserAgent string
	IP        string
}

// AuthAuditEntry is one row of the authentication audit trail. TenantID
// and TargetUserID are "" when not applicable (e.g. a login attempt
// against an unknown tenant).
type AuthAuditEntry struct {
	ID       int64
	TenantID string
	// ActorKind is "user", "api_key", "cli" or "anonymous".
	ActorKind string
	ActorID   string
	// Action is one of login_ok, login_fail, logout, password_change,
	// password_reset, user_create, user_update, user_disable, user_delete,
	// session_revoke.
	Action       string
	TargetUserID string
	IP           string
	Detail       string
	At           time.Time
}

// Credential is an encrypted secret payload (AES-256-GCM), addressed by
// (TenantID, Name) and referenced from connector header configs and the LLM
// plane's injected-auth mode. Ciphertext/Nonce are only populated by Get;
// List returns metadata only (see CredentialStore.List).
type Credential struct {
	ID       string
	TenantID string
	Name     string
	Type     string

	Ciphertext []byte
	Nonce      []byte
	KeyID      string // secret-store key ring id, enables rotation

	FieldNames []string // field names only, for the UI; never values

	CreatedBy string
	CreatedAt time.Time
	RotatedAt *time.Time
}

// Connector is a backend MCP server registration.
type Connector struct {
	ID       string
	TenantID string
	Name     string
	Slug     string
	Endpoint string

	TimeoutMS int
	Status    string // healthy | unhealthy | unknown

	Capabilities map[string]any
	Metadata     map[string]any

	// CatalogID is the MCPCatalogEntry this connector was added from
	// ("" for a hand-registered, custom connector).
	CatalogID string

	CreatedAt time.Time
	UpdatedAt time.Time
	DeletedAt *time.Time
}

// Enabled reports whether the connector may be called. A connector is
// enabled unless metadata.enabled is explicitly false; the flag lives in
// metadata so no schema change is needed and an old row stays enabled.
func (c *Connector) Enabled() bool {
	v, ok := c.Metadata["enabled"].(bool)
	return !ok || v
}

// MCPCatalogField is one value a tenant supplies when adding a catalog
// entry. Query, when set, routes the value into the connector URL as that
// query parameter (an optional project scope, say) instead of into the
// credential.
type MCPCatalogField struct {
	Name        string `json:"name"`
	Label       string `json:"label"`
	Secret      bool   `json:"secret,omitempty"`
	Required    bool   `json:"required,omitempty"`
	Placeholder string `json:"placeholder,omitempty"`
	Help        string `json:"help,omitempty"`
	Query       string `json:"query,omitempty"`
}

// MCPCatalogHeader names the header a credential-bearing auth kind sets.
// Name defaults to "Authorization"; Prefix is prepended to the value
// (bearer defaults to "Bearer ", header to none).
type MCPCatalogHeader struct {
	Name   string `json:"name,omitempty"`
	Prefix string `json:"prefix,omitempty"`
}

// MCPCatalogAuth describes what a tenant must supply for an entry. Kind is
// none | bearer | basic | header | oauth.
type MCPCatalogAuth struct {
	Kind           string            `json:"kind"`
	Fields         []MCPCatalogField `json:"fields"`
	HeaderTemplate *MCPCatalogHeader `json:"header_template,omitempty"`
}

// MCPCatalogEntry is a platform-curated MCP server a tenant admin can add
// to their tenant. Entries are global (no tenant), seeded from config, and
// inert until added: adding creates an ordinary tenant Connector.
type MCPCatalogEntry struct {
	ID string
	// TenantID is the owning tenant; empty for a platform row.
	TenantID    string
	Slug        string
	Name        string
	Description string
	Icon        string
	Category    string
	URL         string
	// URLOverridable lets the tenant replace URL at add time (regional or
	// self-hosted deployments of the same server).
	URLOverridable bool
	Transport      string
	Auth           MCPCatalogAuth
	DefaultHeaders map[string]string
	SuggestedTools []string
	DocsURL        string
	Enabled        bool

	CreatedAt time.Time
	UpdatedAt time.Time
	DeletedAt *time.Time
}

// AgentProfile groups tools (via ProfileTool) from one or more Connectors
// into an allow-list enforced on tools/call.
type AgentProfile struct {
	ID          string
	TenantID    string
	Name        string
	Slug        string
	Description string
	// Instructions is free-text prepended to the resolved profile's MCP
	// initialize response (internal/dataplane/profile), on top of the
	// gateway's own static pointer text and, when the profile has
	// skills, the skill index. At most 8 KiB. Empty is the default (no
	// extra instructions).
	Instructions string

	Metadata map[string]any

	CreatedAt time.Time
	UpdatedAt time.Time
	DeletedAt *time.Time
}

// ProfileTool is one (connector, tool) pair allow-listed on an AgentProfile.
type ProfileTool struct {
	AgentProfileID string
	ConnectorID    string
	ToolNamespace  string
	ToolName       string
}

// Skill is one row of the skills & commands registry: a named, versioned
// bundle of text files (Kind "skill", modeled on the portable Agent
// Skills format) or a reusable prompt template (Kind "command", rendered
// with named Arguments). A row with TenantID "" is a PLATFORM row (tenant_id
// NULL in Postgres) visible to every tenant and read-only through the
// API; a tenant row with the same Name overrides it for that tenant, the
// same convention as Model. Description and Frontmatter are derived from
// the current version's SKILL.md (see SkillStore.AddVersion) rather than
// stored independently of it.
type Skill struct {
	ID          string
	TenantID    string // "" = platform row
	Name        string
	Kind        string // "skill" | "command"
	Description string
	// Frontmatter is SKILL.md's parsed YAML front matter (the portable
	// Agent Skills fields plus the Claude Code fields internal/skills
	// allows; never "hooks").
	Frontmatter map[string]any
	// Arguments declares the named placeholders a "command" kind Skill's
	// template body may reference via {{name}}. Empty for kind "skill".
	Arguments []CommandArgument
	// LatestVersion is the highest version number in this Skill's
	// SkillVersion history; 1 immediately after Create.
	LatestVersion int
	Enabled       bool
	Metadata      map[string]any

	CreatedAt time.Time
	UpdatedAt time.Time
	DeletedAt *time.Time
}

// CommandArgument is one named placeholder a "command" kind Skill's
// template body may reference as {{Name}}.
type CommandArgument struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	Required    bool   `json:"required,omitempty"`
}

// SkillFile is one file within a SkillVersion: SKILL.md (always present)
// plus up to 19 supporting files. SHA256 is the lowercase hex digest of
// Content's raw bytes; Size is len(bytes) -- both computed by
// internal/skills, not supplied by a caller.
type SkillFile struct {
	Path    string `json:"path"`
	Content string `json:"content"`
	SHA256  string `json:"sha256"`
	Size    int    `json:"size"`
}

// SkillVersion is one immutable, numbered snapshot of a Skill's files.
// ListVersions leaves Files nil (a version listing doesn't need file
// bodies); Get/GetVersion populate it.
type SkillVersion struct {
	SkillID   string
	Version   int
	Files     []SkillFile
	CreatedBy string
	CreatedAt time.Time
}

// ProfileSkill is one (skill, optionally pinned version) attachment on an
// AgentProfile. Version nil means "always resolve to the skill's current
// LatestVersion"; a non-nil Version pins a specific one, which can fall
// behind as new versions are added (see docs/profiles.md, "outdated").
type ProfileSkill struct {
	AgentProfileID string
	SkillID        string
	Version        *int
}

// SkillListOptions narrows a SkillStore.List.
type SkillListOptions struct {
	// Kind filters to "skill" or "command" rows only; "" lists both.
	Kind string
}

// CachedTool is one tool advertised by a connector, cached for fast
// tools/list responses and full-text search.
type CachedTool struct {
	TenantID      string
	ConnectorID   string
	ToolNamespace string
	ToolName      string
	Description   string
	InputSchema   map[string]any

	CachedAt  time.Time
	ExpiresAt time.Time
	Stale     bool
}

// Model is one row of the LLM plane's model registry: a client-facing
// name (what a caller puts in a request's "model" field) mapped onto an
// ordered list of upstream Targets. A row with TenantID "" is a PLATFORM
// default (tenant_id NULL in Postgres) visible to every tenant; a tenant
// row with the same Name overrides it for that tenant. Names are
// lowercase [a-z0-9._-]{1,64} and unique per (tenant, name) among live
// (not soft-deleted) rows.
type Model struct {
	ID          string
	TenantID    string // "" = platform row
	Name        string
	Description string
	Enabled     bool

	// Targets is tried in order by the LLM plane: the first that answers
	// wins, the next is attempted only when the previous failed before
	// any byte reached the client. Never empty on a persisted row.
	Targets []ModelTarget
	// Price, when set, overrides pkg/pricing's rate card for calls
	// resolved through this row (USD per million tokens).
	Price *ModelPrice
	// Limits caps what may be spent or sent through this model name, per
	// tenant (enforced by internal/llmplane after the registry lookup, in
	// addition to the calling key's own Limits). nil = unlimited. A
	// platform row's limits apply to each tenant separately.
	Limits   *Limits
	Metadata map[string]any

	// CatalogModelID, when set, is the ModelCatalogModel this row was
	// created from (ModelCatalogStore.Connect) -- "" for a row created by
	// hand via POST /models. It is set once, at Create, and never changes:
	// editing the catalog afterward does not alter rows already connected
	// from it (see ModelCatalogStore's doc comment).
	CatalogModelID string
	// Capabilities mirrors ModelCatalogModel.Capabilities for a row
	// connected from the catalog ({"tools": bool|null, "vision":
	// bool|null, "streaming": bool|null, "max_context": int|null}); nil
	// for a hand-created row. Informational only -- the LLM plane does not
	// enforce it.
	Capabilities map[string]any

	CreatedAt time.Time
	UpdatedAt time.Time
	DeletedAt *time.Time
}

// ModelTarget is one upstream a Model resolves to.
type ModelTarget struct {
	// Vendor is the upstream's wire format: "anthropic", "bedrock",
	// "openai_compat" or "gemini".
	Vendor string `json:"vendor"`
	// Model is the vendor's own model id, written into the upstream
	// request in place of the client's name.
	Model string `json:"model"`
	// BaseURL overrides the vendor's default endpoint; required for
	// openai_compat (which has no single default), unused by bedrock.
	BaseURL string `json:"base_url,omitempty"`
	// Credential names a row in the owning tenant's credential store
	// whose payload authenticates the upstream call. Only the NAME is
	// ever stored or returned; the value is resolved at call time.
	Credential string `json:"credential,omitempty"`
	// Region is the AWS region a bedrock target is signed for; required
	// for bedrock, unused otherwise.
	Region string `json:"region,omitempty"`
	// AllowCallerKey opts a target WITHOUT a Credential into receiving the
	// caller's own vendor key (BYOK) even though it lives on a host other
	// than the gateway's configured default for that vendor. Default
	// false: a caller's key is never sent to a host the operator merely
	// pointed a name at. Mutually exclusive with Credential.
	AllowCallerKey bool `json:"allow_caller_key,omitempty"`
	// Label names the vendor behind an openai_compat target ("groq",
	// "deepseek", "ollama", ...; [a-z0-9._-]{1,32}, never "anthropic" or
	// "bedrock"). When set it is what capture records as resolved_vendor,
	// the provider the call is priced as, and the suffix of the header a
	// caller sends its key for this vendor in (X-Provider-Key-<label>).
	// Empty for every other vendor.
	Label string `json:"label,omitempty"`
}

// ModelPrice is a registry price override, in USD per million tokens.
type ModelPrice struct {
	Input      float64 `json:"input"`
	Output     float64 `json:"output"`
	CacheRead  float64 `json:"cache_read,omitempty"`
	CacheWrite float64 `json:"cache_write,omitempty"`
}

// ListOptions narrows a ModelStore.List.
type ListOptions struct {
	// EnabledOnly drops rows whose Enabled is false.
	EnabledOnly bool
}

// ---------------------------------------------------------------------------
// Interfaces
// ---------------------------------------------------------------------------

// TenantStore manages Tenant rows.
type TenantStore interface {
	Create(ctx context.Context, t *Tenant) error
	GetBySlug(ctx context.Context, slug string) (*Tenant, error)
	List(ctx context.Context) ([]*Tenant, error)
}

// APIKeyStore manages APIKey rows.
type APIKeyStore interface {
	Create(ctx context.Context, k *APIKey) error
	GetByHash(ctx context.Context, keyHash string) (*APIKey, error)
	// GetByID looks up one tenant-scoped API key by id (e.g. so a
	// revoke/rotate handler can recover its KeyHash to invalidate an
	// Authenticator's cache without a full List scan). Returns
	// ErrNotFound if id doesn't exist, or exists under a different
	// tenant.
	GetByID(ctx context.Context, tenantID, id string) (*APIKey, error)
	List(ctx context.Context, tenantID string) ([]*APIKey, error)
	Revoke(ctx context.Context, tenantID, id string) error
	TouchLastUsed(ctx context.Context, id string, at time.Time) error
	// SetLimits replaces a key's Limits (nil or Empty clears them).
	// Returns ErrNotFound if id doesn't exist under tenantID.
	SetLimits(ctx context.Context, tenantID, id string, l *Limits) error
	// SetProfile binds the key to profileID, or unbinds it when nil.
	// The caller must have confirmed profileID belongs to tenantID.
	// Returns ErrNotFound if id doesn't exist under tenantID.
	SetProfile(ctx context.Context, tenantID, id string, profileID *string) error
}

// RevocationNotifier is an OPTIONAL capability of a Store (discovered with a
// type assertion on the store.Store value, like io.WriterTo): a backend that
// can push "this API key changed" events between gateway processes
// implements it, so a revoke/rotate on one replica evicts the key from every
// other replica's lookup cache at once instead of after the cache TTL.
//
// A backend that does not implement it still works: revocation then takes
// effect on other processes within the API-key cache TTL
// (auth.api_keys.cache_ttl).
//
// The producer side is the backend's own concern: its APIKeyStore.Revoke
// must publish the key's hash in the same transaction as the write.
type RevocationNotifier interface {
	// ListenKeyRevocations blocks, delivering the key hash of every
	// revocation published after it is ready, until ctx is done or the
	// underlying connection fails (any error return means "reconnect").
	//
	// onReady is called each time the subscription becomes active, before
	// any event is delivered; the caller flushes its caches there, since
	// events published while no subscription was active are lost.
	ListenKeyRevocations(ctx context.Context, onReady func(), onRevoke func(keyHash string)) error
}

// CredentialStore manages Credential rows. List never populates
// Ciphertext/Nonce (metadata only); only Get returns the full row.
type CredentialStore interface {
	Create(ctx context.Context, c *Credential) error
	Get(ctx context.Context, tenantID, name string) (*Credential, error)
	List(ctx context.Context, tenantID string) ([]*Credential, error)
	// Rotate replaces the encrypted payload (and the field names it
	// contains) and sets RotatedAt.
	Rotate(ctx context.Context, tenantID, name string, ciphertext, nonce []byte, keyID string, fieldNames []string) error
	Delete(ctx context.Context, tenantID, name string) error
}

// ConnectorStore manages Connector rows. SoftDelete hides a row from Get/
// GetBySlug/List without removing it.
type ConnectorStore interface {
	Create(ctx context.Context, c *Connector) error
	Get(ctx context.Context, tenantID, id string) (*Connector, error)
	GetBySlug(ctx context.Context, tenantID, slug string) (*Connector, error)
	List(ctx context.Context, tenantID string) ([]*Connector, error)
	Update(ctx context.Context, c *Connector) error
	SoftDelete(ctx context.Context, tenantID, id string) error
}

// MCPCatalogStore manages MCP catalog entries: PLATFORM rows (TenantID "",
// seeded from config or written by a platform admin) and tenant rows a
// tenant admin curates. Like ModelStore, every tenant-scoped read sees the
// tenant's own rows plus the platform rows, the tenant's row winning on a
// shared Slug; tenantID "" addresses the platform rows alone. Slug is
// unique among live rows per tenant (and among live platform rows).
// SoftDelete hides a row from every read and frees its slug.
type MCPCatalogStore interface {
	// Create inserts e (owned by e.TenantID, "" = platform). ErrConflict
	// when a live row in that scope already has the slug.
	Create(ctx context.Context, e *MCPCatalogEntry) error
	// Get returns the row by id when it belongs to tenantID or is a
	// platform row; ErrNotFound otherwise.
	Get(ctx context.Context, tenantID, id string) (*MCPCatalogEntry, error)
	// GetBySlug returns tenantID's own row with slug, else the platform
	// row, else ErrNotFound. tenantID "" returns the platform row only.
	GetBySlug(ctx context.Context, tenantID, slug string) (*MCPCatalogEntry, error)
	// List returns tenantID's rows merged with the platform rows (tenant
	// wins on a shared slug), ordered by name. tenantID "" lists the
	// platform rows only.
	List(ctx context.Context, tenantID string) ([]*MCPCatalogEntry, error)
	// Update rewrites every mutable column of the row e.ID addresses
	// within e.TenantID ("" = a platform row). Slug is not changed.
	// ErrNotFound when the row is missing, deleted or owned elsewhere.
	Update(ctx context.Context, e *MCPCatalogEntry) error
	// Upsert creates the live row with e.TenantID and e.Slug or rewrites
	// it (the config seed path).
	Upsert(ctx context.Context, e *MCPCatalogEntry) error
	SoftDelete(ctx context.Context, tenantID, id string) error
}

// AgentProfileStore manages AgentProfile rows and their tool allow-lists.
// SetTools replaces a profile's entire tool allow-list.
type AgentProfileStore interface {
	Create(ctx context.Context, p *AgentProfile) error
	Get(ctx context.Context, tenantID, id string) (*AgentProfile, error)
	GetBySlug(ctx context.Context, tenantID, slug string) (*AgentProfile, error)
	List(ctx context.Context, tenantID string) ([]*AgentProfile, error)
	Update(ctx context.Context, p *AgentProfile) error
	SoftDelete(ctx context.Context, tenantID, id string) error

	SetTools(ctx context.Context, tenantID, profileID string, tools []ProfileTool) error
	GetTools(ctx context.Context, tenantID, profileID string) ([]ProfileTool, error)

	// SetSkills replaces profileID's entire skill/command attachment set.
	// Unlike SetTools/GetTools, it is not tenant-scoped: the caller (the
	// Phase 2 API handler) is responsible for confirming profileID
	// belongs to the caller's tenant and that every SkillID is visible to
	// it before calling this -- the same division of responsibility
	// Profiles.SetTools' handler already applies for connector_id
	// ownership on top of SetTools' own FK check.
	SetSkills(ctx context.Context, profileID string, items []ProfileSkill) error
	// GetSkills returns profileID's current skill/command attachments.
	GetSkills(ctx context.Context, profileID string) ([]ProfileSkill, error)
}

// ModelStore manages Model rows (the LLM plane's model registry).
//
// Every tenant-scoped read sees the tenant's own rows plus the platform
// rows (TenantID ""), with the tenant's row winning when both carry the
// same Name. tenantID "" addresses the platform rows alone (used by the
// config seed). SoftDelete hides a row from Get/GetByName/List without
// removing it, and frees its name for re-use.
type ModelStore interface {
	Create(ctx context.Context, m *Model) error
	// Get returns the row by id when it belongs to tenantID or is a
	// platform row; ErrNotFound otherwise.
	Get(ctx context.Context, tenantID, id string) (*Model, error)
	// GetByName returns tenantID's own row named name, else the platform
	// row of that name, else ErrNotFound.
	GetByName(ctx context.Context, tenantID, name string) (*Model, error)
	// List returns tenantID's rows merged with the platform rows (tenant
	// wins on a shared name), ordered by name. tenantID "" lists the
	// platform rows only.
	List(ctx context.Context, tenantID string, opts ListOptions) ([]*Model, error)
	// Update rewrites every mutable column of the row m.ID addresses
	// within m.TenantID ("" = a platform row). ErrNotFound when the row
	// is missing, deleted, or owned elsewhere; ErrConflict on a name
	// collision.
	Update(ctx context.Context, m *Model) error
	SoftDelete(ctx context.Context, tenantID, id string) error
}

// ---------------------------------------------------------------------------
// Model catalog
// ---------------------------------------------------------------------------

// ModelCatalogProvider is one platform-level catalog entry: a known
// OpenAI-compatible vendor a tenant can "connect" (ModelCatalogStore.Connect)
// by supplying an API key once. Not callable on its own -- connecting
// copies its models' fields onto ordinary tenant Model rows, which are the
// ones the LLM plane resolves. Rows are platform-wide (no TenantID): every
// tenant sees the same catalog.
type ModelCatalogProvider struct {
	ID          string
	Slug        string // ^[a-z0-9][a-z0-9-]{0,31}$, e.g. "nebius"
	DisplayName string
	// Vendor is the upstream wire format the provider's models speak.
	// "openai_compat" only in v1 (see model_catalog_providers' CHECK).
	Vendor  string
	BaseURL string // OpenAI-compatible base INCLUDING the version segment, no trailing slash
	DocsURL string
	Enabled bool

	// Models is populated by ListProviders and GetProvider only -- never
	// by Create/Update, which address the provider row alone.
	Models []ModelCatalogModel

	CreatedAt time.Time
	UpdatedAt time.Time
}

// ModelCatalogModel is one model a ModelCatalogProvider offers.
type ModelCatalogModel struct {
	ID         string
	ProviderID string
	// ModelID is the vendor's own id (e.g. "zai-org/GLM-5.3"), written
	// verbatim into a connected Model's target.
	ModelID     string
	DisplayName string
	// SuggestedName is the registry name a Connect call gives the tenant
	// Model it creates: ^[a-z0-9][a-z0-9._-]{0,63}$, unique across the
	// whole catalog (model_catalog_models.suggested_name).
	SuggestedName string
	// Price is nil when unknown -- never a guessed rate.
	Price *ModelPrice
	// Capabilities is {"tools": bool|null, "vision": bool|null,
	// "streaming": bool|null, "max_context": int|null}; a missing or null
	// field means unknown, never assumed false/true.
	Capabilities map[string]any
	// Notes is a free-text admin note, e.g. explaining a false capability
	// (a model whose tool use isn't supported through the current
	// translation path). "" when there is nothing to say.
	Notes   string
	Enabled bool

	CreatedAt time.Time
	UpdatedAt time.Time
}

// CatalogUsage is one catalog model's tenant fan-out, as reported by
// ModelCatalogStore.CountTenantUsage: how many live tenant Model rows
// (across every tenant -- a platform-wide count) still carry this model's
// CatalogModelID.
type CatalogUsage struct {
	CatalogModelID string
	TenantModels   int
}

// CatalogConnectCredential is ModelCatalogStore.Connect's credential
// input: exactly one of Existing or New is set.
type CatalogConnectCredential struct {
	// Existing names a credential already in the tenant's store; Connect
	// only verifies it exists (ErrNotFound otherwise) and uses its name on
	// every created target -- it never decrypts it.
	Existing string
	// New, when non-nil, is sealed (already encrypted, by the same cipher
	// path store.Credential.Create Rotate use -- see secrets.Service.Seal)
	// before Connect is called, so Connect's own transaction never handles
	// plaintext. Its Name must not already exist in the tenant (Connect
	// returns a *CredentialNameConflictError otherwise).
	New *CatalogConnectNewCredential
}

// CatalogConnectNewCredential is the sealed payload for a brand-new
// credential Connect creates alongside the tenant models, in the same
// transaction.
type CatalogConnectNewCredential struct {
	Name string
	// Type is "api_key", matching the console's own "Accept API key"
	// credential (web/src/components/app/models/ModelFormDialog.tsx) --
	// the same type/payload shape everywhere a model-registry target's
	// credential is created.
	Type       string
	Ciphertext []byte
	Nonce      []byte
	KeyID      string
	FieldNames []string
}

// CatalogConnectModel is one catalog model Connect registers for a tenant.
// The caller (the API handler) resolves these fields from the
// ModelCatalogProvider/ModelCatalogModel rows and pre-validates that the
// provider and model are both enabled -- Connect itself only enforces the
// per-name conflict rule below.
type CatalogConnectModel struct {
	CatalogModelID string
	// Name is the tenant Model's name (ModelCatalogModel.SuggestedName).
	Name string
	// Description is the tenant Model's description (ModelCatalogModel.DisplayName).
	Description string
	ModelID     string // vendor model id, becomes the target's Model
	BaseURL     string // the provider's BaseURL, becomes the target's BaseURL
	// Label is the provider's slug (e.g. "nebius"), becomes the target's
	// Label -- the same convention the console's own vendor presets use
	// (web/src/components/app/models/targets.ts), so a model connected
	// from the catalog and one created by hand through the console's
	// vendor dropdown report the same resolved_vendor/provider.
	Label        string
	Price        *ModelPrice
	Capabilities map[string]any
}

// CatalogConnectResult is ModelCatalogStore.Connect's result.
type CatalogConnectResult struct {
	CredentialName    string
	CredentialCreated bool
	Models            []CatalogConnectModelResult
}

// CatalogConnectModelResult is one CatalogConnectModel's outcome.
type CatalogConnectModelResult struct {
	CatalogModelID string
	// ModelID is the created (or already-existing) tenant Model's own id.
	ModelID string
	Name    string
	// Status is "created" (a new tenant Model row was inserted) or
	// "exists" (a live tenant Model already named Name and connected from
	// this same CatalogModelID -- treated as a no-op success, not a
	// conflict).
	Status string
}

// CredentialNameConflictError is returned by Connect when a
// CatalogConnectCredential.New's Name already names a live credential in
// the tenant: unlike CatalogConnectModel's "exists" outcome (safe to
// repeat), a caller-chosen new credential name colliding with an existing,
// possibly unrelated credential is never silently reused. Unwraps to
// ErrConflict.
type CredentialNameConflictError struct{ Name string }

func (e *CredentialNameConflictError) Error() string {
	return fmt.Sprintf("credential %q already exists", e.Name)
}
func (e *CredentialNameConflictError) Unwrap() error { return ErrConflict }

// ModelNameConflictError is returned by Connect when a
// CatalogConnectModel's Name already names a live tenant Model NOT
// connected from the same CatalogModelID (i.e. a hand-created or
// differently-sourced row). Unwraps to ErrConflict.
type ModelNameConflictError struct{ Name string }

func (e *ModelNameConflictError) Error() string {
	return fmt.Sprintf("model name %q is already registered", e.Name)
}
func (e *ModelNameConflictError) Unwrap() error { return ErrConflict }

// PriceUpdate is one catalog model's new price for
// ModelCatalogStore.ApplyPrices.
type PriceUpdate struct {
	CatalogModelID string
	// Price is the new price to write; nil clears it (no price known).
	Price *ModelPrice
}

// ApplyPricesResult reports ModelCatalogStore.ApplyPrices' outcome.
type ApplyPricesResult struct {
	// CatalogUpdated is how many catalog models had Price rewritten (==
	// len(updates) on success -- ApplyPrices has no partial-skip case).
	CatalogUpdated int
	// TenantUpdated is how many tenant Model rows, across every tenant,
	// had Price rewritten because it still matched the catalog model's
	// OLD price. Always 0 when updateTenantModels is false.
	TenantUpdated int
	// TenantIDs is the distinct set of tenants that had at least one
	// Model row updated -- so a caller with a registry cache (see
	// handlers.ModelInvalidator) can invalidate exactly those tenants
	// instead of every tenant or none. Always empty when
	// updateTenantModels is false.
	TenantIDs []string
}

// ModelCatalogStore manages the platform-wide model catalog (providers and
// their models) and Connect, the one operation that turns a tenant's
// selection of catalog models into real, callable tenant Model rows.
//
// Every method here is platform-scoped (no tenantID parameter) except
// Connect, which is where a specific tenant's credential and Model rows
// are created.
type ModelCatalogStore interface {
	// ListProviders returns every provider ordered by slug, each with its
	// Models populated (ordered by suggested_name).
	ListProviders(ctx context.Context) ([]*ModelCatalogProvider, error)
	// GetProvider returns one provider with its Models populated.
	// ErrNotFound if id doesn't exist.
	GetProvider(ctx context.Context, id string) (*ModelCatalogProvider, error)
	CreateProvider(ctx context.Context, p *ModelCatalogProvider) error
	// UpdateProvider rewrites every mutable column (Slug, DisplayName,
	// BaseURL, DocsURL, Enabled). ErrNotFound if p.ID doesn't exist;
	// ErrConflict on a slug collision.
	UpdateProvider(ctx context.Context, p *ModelCatalogProvider) error
	// DeleteProvider hard-deletes the provider and, by ON DELETE CASCADE,
	// its models; every tenant Model row that referenced one of those
	// models has its CatalogModelID cleared (ON DELETE SET NULL) rather
	// than being touched otherwise -- a tenant's already-registered models
	// keep working. The caller is responsible for the 409-with-usage-count
	// confirmation (CountTenantUsage) before calling this.
	DeleteProvider(ctx context.Context, id string) error

	// GetModel returns one catalog model. ErrNotFound if id doesn't exist.
	GetModel(ctx context.Context, id string) (*ModelCatalogModel, error)
	// CreateModel inserts m under m.ProviderID (the caller has already
	// confirmed the provider exists). ErrConflict on a (provider_id,
	// model_id) or suggested_name collision.
	CreateModel(ctx context.Context, m *ModelCatalogModel) error
	// UpdateModel rewrites ModelID, DisplayName, SuggestedName, Price,
	// Capabilities and Enabled; ProviderID is immutable. ErrNotFound if
	// m.ID doesn't exist; ErrConflict on a suggested_name collision.
	UpdateModel(ctx context.Context, m *ModelCatalogModel) error
	// DeleteModel hard-deletes the model; every tenant Model row that
	// referenced it has its CatalogModelID cleared. Same caller
	// responsibility as DeleteProvider.
	DeleteModel(ctx context.Context, id string) error

	// CountTenantUsage counts live tenant Model rows (every tenant) whose
	// CatalogModelID is catalogModelID -- the "in use" count a delete
	// confirmation needs.
	CountTenantUsage(ctx context.Context, catalogModelID string) (int, error)

	// Connect runs entirely in one transaction: it creates cred's
	// credential (skipped, CredentialCreated=false, when cred.Existing is
	// set -- Existing must already exist, ErrNotFound otherwise), then for
	// each of models creates (or, on an exact catalog-model re-request,
	// leaves alone -- see CatalogConnectModelResult.Status) a tenant Model
	// row. A conflict on ANY requested model aborts the whole call (no
	// partial registration): see ModelNameConflictError.
	Connect(ctx context.Context, tenantID string, cred CatalogConnectCredential, models []CatalogConnectModel) (*CatalogConnectResult, error)

	// ApplyPrices runs entirely in one transaction: for each of updates it
	// rewrites that catalog model's Price (ErrNotFound if its
	// CatalogModelID doesn't exist -- aborts the whole call, nothing
	// partially applied). When updateTenantModels is true, it additionally
	// rewrites the Price of every live tenant Model row (across every
	// tenant) whose CatalogModelID matches AND whose current Price is
	// unchanged from that catalog model's price as it was immediately
	// before this call (compared null-safely: a tenant row with no price
	// override, under a catalog model that also had none, counts as
	// matching) -- so a tenant admin's own manual override on a connected
	// model is never clobbered by a platform-wide price refresh.
	ApplyPrices(ctx context.Context, updates []PriceUpdate, updateTenantModels bool) (*ApplyPricesResult, error)
}

// SkillStore manages Skill and SkillVersion rows (the skills & commands
// registry).
//
// Every tenant-scoped read sees the tenant's own rows plus the platform
// rows (TenantID ""), with the tenant's row winning when both carry the
// same Name -- the same convention ModelStore documents. tenantID ""
// addresses the platform rows alone (used by the seed).
type SkillStore interface {
	// Create persists sk and its version-1 files in one transaction,
	// setting sk.ID and sk.LatestVersion=1. sk.Description and
	// sk.Frontmatter are taken as given (the caller -- the API handler or
	// the seed -- is responsible for deriving them from files' SKILL.md
	// via internal/skills before calling Create); Create does not
	// re-derive them.
	Create(ctx context.Context, sk *Skill, files []SkillFile, createdBy string) error
	// Get returns the row by id when it belongs to tenantID or is a
	// platform row; ErrNotFound otherwise.
	Get(ctx context.Context, tenantID, id string) (*Skill, error)
	// GetByName returns tenantID's own row named name, else the platform
	// row of that name, else ErrNotFound.
	GetByName(ctx context.Context, tenantID, name string) (*Skill, error)
	// List returns tenantID's rows merged with the platform rows (tenant
	// wins on a shared name), ordered by name, filtered by opts.Kind
	// when non-empty. The returned int is len of the returned slice
	// (Phase 1 does no store-side pagination).
	List(ctx context.Context, tenantID string, opts SkillListOptions) ([]Skill, int, error)
	// Update rewrites Description, Enabled, Metadata and Arguments only
	// -- Name, Kind and Frontmatter are immutable outside of AddVersion.
	// ErrNotFound when the row is missing, deleted, or owned elsewhere.
	Update(ctx context.Context, sk *Skill) error
	SoftDelete(ctx context.Context, tenantID, id string) error
	// AddVersion appends a new SkillVersion (version = current
	// LatestVersion + 1), bumps the row's LatestVersion, and refreshes
	// Description and Frontmatter from the new files' SKILL.md (a new
	// version's SKILL.md is the new source of truth for both -- see
	// internal/skills.ParseFrontmatter). ErrNotFound when id doesn't
	// belong to tenantID and isn't a platform row.
	AddVersion(ctx context.Context, tenantID, id string, files []SkillFile, createdBy string) (*SkillVersion, error)
	// GetVersion returns one version's files. Not tenant-scoped -- the
	// caller confirms visibility via Get first (see Profiles' analogous
	// GetTools doc comment for why).
	GetVersion(ctx context.Context, skillID string, version int) (*SkillVersion, error)
	// ListVersions returns every version, newest first, with Files left
	// nil (a listing doesn't need file bodies; GetVersion populates
	// them).
	ListVersions(ctx context.Context, skillID string) ([]SkillVersion, error)
}

// UserListOptions filters UserStore.List.
type UserListOptions struct {
	// Role, when non-empty, restricts the list to one role.
	Role string
}

// UserStore manages User rows. Every method is tenant-scoped: a user of
// tenant A is never visible through a call that names tenant B, and a
// soft-deleted user is invisible to every read.
type UserStore interface {
	// Create persists u, setting ID, PasswordChangedAt, CreatedAt and
	// UpdatedAt. ErrConflict when a live user with the same (tenant,
	// username) exists; a soft-deleted user's username can be reused.
	Create(ctx context.Context, u *User) error
	Get(ctx context.Context, tenantID, id string) (*User, error)
	GetByUsername(ctx context.Context, tenantID, username string) (*User, error)
	// List returns the tenant's live users ordered by username.
	List(ctx context.Context, tenantID string, opts UserListOptions) ([]*User, error)
	// Update rewrites DisplayName, Role and Disabled only (Username is
	// immutable; password state has its own methods). ErrNotFound when
	// the user is missing, deleted, or in another tenant.
	Update(ctx context.Context, u *User) error
	SoftDelete(ctx context.Context, tenantID, id string) error
	// SetPassword replaces the hash, sets MustChangePassword, stamps
	// PasswordChangedAt, and clears FailedLogins/LockedUntil.
	SetPassword(ctx context.Context, tenantID, id, hash string, mustChange bool) error
	// RecordLoginFailure atomically increments FailedLogins; when the new
	// count reaches maxFailures it sets LockedUntil = at+lockout and
	// resets the counter to zero.
	RecordLoginFailure(ctx context.Context, tenantID, id string, maxFailures int, lockout time.Duration, at time.Time) error
	// RecordLoginSuccess clears FailedLogins/LockedUntil and sets
	// LastLoginAt = at.
	RecordLoginSuccess(ctx context.Context, tenantID, id string, at time.Time) error
	// CountActiveAdmins counts the tenant's live, non-disabled admins.
	CountActiveAdmins(ctx context.Context, tenantID string) (int, error)
}

// UserSessionStore manages UserSession rows. Get returns a session even
// when it is revoked or past ExpiresAt: deciding whether it is still
// usable (revocation, absolute and idle expiry) is the session
// authenticator's job, so it can tell the cases apart in its logs.
type UserSessionStore interface {
	// Create persists s. ErrConflict on a duplicate ID.
	Create(ctx context.Context, s *UserSession) error
	// Get returns the session by ID (the token hash); ErrNotFound if
	// unknown.
	Get(ctx context.Context, id string) (*UserSession, error)
	// Touch sets LastSeenAt.
	Touch(ctx context.Context, id string, lastSeen time.Time) error
	// Revoke stamps RevokedAt (idempotent). ErrNotFound if unknown.
	Revoke(ctx context.Context, id string) error
	// RevokeAllForUser revokes every live session of userID except
	// exceptID ("" revokes all), returning how many it revoked.
	RevokeAllForUser(ctx context.Context, userID, exceptID string) (int, error)
}

// AuthAuditListOptions filters and pages AuthAuditStore.List.
type AuthAuditListOptions struct {
	Limit        int // <= 0 means 50
	Offset       int
	Action       string // optional exact match
	TargetUserID string // optional exact match
}

// AuthAuditStore is the append-only authentication audit trail.
type AuthAuditStore interface {
	// Append records e, setting ID and At when At is zero.
	Append(ctx context.Context, e *AuthAuditEntry) error
	// List returns the tenant's entries newest first, plus the total
	// number of matching entries ignoring Limit/Offset.
	List(ctx context.Context, tenantID string, opts AuthAuditListOptions) ([]*AuthAuditEntry, int, error)
}

// ToolCacheStore manages CachedTool rows.
type ToolCacheStore interface {
	Upsert(ctx context.Context, tools ...*CachedTool) error
	ListByConnector(ctx context.Context, tenantID, connectorID string) ([]*CachedTool, error)
	ListByTenant(ctx context.Context, tenantID string) ([]*CachedTool, error)
	// Search full-text/substring-searches the tenant's cached tools.
	// includeStale false (the common case) excludes rows with is_stale
	// true; true includes them.
	Search(ctx context.Context, tenantID, query string, limit int, includeStale bool) ([]*CachedTool, error)
	MarkStale(ctx context.Context, connectorID string) error
	DeleteByConnector(ctx context.Context, connectorID string) error
	DeleteExpired(ctx context.Context) (int64, error)
}

// Store aggregates every sub-interface above plus lifecycle methods.
//
// It aggregates them as accessor methods (Tenants(), APIKeys(), ...) rather
// than by embedding the sub-interfaces directly: several of them
// deliberately reuse short method names (Create, Get, List, Update, ...)
// for readability in isolation, which would collide if flattened into one
// interface (Go has no method overloading, and embedding two interfaces
// with same-named-but-different-signature methods is a compile error).
// Backends implement each facet on its own type; see
// internal/store/postgres for the reference shape.
type Store interface {
	Tenants() TenantStore
	APIKeys() APIKeyStore
	Credentials() CredentialStore
	Connectors() ConnectorStore
	MCPCatalog() MCPCatalogStore
	AgentProfiles() AgentProfileStore
	ToolCache() ToolCacheStore
	Models() ModelStore
	ModelCatalog() ModelCatalogStore
	Skills() SkillStore
	Users() UserStore
	UserSessions() UserSessionStore
	AuthAudit() AuthAuditStore

	// Migrate brings the backend's schema up to date. It must be safe to
	// call repeatedly (a no-op once already current).
	Migrate(ctx context.Context) error
	// Ping verifies connectivity to the backend.
	Ping(ctx context.Context) error
	// Close releases any resources (connection pools, ...). Store is not
	// usable after Close.
	Close() error
}

// ---------------------------------------------------------------------------
// Driver registry
// ---------------------------------------------------------------------------

// Ctor builds a Store from a config.Database. Backends register one via
// Register, typically from an init() so importing the backend package for
// its side effect is enough to make its driver name usable with Open.
type Ctor func(ctx context.Context, cfg config.Database) (Store, error)

var (
	registryMu sync.RWMutex
	registry   = map[string]Ctor{}
)

// Register makes a backend's constructor available under driver for Open.
// It panics on a nil ctor or a duplicate driver name — both are
// programmer errors caught at init time, not runtime conditions to
// recover from.
func Register(driver string, ctor Ctor) {
	if ctor == nil {
		panic("store: Register called with a nil ctor for driver " + driver)
	}

	registryMu.Lock()
	defer registryMu.Unlock()
	if _, exists := registry[driver]; exists {
		panic("store: Register called twice for driver " + driver)
	}
	registry[driver] = ctor
}

// Open builds a Store using the backend registered under driver. cfg must
// be a config.Database; it is typed any here so this package's public API
// doesn't otherwise depend on how a given backend wants its config shaped,
// even though every backend shipped in this module happens to take the
// same config.Database today.
//
// Callers must blank-import the backend package (e.g.
// _ "github.com/Tuskira/tusk-ai-secured-gateway/internal/store/postgres")
// so its init() registers the driver before Open is called.
func Open(ctx context.Context, driver string, cfg any) (Store, error) {
	registryMu.RLock()
	ctor, ok := registry[driver]
	registryMu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("store: unknown driver %q (missing blank import?)", driver)
	}

	dbCfg, ok := cfg.(config.Database)
	if !ok {
		return nil, fmt.Errorf("store: driver %q requires a config.Database, got %T", driver, cfg)
	}

	s, err := ctor(ctx, dbCfg)
	if err != nil {
		return nil, fmt.Errorf("store: open driver %q: %w", driver, err)
	}
	return s, nil
}
