# Copyright 2026 The nbsandbox Authors
# SPDX-License-Identifier: Apache-2.0
"""HTTP APIs through the fetcher (httpapi), against a local gateway (CONNECT
only, checking the run's gateway token) and a local API: what reaches the
API, what the notebook gets back, and what is refused. Runs in the image
(make fetcher-test) or with any Python 3.12+ (no packages needed)."""

import base64
import http.client
import json
import os
import socket
import sys
import threading
import time
import unittest
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

sys.path.insert(0, "/opt/nbsandbox-fetcher")
sys.path.insert(0, os.path.join(os.path.dirname(os.path.abspath(__file__)), ".."))
import httpapi  # noqa: E402

RUN_TOKEN = "run-token-123"
GW_TOKEN = "gw-token"


class FakeAPI(BaseHTTPRequestHandler):
    """Echoes what it got; a few paths behave specially."""

    protocol_version = "HTTP/1.1"
    seen = []
    oauth_issued = []

    def log_message(self, *a):
        pass

    def _send(self, status, body, ctype="application/json", extra=()):
        self.send_response(status)
        self.send_header("Content-Type", ctype)
        self.send_header("Content-Length", str(len(body)))
        for k, v in extra:
            self.send_header(k, v)
        self.end_headers()
        if self.command != "HEAD":
            self.wfile.write(body)

    def _read_body(self):
        if self.headers.get("Transfer-Encoding", "").lower() == "chunked":
            out = b""
            while True:
                n = int(self.rfile.readline().strip(), 16)
                if n == 0:
                    self.rfile.readline()
                    return out
                out += self.rfile.read(n)
                self.rfile.readline()
        return self.rfile.read(int(self.headers.get("Content-Length") or 0))

    def handle_one(self):
        body = self._read_body()
        FakeAPI.seen.append({"method": self.command, "path": self.path, "headers": dict(self.headers), "body": body})
        if self.path == "/oauth/token":
            form = dict(p.split("=", 1) for p in body.decode().split("&"))
            if form.get("client_secret") != "cs":
                return self._send(401, b'{"error":"invalid_client"}')
            tok = f"access-{len(FakeAPI.oauth_issued)}"
            FakeAPI.oauth_issued.append(tok)
            return self._send(200, json.dumps({"access_token": tok, "expires_in": 3600}).encode())
        if self.path.startswith("/v2/needs-fresh"):
            if self.headers.get("Authorization") != "Bearer access-1":
                return self._send(401, b'{"error":"expired"}')
        if self.path.startswith("/v2/sse"):
            self.send_response(200)
            self.send_header("Content-Type", "text/event-stream")
            self.send_header("Transfer-Encoding", "chunked")
            self.end_headers()
            for i in range(3):
                chunk = f"data: {i}\n\n".encode()
                self.wfile.write(f"{len(chunk):x}\r\n".encode() + chunk + b"\r\n")
                self.wfile.flush()
                time.sleep(0.2)
            self.wfile.write(b"0\r\n\r\n")
            return
        if self.path.startswith("/v2/redirect-in"):
            return self._send(302, b"", extra=[("Location", "/v2/target?x=1")])
        if self.path.startswith("/v2/redirect-out"):
            return self._send(302, b"", extra=[("Location", "https://elsewhere.invalid/x")])
        if self.path.startswith("/v2/empty"):
            return self._send(204, b"")
        echo = {"method": self.command, "path": self.path, "auth": self.headers.get("Authorization"),
                "key": self.headers.get("X-API-Key"), "token": self.headers.get("X-Nbsandbox-Token"),
                "host": self.headers.get("Host"), "body": body.decode(errors="replace")}
        self._send(200, json.dumps(echo).encode())

    do_GET = do_POST = do_PUT = do_DELETE = do_HEAD = do_PATCH = handle_one


class FakeGateway(BaseHTTPRequestHandler):
    """CONNECT only, to the fake API's address, with the gateway token."""

    protocol_version = "HTTP/1.1"
    tunnels = []

    def log_message(self, *a):
        pass

    def do_CONNECT(self):
        if self.headers.get("Proxy-Authorization") != "Bearer " + GW_TOKEN:
            self.send_response(407)
            self.send_header("Content-Length", "0")
            self.end_headers()
            return
        host, port = self.path.rsplit(":", 1)
        FakeGateway.tunnels.append(self.path)
        if host == "refused.invalid":
            body = b"not one of the run's endpoints"
            self.send_response(403)
            self.send_header("Content-Length", str(len(body)))
            self.end_headers()
            self.wfile.write(body)
            return
        up = socket.create_connection(("127.0.0.1", self.server.api_port))
        self.send_response(200)
        self.end_headers()
        self.wfile.flush()
        down = self.connection

        def pipe(a, b):
            try:
                while True:
                    d = a.recv(65536)
                    if not d:
                        break
                    b.sendall(d)
            except OSError:
                pass
            finally:
                try:
                    b.shutdown(socket.SHUT_WR)
                except OSError:
                    pass

        t = threading.Thread(target=pipe, args=(up, down), daemon=True)
        t.start()
        pipe(down, up)
        t.join()
        up.close()
        self.close_connection = True


def _start(handler, **attrs):
    srv = ThreadingHTTPServer(("127.0.0.1", 0), handler)
    srv.daemon_threads = True
    for k, v in attrs.items():
        setattr(srv, k, v)
    threading.Thread(target=srv.serve_forever, daemon=True).start()
    return srv


class HTTPAPITest(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.api = _start(FakeAPI)
        cls.gw = _start(FakeGateway, api_port=cls.api.server_address[1])
        gw = f"127.0.0.1:{cls.gw.server_address[1]}"
        apis = {
            "events": httpapi.API("events", {"url": "http://api.test/v2/"}, "s3cret", gw, GW_TOKEN),
            "keyed": httpapi.API("keyed", {"url": "http://api.test/v2", "auth": "header", "auth_header": "X-API-Key",
                                           "auth_prefix": ""}, "k3y", gw, GW_TOKEN),
            "basic": httpapi.API("basic", {"url": "http://api.test/v2/", "auth": "basic", "user": "ann"}, "pw", gw, GW_TOKEN),
            "open": httpapi.API("open", {"url": "http://api.test:8080/v2/?fixed=1", "auth": "none"}, "", gw, GW_TOKEN),
            "oauth": httpapi.API("oauth", {"url": "http://api.test/v2/", "auth": "oauth2", "client_id": "cid",
                                           "token_url": "http://idp.test/oauth/token", "scope": "read"}, "cs", gw, GW_TOKEN),
            "blocked": httpapi.API("blocked", {"url": "http://refused.invalid/"}, "x", gw, GW_TOKEN),
        }
        cls.srv = httpapi.Server("127.0.0.1:0", RUN_TOKEN, apis)
        threading.Thread(target=cls.srv.serve_forever, daemon=True).start()
        cls.port = cls.srv.server_address[1]

    @classmethod
    def tearDownClass(cls):
        for s in (cls.srv, cls.gw, cls.api):
            s.shutdown()

    def call(self, method, path, body=None, headers=None, token=True):
        conn = http.client.HTTPConnection("127.0.0.1", self.port, timeout=10)
        h = dict(headers or {})
        if token:
            h.setdefault("X-Nbsandbox-Token", RUN_TOKEN)
        conn.request(method, path, body=body, headers=h)
        r = conn.getresponse()
        data = r.read()
        conn.close()
        return r, data

    def test_forwards_with_the_credential_and_without_the_run_token(self):
        r, data = self.call("GET", "/events/orders?since=2026-11-01")
        self.assertEqual(r.status, 200)
        got = json.loads(data)
        self.assertEqual(got["path"], "/v2/orders?since=2026-11-01")
        self.assertEqual(got["auth"], "Bearer s3cret")
        self.assertIsNone(got["token"])
        self.assertEqual(got["host"], "api.test")

    def test_the_run_token_in_the_connections_own_header(self):
        # An SDK given the run's token as its API key.
        r, data = self.call("GET", "/keyed/x", headers={"X-API-Key": RUN_TOKEN}, token=False)
        self.assertEqual(r.status, 200, data)
        self.assertEqual(json.loads(data)["key"], "k3y")
        r, data = self.call("GET", "/events/x", headers={"Authorization": "Bearer " + RUN_TOKEN}, token=False)
        self.assertEqual(json.loads(data)["auth"], "Bearer s3cret")

    def test_basic_and_none(self):
        _, data = self.call("GET", "/basic/x")
        self.assertEqual(json.loads(data)["auth"], "Basic " + base64.b64encode(b"ann:pw").decode())
        _, data = self.call("GET", "/open/x?y=2")
        got = json.loads(data)
        self.assertIsNone(got["auth"])
        self.assertEqual(got["path"], "/v2/x?fixed=1&y=2")
        self.assertEqual(got["host"], "api.test:8080")

    def test_oauth2_client_credentials(self):
        FakeAPI.oauth_issued.clear()
        _, data = self.call("GET", "/oauth/a")
        self.assertEqual(json.loads(data)["auth"], "Bearer access-0")
        _, data = self.call("GET", "/oauth/b")
        self.assertEqual(json.loads(data)["auth"], "Bearer access-0")  # kept until it expires
        self.assertEqual(len(FakeAPI.oauth_issued), 1)
        r, data = self.call("GET", "/oauth/needs-fresh")  # a 401: once more with a new token
        self.assertEqual(r.status, 200, data)
        self.assertEqual(json.loads(data)["auth"], "Bearer access-1")
        token_calls = [s for s in FakeAPI.seen if s["path"] == "/oauth/token"]
        self.assertIn("scope=read", token_calls[-1]["body"].decode())
        self.assertIn("idp.test:80", FakeGateway.tunnels)

    def test_bodies_stream_both_ways(self):
        big = b"x" * (3 << 20)
        r, data = self.call("POST", "/events/upload", body=big, headers={"Content-Type": "application/octet-stream"})
        self.assertEqual(r.status, 200)
        self.assertEqual(len(json.loads(data)["body"]), len(big))

        def chunks():
            yield b"part1-"
            yield b"part2"

        conn = http.client.HTTPConnection("127.0.0.1", self.port, timeout=10)
        conn.request("PUT", "/events/chunked", body=chunks(), headers={"X-Nbsandbox-Token": RUN_TOKEN},
                     encode_chunked=True)
        got = json.loads(conn.getresponse().read())
        self.assertEqual(got["body"], "part1-part2")

    def test_server_sent_events_arrive_as_sent(self):
        conn = http.client.HTTPConnection("127.0.0.1", self.port, timeout=10)
        conn.request("GET", "/events/sse", headers={"X-Nbsandbox-Token": RUN_TOKEN})
        r = conn.getresponse()
        self.assertEqual(r.getheader("Content-Type"), "text/event-stream")
        start = time.time()
        first = r.read1(100)
        self.assertIn(b"data: 0", first)
        self.assertLess(time.time() - start, 0.35)  # not held back until the stream ends
        rest = r.read()
        self.assertIn(b"data: 2", first + rest)

    def test_refusals(self):
        cases = [
            ("/events/x", False, 401),
            ("/nope/x", True, 404),
            ("/events/../admin", True, 403),
            ("/events/a/../../admin", True, 403),
            ("/events/%2e%2e/admin", True, 403),
            ("/events/a%2f..%2f..%2fadmin", True, 403),
            ("http://api.test/v2/x", True, 400),
        ]
        for path, token, want in cases:
            with self.subTest(path=path):
                r, data = self.call("GET", path, token=token)
                self.assertEqual(r.status, want, data)
                self.assertEqual(r.getheader("X-Nbsandbox-Error"), "fetcher")
        r, _ = self.call("GET", "/events/x", headers={"X-Nbsandbox-Token": "wrong"}, token=False)
        self.assertEqual(r.status, 401)
        r, _ = self.call("GET", "/events/x", headers={"Upgrade": "websocket", "Connection": "Upgrade"})
        self.assertEqual(r.status, 501)

    def test_dot_dot_inside_the_path_is_fine(self):
        _, data = self.call("GET", "/events/a/b/../c")
        self.assertEqual(json.loads(data)["path"], "/v2/a/c")

    def test_the_gateways_refusal_is_the_message(self):
        r, data = self.call("GET", "/blocked/x")
        self.assertEqual(r.status, 502)
        self.assertIn("not one of the run's endpoints", json.loads(data)["error"])

    def test_redirects(self):
        r, _ = self.call("GET", "/events/redirect-in")
        self.assertEqual(r.status, 302)
        self.assertEqual(r.getheader("Location"), f"http://127.0.0.1:{self.port}/events/target?x=1")
        r, _ = self.call("GET", "/events/redirect-out")
        self.assertEqual(r.getheader("Location"), "https://elsewhere.invalid/x")

    def test_no_body_answers(self):
        r, data = self.call("GET", "/events/empty")
        self.assertEqual((r.status, data), (204, b""))
        r, data = self.call("HEAD", "/events/x")
        self.assertEqual((r.status, data), (200, b""))

    def test_keep_alive(self):
        conn = http.client.HTTPConnection("127.0.0.1", self.port, timeout=10)
        for i in range(3):
            conn.request("GET", f"/events/k{i}", headers={"X-Nbsandbox-Token": RUN_TOKEN})
            self.assertEqual(json.loads(conn.getresponse().read())["path"], f"/v2/k{i}")
        conn.close()


if __name__ == "__main__":
    unittest.main()
