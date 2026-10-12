import json
import os
from http.server import BaseHTTPRequestHandler, HTTPServer


class Handler(BaseHTTPRequestHandler):
    def do_GET(self):
        body = json.dumps({"app": "python-http", "path": self.path}).encode()
        self.send_response(200)
        self.send_header("content-type", "application/json")
        self.end_headers()
        self.wfile.write(body)


port = int(os.environ.get("PORT", "8080"))
print(f"listening on {port}", flush=True)
HTTPServer(("0.0.0.0", port), Handler).serve_forever()
