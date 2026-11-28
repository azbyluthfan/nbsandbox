# Copyright 2026 The nbsandbox Authors
# SPDX-License-Identifier: Apache-2.0
"""HTTP APIs through the fetcher (docs/plan-data-access.md, "HTTP APIs
through the fetcher").

The notebook's code calls http://<fetcher>:8816/<name>/<path> with the run's
token; the fetcher checks the token and the path, sets the connection's
credential, and forwards the request to the connection's URL through the
egress gateway. Requests and responses stream both ways. The credential
never reaches the run.
"""

import base64
import hmac
import http.client
import json
import socket
import ssl
import sys
import threading
import time
import urllib.parse
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

SYSTEM_CAS = "/etc/ssl/certs/ca-certificates.crt"
TOKEN_HEADER = "X-Nbsandbox-Token"
MAX_REQUESTS = 32
CHUNK = 65536

# Headers that belong to one hop, never forwarded either way.
HOP = {
    "connection",
    "keep-alive",
    "proxy-authenticate",
    "proxy-authorization",
    "proxy-connection",
    "te",
    "trailer",
    "transfer-encoding",
    "upgrade",
}


class GatewayRefused(OSError):
    """The gateway refused a tunnel; the message is its status and reason."""


def gateway_tunnel(gateway, token, target, timeout=30):
    """A socket tunnelled to target (host:port) through the gateway, and any
    bytes that came after the gateway's answer."""
    host, port = gateway.rsplit(":", 1)
    up = socket.create_connection((host, int(port)), timeout=timeout)
    up.sendall(
        f"CONNECT {target} HTTP/1.1\r\nHost: {target}\r\nProxy-Authorization: Bearer {token}\r\n\r\n".encode()
    )
    head = b""
    while b"\r\n\r\n" not in head:
        b = up.recv(4096)
        if not b:
            break
        head += b
        if len(head) > 16384:
            break
    status = head.split(b"\r\n", 1)[0].decode(errors="replace")
    if " 200 " not in status + " ":
        body = head.split(b"\r\n\r\n", 1)[1] if b"\r\n\r\n" in head else b""
        up.settimeout(1)
        try:
            while len(body) < 2048:
                b = up.recv(2048)
                if not b:
                    break
                body += b
        except OSError:
            pass
        up.close()
        why = " ".join(status.split(" ", 1)[1:]).strip()
        reason = " ".join(body[:2048].decode(errors="replace").split())
        raise GatewayRefused(f"the gateway refused {target} ({why})" + (f": {reason}" if reason else ""))
    up.settimeout(None)
    return up, head.split(b"\r\n\r\n", 1)[1]


class _TunnelConnection(http.client.HTTPConnection):
    """An HTTP(S) connection whose socket is a gateway tunnel."""

    def __init__(self, api, timeout=None):
        super().__init__(api.host, api.port, timeout=timeout)
        self.api = api

    def connect(self):
        sock, rest = gateway_tunnel(self.api.gateway, self.api.gateway_token, f"{self.api.host}:{self.api.port}")
        if rest:  # a server never speaks first over HTTP: nothing should be here
            sock.close()
            raise OSError("unexpected bytes from the gateway")
        if self.api.scheme == "https":
            sock = self.api.tls.wrap_socket(sock, server_hostname=self.api.host)
        self.sock = sock


class API:
    """One HTTP connection of the run: where it points and its credential."""

    def __init__(self, name, settings, secret, gateway, gateway_token, ca_file=None):
        self.name = name
        u = urllib.parse.urlsplit(settings["url"])
        self.scheme = u.scheme
        self.host = u.hostname
        self.port = u.port or (443 if u.scheme == "https" else 80)
        path = u.path or "/"
        self.base_path = path if path.endswith("/") else path + "/"
        self.base_query = u.query
        self.auth = settings.get("auth") or "header"
        self.auth_header = settings.get("auth_header") or "Authorization"
        prefix = settings.get("auth_prefix")
        self.auth_prefix = "Bearer " if prefix is None else prefix
        self.user = settings.get("user", "")
        self.secret = secret
        self.gateway, self.gateway_token = gateway, gateway_token
        self.tls = None
        if self.scheme == "https":
            self.tls = ssl.create_default_context(cafile=ca_file or SYSTEM_CAS)
        self.oauth = None
        if self.auth == "oauth2":
            self.oauth = OAuth2(settings, secret, gateway, gateway_token, self.tls_for)
        # The header the run's token may come in instead of X-Nbsandbox-Token,
        # so an SDK given the token as its API key works unchanged.
        self.token_header = self.auth_header if self.auth == "header" else "Authorization"
        self.token_prefix = self.auth_prefix if self.auth == "header" else "Bearer "

    def tls_for(self, scheme):
        if scheme != "https":
            return None
        return self.tls or ssl.create_default_context(cafile=SYSTEM_CAS)

    def credential(self, refresh=False):
        """The header to set: (name, value), or None."""
        if self.auth == "none" or (self.auth == "header" and not self.secret):
            return None
        if self.auth == "basic":
            v = base64.b64encode(f"{self.user}:{self.secret}".encode()).decode()
            return ("Authorization", "Basic " + v)
        if self.auth == "oauth2":
            return ("Authorization", "Bearer " + self.oauth.token(refresh))
        return (self.auth_header, self.auth_prefix + self.secret)

    def target(self, rest, query):
        """The path (and query) on the API for rest, a path under the
        connection's name; None if it leaves the connection's path."""
        segs = []
        for raw in rest.split("/"):
            seg = urllib.parse.unquote(raw)
            if "/" in seg or "\\" in seg:
                return None  # an encoded slash would hide a "..": refuse it
            if seg == "..":
                if not segs:
                    return None
                segs.pop()
            elif seg not in ("", "."):
                segs.append(raw)
        if rest.endswith("/") and segs:
            segs.append("")
        path = self.base_path + "/".join(segs)
        q = "&".join(p for p in (self.base_query, query) if p)
        return path + ("?" + q if q else "")

    def local(self, location, base):
        """A redirect's Location rewritten to the fetcher's address if it is
        under the connection's URL; as it is otherwise."""
        u = urllib.parse.urlsplit(urllib.parse.urljoin(f"{self.scheme}://{self.host}:{self.port}{self.base_path}", location))
        port = u.port or (443 if u.scheme == "https" else 80)
        if (u.scheme, (u.hostname or "").lower(), port) != (self.scheme, self.host.lower(), self.port):
            return location
        if not (u.path + "/").startswith(self.base_path) and u.path != self.base_path.rstrip("/"):
            return location
        rest = u.path[len(self.base_path) :] if u.path.startswith(self.base_path) else ""
        return base + self.name + "/" + rest + ("?" + u.query if u.query else "")


class OAuth2:
    """OAuth 2.0 client credentials: an access token kept in memory until a
    minute before it expires."""

    def __init__(self, settings, secret, gateway, gateway_token, tls_for):
        self.url = settings["token_url"]
        self.client_id = settings["client_id"]
        self.scope = settings.get("scope", "")
        self.audience = settings.get("audience", "")
        self.secret = secret
        self.gateway, self.gateway_token, self.tls_for = gateway, gateway_token, tls_for
        self.lock = threading.Lock()
        self.access, self.expires = "", 0.0

    def token(self, refresh=False):
        with self.lock:
            if refresh or not self.access or time.time() > self.expires - 60:
                self.access, self.expires = self._fetch()
            return self.access

    def _fetch(self):
        u = urllib.parse.urlsplit(self.url)
        port = u.port or (443 if u.scheme == "https" else 80)
        form = {"grant_type": "client_credentials", "client_id": self.client_id, "client_secret": self.secret}
        if self.scope:
            form["scope"] = self.scope
        if self.audience:
            form["audience"] = self.audience
        body = urllib.parse.urlencode(form).encode()
        sock, _ = gateway_tunnel(self.gateway, self.gateway_token, f"{u.hostname}:{port}")
        tls = self.tls_for(u.scheme)
        if tls:
            sock = tls.wrap_socket(sock, server_hostname=u.hostname)
        conn = http.client.HTTPConnection(u.hostname, port, timeout=30)
        conn.sock = sock
        try:
            conn.request(
                "POST",
                (u.path or "/") + ("?" + u.query if u.query else ""),
                body=body,
                headers={"Content-Type": "application/x-www-form-urlencoded", "Accept": "application/json"},
            )
            r = conn.getresponse()
            data = r.read(1 << 20)
        finally:
            conn.close()
        if r.status != 200:
            raise OSError(f"the token URL answered {r.status} {r.reason}: {' '.join(data[:300].decode(errors='replace').split())}")
        try:
            got = json.loads(data)
            access = got["access_token"]
        except (ValueError, KeyError, TypeError):
            raise OSError("the token URL's answer has no access_token") from None
        return access, time.time() + float(got.get("expires_in") or 300)


def _log(*parts):
    print("fetcher: http " + " ".join(str(p) for p in parts), file=sys.stderr)


class Handler(BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"
    server_version = "nbsandbox-fetcher"
    sys_version = ""

    def log_message(self, format, *args):  # noqa: A002 - BaseHTTPRequestHandler's name
        pass  # one line per request, written by _proxy

    def _fail(self, status, msg):
        body = (json.dumps({"error": msg}) + "\n").encode()
        self.send_response(status)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        self.send_header("X-Nbsandbox-Error", "fetcher")
        self.end_headers()
        if self.command != "HEAD":
            self.wfile.write(body)

    def _discard_body(self):
        n = int(self.headers.get("Content-Length") or 0)
        if self.headers.get("Transfer-Encoding", "").lower() == "chunked":
            for _ in self._chunks():
                pass
        while n > 0:
            b = self.rfile.read(min(n, CHUNK))
            if not b:
                break
            n -= len(b)

    def _chunks(self):
        """A chunked request body, decoded, chunk by chunk."""
        while True:
            line = self.rfile.readline(1024)
            size = int(line.split(b";", 1)[0].strip() or b"0", 16)
            if size == 0:
                while self.rfile.readline(1024) not in (b"\r\n", b"\n", b""):
                    pass  # trailers
                return
            left = size
            while left > 0:
                b = self.rfile.read(min(left, CHUNK))
                if not b:
                    return
                left -= len(b)
                yield b
            self.rfile.readline(4)

    def _body(self):
        """The request's body as an iterable (and its length, or None for
        chunked), or (None, 0) for none."""
        if self.headers.get("Transfer-Encoding", "").lower() == "chunked":
            return self._chunks(), None
        n = int(self.headers.get("Content-Length") or 0)
        if n <= 0:
            return None, 0

        def read():
            left = n
            while left > 0:
                b = self.rfile.read(min(left, CHUNK))
                if not b:
                    return
                left -= len(b)
                yield b

        return read(), n

    def _token_ok(self, api):
        want = self.server.token
        got = self.headers.get(TOKEN_HEADER)
        if got is None:
            got = self.headers.get(api.token_header) or ""
            if api.token_prefix and got.startswith(api.token_prefix):
                got = got[len(api.token_prefix) :]
        return bool(got) and _same(got, want)

    def _proxy(self):
        if not self.server.slots.acquire(blocking=False):
            self._discard_body()
            return self._fail(429, f"too many requests at once (at most {MAX_REQUESTS} per run)")
        try:
            self._forward()
        finally:
            self.server.slots.release()

    def _forward(self):
        start = time.time()
        raw_path, _, query = self.path.partition("?")
        if raw_path.startswith("http://") or raw_path.startswith("https://"):
            self._discard_body()
            return self._fail(400, "call the fetcher's address, not the API's: nbsandbox.http(name) or nbsandbox.connection(name).url")
        name, _, rest = raw_path.lstrip("/").partition("/")
        name = urllib.parse.unquote(name)
        api = self.server.apis.get(name)
        if api is None:
            self._discard_body()
            known = ", ".join(sorted(self.server.apis)) or "none"
            return self._fail(404, f"this run has no HTTP connection named {name!r} (its HTTP connections: {known})")
        if not self._token_ok(api):
            self._discard_body()
            return self._fail(401, "the run's token is needed (X-Nbsandbox-Token, or the connection's own auth header)")
        if self.headers.get("Upgrade"):
            self._discard_body()
            return self._fail(501, "upgrades (WebSocket) aren't supported through the fetcher")
        path = api.target(rest, query)
        if path is None:
            self._discard_body()
            return self._fail(403, f"the path leaves {name!r}'s URL")
        headers = []
        # The body's framing is the fetcher's to set (one Content-Length, or
        # chunked): never the notebook's, which could disagree with it.
        drop = HOP | {"host", "content-length", TOKEN_HEADER.lower(), api.token_header.lower()}
        for k in self.headers.get("Connection", "").split(","):
            drop.add(k.strip().lower())
        for k, v in self.headers.items():
            if k.lower() not in drop:
                headers.append((k, v))
        body, length = self._body()
        try:
            resp, conn, sent = self._send(api, path, headers, body, length)
        except GatewayRefused as e:
            _log(name, self.command, path.split("?", 1)[0], 502, 0, 0, f"{time.time() - start:.3f}s", e)
            return self._fail(502, str(e))
        except (OSError, http.client.HTTPException, ssl.SSLError) as e:
            _log(name, self.command, path.split("?", 1)[0], 502, 0, 0, f"{time.time() - start:.3f}s", e)
            return self._fail(502, f"{name!r}: {e}")
        try:
            received = self._relay(api, resp)
        finally:
            conn.close()
        _log(name, self.command, path.split("?", 1)[0], resp.status, sent, received, f"{time.time() - start:.3f}s")

    def _send(self, api, path, headers, body, length, retried=False):
        conn = _TunnelConnection(api, timeout=None)
        conn.connect()
        conn.putrequest(self.command, path, skip_host=True, skip_accept_encoding=True)
        conn.putheader("Host", api.host if api.port in (80, 443) else f"{api.host}:{api.port}")
        for k, v in headers:
            conn.putheader(k, v)
        cred = api.credential(refresh=retried)
        if cred:
            conn.putheader(*cred)
        sent = 0
        if body is None:
            if self.command in ("POST", "PUT", "PATCH"):
                conn.putheader("Content-Length", "0")
            conn.endheaders()
        elif length is not None:
            conn.putheader("Content-Length", str(length))
            conn.endheaders()
            for b in body:
                conn.send(b)
                sent += len(b)
        else:
            conn.putheader("Transfer-Encoding", "chunked")
            conn.endheaders()
            for b in body:
                conn.send(f"{len(b):x}\r\n".encode() + b + b"\r\n")
                sent += len(b)
            conn.send(b"0\r\n\r\n")
        resp = conn.getresponse()
        # An OAuth access token revoked before it expired: once more, with a
        # new one, if the request had no body to send again.
        if resp.status == 401 and api.oauth and not retried and body is None:
            resp.read()
            conn.close()
            return self._send(api, path, headers, None, 0, retried=True)
        return resp, conn, sent

    def _relay(self, api, resp):
        self.send_response(resp.status, resp.reason)
        drop = set(HOP) | {"content-length"}
        for k in (resp.getheader("Connection") or "").split(","):
            drop.add(k.strip().lower())
        for k, v in resp.getheaders():
            if k.lower() in drop:
                continue
            if k.lower() == "location":
                v = api.local(v, f"http://{self.headers.get('Host') or self.server.listen}/")
            self.send_header(k, v)
        no_body = self.command == "HEAD" or resp.status in (204, 304) or 100 <= resp.status < 200
        length = resp.getheader("Content-Length")
        if no_body:
            if length is not None:
                self.send_header("Content-Length", length)
            self.end_headers()
            return 0
        if length is not None and resp.getheader("Transfer-Encoding") is None:
            self.send_header("Content-Length", length)
            self.end_headers()
            received = 0
            while True:
                b = resp.read1(CHUNK)
                if not b:
                    break
                self.wfile.write(b)
                received += len(b)
            return received
        self.send_header("Transfer-Encoding", "chunked")
        self.end_headers()
        received = 0
        while True:
            b = resp.read1(CHUNK)  # as it comes: server-sent events aren't held back
            if not b:
                break
            self.wfile.write(f"{len(b):x}\r\n".encode() + b + b"\r\n")
            self.wfile.flush()
            received += len(b)
        self.wfile.write(b"0\r\n\r\n")
        return received

    do_GET = do_POST = do_PUT = do_PATCH = do_DELETE = do_HEAD = do_OPTIONS = _proxy


def _same(a, b):
    return hmac.compare_digest(a.encode(), b.encode())


class Server(ThreadingHTTPServer):
    daemon_threads = True
    allow_reuse_address = True

    def __init__(self, listen, token, apis):
        host, port = listen.rsplit(":", 1)
        super().__init__((host, int(port)), Handler)
        self.token, self.apis, self.listen = token, apis, listen
        self.slots = threading.BoundedSemaphore(MAX_REQUESTS)


def serve(listen, token, apis):
    """Starts serving apis (name -> API) on listen (host:port) in the
    background; returns the server."""
    srv = Server(listen, token, apis)
    threading.Thread(target=srv.serve_forever, daemon=True).start()
    return srv
