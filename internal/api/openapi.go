package api

import (
	"encoding/json"
	"sort"
	"strings"
)

// buildOpenAPI renders routes as a minimal OpenAPI 3.0 document. It is the
// only place that shape gets built, so GET /api/v1/openapi.json always
// describes exactly the routes buildRoutes mounted -- see
// TestOpenAPI_CoversEveryRoute (router_test.go), which walks the live chi
// tree and asserts every (method, path) pair it finds is a key in the
// returned document's paths.
func buildOpenAPI(routes []routeSpec, version string) []byte {
	paths := map[string]map[string]any{}

	for _, rt := range routes {
		fullPath := "/api/v1" + rt.Pattern
		ops, ok := paths[fullPath]
		if !ok {
			ops = map[string]any{}
			paths[fullPath] = ops
		}

		op := map[string]any{
			"summary":   rt.Summary,
			"tags":      []string{rt.Tag},
			"responses": operationResponses(rt),
		}
		if !rt.Public {
			op["security"] = []map[string][]string{{"bearerAuth": {}}, {"cookieAuth": {}}}
		}
		if rt.Permission != "" {
			op["x-permission"] = rt.Permission
		}
		if params := pathParams(rt.Pattern); len(params) > 0 {
			paramList := make([]map[string]any, 0, len(params))
			for _, name := range params {
				paramList = append(paramList, map[string]any{
					"name": name, "in": "path", "required": true,
					"schema": map[string]any{"type": "string"},
				})
			}
			op["parameters"] = paramList
		}

		ops[strings.ToLower(rt.Method)] = op
	}

	doc := map[string]any{
		"openapi": "3.0.3",
		"info": map[string]any{
			"title":   "Tusk AI Secured Gateway - Control Plane API",
			"version": version,
		},
		"servers": []map[string]string{{"url": "/api/v1"}},
		"paths":   paths,
		"components": map[string]any{
			"securitySchemes": map[string]any{
				"bearerAuth": map[string]any{
					"type":        "http",
					"scheme":      "bearer",
					"description": "A gateway API key (gk_...), as \"Authorization: Bearer gk_...\" or \"X-Gateway-Key: gk_...\".",
				},
				"cookieAuth": map[string]any{
					"type":        "apiKey",
					"in":          "cookie",
					"name":        "gw_session",
					"description": "The console session cookie set by POST /auth/login. State-changing requests must also send the session's CSRF token in X-CSRF-Token.",
				},
			},
		},
	}

	out, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		// doc is built entirely from static, marshalable types above; a
		// failure here would be a programmer error, not a runtime
		// condition.
		panic("api: marshal openapi document: " + err.Error())
	}
	return out
}

// operationResponses returns a minimal responses object: the route's
// primary status code plus whichever of 400/401/403/404/500 apply. It's
// deliberately coarse -- this hand-written spec favors "every route is
// documented, every status code it can actually return appears" over
// exhaustive per-route response body schemas.
func operationResponses(rt routeSpec) map[string]any {
	success := "200"
	switch {
	case rt.Method == "DELETE":
		success = "204"
	case rt.Method == "POST" && strings.HasSuffix(rt.Pattern, "/rotate"):
		success = "200" // an action, not a resource creation: 200 with the new key
	case rt.Method == "POST" && (rt.Pattern == "/connectors/{id}/discover" || strings.HasPrefix(rt.Pattern, "/cache/refresh")):
		success = "200" // an action returning a body, not a resource creation
	case rt.Method == "POST":
		success = "201"
	}

	resp := map[string]any{
		success: map[string]any{"description": "OK"},
	}
	if !rt.Public {
		resp["401"] = map[string]any{"description": "authentication required"}
		resp["403"] = map[string]any{"description": "insufficient permissions"}
	}
	if strings.Contains(rt.Pattern, "{") {
		resp["404"] = map[string]any{"description": "not found"}
	}
	if rt.Method == "POST" || rt.Method == "PUT" || rt.Method == "PATCH" {
		resp["400"] = map[string]any{"description": "validation error"}
	}
	if isOpsBackedRoute(rt.Pattern) {
		resp["503"] = map[string]any{"description": "MCP data plane not enabled"}
	}
	resp["500"] = map[string]any{"description": "internal error"}
	return resp
}

// isOpsBackedRoute reports whether pattern is served by ConnectorOps or
// CacheOps (see handlers.Deps), and so can answer 503 when the MCP data
// plane is not enabled on this instance.
func isOpsBackedRoute(pattern string) bool {
	switch {
	case pattern == "/connectors/{id}/health", pattern == "/connectors/{id}/discover":
		return true
	case pattern == "/cache" || strings.HasPrefix(pattern, "/cache/"):
		return true
	default:
		return false
	}
}

// pathParams extracts every "{name}" segment from a chi pattern, in
// order.
func pathParams(pattern string) []string {
	var out []string
	for _, seg := range strings.Split(pattern, "/") {
		if strings.HasPrefix(seg, "{") && strings.HasSuffix(seg, "}") {
			out = append(out, strings.Trim(seg, "{}"))
		}
	}
	sort.Strings(out) // deterministic order; these routes have at most one param today
	return out
}
