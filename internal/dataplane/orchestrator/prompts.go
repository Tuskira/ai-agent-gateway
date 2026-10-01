package orchestrator

import (
	"context"
	"errors"
	"sort"
	"strings"
	"sync"

	"github.com/Tuskira/tusk-ai-secured-gateway/internal/dataplane/client"
	"github.com/Tuskira/tusk-ai-secured-gateway/internal/dataplane/profile"
	"github.com/Tuskira/tusk-ai-secured-gateway/internal/dataplane/reqctx"
	"github.com/Tuskira/tusk-ai-secured-gateway/internal/dataplane/router"
	"github.com/Tuskira/tusk-ai-secured-gateway/internal/dataplane/skillsext"
	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/mcp"
	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/store"
)

// Prompts and resources are passed through from the backends with the
// same namespacing idea as tools -- a prompt is "<connector>__<prompt>",
// a resource URI is "gw://<connector>/<backend uri>" -- and with three
// deliberate simplifications against the tools path:
//
//   - no cache: both families are fetched live on every request. They
//     are cheap and rarely called, and a stale prompt is worse than a
//     slow one;
//   - no gateway pagination: every */list follows each backend's
//     nextCursor to the end and answers with one merged page. An inbound
//     cursor is ignored;
//   - no per-item grants: a connector's prompts and resources are
//     visible exactly when the caller's profile grants at least one of
//     that connector's tools (profile.AllowList.AllowsConnector).

// Capability keys as recorded on store.Connector.Capabilities by the
// handshake.
const (
	capabilityPrompts   = "prompts"
	capabilityResources = "resources"
)

// ---------------------------------------------------------------------------
// */list
// ---------------------------------------------------------------------------

func (o *Orchestrator) handlePromptsList(ctx context.Context, in Request) Result {
	// Resolved separately from fanOutCatalog's own internal resolution
	// (cheap: it shares the Enforcer's 30s cache) purely to merge in the
	// profile's native commands, which have no connector to fan out to.
	allow, mcpErr := o.resolveProfile(ctx, in)
	if mcpErr != nil {
		return o.fail(in, mcpErr)
	}

	var (
		mu      sync.Mutex
		prompts = []mcp.Prompt{}
	)
	if r, failed := o.fanOutCatalog(ctx, in, capabilityPrompts, mcp.MethodPromptsList,
		func(ctx context.Context, call client.Call) (*client.Result, error) {
			got, result, err := o.deps.Client.ListPrompts(ctx, call)
			if err != nil {
				return result, err
			}
			mu.Lock()
			prompts = append(prompts, got...)
			mu.Unlock()
			return result, nil
		}); failed {
		return r
	}

	if allow != nil && allow.Found {
		prompts = append(prompts, nativeCommandPrompts(allow.Commands)...)
	}

	sort.Slice(prompts, func(i, j int) bool { return prompts[i].Name < prompts[j].Name })
	return Result{Response: mcp.NewSuccessResponse(in.JSONRPC.ID, mcp.PromptsListResult{Prompts: prompts})}
}

func (o *Orchestrator) handleResourcesList(ctx context.Context, in Request) Result {
	var (
		mu        sync.Mutex
		resources = []mcp.Resource{}
	)
	if r, failed := o.fanOutCatalog(ctx, in, capabilityResources, mcp.MethodResourcesList,
		func(ctx context.Context, call client.Call) (*client.Result, error) {
			got, result, err := o.deps.Client.ListResources(ctx, call)
			if err != nil {
				return result, err
			}
			mu.Lock()
			resources = append(resources, got...)
			mu.Unlock()
			return result, nil
		}); failed {
		return r
	}

	sort.Slice(resources, func(i, j int) bool { return resources[i].URI < resources[j].URI })
	return Result{Response: mcp.NewSuccessResponse(in.JSONRPC.ID, mcp.ResourcesListResult{Resources: resources})}
}

func (o *Orchestrator) handleResourceTemplatesList(ctx context.Context, in Request) Result {
	var (
		mu        sync.Mutex
		templates = []mcp.ResourceTemplate{}
	)
	if r, failed := o.fanOutCatalog(ctx, in, capabilityResources, mcp.MethodResourcesTemplatesList,
		func(ctx context.Context, call client.Call) (*client.Result, error) {
			got, result, err := o.deps.Client.ListResourceTemplates(ctx, call)
			if err != nil {
				return result, err
			}
			mu.Lock()
			templates = append(templates, got...)
			mu.Unlock()
			return result, nil
		}); failed {
		return r
	}

	sort.Slice(templates, func(i, j int) bool { return templates[i].URITemplate < templates[j].URITemplate })
	return Result{Response: mcp.NewSuccessResponse(in.JSONRPC.ID, mcp.ResourceTemplatesListResult{ResourceTemplates: templates})}
}

// fanOutCatalog runs fetch concurrently against every connector the
// caller may see: the same callable set tools/list fans out to (so an
// unhealthy connector inside its cool-down is skipped), narrowed to the
// connectors the caller's profile grants at least one tool on.
//
// As with tools/list, a connector that fails is logged and skipped
// rather than failing the whole list, and a list failure does not mark
// it unhealthy. A connector whose handshake recorded that it lacks the
// capability is skipped without a call. The returned Result is set, and
// failed true, only when the request itself must fail.
func (o *Orchestrator) fanOutCatalog(ctx context.Context, in Request, capability, method string,
	fetch func(context.Context, client.Call) (*client.Result, error)) (Result, bool) {

	allow, mcpErr := o.resolveProfile(ctx, in)
	if mcpErr != nil {
		return o.fail(in, mcpErr), true
	}

	connectors, err := o.deps.Router.Callable(ctx, in.Principal.TenantID)
	if err != nil {
		o.log.Error("failed to list connectors", "method", method, "tenant_id", in.Principal.TenantID, "error", err)
		return o.fail(in, mcp.NewInternalError("failed to list connectors")), true
	}

	var wg sync.WaitGroup
	for _, conn := range connectors {
		if allow != nil && !allow.AllowsConnector(conn.ID) {
			continue
		}

		wg.Add(1)
		go func(conn *store.Connector) {
			defer wg.Done()

			backend, err := o.ensureBackend(ctx, in, conn)
			if err != nil {
				o.log.Warn("skipping connector", "method", method, "connector_id", conn.ID, "error", err)
				return
			}
			if has, recorded := conn.Capabilities[capability].(bool); recorded && !has {
				return
			}

			result, err := fetch(ctx, client.Call{
				Connector:       conn,
				SessionID:       backend.SessionID,
				ProtocolVersion: backend.ProtocolVersion,
				Inbound:         in.Inbound,
				Trace:           in.Trace,
			})
			if err != nil {
				o.log.Warn("list failed for connector", "method", method, "connector_id", conn.ID, "error", err)
				return
			}
			o.rememberBackend(in, conn.ID, result)
		}(conn)
	}
	wg.Wait()

	return Result{}, false
}

// ---------------------------------------------------------------------------
// prompts/get, resources/read
// ---------------------------------------------------------------------------

func (o *Orchestrator) handlePromptsGet(ctx context.Context, in Request) Result {
	var params mcp.PromptsGetParams
	if err := decodeParams(in.JSONRPC.Params, &params); err != nil {
		return o.fail(in, mcp.NewInvalidParamsError(err.Error()))
	}
	if params.Name == "" {
		return o.fail(in, mcp.NewInvalidParamsError(`"name" is required`))
	}
	reqctx.From(ctx).SetTool(params.Name)

	// A name with no "__" is a native command, never a connector's --
	// intercepted here, before router.ParsePromptName (which would
	// otherwise reject it as malformed), and answered entirely from the
	// resolved profile.
	if !strings.Contains(params.Name, "__") {
		return o.handleNativePromptGet(ctx, in, params)
	}

	slug, name, err := router.ParsePromptName(params.Name)
	if err != nil {
		return o.fail(in, routingError(err))
	}
	conn, probe, r, failed := o.routeCatalog(ctx, in, slug, "prompt", params.Name)
	if failed {
		return r
	}

	backend, err := o.ensureBackend(ctx, in, conn)
	if err != nil {
		o.log.Warn("failed to prepare backend session", "connector_id", conn.ID, "error", err)
		return o.fail(in, mcp.NewError(mcp.ErrorCodeToolExecution, "connector handshake failed: "+err.Error(), nil))
	}

	out, result, err := o.deps.Client.GetPrompt(ctx, client.Call{
		Connector:       conn,
		SessionID:       backend.SessionID,
		ProtocolVersion: backend.ProtocolVersion,
		Inbound:         in.Inbound,
		Trace:           in.Trace,
	}, name, params.Arguments)
	if err != nil {
		return o.catalogFailure(ctx, in, conn, probe, mcp.MethodPromptsGet, params.Name, err)
	}

	o.rememberBackend(in, conn.ID, result)
	o.markRecovered(ctx, conn, probe)
	return Result{Response: mcp.NewSuccessResponse(in.JSONRPC.ID, out), ConnectorID: conn.ID}
}

func (o *Orchestrator) handleResourcesRead(ctx context.Context, in Request) Result {
	var params mcp.ResourcesReadParams
	if err := decodeParams(in.JSONRPC.Params, &params); err != nil {
		return o.fail(in, mcp.NewInvalidParamsError(err.Error()))
	}
	if params.URI == "" {
		return o.fail(in, mcp.NewInvalidParamsError(`"uri" is required`))
	}
	reqctx.From(ctx).SetTool(params.URI)

	// A "skill://" URI is never a connector resource: it names a file of
	// a skill attached to the caller's agent profile, served by the
	// gateway itself under the MCP Skills Extension (SEP-2640; see
	// skills.go). Route it there before the "gw://<connector>/" parsing
	// below, which would otherwise reject it as a malformed gateway URI.
	if strings.HasPrefix(params.URI, skillsext.Scheme) {
		return o.handleSkillResourceRead(ctx, in, params.URI)
	}

	slug, uri, err := router.ParseResourceURI(params.URI)
	if err != nil {
		return o.fail(in, routingError(err))
	}
	conn, probe, r, failed := o.routeCatalog(ctx, in, slug, "resource", params.URI)
	if failed {
		return r
	}

	backend, err := o.ensureBackend(ctx, in, conn)
	if err != nil {
		o.log.Warn("failed to prepare backend session", "connector_id", conn.ID, "error", err)
		return o.fail(in, mcp.NewError(mcp.ErrorCodeToolExecution, "connector handshake failed: "+err.Error(), nil))
	}

	out, result, err := o.deps.Client.ReadResource(ctx, client.Call{
		Connector:       conn,
		SessionID:       backend.SessionID,
		ProtocolVersion: backend.ProtocolVersion,
		Inbound:         in.Inbound,
		Trace:           in.Trace,
	}, uri)
	if err != nil {
		return o.catalogFailure(ctx, in, conn, probe, mcp.MethodResourcesRead, params.URI, err)
	}

	o.rememberBackend(in, conn.ID, result)
	o.markRecovered(ctx, conn, probe)
	return Result{Response: mcp.NewSuccessResponse(in.JSONRPC.ID, out), ConnectorID: conn.ID}
}

// routeCatalog resolves the caller's profile and the connector a prompt
// or resource names, and applies the profile gate: the connector must be
// one the profile grants at least one tool on. kind ("prompt" or
// "resource") and qualified only shape the denial.
func (o *Orchestrator) routeCatalog(ctx context.Context, in Request, slug, kind, qualified string) (*store.Connector, bool, Result, bool) {
	allow, mcpErr := o.resolveProfile(ctx, in)
	if mcpErr != nil {
		return nil, false, o.fail(in, mcpErr), true
	}

	conn, probe, err := o.deps.Router.ResolveConnector(ctx, in.Principal.TenantID, slug)
	if err != nil {
		return nil, false, o.fail(in, routingError(err)), true
	}
	reqctx.From(ctx).SetConnector(conn.ID)

	if allow != nil && !allow.AllowsConnector(conn.ID) {
		o.log.Info(kind+" denied by agent profile",
			"profile", allow.Name, "profile_found", allow.Found,
			kind, qualified, "connector_id", conn.ID)
		return nil, false, o.fail(in, catalogDenied(allow, kind, qualified, slug)), true
	}
	return conn, probe, Result{}, false
}

func catalogDenied(allow *profile.AllowList, kind, qualified, slug string) *mcp.Error {
	return mcp.NewError(mcp.ErrorCodeToolNotAllowed,
		kind+" not allowed by profile: the profile grants no tool on connector \""+slug+"\"",
		map[string]any{
			kind:      qualified,
			"profile": allow.Name,
		})
}

// catalogFailure turns a failed prompts/get or resources/read into the
// caller's answer.
//
// A JSON-RPC error the backend itself answered with (an unknown prompt,
// a missing resource) is forwarded unchanged: the backend is up, the
// caller asked for something it does not have, and its code -- e.g. the
// spec's -32002 for a missing resource -- is the useful answer. Anything
// else is handled as tools/call handles it, except that a timeout is a
// protocol error here: there is no model-readable content to put it in.
func (o *Orchestrator) catalogFailure(ctx context.Context, in Request, conn *store.Connector, probe bool, method, qualified string, err error) Result {
	if rpcErr, ok := client.AsRPCError(err); ok {
		o.markRecovered(ctx, conn, probe)
		return Result{Response: mcp.NewErrorResponse(in.JSONRPC.ID, rpcErr.Err), ConnectorID: conn.ID}
	}

	o.forgetBackend(in, conn.ID)

	var msg string
	switch {
	case client.IsTimeout(err):
		o.log.Warn(method+" timed out", "target", qualified, "connector_id", conn.ID, "error", err)
		msg = method + " timed out"
	case errors.Is(err, context.Canceled):
		o.log.Debug(method+" cancelled by the caller", "target", qualified, "connector_id", conn.ID)
		msg = "request cancelled"
	default:
		o.log.Error(method+" failed", "target", qualified, "connector_id", conn.ID, "error", err)
		if err := o.deps.Router.MarkUnhealthy(ctx, conn); err != nil {
			o.log.Warn("failed to persist connector health", "connector_id", conn.ID, "error", err)
		}
		msg = method + " failed: " + err.Error()
	}
	return Result{
		Response:    mcp.NewErrorResponse(in.JSONRPC.ID, mcp.NewError(mcp.ErrorCodeToolExecution, msg, nil)),
		ConnectorID: conn.ID,
	}
}
