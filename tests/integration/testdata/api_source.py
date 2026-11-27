# Copyright 2026 The nbsandbox Authors
# SPDX-License-Identifier: Apache-2.0
"""A test API for HTTP connections (connections_http_test.go): checks each
kind of credential, streams server-sent events, echoes what it got, and has
a path outside every connection's URL that must never be reached."""

import base64
import json
import os
import ssl
import threading
import time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

TOKEN = "api-SECRET-tok-42"
BASIC = "Basic " + base64.b64encode(b"ann:basic-SECRET-pw").decode()
CLIENT_SECRET = "oauth-SECRET-cs"
issued = set()
admin_hits = []


class API(BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"

    def log_message(self, *a):
        pass

    def send(self, status, obj, ctype="application/json"):
        body = (json.dumps(obj) if ctype == "application/json" else obj).encode()
        self.send_response(status)
        self.send_header("Content-Type", ctype)
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def body(self):
        return self.rfile.read(int(self.headers.get("Content-Length") or 0))

    def authorized(self):
        a = self.headers.get("Authorization", "")
        p = self.path
        if p.startswith("/token-api/"):
            return a == "Bearer " + TOKEN
        if p.startswith("/basic-api/"):
            return a == BASIC
        if p.startswith("/oauth-api/"):
            return a.removeprefix("Bearer ") in issued
        if p.startswith("/open-api/"):
            return a == ""
        if p.startswith("/keycloak-api/"):
            # A token Keycloak issued to reader-app (client credentials). The
            # signature isn't checked here: what's tested is that the fetcher
            # got one from Keycloak and sent it.
            try:
                claims = json.loads(base64.urlsafe_b64decode(a.removeprefix("Bearer ").split(".")[1] + "=="))
            except (ValueError, IndexError):
                return False
            return claims.get("azp") == "reader-app" and claims.get("iss", "").endswith("/realms/nbsandbox")
        return False

    def handle_any(self):
        data = self.body()
        if self.path == "/oauth/token":
            form = dict(x.split("=", 1) for x in data.decode().split("&") if "=" in x)
            if form.get("client_id") != "nb-client" or form.get("client_secret") != CLIENT_SECRET:
                return self.send(401, {"error": "invalid_client"})
            tok = f"issued-{len(issued)}-{time.time_ns()}"
            issued.add(tok)
            return self.send(200, {"access_token": tok, "token_type": "Bearer", "expires_in": 600})
        if self.path == "/admin-hits":
            return self.send(200, {"hits": admin_hits})
        if self.path.startswith("/admin"):
            admin_hits.append(self.path)
            return self.send(200, {"admin": True})
        if not self.authorized():
            return self.send(401, {"error": "unauthorized", "path": self.path})
        if self.path.endswith("/events"):
            self.send_response(200)
            self.send_header("Content-Type", "text/event-stream")
            self.send_header("Transfer-Encoding", "chunked")
            self.end_headers()
            for i in range(5):
                chunk = f"id: {i}\ndata: {{\"n\": {i}}}\n\n".encode()
                self.wfile.write(f"{len(chunk):x}\r\n".encode() + chunk + b"\r\n")
                self.wfile.flush()
                time.sleep(0.1)
            self.wfile.write(b"0\r\n\r\n")
            return
        if "/orders" in self.path:
            return self.send(200, {"orders": [{"id": i, "region": "eu" if i % 2 == 0 else "us"} for i in range(1, 11)]})
        self.send(200, {"method": self.command, "path": self.path, "bytes": len(data),
                        "headers": {k.lower(): v for k, v in self.headers.items() if k.lower() != "authorization"}})

    do_GET = do_POST = do_PUT = do_DELETE = do_PATCH = handle_any


if os.environ.get("TLS_CERT"):  # also HTTPS on 8443, with the test authority's certificate
    os.makedirs("/tmp/tls", exist_ok=True)
    for name in ("cert", "key"):
        with open(f"/tmp/tls/{name}.pem", "w") as f:
            f.write(os.environ["TLS_" + name.upper()])
    secure = ThreadingHTTPServer(("0.0.0.0", 8443), API)
    ctx = ssl.SSLContext(ssl.PROTOCOL_TLS_SERVER)
    ctx.load_cert_chain("/tmp/tls/cert.pem", "/tmp/tls/key.pem")
    secure.socket = ctx.wrap_socket(secure.socket, server_side=True)
    threading.Thread(target=secure.serve_forever, daemon=True).start()
ThreadingHTTPServer(("0.0.0.0", 8080), API).serve_forever()
