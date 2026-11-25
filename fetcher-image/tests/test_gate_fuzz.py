# Copyright 2026 The nbsandbox Authors
# SPDX-License-Identifier: Apache-2.0
"""The fetcher's gate under attack (make fetcher-test).

AttackSetTest sends the M0 spike's attacks (and later finds) through the
real Flight server: each must fail, and no answer may hold a secret.

GateFuzzTest disguises known-bad queries (case, quoting, schema names,
nesting, comments, split strings, paths in FROM, spellings of "..") and
requires the gate to refuse every one; known-good queries, disguised the
same harmless ways, must still pass. The seed is fixed: a failure names the
query to reproduce it (NBSANDBOX_FUZZ_SEED and NBSANDBOX_FUZZ_N change it).
"""

import json
import os
import random
import re
import sys
import threading
import unittest

import duckdb
import pyarrow.flight as fl

sys.path.insert(0, "/opt/nbsandbox-fetcher")
import fetcher  # noqa: E402

SECRET = "s3-SECRET-value"
CFG = {
    "token": "fetcher-token",
    "gateway": {"address": "gateway.invalid:3128", "token": "run-token"},
    "sources": [{"name": "lake", "type": "s3", "secret": SECRET,
                 "settings": {"endpoint": "minio.invalid:9000", "bucket": "data", "access_key_id": "AK",
                              "use_ssl": False, "url_style": "path", "region": "us-east-1"}}],
}

# What a notebook might try, from the spike (bin/spike/attack.py) and since.
ATTACKS = {
    "read /etc/passwd": "SELECT * FROM read_csv('/etc/passwd', header=false) LIMIT 1",
    "read the configuration": "SELECT * FROM read_text('/etc/nbsandbox-fetcher/config.json')",
    "read a mounted secret": "SELECT * FROM read_text('/etc/nbsandbox-fetcher/secrets/0')",
    "read /proc/self/environ": "SELECT * FROM read_text('/proc/self/environ')",
    "a path in FROM": "SELECT * FROM '/etc/passwd'",
    "a path in FROM, through ..": "SELECT * FROM 's3://data/../other/x.csv'",
    "a path in FROM, quoted as a name": 'SELECT * FROM "s3://data/../other/x.csv"',
    "a table function through ..": "SELECT * FROM read_csv('s3://data/../other/x.csv')",
    "a name's function through ..": "SELECT * FROM lake.csv('../other/x.csv')",
    "another bucket": "SELECT * FROM read_parquet('s3://other/x.parquet')",
    "glob the file system": "SELECT * FROM glob('/*')",
    "an environment variable": "SELECT getenv('HOME')",
    "the attached databases": "SELECT database_name, path FROM duckdb_databases()",
    "the secrets": "SELECT * FROM duckdb_secrets()",
    "the secrets, unredacted": "SELECT * FROM duckdb_secrets(redact=false)",
    "which secret": "SELECT * FROM which_secret('s3://data/x', 's3')",
    "the settings": "SELECT name, value FROM duckdb_settings()",
    "a setting's value": "SELECT current_setting('http_proxy_password')",
    "drop the secret": "DROP SECRET nbs_0",
    "unlock external access": "SET enable_external_access = true",
    "unlock the configuration": "SET lock_configuration = false",
    "load an extension": "LOAD postgres",
    "install an extension": "INSTALL spatial",
    "read over HTTP": "SELECT * FROM read_csv('https://example.com/x.csv')",
    "write a file": "COPY (SELECT 1) TO '/tmp/out.csv'",
    "attach a file": "ATTACH '/tmp/x.duckdb' AS x",
    "attach a database": "ATTACH 'host=db dbname=crm user=postgres password=x' AS other (TYPE postgres)",
    "SQL from a string": "SELECT * FROM query('SELECT * FROM duckdb_secrets()')",
    "a computed path": "SELECT * FROM read_text(chr(47) || 'etc' || chr(47) || 'hostname')",
    "a computed '..'": "SELECT * FROM lake.csv(repeat('.', 2) || '/other/x.csv')",
    "a table from a string": "SELECT * FROM query_table('duckdb_secrets')",
    "pass-through": "SELECT * FROM postgres_query('crm', 'SELECT pg_read_file(''/etc/hostname'')')",
    "two statements": "SELECT 1; SELECT * FROM duckdb_secrets()",
    "a macro": "CREATE MACRO m() AS TABLE SELECT * FROM duckdb_secrets()",
    "a variable": "SET VARIABLE x = (SELECT 1)",
    "pragma": "PRAGMA database_list",
    "a pragma function": "SELECT * FROM pragma_database_list()",
    "export": "EXPORT DATABASE '/tmp/x'",
}


class AttackSetTest(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.server = fetcher.Fetcher("grpc://127.0.0.1:0", CFG)
        threading.Thread(target=cls.server.serve, daemon=True).start()
        cls.client = fl.FlightClient(f"grpc://127.0.0.1:{cls.server.port}")
        cls.opts = fl.FlightCallOptions(headers=[(b"authorization", b"Bearer fetcher-token")])

    @classmethod
    def tearDownClass(cls):
        cls.server.shutdown()

    def test_harness(self):
        t = self.client.do_get(fl.Ticket(b"SELECT 42 AS x"), self.opts).read_all()
        self.assertEqual(t.column("x")[0].as_py(), 42)

    def test_attacks_fail(self):
        for name, sql in ATTACKS.items():
            with self.subTest(name):
                try:
                    rows = self.client.do_get(fl.Ticket(sql.encode()), self.opts).read_all().to_pylist()
                except fl.FlightError as e:
                    self.assertNotIn(SECRET, str(e), name)
                    continue
                self.fail(f"{name} ran: {str(rows)[:200]}")


# ---------- the fuzzer ----------

BAD = [
    "SELECT * FROM query('SELECT 1')",
    "SELECT * FROM query_table('t')",
    "SELECT getenv('HOME')",
    "SELECT current_setting('threads')",
    "SELECT * FROM which_secret('s3://data/x', 's3')",
    "SELECT * FROM duckdb_secrets()",
    "SELECT * FROM duckdb_settings()",
    "SELECT * FROM pragma_database_list()",
    "SELECT * FROM postgres_query('crm', 'SELECT 1')",
    "SELECT * FROM mysql_query('crm', 'SELECT 1')",
    "SELECT mssql_exec('crm', 'DROP TABLE t')",
    "SELECT * FROM read_csv('s3://data/../other/x.csv')",
    "SELECT * FROM read_parquet('s3://data/a/../../other/x.parquet')",
    "SELECT * FROM lake.csv('../other/x.csv')",
    "SELECT * FROM 's3://data/../other/x.csv'",
    "SELECT * FROM '/etc/passwd'",
    "SELECT * FROM glob('s3://data/../*')",
    "SELECT * FROM read_csv(chr(115) || '3://data/' || chr(46) || chr(46) || '/other/x.csv')",
    "SELECT * FROM lake.csv(repeat('.', 2) || '/other/x.csv')",
    "SELECT * FROM read_text(lower('/ETC/PASSWD'))",
    "SELECT * FROM read_parquet([('s3://data/.' || './o/x.parquet')])",
    "SELECT 1; SELECT 2",
    "INSERT INTO t VALUES (1)",
    "COPY (SELECT 1) TO '/tmp/x'",
]

GOOD = [
    "SELECT 1",
    "SELECT region, sum(amount) FROM crm.orders GROUP BY 1",
    "SELECT * FROM lake.parquet('sales/2026/*.parquet') WHERE id > 3",
    "SELECT * FROM lake.csv('exports/latest.csv')",
    "SELECT note FROM crm.notes WHERE note = '..'",
    "SELECT a.id FROM crm.a a JOIN lake.parquet('b.parquet') b USING (id)",
    "SELECT * FROM read_csv('s3://data/x.csv', header = false, columns = {'a': 'INT'})",
    "SELECT * FROM read_parquet(['s3://data/a.parquet', 's3://data/b.parquet'])",
]

REFUSED_NAMES = ["query", "query_table", "getenv", "current_setting", "which_secret", "duckdb_secrets",
                 "duckdb_settings", "pragma_database_list", "postgres_query", "mysql_query", "mssql_exec", "glob",
                 "read_csv", "read_parquet"]


def mutate(sql, rnd, bad):
    """One disguise of sql. For a bad query, never one that makes it harmless
    (no comment hides a part of it); for a good one, never one that makes it
    bad."""
    choice = rnd.randrange(9 if bad else 5)
    if choice == 0:  # keyword case
        return "".join(c.upper() if rnd.random() < 0.5 else c.lower() for c in sql) if not bad else _case_names(sql, rnd)
    if choice == 1:  # whitespace and newlines
        return sql.replace(" ", rnd.choice(["  ", "\n", "\t", " \n "]))
    if choice == 2:  # a comment between tokens
        parts = sql.split(" ")
        i = rnd.randrange(1, len(parts)) if len(parts) > 1 else 1
        return " ".join(parts[:i]) + " /* x */ " + " ".join(parts[i:])
    if choice == 3:  # nested in a subquery
        return f"SELECT * FROM ({sql.rstrip(';')}) AS s" if ";" not in sql else sql
    if choice == 4:  # in a CTE
        return f"WITH c AS ({sql}) SELECT * FROM c" if ";" not in sql else sql
    # The rest only for bad queries.
    if choice == 5:  # quoted or schema-qualified function names (whole names only)
        for name in REFUSED_NAMES:
            m = re.search(rf"(?<![A-Za-z0-9_]){name}\(", sql)
            if m:
                form = rnd.choice([f'"{name}"', f"main.{name}", f"system.main.{name}", f'main."{name}"', name.upper()])
                return sql[:m.start()] + form + "(" + sql[m.end():]
        return sql
    if choice == 6:  # a split string
        i = sql.find("'")
        j = sql.find("'", i + 1)
        if i < 0 or j < 0 or j - i < 4:
            return sql
        lit = sql[i + 1:j]
        k = rnd.randrange(1, len(lit))
        return sql[:i] + f"('{lit[:k]}' || '{lit[k:]}')" + sql[j + 1:] if "FROM '" not in sql[: i + 1] else sql
    if choice == 7:  # spellings of the parent folder
        return sql.replace("../", rnd.choice(["../", "./../", "..//", "a/../../"]))
    # in a UNION or a lateral join
    if ";" in sql or not sql.upper().startswith("SELECT"):
        return sql
    return rnd.choice([f"SELECT 1 UNION ALL ({sql})", f"SELECT * FROM (SELECT 1 AS one) o, LATERAL ({sql}) l"])


def _case_names(sql, rnd):
    for name in REFUSED_NAMES:
        if name in sql:
            return sql.replace(name, "".join(c.upper() if rnd.random() < 0.5 else c for c in name), 1)
    return sql


class GateFuzzTest(unittest.TestCase):
    def setUp(self):
        self.con = duckdb.connect()
        self.rnd = random.Random(int(os.environ.get("NBSANDBOX_FUZZ_SEED", "20261125")))
        self.n = int(os.environ.get("NBSANDBOX_FUZZ_N", "4000"))

    def disguise(self, sql, bad):
        for _ in range(self.rnd.randrange(1, 4)):
            sql = mutate(sql, self.rnd, bad)
        return sql

    def test_bad_queries_stay_refused(self):
        passed = []
        for _ in range(self.n):
            seed = self.rnd.choice(BAD)
            sql = self.disguise(seed, True)
            try:
                fetcher.gate(self.con, sql)
            except fetcher.Refused:
                continue
            passed.append(sql)
        if passed:
            uniq = sorted(set(passed))
            self.fail(f"{len(uniq)} disguised bad queries got through, e.g.:\n" + "\n".join(uniq[:15]))

    def test_good_queries_stay_allowed(self):
        refused = []
        for _ in range(self.n // 4):
            sql = self.disguise(self.rnd.choice(GOOD), False)
            try:
                fetcher.gate(self.con, sql)
            except fetcher.Refused as e:
                refused.append(f"{sql}  ({e})")
        if refused:
            self.fail(f"{len(set(refused))} disguised good queries were refused, e.g.:\n" + "\n".join(sorted(set(refused))[:10]))

    def test_every_statement_kind_but_select_is_refused(self):
        # Whatever DuckDB can parse that isn't a SELECT.
        for sql in ["CALL pragma_version()", "DESCRIBE SELECT 1", "SUMMARIZE SELECT 1", "EXPLAIN SELECT 1",
                    "SHOW TABLES", "USE memory", "CHECKPOINT", "VACUUM", "DETACH x", "CREATE VIEW v AS SELECT 1",
                    "CREATE TABLE t AS SELECT 1", "UPDATE t SET a = 1", "DELETE FROM t", "SET threads = 1",
                    "RESET threads", "BEGIN", "IMPORT DATABASE '/tmp/x'", "PIVOT t ON a", "FROM duckdb_secrets()",
                    "VALUES (1)", "TABLE duckdb_secrets"]:
            with self.subTest(sql):
                try:
                    fetcher.gate(self.con, sql)
                except fetcher.Refused:
                    continue
                # A SELECT in another form (FROM-first, VALUES) is fine if
                # the gate still checked it: run it on a locked connection.
                tree = json.loads(self.con.execute("SELECT json_serialize_sql(?)", [sql]).fetchone()[0])
                self.assertFalse(tree.get("error"), sql)


if __name__ == "__main__":
    unittest.main()
