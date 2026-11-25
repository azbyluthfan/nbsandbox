# Copyright 2026 The nbsandbox Authors
# SPDX-License-Identifier: Apache-2.0
"""nbsandbox fetcher: a run's reader of external data (docs/plan-data-access.md).

It holds the run's credentials, which the run never sees, and answers the
run's SQL over Arrow Flight: a DoGet ticket is one SELECT, and the result
streams back as Arrow record batches. Three layers keep a query to reading
what the run's space binds:

1. the gate: one SELECT statement, no pass-through functions (SQL sent to a
   source as is) and nothing that runs SQL from a string;
2. DuckDB itself, locked after set-up: no file or network access beyond the
   run's sources, no extensions loaded or installed, settings locked;
3. the source's own role, which the connection's owner grants: read-only.

Every source is reached through the egress gateway, with the run's token.
"""

import hmac
import json
import os
import re
import select
import socket
import sys
import threading

import duckdb
import pyarrow.flight as fl

EXTENSIONS = "/opt/nbsandbox-fetcher/extensions"
BATCH_ROWS = 65536

# Functions the gate refuses: they send SQL to a source as is, run SQL from
# a string, or tell what the fetcher holds.
REFUSED = {"query", "query_table", "getenv", "which_secret", "current_setting"}
REFUSED_PREFIXES = ("postgres_", "mysql_", "mssql_", "sqlite_", "odbc_", "duckdb_", "pragma_")
DOTDOT = re.compile(r"(^|/)\.\.(/|$)")


class Refused(Exception):
    """A query the gate refuses; the message says why."""


def _walk(node, fn):
    if isinstance(node, dict):
        fn(node)
        for v in node.values():
            _walk(v, fn)
    elif isinstance(node, list):
        for v in node:
            _walk(v, fn)


def _strings(node):
    out = []

    def add(n):
        out.extend(v for v in n.values() if isinstance(v, str))

    _walk(node, add)
    return out


def gate(con, sql):
    """Refuses anything but one plain SELECT. con is a connection to parse with."""
    tree = json.loads(con.execute("SELECT json_serialize_sql(?)", [sql]).fetchone()[0])
    if tree.get("error"):
        msg = tree.get("error_message", "")
        if "Only SELECT" in msg:
            raise Refused("only SELECT queries are allowed")
        raise Refused(msg.splitlines()[0] if msg else "the query doesn't parse")
    if len(tree.get("statements", [])) != 1:
        raise Refused("one statement at a time")

    def check(n):
        name = n.get("function_name")
        if isinstance(name, str):
            low = name.lower()
            if low in REFUSED or low.startswith(REFUSED_PREFIXES):
                raise Refused(f"{name}() isn't allowed")
        if n.get("type") == "TABLE_FUNCTION":
            for s in _strings(n.get("function", {})):
                if DOTDOT.search(s):
                    raise Refused("paths may not contain '..'")

    _walk(tree, check)


def _lit(v):
    """A SQL string literal."""
    return "'" + str(v).replace("'", "''") + "'"


def _ident(v):
    """A SQL identifier (names are checked by nbsandboxd: [a-z][a-z0-9_-]*)."""
    return '"' + str(v).replace('"', '""') + '"'


class Vault:
    """Reads the run's secrets from Vault, as the fetchers' role: it logs in
    with its own service account token (Kubernetes auth), reaches Vault
    through the gateway like any source, and revokes its token when done."""

    def __init__(self, cfg, gw):
        import ssl
        import urllib.request

        self.cfg = cfg
        proxy = f"http://run:{gw['token']}@{gw['address']}"
        ctx = ssl.create_default_context(cadata=cfg["ca_pem"]) if cfg.get("ca_pem") else ssl.create_default_context()
        self.opener = urllib.request.build_opener(
            urllib.request.ProxyHandler({"http": proxy, "https": proxy}), urllib.request.HTTPSHandler(context=ctx)
        )
        with open(cfg["token_file"]) as f:
            jwt = f.read().strip()
        auth = self._call("POST", f"/v1/auth/{cfg['auth_mount']}/login", {"role": cfg["role"], "jwt": jwt})
        self.token = auth["auth"]["client_token"]

    def _call(self, method, path, body=None):
        import urllib.error
        import urllib.request

        req = urllib.request.Request(
            self.cfg["address"].rstrip("/") + path, method=method,
            data=json.dumps(body).encode() if body is not None else None,
            headers={"Content-Type": "application/json"},
        )
        if getattr(self, "token", None):
            req.add_header("X-Vault-Token", self.token)
        try:
            with self.opener.open(req, timeout=15) as r:
                raw = r.read()
        except urllib.error.HTTPError as e:
            errs = json.loads(e.read() or b"{}").get("errors") or []
            raise OSError(f"vault {method} {path.split('?')[0]}: {e.code} {'; '.join(errs)}") from None
        return json.loads(raw) if raw else {}

    def read(self, path, key):
        data = self._call("GET", f"/v1/{self.cfg['mount']}/data/{path}")["data"]["data"]
        if key not in data:
            raise OSError(f"vault: no key {key!r} at {path}")
        return str(data[key])

    def close(self):
        try:
            self._call("POST", "/v1/auth/token/revoke-self")
        except OSError:
            pass


class Forwarder:
    """A local TCP port tunnelled to host:port through the gateway (CONNECT).

    Database drivers speak their own protocol and know no HTTP proxy: they
    connect here, and each connection becomes a tunnel."""

    def __init__(self, gateway, token, target):
        self.gateway, self.token, self.target = gateway, token, target
        self.sock = socket.socket(socket.AF_INET, socket.SOCK_STREAM)
        self.sock.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
        self.sock.bind(("127.0.0.1", 0))
        self.sock.listen(64)
        self.port = self.sock.getsockname()[1]
        threading.Thread(target=self._accept, daemon=True).start()

    def _accept(self):
        while True:
            try:
                c, _ = self.sock.accept()
            except OSError:
                return
            threading.Thread(target=self._serve, args=(c,), daemon=True).start()

    def _open(self):
        host, port = self.gateway.rsplit(":", 1)
        up = socket.create_connection((host, int(port)), timeout=10)
        up.sendall(
            f"CONNECT {self.target} HTTP/1.1\r\nHost: {self.target}\r\n"
            f"Proxy-Authorization: Bearer {self.token}\r\n\r\n".encode()
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
            up.close()
            raise OSError(f"gateway refused {self.target}: {status}")
        up.settimeout(None)
        rest = head.split(b"\r\n\r\n", 1)[1]
        return up, rest

    def _serve(self, down):
        try:
            up, rest = self._open()
        except OSError as e:
            print(f"fetcher: {e}", file=sys.stderr)
            down.close()
            return
        if rest:
            down.sendall(rest)
        socks = [down, up]
        try:
            while True:
                r, _, _ = select.select(socks, [], [])
                for s in r:
                    b = s.recv(65536)
                    if not b:
                        return
                    (up if s is down else down).sendall(b)
        except OSError:
            pass
        finally:
            down.close()
            up.close()


def connect(cfg):
    """A DuckDB connection with the run's sources attached, then locked."""
    # SQL Server's extension is a community one (signed with the community
    # key): allowed only when a source needs it, to load the copy installed
    # in the image. Once locked, nothing else can be installed or loaded.
    community = any(src.get("type") == "sqlserver" for src in cfg.get("sources", []))
    con = duckdb.connect(
        config={
            "extension_directory": EXTENSIONS,
            "autoinstall_known_extensions": "false",
            "autoload_known_extensions": "false",
            "allow_community_extensions": "true" if community else "false",
            "allow_unsigned_extensions": "false",
            "allow_persistent_secrets": "false",
        }
    )
    gw = dict(cfg["gateway"])
    # On Kubernetes the worker resolves the gateway (fetchers have no DNS).
    gw["address"] = os.environ.get("NBSANDBOX_FETCHER_GATEWAY") or gw["address"]
    allowed = []
    served = []
    loaded = set()
    # Secrets in Vault: read now, all at once, and the token revoked.
    from_vault = {}
    if cfg.get("vault"):
        vault = Vault(cfg["vault"], gw)
        try:
            for src in cfg.get("sources", []):
                if src.get("secret_vault"):
                    v = src["secret_vault"]
                    from_vault[src["name"]] = vault.read(v["path"], v["key"])
        finally:
            vault.close()
    for i, src in enumerate(cfg.get("sources", [])):
        name, typ, s = src["name"], src["type"], src.get("settings", {})
        secret = src.get("secret") or from_vault.get(name, "")
        if src.get("secret_file"):  # a mounted Secret (Kubernetes): nbsandboxd never read it
            with open(src["secret_file"]) as f:
                secret = f.read().strip()
        sec = f"nbs_{i}"
        if typ == "postgresql":
            if "postgres" not in loaded:
                con.execute("LOAD postgres")
                loaded.add("postgres")
            fwd = Forwarder(gw["address"], gw["token"], f"{s['host']}:{s['port']}")
            con.execute(
                f"CREATE SECRET {sec} (TYPE postgres, HOST '127.0.0.1', PORT {fwd.port}, "
                f"DATABASE {_lit(s['database'])}, USER {_lit(s['user'])}, PASSWORD {_lit(secret)})"
            )
            con.execute(f"ATTACH '' AS {_ident(name)} (TYPE postgres, SECRET {sec}, READ_ONLY)")
        elif typ in ("mysql", "sqlserver"):
            ext, kind = ("mysql", "mysql") if typ == "mysql" else ("mssql", "mssql")
            if ext not in loaded:
                con.execute(f"LOAD {ext}")
                loaded.add(ext)
            fwd = Forwarder(gw["address"], gw["token"], f"{s['host']}:{s['port']}")
            con.execute(
                f"CREATE SECRET {sec} (TYPE {kind}, HOST '127.0.0.1', PORT {fwd.port}, "
                f"DATABASE {_lit(s['database'])}, USER {_lit(s['user'])}, PASSWORD {_lit(secret)})"
            )
            con.execute(f"ATTACH '' AS {_ident(name)} (TYPE {kind}, SECRET {sec}, READ_ONLY)")
        elif typ == "s3":
            if "httpfs" not in loaded:
                con.execute("LOAD httpfs")
                loaded.add("httpfs")
            root = f"s3://{s['bucket']}/"
            opts = [
                "TYPE s3",
                f"ENDPOINT {_lit(s['endpoint'])}",
                f"REGION {_lit(s.get('region') or 'us-east-1')}",
                f"URL_STYLE {_lit(s.get('url_style') or 'vhost')}",
                f"USE_SSL {'true' if s.get('use_ssl', True) else 'false'}",
                f"SCOPE {_lit(root)}",
            ]
            if s.get("access_key_id"):
                opts += [f"KEY_ID {_lit(s['access_key_id'])}", f"SECRET {_lit(secret)}"]
            con.execute(f"CREATE SECRET {sec} ({', '.join(opts)})")
            schema = _ident(name)
            con.execute(f"CREATE SCHEMA {schema}")
            con.execute(f"CREATE MACRO {schema}.parquet(p) AS TABLE SELECT * FROM read_parquet({_lit(root)} || p)")
            con.execute(f"CREATE MACRO {schema}.csv(p) AS TABLE SELECT * FROM read_csv({_lit(root)} || p)")
            allowed.append(root)
        else:
            continue  # served elsewhere (injected mode), or not yet
        served.append({"name": name, "type": typ})
    # Under the container's limit, with room for Arrow buffers and Python:
    # past it DuckDB spills to /tmp rather than being killed.
    mem = int(os.environ.get("NBSANDBOX_FETCHER_MEMORY", "0") or 0)
    if mem > 0:
        con.execute(f"SET memory_limit = '{mem * 3 // 4 // (1 << 20)}MiB'")
        con.execute("SET temp_directory = '/tmp/duckdb'")
    if "httpfs" in loaded:
        con.execute(f"SET http_proxy = {_lit(gw['address'])}")
        con.execute("SET http_proxy_username = 'run'")
        con.execute(f"SET http_proxy_password = {_lit(gw['token'])}")
    con.execute(f"SET allowed_directories = [{', '.join(_lit(a) for a in allowed)}]")
    con.execute("SET enable_external_access = false")
    con.execute("SET lock_configuration = true")
    return con, served


class _Auth(fl.ServerMiddlewareFactory):
    def __init__(self, token):
        self.want = ("Bearer " + token).encode()

    def start_call(self, info, headers):
        got = (headers.get("authorization") or [""])[0].encode()
        if not hmac.compare_digest(got, self.want):
            raise fl.FlightUnauthenticatedError("a valid fetcher token is needed")
        return None


class Fetcher(fl.FlightServerBase):
    def __init__(self, location, cfg):
        super().__init__(location, middleware={"auth": _Auth(cfg["token"])})
        self.base, self.served = connect(cfg)
        self.parser = duckdb.connect()  # parses only: holds nothing

    def do_get(self, context, ticket):
        sql = ticket.ticket.decode()
        try:
            gate(self.parser, sql)
        except Refused as e:
            raise fl.FlightServerError(f"refused: {e}") from None
        cur = self.base.cursor()
        try:
            reader = cur.execute(sql).to_arrow_reader(BATCH_ROWS)
        except duckdb.Error as e:
            raise fl.FlightServerError(str(e).splitlines()[0]) from None
        return fl.RecordBatchStream(reader)

    def list_actions(self, context):
        return [("connections", "the names this run can read")]

    def do_action(self, context, action):
        if action.type == "connections":
            yield fl.Result(json.dumps(self.served).encode())
            return
        raise fl.FlightServerError(f"unknown action {action.type!r}")


def main():
    # The configuration comes in the environment (Docker) or a file (a
    # mounted Secret); from the environment, it is dropped once read.
    raw = os.environ.pop("NBSANDBOX_FETCHER_CONFIG_JSON", "")
    if raw:
        cfg = json.loads(raw)
    else:
        with open(os.environ.get("NBSANDBOX_FETCHER_CONFIG", "/etc/nbsandbox-fetcher/config.json")) as f:
            cfg = json.load(f)
    listen = os.environ.get("NBSANDBOX_FETCHER_LISTEN", "grpc://0.0.0.0:8815")
    server = Fetcher(listen, cfg)
    print(f"fetcher: serving {len(server.served)} source(s) on {listen}", file=sys.stderr)
    server.serve()


if __name__ == "__main__":
    main()
