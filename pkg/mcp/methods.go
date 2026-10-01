package mcp

// MCP method names. The gateway serves initialize, ping, the tools
// family, the prompts family (prompts/list, prompts/get), the resources
// family (resources/list, resources/read, resources/templates/list,
// resources/subscribe, resources/unsubscribe), the skills family
// (skills/list, skills/get -- MCP Skills Extension, SEP-2640) and
// notifications/cancelled inbound; the rest are recognized (so a request
// for one gets a -32601 rather than a parse-level rejection) but not
// served.
const (
	// MethodInitialize opens an MCP session.
	MethodInitialize = "initialize"
	// MethodInitialized is the client's post-initialize notification.
	MethodInitialized = "notifications/initialized"
	// MethodPing is a liveness round-trip.
	MethodPing = "ping"
	// MethodToolsList lists the tools available to the caller.
	MethodToolsList = "tools/list"
	// MethodToolsCall executes one tool.
	MethodToolsCall = "tools/call"

	// MethodResourcesList, MethodResourcesRead,
	// MethodResourcesTemplatesList, MethodResourcesSubscribe and
	// MethodResourcesUnsubscribe are the resources family.
	MethodResourcesList          = "resources/list"
	MethodResourcesRead          = "resources/read"
	MethodResourcesTemplatesList = "resources/templates/list"
	MethodResourcesSubscribe     = "resources/subscribe"
	MethodResourcesUnsubscribe   = "resources/unsubscribe"

	// MethodPromptsList and MethodPromptsGet are the prompts family.
	MethodPromptsList = "prompts/list"
	MethodPromptsGet  = "prompts/get"

	// MethodSkillsList and MethodSkillsGet are the MCP Skills Extension
	// (SEP-2640) family: discovering and re-fetching the skills attached
	// to the caller's agent profile. A skill's file contents (its
	// SKILL.md and any supporting files) are served through the
	// existing MethodResourcesRead, under "skill://<name>/<path>" URIs.
	MethodSkillsList = "skills/list"
	MethodSkillsGet  = "skills/get"

	// MethodCompletionComplete requests an argument completion.
	MethodCompletionComplete = "completion/complete"
	// MethodLoggingSetLevel sets a server's log level.
	MethodLoggingSetLevel = "logging/setLevel"

	// NotificationToolsListChanged tells a connected client that the tool
	// list it last fetched is out of date. The gateway pushes this over
	// GET /mcp/stream when a tenant's tool cache is refreshed.
	NotificationToolsListChanged = "notifications/tools/list_changed"

	// NotificationCancelled asks the receiver to abandon an in-flight
	// request (params: CancelledParams). The gateway honours it for a
	// tools/call in flight on the same session and forwards it to the
	// connector serving that call.
	NotificationCancelled = "notifications/cancelled"
	// NotificationProgress reports progress on a request that carried
	// params._meta.progressToken (params: ProgressParams). The gateway
	// relays a connector's progress for a tools/call to the calling
	// session's GET /mcp/stream.
	NotificationProgress = "notifications/progress"

	// NotificationResourcesUpdated tells a client that a resource it
	// subscribed to changed (params: ResourceUpdatedParams). The gateway
	// relays a connector's, re-namespaced, to the subscribing session.
	NotificationResourcesUpdated = "notifications/resources/updated"
	// NotificationResourcesListChanged and NotificationPromptsListChanged
	// tell a client its resource or prompt list is out of date. The
	// gateway relays a connector's to the sessions with a stream open to
	// that connector.
	NotificationResourcesListChanged = "notifications/resources/list_changed"
	NotificationPromptsListChanged   = "notifications/prompts/list_changed"
)

// IsValidMethod reports whether method is a method name defined by the
// MCP specification (whether or not this gateway serves it).
func IsValidMethod(method string) bool {
	switch method {
	case MethodInitialize, MethodInitialized, MethodPing,
		MethodToolsList, MethodToolsCall,
		MethodResourcesList, MethodResourcesRead, MethodResourcesTemplatesList,
		MethodResourcesSubscribe, MethodResourcesUnsubscribe,
		MethodPromptsList, MethodPromptsGet,
		MethodSkillsList, MethodSkillsGet,
		MethodCompletionComplete, MethodLoggingSetLevel,
		NotificationCancelled, NotificationProgress:
		return true
	default:
		return false
	}
}
