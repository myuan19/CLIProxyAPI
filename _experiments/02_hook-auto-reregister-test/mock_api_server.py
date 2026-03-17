"""
Mock CLIProxyAPI management server for testing auto-reregister hook.

Simulates the following endpoints:
  DELETE /v0/management/auth-files?name=xxx
  POST   /v0/management/auth-files?name=xxx
  GET    /v0/management/unified-routing/config/routes/:id/pipeline
  PUT    /v0/management/unified-routing/config/routes/:id/pipeline
"""

import json
import sys
import os
from http.server import HTTPServer, BaseHTTPRequestHandler
from urllib.parse import urlparse, parse_qs

CALLS_LOG = []
PIPELINES = {}
AUTH_FILES = set()


class MockHandler(BaseHTTPRequestHandler):
    def log_message(self, format, *args):
        msg = format % args
        print(f"  [mock-api] {msg}")

    def _send_json(self, code, data):
        body = json.dumps(data).encode()
        self.send_response(code)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def _read_body(self):
        length = int(self.headers.get("Content-Length", 0))
        if length > 0:
            return self.rfile.read(length)
        return b""

    def do_DELETE(self):
        parsed = urlparse(self.path)
        qs = parse_qs(parsed.query)

        if parsed.path == "/v0/management/auth-files":
            name = qs.get("name", [""])[0]
            CALLS_LOG.append({"method": "DELETE", "endpoint": "auth-files", "name": name})
            AUTH_FILES.discard(name)
            self._send_json(200, {"ok": True, "deleted": name})
            return

        self.send_error(404)

    def do_POST(self):
        parsed = urlparse(self.path)
        qs = parse_qs(parsed.query)
        body = self._read_body()

        if parsed.path == "/v0/management/auth-files":
            name = qs.get("name", [""])[0]
            try:
                payload = json.loads(body) if body else {}
            except json.JSONDecodeError:
                payload = {}
            CALLS_LOG.append({"method": "POST", "endpoint": "auth-files", "name": name, "body": payload})
            AUTH_FILES.add(name)
            self._send_json(200, {"ok": True, "uploaded": name})
            return

        self.send_error(404)

    def do_GET(self):
        parsed = urlparse(self.path)

        if "/unified-routing/config/routes/" in parsed.path and parsed.path.endswith("/pipeline"):
            parts = parsed.path.split("/")
            route_idx = parts.index("routes") + 1
            route_id = parts[route_idx] if route_idx < len(parts) else "unknown"

            pipeline = PIPELINES.get(route_id, {
                "layers": [{
                    "level": 1,
                    "strategy": "round-robin",
                    "targets": [
                        {
                            "id": "target-existing",
                            "credential_id": "token_existing.json",
                            "model": "gpt-4",
                            "weight": 1,
                            "enabled": True
                        }
                    ]
                }]
            })
            CALLS_LOG.append({"method": "GET", "endpoint": "pipeline", "route_id": route_id})
            self._send_json(200, pipeline)
            return

        # Diagnostic endpoint
        if parsed.path == "/v0/management/_test/calls":
            self._send_json(200, CALLS_LOG)
            return

        self.send_error(404)

    def do_PUT(self):
        parsed = urlparse(self.path)
        body = self._read_body()

        if "/unified-routing/config/routes/" in parsed.path and parsed.path.endswith("/pipeline"):
            parts = parsed.path.split("/")
            route_idx = parts.index("routes") + 1
            route_id = parts[route_idx] if route_idx < len(parts) else "unknown"
            try:
                payload = json.loads(body) if body else {}
            except json.JSONDecodeError:
                payload = {}
            PIPELINES[route_id] = payload
            CALLS_LOG.append({"method": "PUT", "endpoint": "pipeline", "route_id": route_id, "body": payload})
            self._send_json(200, {"ok": True})
            return

        self.send_error(404)


def main():
    port = int(sys.argv[1]) if len(sys.argv) > 1 else 18787
    server = HTTPServer(("127.0.0.1", port), MockHandler)
    print(f"[mock-api] Listening on http://127.0.0.1:{port}")
    sys.stdout.flush()
    try:
        server.serve_forever()
    except KeyboardInterrupt:
        pass
    finally:
        server.server_close()


if __name__ == "__main__":
    main()
