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
        cls.con, cls.served = fetcher.connect(CFG)

    def run_sql(self, q):
        return self.con.cursor().execute(q).fetchall()

    def test_serves_only_its_sources(self):
        self.assertEqual(self.served, [{"name": "lake", "type": "s3"}])

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
        self.assertEqual(got, [[{"name": "lake", "type": "s3"}]])

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


if __name__ == "__main__":
    unittest.main()
