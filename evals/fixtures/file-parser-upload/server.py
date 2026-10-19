import json
from http.server import BaseHTTPRequestHandler, HTTPServer

from importer.records import parse_records

MAX_BODY = 1 << 20


def dispatch(method, target, headers, body=b""):
    if method == "GET" and target == "/health":
        return 200, {"status": "ok"}
    if method == "POST" and target == "/import":
        try:
            records = parse_records(body)
        except ValueError as exc:
            return 400, {"error": str(exc)}
        return 200, {"records": len(records), "bytes": sum(len(r) for r in records)}
    return 404, {"error": "not found"}


class Handler(BaseHTTPRequestHandler):
    def do_GET(self):
        self.respond()

    def do_POST(self):
        self.respond()

    def respond(self):
        length = int(self.headers.get("Content-Length", "0"))
        if length > MAX_BODY:
            self.send_error(413)
            return
        body = self.rfile.read(length) if length else b""
        status, result = dispatch(self.command, self.path, self.headers, body)
        data = json.dumps(result).encode("utf-8")
        self.send_response(status)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(data)))
        self.end_headers()
        self.wfile.write(data)


if __name__ == "__main__":
    HTTPServer(("localhost", 8080), Handler).serve_forever()
