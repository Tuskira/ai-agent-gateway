#!/usr/bin/env python3
"""Header-echo MCP server (Streamable HTTP transport, stdlib only).

Used by 06-credentials-and-headers/run.sh as the backend a connector
points at. It exposes two tools:

  - "headers"    returns the HTTP headers this call arrived with, so
                 run.sh can prove the gateway injected a credential and
                 a static value, and that it did NOT forward the
                 caller's own Authorization header.
  - "echo-args"  returns the tool arguments this call received, so
                 run.sh can prove metadata.tool_arg_overrides overwrote
                 "region" server-side while leaving other arguments
                 (e.g. "q") untouched.

Speaks JSON-RPC 2.0 over MCP's Streamable HTTP transport: POST /mcp for
every request, always answering a plain JSON body (never SSE), and
"Mcp-Session-Id" minted on initialize and accepted (but not required)
afterwards. Protocol version: mirrors whatever the caller requested if
it is one this server knows (2025-06-18 or the legacy 2024-11-05),
otherwise falls back to 2025-06-18 -- so the gateway never needs to
downgrade against this server.
"""
import json
import sys
import uuid
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

KNOWN_PROTOCOL_VERSIONS = {"2025-06-18", "2024-11-05"}

TOOLS = [
    {
        "name": "headers",
        "description": "Return the HTTP headers this MCP server received on this call.",
        "inputSchema": {"type": "object", "properties": {}},
    },
    {
        "name": "echo-args",
        "description": "Return the tool arguments this call received, verbatim.",
        "inputSchema": {
            "type": "object",
            "properties": {
                "region": {"type": "string", "description": "Tenant region (server may override this)."},
                "q": {"type": "string", "description": "Free-form query text."},
            },
        },
    },
]


def tool_result(payload):
    return {"content": [{"type": "text", "text": json.dumps(payload)}], "isError": False}


class Handler(BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"

    def log_message(self, *_args):
        pass  # keep run.sh's output readable; nothing here is a failure signal

    def _send_json(self, code, obj, extra_headers=None):
        body = json.dumps(obj).encode()
        self.send_response(code)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        for k, v in (extra_headers or {}).items():
            self.send_header(k, v)
        self.end_headers()
        self.wfile.write(body)

    def do_GET(self):
        if self.path.startswith("/health"):
            return self._send_json(200, {"status": "ok"})
        # Streamable HTTP allows a GET to open an SSE stream for
        # server-initiated messages; this server never sends any, so it
        # declines rather than hanging the connection open.
        self._send_json(405, {"error": "GET /mcp (server-initiated SSE) is not used by this example"})

    def do_DELETE(self):
        # Session teardown: no per-session state is kept, so there is
        # nothing to release.
        self.send_response(204)
        self.send_header("Content-Length", "0")
        self.end_headers()

    def do_POST(self):
        if not self.path.startswith("/mcp"):
            return self._send_json(404, {"error": "not found"})

        length = int(self.headers.get("Content-Length", 0) or 0)
        raw = self.rfile.read(length) if length else b""
        try:
            req = json.loads(raw or b"{}")
        except json.JSONDecodeError:
            return self._send_json(400, {"jsonrpc": "2.0", "id": None,
                                          "error": {"code": -32700, "message": "parse error"}})

        method = req.get("method")
        has_id = "id" in req
        rid = req.get("id")

        # A JSON-RPC notification (no "id") gets no body at all -- the
        # gateway's own client only checks the HTTP status for one
        # (200/202/204 all count as success; see client.go's exchange()).
        if not has_id:
            self.send_response(202)
            self.send_header("Content-Length", "0")
            self.end_headers()
            return

        extra_headers = {}
        if method == "initialize":
            requested = req.get("params", {}).get("protocolVersion", "2025-06-18")
            version = requested if requested in KNOWN_PROTOCOL_VERSIONS else "2025-06-18"
            result = {
                "protocolVersion": version,
                "capabilities": {"tools": {"listChanged": False}},
                "serverInfo": {"name": "header-echo", "version": "0.1.0"},
            }
            extra_headers["Mcp-Session-Id"] = uuid.uuid4().hex
        elif method == "ping":
            result = {}
        elif method == "tools/list":
            result = {"tools": TOOLS}
        elif method == "tools/call":
            params = req.get("params", {}) or {}
            name = params.get("name")
            args = params.get("arguments") or {}
            if name == "headers":
                result = tool_result(dict(self.headers.items()))
            elif name == "echo-args":
                result = tool_result(args)
            else:
                return self._send_json(200, {"jsonrpc": "2.0", "id": rid,
                                              "error": {"code": -32602, "message": f"unknown tool: {name}"}})
        else:
            return self._send_json(200, {"jsonrpc": "2.0", "id": rid,
                                          "error": {"code": -32601, "message": f"method not found: {method}"}})

        self._send_json(200, {"jsonrpc": "2.0", "id": rid, "result": result}, extra_headers)


def main():
    port = int(sys.argv[1]) if len(sys.argv) > 1 else 23015
    print(f"header-echo MCP server listening on http://0.0.0.0:{port}/mcp", flush=True)
    ThreadingHTTPServer(("0.0.0.0", port), Handler).serve_forever()


if __name__ == "__main__":
    main()
