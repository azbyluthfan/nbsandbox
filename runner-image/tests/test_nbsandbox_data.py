# Copyright 2026 The nbsandbox Authors
# SPDX-License-Identifier: Apache-2.0
"""nbsandbox.sql, stream and connections against a stand-in fetcher (make runner-test).

The stand-in speaks the fetcher contract (docs/plan-data-access.md): plain
Arrow Flight, a bearer token on every call, a DoGet ticket that is the SQL,
and a "connections" action."""

import json
import os
import sys
import threading
import unittest
from unittest import mock

import pyarrow as pa
import pyarrow.flight as fl

sys.path.insert(0, os.path.join(os.path.dirname(__file__), ".."))

import nbsandbox  # noqa: E402


class _Auth(fl.ServerMiddlewareFactory):
    def start_call(self, info, headers):
        if (headers.get("authorization") or [""])[0] != "Bearer tok":
            raise fl.FlightUnauthenticatedError("a valid fetcher token is needed")


class StandIn(fl.FlightServerBase):
    def __init__(self):
        super().__init__("grpc://127.0.0.1:0", middleware={"auth": _Auth()})

    def do_get(self, context, ticket):
        sql = ticket.ticket.decode()
        if "refuse" in sql:
            raise fl.FlightServerError("refused: query() isn't allowed")
        n = 100_000 if "big" in sql else 3
        t = pa.table({"n": pa.array(range(n), pa.int64()), "region": pa.array(["eu"] * n)})
        return fl.RecordBatchStream(pa.RecordBatchReader.from_batches(t.schema, t.to_batches(max_chunksize=40_000)))

    def do_action(self, context, action):
        yield fl.Result(json.dumps([{"name": "crm", "type": "postgresql"}]).encode())


class DataTest(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.server = StandIn()
        threading.Thread(target=cls.server.serve, daemon=True).start()
        cls.addr = f"grpc://127.0.0.1:{cls.server.port}"

    @classmethod
    def tearDownClass(cls):
        cls.server.shutdown()

    def setUp(self):
        nbsandbox._flight.clear()
        self.addCleanup(nbsandbox._flight.clear)

    def env(self, token="tok"):
        p = mock.patch.dict(os.environ, {"NBSANDBOX_FETCHER": self.addr, "NBSANDBOX_FETCHER_TOKEN": token})
        p.start()
        self.addCleanup(p.stop)

    def test_sql(self):
        self.env()
        df = nbsandbox.sql("SELECT n, region FROM crm.orders")
        self.assertEqual(list(df.columns), ["n", "region"])
        self.assertEqual(df["n"].tolist(), [0, 1, 2])

    def test_stream(self):
        self.env()
        batches = list(nbsandbox.stream("SELECT * FROM big"))
        self.assertGreater(len(batches), 1)
        self.assertTrue(all(isinstance(b, pa.RecordBatch) for b in batches))
        self.assertEqual(sum(b.num_rows for b in batches), 100_000)

    def test_connections(self):
        self.env()
        self.assertEqual(nbsandbox.connections(), [{"name": "crm", "type": "postgresql"}])

    def test_refused(self):
        self.env()
        with self.assertRaises(nbsandbox.QueryError) as e:
            nbsandbox.sql("SELECT * FROM query('refuse')")
        self.assertIn("refused: query() isn't allowed", str(e.exception))
        self.assertNotIn("gRPC", str(e.exception))
        with self.assertRaises(nbsandbox.QueryError):
            list(nbsandbox.stream("refuse"))

    def test_wrong_token(self):
        self.env(token="nope")
        with self.assertRaises(nbsandbox.QueryError):
            nbsandbox.sql("SELECT 1")
        with self.assertRaises(nbsandbox.QueryError):
            nbsandbox.connections()

    def test_fetcher_gone(self):
        import socket

        with socket.socket() as sock:  # a port nothing listens on
            sock.bind(("127.0.0.1", 0))
            port = sock.getsockname()[1]
        with mock.patch.dict(os.environ, {"NBSANDBOX_FETCHER": f"grpc://127.0.0.1:{port}", "NBSANDBOX_FETCHER_TOKEN": "tok"}):
            for call in (lambda: nbsandbox.sql("SELECT 1"), lambda: list(nbsandbox.stream("SELECT 1")), nbsandbox.connections):
                with self.assertRaises(nbsandbox.QueryError) as e:
                    call()
                self.assertIn("the fetcher can't be reached", str(e.exception))
                self.assertNotIn("127.0.0.1", str(e.exception))

    def test_no_fetcher(self):
        with mock.patch.dict(os.environ, {"NBSANDBOX_FETCHER": ""}):
            with self.assertRaisesRegex(nbsandbox.QueryError, "binds no names"):
                nbsandbox.sql("SELECT 1")
        with self.assertRaises(TypeError):
            self.env()
            nbsandbox.sql("")


if __name__ == "__main__":
    unittest.main()
