# Copyright 2026 The nbsandbox Authors
# SPDX-License-Identifier: Apache-2.0
"""Tests for the nbsandbox module (run inside the runner image: make runner-test)."""

import datetime as dt
import decimal
import json
import os
import sys
import tempfile
import unittest
from unittest import mock

sys.path.insert(0, os.path.join(os.path.dirname(__file__), ".."))

import nbsandbox  # noqa: E402


class NbsandboxModuleTest(unittest.TestCase):
    def setUp(self):
        self.run = tempfile.mkdtemp()
        self.env = mock.patch.dict(os.environ, {"NBSANDBOX_RUN_DIR": self.run, "NBSANDBOX_MAX_OUTPUT": "4096"})
        self.env.start()

    def tearDown(self):
        self.env.stop()

    def written(self):
        with open(os.path.join(self.run, "output.json"), encoding="utf-8") as f:
            return json.load(f)

    def test_parameters(self):
        self.assertEqual(nbsandbox.parameters(), {})
        with open(os.path.join(self.run, ".nbsandbox-params.json"), "w") as f:
            f.write('{"big": 123456789012345678901234567890, "cfg": {"a": [1, null]}}')
        p = nbsandbox.parameters()
        self.assertEqual(p["big"], 123456789012345678901234567890)
        self.assertEqual(p["cfg"], {"a": [1, None]})

    def events(self):
        with open(os.path.join(self.run, ".nbsandbox-events.jsonl"), encoding="utf-8") as f:
            return [json.loads(line) for line in f]

    def test_emit_and_progress(self):
        with mock.patch.dict(os.environ, {"NBSANDBOX_OUTPUT_MODE": "stream"}):
            nbsandbox.emit({"day": dt.date(2026, 1, 2), "n": decimal.Decimal("1.5")})
            nbsandbox.emit([1, 2], total=2)
            nbsandbox.progress(0.25, "a quarter")
            nbsandbox.progress(1)
            self.assertEqual(self.events(), [
                {"kind": "item", "item": {"day": "2026-01-02", "n": 1.5}},
                {"kind": "item", "item": [1, 2], "total": 2},
                {"kind": "progress", "fraction": 0.25, "message": "a quarter"},
                {"kind": "progress", "fraction": 1.0, "message": ""},
            ])
            for bad in (0, -1, True, 1.5):
                with self.assertRaises(ValueError):
                    nbsandbox.emit(1, total=bad)
            with self.assertRaises(ValueError):
                nbsandbox.emit("x" * (70 << 10))
            with self.assertRaises(TypeError):
                nbsandbox.emit(object())
            with self.assertRaises(RuntimeError):  # the other mode's function
                nbsandbox.output(1)
        for bad in (-0.1, 1.5, True, "half"):
            with self.assertRaises(ValueError):
                nbsandbox.progress(bad)
        with self.assertRaises(TypeError):
            nbsandbox.progress(0.5, 3)
        with self.assertRaises(RuntimeError):  # single mode, the default
            nbsandbox.emit(1)
        self.assertEqual(len(self.events()), 4)

    def test_emit_is_paced(self):
        with mock.patch.dict(os.environ, {"NBSANDBOX_OUTPUT_MODE": "stream"}):
            import time
            start = time.monotonic()
            for i in range(5):
                nbsandbox.emit(i)
            self.assertGreaterEqual(time.monotonic() - start, 0.35)  # 10 a second

    def test_plain_values_and_replace(self):
        nbsandbox.output({"a": [1, 2.5, "x", True, None], "n": {"deep": {"er": 1}}})
        self.assertEqual(self.written(), {"a": [1, 2.5, "x", True, None], "n": {"deep": {"er": 1}}})
        nbsandbox.output([1, 2])
        self.assertEqual(self.written(), [1, 2])
        nbsandbox.output("just a string")
        self.assertEqual(self.written(), "just a string")
        self.assertFalse(os.path.exists(os.path.join(self.run, ".nbsandbox-output.tmp")))

    def test_conversions(self):
        value = {
            "when": dt.datetime(2026, 11, 6, 12, 30), "day": dt.date(2026, 11, 6),
            "dec": decimal.Decimal("1.25"), "t": (1, 2), "s": {3}, "nan": float("nan"), "inf": float("inf"),
            1: "int key",
        }
        self.assertEqual(self.written_after(value), {
            "when": "2026-11-06T12:30:00", "day": "2026-11-06", "dec": 1.25, "t": [1, 2], "s": [3],
            "nan": None, "inf": None, "1": "int key",
        })

    def written_after(self, value):
        nbsandbox.output(value)
        return self.written()

    def test_numpy_and_pandas(self):
        try:
            import numpy as np
            import pandas as pd
        except ImportError:  # pragma: no cover - always present in the runner image
            self.skipTest("numpy/pandas not installed")
        df = pd.DataFrame({"region": ["a", "b"], "value": [1.5, np.nan], "n": np.array([1, 2], dtype=np.int64)})
        got = self.written_after({
            "arr": np.arange(3), "f": np.float32(0.5), "i": np.int64(7), "b": np.bool_(True),
            "df": df, "s": pd.Series([1, 2]), "ts": pd.Timestamp("2026-11-06"),
        })
        self.assertEqual(got["arr"], [0, 1, 2])
        self.assertEqual(got["f"], 0.5)
        self.assertEqual(got["i"], 7)
        self.assertIs(got["b"], True)
        self.assertEqual(got["df"], [{"region": "a", "value": 1.5, "n": 1}, {"region": "b", "value": None, "n": 2}])
        self.assertEqual(got["s"], [1, 2])
        self.assertEqual(got["ts"], "2026-11-06T00:00:00")

    def test_rejects(self):
        for bad in (b"bytes", object(), {(1, 2): "tuple key"}):
            with self.assertRaises(TypeError):
                nbsandbox.output(bad)
        with self.assertRaises(ValueError):
            nbsandbox.output("x" * 5000)  # over NBSANDBOX_MAX_OUTPUT
        deep = []
        for _ in range(100):
            deep = [deep]
        with self.assertRaises(ValueError):
            nbsandbox.output(deep)
        self.assertFalse(os.path.exists(os.path.join(self.run, "output.json")))


if __name__ == "__main__":
    unittest.main()
