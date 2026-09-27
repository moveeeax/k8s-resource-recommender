#!/usr/bin/env python3
"""A stand-in Prometheus for the demo: answers /api/v1/query from fixtures.

It looks at which metric the PromQL expression names and replies with the
matching recorded response, so `examples/demo.sh` can show a real run of
k8s-resource-recommender without a cluster.
"""

import json
import sys
from http.server import BaseHTTPRequestHandler, HTTPServer
from pathlib import Path
from urllib.parse import parse_qs, urlparse

FIXTURES = Path(__file__).parent / "fixtures"


def load(name: str) -> bytes:
    return (FIXTURES / name).read_bytes()


class Handler(BaseHTTPRequestHandler):
    def _query(self) -> str:
        parsed = urlparse(self.path)
        params = parse_qs(parsed.query)
        if self.command == "POST":
            length = int(self.headers.get("Content-Length") or 0)
            body = self.rfile.read(length).decode()
            params.update(parse_qs(body))
        return (params.get("query") or [""])[0]

    def _respond(self) -> None:
        if urlparse(self.path).path != "/api/v1/query":
            self.send_error(404, "only /api/v1/query is implemented")
            return

        query = self._query()
        if "container_cpu_usage_seconds_total" in query:
            payload = load("cpu.json")
        elif "container_memory_working_set_bytes" in query:
            payload = load("memory.json")
        else:
            payload = json.dumps(
                {"status": "error", "errorType": "bad_data", "error": f"unexpected query: {query}"}
            ).encode()

        self.send_response(200)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(payload)))
        self.end_headers()
        self.wfile.write(payload)

    do_GET = _respond
    do_POST = _respond

    def log_message(self, *_args) -> None:
        pass  # keep the demo output clean


if __name__ == "__main__":
    port = int(sys.argv[1]) if len(sys.argv) > 1 else 9090
    HTTPServer(("127.0.0.1", port), Handler).serve_forever()
