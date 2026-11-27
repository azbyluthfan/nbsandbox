# Copyright 2026 The nbsandbox Authors
# SPDX-License-Identifier: Apache-2.0
"""Fetcher tests, run inside the image without a network (make fetcher-test).

Sources need the network, so here an s3 source points nowhere: what's
tested is the gate and the lockdown around it. Reading real sources through
the gateway is tested end to end (make test-integration)."""

import json
import sys
import threading
import unittest

import duckdb
import pyarrow.flight as fl

sys.path.insert(0, "/opt/nbsandbox-fetcher")
import fetcher  # noqa: E402

CFG = {
    "token": "fetcher-token",
    "gateway": {"address": "gateway.invalid:3128", "token": "run-token"},
    "sources": [
        {"name": "lake", "type": "s3", "secret": "s3-SECRET",
         "settings": {"endpoint": "minio.invalid:9000", "bucket": "data", "access_key_id": "AK",
                      "use_ssl": False, "url_style": "path", "region": "us-east-1"}},
        {"name": "api", "type": "http", "secret": "t", "settings": {"url": "https://api.invalid"}},
    ],
}


class GateTest(unittest.TestCase):
    def setUp(self):
        self.con = duckdb.connect()

    def test_allows_selects(self):
        for q in [
            "SELECT 1",
            "SELECT region, sum(amount) FROM crm.public.orders GROUP BY 1",
            "WITH x AS (SELECT * FROM crm.orders) SELECT count(*) FROM x",
            "SELECT * FROM lake.parquet('2026/11/*.parquet') WHERE id > 3",
            "SELECT note FROM crm.notes WHERE note = '..'",
        ]:
            fetcher.gate(self.con, q)

    def test_refuses(self):
        for q in [
            "SELECT 1; SELECT 2",
            "CREATE TABLE t AS SELECT 1",
            "INSERT INTO crm.orders VALUES (1)",
            "ATTACH 'x' AS y",
            "SET threads = 1",
            "PRAGMA version",
            "COPY (SELECT 1) TO 'x'",
            "INSTALL httpfs",
            "SELECT * FROM postgres_query('crm', 'SELECT 1')",
            "SELECT * FROM (SELECT * FROM postgres_execute('crm', 'DROP TABLE x'))",
            "SELECT * FROM query('ATTACH ''x'' AS y')",
            "SELECT * FROM mysql_query('crm', 'SELECT 1')",
            "SELECT * FROM mssql_scan('crm', 'SELECT 1')",
            "SELECT mssql_exec('crm', 'DROP TABLE orders')",
            "SELECT * FROM query_table('t')",
            "SELECT * FROM duckdb_secrets()",
            "SELECT * FROM duckdb_databases()",
            "SELECT getenv('HOME')",
            "SELECT current_setting('http_proxy_password')",
            "SELECT * FROM which_secret('s3://data/x', 's3')",
            "SELECT * FROM pragma_table_info('t')",
            "SELECT * FROM lake.parquet('../other/x.parquet')",
            "SELECT * FROM read_parquet('s3://data/a/../../other/x.parquet')",
            "not sql at all",
        ]:
            with self.assertRaises(fetcher.Refused, msg=q):
                fetcher.gate(self.con, q)


class LockdownTest(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.con, cls.served, cls.apis = fetcher.connect(CFG)

    def run_sql(self, q):
        return self.con.cursor().execute(q).fetchall()

    def test_serves_only_its_sources(self):
        # SQL for lake; the HTTP API through httpapi, not in SQL.
        self.assertEqual(self.served, [{"name": "lake", "type": "s3"}, {"name": "api", "type": "http"}])
        self.assertEqual(list(self.apis), ["api"])
        self.assertEqual(self.apis["api"].credential(), ("Authorization", "Bearer t"))

    def test_no_local_files(self):
        for q in ["SELECT * FROM read_csv('/etc/passwd')", "SELECT * FROM read_text('/etc/hostname')",
                  "SELECT * FROM read_text('/etc/nbsandbox-fetcher/config.json')"]:
            with self.assertRaises(duckdb.Error, msg=q):
                self.run_sql(q)

    def test_no_other_buckets_or_urls(self):
        for q in ["SELECT * FROM read_parquet('s3://other/x.parquet')",
                  "SELECT * FROM read_csv('http://example.com/x.csv')",
                  "SELECT * FROM read_csv('https://example.com/x.csv')"]:
            with self.assertRaisesRegex(duckdb.Error, "disabled by configuration", msg=q):
                self.run_sql(q)

    def test_locked(self):
        for q in ["SET enable_external_access = true", "SET allowed_directories = ['/']",
                  "SET http_proxy = 'elsewhere:1'", "RESET lock_configuration"]:
            with self.assertRaises(duckdb.Error, msg=q):
                self.run_sql(q)
        for q in ["INSTALL mysql", "LOAD mysql", "ATTACH '/tmp/x.db' AS x"]:
            with self.assertRaises(duckdb.Error, msg=q):
                self.run_sql(q)

    def test_secret_redacted(self):
        # The gate refuses duckdb_secrets(); even past it, values are redacted.
        rows = self.run_sql("SELECT secret_string FROM duckdb_secrets()")
        self.assertTrue(rows)
        self.assertNotIn("s3-SECRET", json.dumps(rows))

    def test_macros(self):
        # Reaching the source fails (no network); the macro and the path are fine.
        with self.assertRaises(duckdb.Error) as e:
            self.run_sql("SELECT * FROM lake.parquet('a.parquet')")
        self.assertNotIn("disabled by configuration", str(e.exception))


class MountedSecretTest(unittest.TestCase):
    def test_secret_file_and_gateway_override(self):
        import os
        import tempfile
        from unittest import mock

        with tempfile.NamedTemporaryFile("w", dir="/tmp", delete=False) as f:
            f.write("from-a-file\n")
        cfg = json.loads(json.dumps(CFG))
        cfg["sources"][0].pop("secret")
        cfg["sources"][0]["secret_file"] = f.name
        with mock.patch.dict(os.environ, {"NBSANDBOX_FETCHER_GATEWAY": "10.96.0.9:3128"}):
            con, _, _ = fetcher.connect(cfg)
        rows = con.cursor().execute("SELECT current_setting('http_proxy')").fetchall()
        self.assertEqual(rows, [("10.96.0.9:3128",)])


class TLSTest(unittest.TestCase):
    """A source's ca reaches the queries: each runs on a cursor of its own,
    and some settings (ca_cert_file) are per connection."""

    def _ca(self):
        import glob

        with open(sorted(glob.glob("/etc/ssl/certs/*.pem"))[0]) as f:
            return f.read()

    def test_s3_ca_reaches_every_cursor(self):
        cfg = json.loads(json.dumps(CFG))
        cfg["sources"][0]["settings"].update(use_ssl=True, ca=self._ca())
        con, _, _ = fetcher.connect(cfg)
        path = con.cursor().execute("SELECT current_setting('ca_cert_file')").fetchone()[0]
        self.assertTrue(path.startswith("/tmp/nbsandbox-ca/"), path)
        with open(path) as f:
            bundle = f.read()
        self.assertIn(self._ca().strip(), bundle)
        with open(fetcher.SYSTEM_CAS) as f:
            self.assertIn(f.read().strip(), bundle)  # the system's too
        with self.assertRaises(duckdb.Error):
            con.cursor().execute("SET ca_cert_file = '/etc/passwd'")

    def test_no_ca_no_setting(self):
        con, _, _ = fetcher.connect(CFG)
        self.assertEqual(con.cursor().execute("SELECT current_setting('ca_cert_file')").fetchone()[0], "")

    def test_modes_in_each_driver(self):
        for mode in ("off", "prefer", "require", "verify"):
            self.assertIn(mode, fetcher.PG_SSLMODE)
            self.assertIn(mode, fetcher.MYSQL_SSL_MODE)
        self.assertEqual(fetcher.PG_SSLMODE["verify"], "verify-full")
        self.assertEqual(fetcher.MYSQL_SSL_MODE["verify"], "VERIFY_IDENTITY")


class MemoryTest(unittest.TestCase):
    def test_spills_under_its_limit(self):
        import os
        from unittest import mock

        with mock.patch.dict(os.environ, {"NBSANDBOX_FETCHER_MEMORY": str(256 << 20)}):
            con, _, _ = fetcher.connect(CFG)
        cur = con.cursor()
        self.assertEqual(cur.execute("SELECT current_setting('memory_limit')").fetchone()[0], "192.0 MiB")
        # Sorting more than the limit spills to /tmp instead of failing.
        n = cur.execute("SELECT count(*) FROM (SELECT range AS r, md5(range::VARCHAR) AS h FROM range(6000000) ORDER BY h)").fetchone()[0]
        self.assertEqual(n, 6000000)


class VaultTest(unittest.TestCase):
    """The fetcher reads Vault through the gateway: a stand-in proxy that
    checks the run's token and answers as Vault would."""

    def test_reads_through_the_gateway(self):
        import base64
        import http.server
        import os
        import tempfile

        seen = []

        class Proxy(http.server.BaseHTTPRequestHandler):
            def log_message(self, *a):
                pass

            def _answer(self, code, body):
                b = json.dumps(body).encode()
                self.send_response(code)
                self.send_header("Content-Length", str(len(b)))
                self.end_headers()
                self.wfile.write(b)

            def _route(self):
                auth = self.headers.get("Proxy-Authorization", "")
                if auth != "Basic " + base64.b64encode(b"run:run-token").decode():
                    return self._answer(407, {})
                seen.append((self.command, self.path))
                n = int(self.headers.get("Content-Length") or 0)
                body = json.loads(self.rfile.read(n) or b"{}")
                if self.path == "http://vault.internal:8200/v1/auth/kubernetes/login":
                    ok = body == {"role": "fetchers", "jwt": "sa-jwt"}
                    return self._answer(200 if ok else 403, {"auth": {"client_token": "vt"}} if ok else {"errors": ["denied"]})
                if self.headers.get("X-Vault-Token") != "vt":
                    return self._answer(403, {"errors": ["permission denied"]})
                if self.path == "http://vault.internal:8200/v1/secret/data/nbsandbox/connections/conn-1":
                    return self._answer(200, {"data": {"data": {"secret": "s3-from-vault"}}})
                if self.path == "http://vault.internal:8200/v1/auth/token/revoke-self":
                    return self._answer(204, {})
                return self._answer(404, {"errors": []})

            do_GET = do_POST = _route

        srv = http.server.HTTPServer(("127.0.0.1", 0), Proxy)
        threading.Thread(target=srv.serve_forever, daemon=True).start()
        self.addCleanup(srv.shutdown)
        tok = tempfile.NamedTemporaryFile("w", dir="/tmp", delete=False)
        tok.write("sa-jwt\n")
        tok.close()
        cfg = json.loads(json.dumps(CFG))
        cfg["gateway"] = {"address": f"127.0.0.1:{srv.server_port}", "token": "run-token"}
        cfg["sources"][0].pop("secret")
        cfg["sources"][0]["secret_vault"] = {"path": "nbsandbox/connections/conn-1", "key": "secret"}
        cfg["vault"] = {"address": "http://vault.internal:8200", "mount": "secret", "auth_mount": "kubernetes",
                        "role": "fetchers", "token_file": tok.name}
        con, served, _ = fetcher.connect(cfg)
        self.assertEqual(served[0], {"name": "lake", "type": "s3"})
        self.assertEqual([p for _, p in seen], [
            "http://vault.internal:8200/v1/auth/kubernetes/login",
            "http://vault.internal:8200/v1/secret/data/nbsandbox/connections/conn-1",
            "http://vault.internal:8200/v1/auth/token/revoke-self",
        ])
        os.unlink(tok.name)

    def test_refused_login_fails_the_start(self):
        cfg = json.loads(json.dumps(CFG))
        cfg["vault"] = {"address": "http://127.0.0.1:9", "mount": "secret", "auth_mount": "kubernetes",
                        "role": "r", "token_file": "/nonexistent"}
        with self.assertRaises(OSError):
            fetcher.connect(cfg)


class FlightTest(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.server = fetcher.Fetcher("grpc://127.0.0.1:0", CFG)
        threading.Thread(target=cls.server.serve, daemon=True).start()
        cls.loc = f"grpc://127.0.0.1:{cls.server.port}"

    @classmethod
    def tearDownClass(cls):
        cls.server.shutdown()

    def client(self, token):
        c = fl.FlightClient(self.loc)
        opts = fl.FlightCallOptions(headers=[(b"authorization", f"Bearer {token}".encode())])
        return c, opts

    def test_query(self):
        c, o = self.client("fetcher-token")
        t = c.do_get(fl.Ticket(b"SELECT range AS n FROM range(200000)"), o).read_all()
        self.assertEqual(t.num_rows, 200000)
        self.assertEqual(t.column("n")[-1].as_py(), 199999)

    def test_connections(self):
        c, o = self.client("fetcher-token")
        got = [json.loads(r.body.to_pybytes()) for r in c.do_action(fl.Action("connections", b""), o)]
        self.assertEqual(got, [[{"name": "lake", "type": "s3"}, {"name": "api", "type": "http"}]])

    def test_refused(self):
        c, o = self.client("fetcher-token")
        with self.assertRaisesRegex(fl.FlightError, "refused: .*query"):
            c.do_get(fl.Ticket(b"SELECT * FROM query('SELECT 1')"), o).read_all()

    def test_token(self):
        for tok in ["wrong", ""]:
            c, o = self.client(tok)
            with self.assertRaises(fl.FlightUnauthenticatedError):
                c.do_get(fl.Ticket(b"SELECT 1"), o).read_all()
            with self.assertRaises(fl.FlightUnauthenticatedError):
                list(c.do_action(fl.Action("connections", b""), o))


class StartErrorTest(unittest.TestCase):
    """A source that can't be attached: one line naming it and where it
    points, without the forwarder's local address."""

    def test_names_the_source(self):
        cfg = dict(CFG, sources=[{"name": "crm", "type": "postgresql", "secret": "pw",
                                  "settings": {"host": "pg.invalid", "port": 5432, "database": "crm", "user": "reader", "tls": "off"}}])
        with self.assertRaises(Exception) as ctx:
            fetcher.connect(cfg)
        msg = fetcher.start_error(ctx.exception)
        self.assertTrue(msg.startswith("crm (postgresql, pg.invalid:5432): "), msg)
        self.assertNotIn("127.0.0.1", msg)
        self.assertNotIn("hostaddr", msg)
        self.assertNotIn("\n", msg)

    def test_the_gateways_reason_wins(self):
        # The driver sees only its connection close; the gateway said why.
        fetcher._attaching.update(name="crm", type="postgresql", target="pg.invalid:5432")
        fetcher._refused["pg.invalid:5432"] = "the gateway refused pg.invalid:5432 (502 Bad Gateway): lookup pg.invalid: no such host"
        try:
            msg = fetcher.start_error(Exception("IO Error: server closed the connection unexpectedly"))
        finally:
            fetcher._attaching.clear()
            fetcher._refused.clear()
        self.assertEqual(msg, "crm (postgresql, pg.invalid:5432): the gateway refused pg.invalid:5432 (502 Bad Gateway): lookup pg.invalid: no such host")


if __name__ == "__main__":
    unittest.main()
