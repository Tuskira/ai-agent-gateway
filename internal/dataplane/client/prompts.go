package client

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/mcp"
)

// ResourceURIScheme is the scheme of every resource URI the gateway
// advertises: "gw://<connector qualifier>/<backend's own uri>".
//
// A backend's resource URIs are opaque and already carry a scheme of
// their own (file://, test://, postgres://, ...), so they cannot be
// prefixed the way tool names are without risking a collision with a
// URI a backend really uses. Wrapping the whole URI behind a gateway
// scheme keeps it intact: stripping "gw://<qualifier>/" gives back the
// exact bytes the backend advertised.
const ResourceURIScheme = "gw://"

// maxListPages bounds how many pages one */list follows on one backend.
// It is a guard against a backend that keeps handing back a cursor, not
// a limit anyone should reach.
const maxListPages = 100

// RPCError is a JSON-RPC error the backend itself answered with: the
// backend is up and understood the request, it just refused it (an
// unknown prompt, a resource that does not exist, a bad argument). The
// orchestrator forwards it to the caller as-is rather than treating it
// as a connector failure.
type RPCError struct {
	Method string
	Err    *mcp.Error
}

func (e *RPCError) Error() string {
	return fmt.Sprintf("client: %s failed: %s", e.Method, e.Err.Message)
}

// AsRPCError reports whether err is a backend-answered JSON-RPC error.
func AsRPCError(err error) (*RPCError, bool) {
	var rpcErr *RPCError
	ok := errors.As(err, &rpcErr)
	return rpcErr, ok
}

// QualifyResourceURI renders the gateway-visible URI of a backend
// resource (or resource template).
func QualifyResourceURI(qualifier, uri string) string {
	return ResourceURIScheme + qualifier + "/" + uri
}

// ListPrompts fetches every prompt a backend offers, following its
// nextCursor to the last page, with each name qualified exactly as a
// tool name is: "<connector>__<prompt>".
func (c *Client) ListPrompts(ctx context.Context, call Call) ([]mcp.Prompt, *Result, error) {
	prompts := []mcp.Prompt{}
	result, err := c.listAll(ctx, call, mcp.MethodPromptsList, func(raw json.RawMessage) (string, error) {
		var page mcp.PromptsListResult
		if err := json.Unmarshal(raw, &page); err != nil {
			return "", err
		}
		for _, p := range page.Prompts {
			p.Name = QualifyTool(Qualifier(call.Connector), p.Name)
			prompts = append(prompts, p)
		}
		return page.NextCursor, nil
	})
	if err != nil {
		return nil, result, err
	}
	return prompts, result, nil
}

// ListResources fetches every resource a backend offers, following its
// nextCursor to the last page, with each URI wrapped in the gateway's
// namespace (see ResourceURIScheme).
func (c *Client) ListResources(ctx context.Context, call Call) ([]mcp.Resource, *Result, error) {
	resources := []mcp.Resource{}
	result, err := c.listAll(ctx, call, mcp.MethodResourcesList, func(raw json.RawMessage) (string, error) {
		var page mcp.ResourcesListResult
		if err := json.Unmarshal(raw, &page); err != nil {
			return "", err
		}
		for _, r := range page.Resources {
			r.URI = QualifyResourceURI(Qualifier(call.Connector), r.URI)
			resources = append(resources, r)
		}
		return page.NextCursor, nil
	})
	if err != nil {
		return nil, result, err
	}
	return resources, result, nil
}

// ListResourceTemplates fetches every resource template a backend
// offers, following its nextCursor to the last page, with each
// uriTemplate wrapped in the gateway's namespace. A client that expands
// the template gets a URI resources/read routes back to this backend.
func (c *Client) ListResourceTemplates(ctx context.Context, call Call) ([]mcp.ResourceTemplate, *Result, error) {
	templates := []mcp.ResourceTemplate{}
	result, err := c.listAll(ctx, call, mcp.MethodResourcesTemplatesList, func(raw json.RawMessage) (string, error) {
		var page mcp.ResourceTemplatesListResult
		if err := json.Unmarshal(raw, &page); err != nil {
			return "", err
		}
		for _, t := range page.ResourceTemplates {
			t.URITemplate = QualifyResourceURI(Qualifier(call.Connector), t.URITemplate)
			templates = append(templates, t)
		}
		return page.NextCursor, nil
	})
	if err != nil {
		return nil, result, err
	}
	return templates, result, nil
}

// GetPrompt renders one prompt on a backend. name is the backend's own,
// unprefixed prompt name; the arguments are forwarded verbatim.
func (c *Client) GetPrompt(ctx context.Context, call Call, name string, args map[string]string) (*mcp.PromptsGetResult, *Result, error) {
	raw, err := json.Marshal(mcp.PromptsGetParams{Name: name, Arguments: args})
	if err != nil {
		return nil, nil, fmt.Errorf("client: marshal prompts/get params: %w", err)
	}
	call.Request = &mcp.Request{
		JSONRPC: mcp.Version,
		ID:      "prompts/get:" + name,
		Method:  mcp.MethodPromptsGet,
		Params:  raw,
	}

	var out mcp.PromptsGetResult
	result, err := c.doDecode(ctx, call, &out)
	if err != nil {
		return nil, result, err
	}
	if out.Messages == nil {
		out.Messages = []mcp.PromptMessage{}
	}
	return &out, result, nil
}

// ReadResource reads one resource on a backend. uri is the backend's own
// URI, with the gateway's "gw://<connector>/" prefix already removed.
func (c *Client) ReadResource(ctx context.Context, call Call, uri string) (*mcp.ResourcesReadResult, *Result, error) {
	raw, err := json.Marshal(mcp.ResourcesReadParams{URI: uri})
	if err != nil {
		return nil, nil, fmt.Errorf("client: marshal resources/read params: %w", err)
	}
	call.Request = &mcp.Request{
		JSONRPC: mcp.Version,
		ID:      "resources/read",
		Method:  mcp.MethodResourcesRead,
		Params:  raw,
	}

	var out mcp.ResourcesReadResult
	result, err := c.doDecode(ctx, call, &out)
	if err != nil {
		return nil, result, err
	}
	if out.Contents == nil {
		out.Contents = []mcp.ResourceContents{}
	}
	return &out, result, nil
}

// doDecode runs call and decodes its result into out. A JSON-RPC error
// from the backend comes back as an *RPCError.
func (c *Client) doDecode(ctx context.Context, call Call, out any) (*Result, error) {
	method := call.Request.Method

	result, err := c.Do(ctx, call)
	if err != nil {
		return result, err
	}
	if result.Response == nil {
		return result, fmt.Errorf("client: %s returned no response", method)
	}
	if result.Response.Error != nil {
		return result, &RPCError{Method: method, Err: result.Response.Error}
	}
	if err := json.Unmarshal(result.Response.Result, out); err != nil {
		return result, fmt.Errorf("client: decode %s result: %w", method, err)
	}
	return result, nil
}

// listAll pages through one */list method. page decodes one result and
// returns its nextCursor. The backend session id the last page returned
// is carried into the next page's request, so a backend that rotates it
// mid-list is followed, and the last page's Result is what is returned.
func (c *Client) listAll(ctx context.Context, call Call, method string, page func(json.RawMessage) (string, error)) (*Result, error) {
	var (
		cursor string
		result *Result
		seen   = map[string]struct{}{}
	)

	for i := 0; i < maxListPages; i++ {
		var params json.RawMessage
		if cursor != "" {
			raw, err := json.Marshal(mcp.PaginatedParams{Cursor: cursor})
			if err != nil {
				return result, fmt.Errorf("client: marshal %s params: %w", method, err)
			}
			params = raw
		}
		call.Request = &mcp.Request{JSONRPC: mcp.Version, ID: method, Method: method, Params: params}

		var raw json.RawMessage
		var err error
		result, err = c.doDecode(ctx, call, &raw)
		if err != nil {
			return result, err
		}
		call.SessionID = result.SessionID
		call.ProtocolVersion = result.ProtocolVersion

		next, err := page(raw)
		if err != nil {
			return result, fmt.Errorf("client: decode %s result: %w", method, err)
		}
		if next == "" {
			return result, nil
		}
		// A cursor seen before is a loop, not another page.
		if _, dup := seen[next]; dup {
			return result, fmt.Errorf("client: %s returned a repeated cursor %q", method, next)
		}
		seen[next] = struct{}{}
		cursor = next
	}

	return result, fmt.Errorf("client: %s did not finish within %d pages", method, maxListPages)
}
