// Package mcp implements the Model Context Protocol's wire types: the
// JSON-RPC 2.0 envelope every MCP message rides in (jsonrpc.go), the
// protocol's method names (methods.go), and the request/result payloads
// for the methods the gateway speaks (types.go).
//
// It is transport-agnostic and dependency-free on purpose: both the
// gateway's inbound listener (internal/dataplane/transport) and its
// outbound backend client (internal/dataplane/client) marshal the same
// types, so a shape can never drift between the two directions.
package mcp
