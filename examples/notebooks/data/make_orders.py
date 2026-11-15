# Copyright 2026 The nbsandbox Authors
# SPDX-License-Identifier: Apache-2.0
"""Writes stores.csv and orders.csv: synthetic weekly sales of a made-up
chain of stores in Java, for the example notebooks (sales_dashboard.ipynb,
sales_map.ipynb). Deterministic: running it again gives the same files.

    python3 examples/notebooks/data/make_orders.py

Store locations are the real cities' approximate centres; everything else is
invented."""

import csv
import datetime
import math
import os
import random

HERE = os.path.dirname(os.path.abspath(__file__))

# store, city, province (as in java_provinces.geojson), lat, lon, size
STORES = [
    ("JKT-1", "Jakarta Pusat", "DKI Jakarta", -6.186, 106.834, 1.6),
    ("JKT-2", "Jakarta Selatan", "DKI Jakarta", -6.261, 106.810, 1.4),
    ("TNG-1", "Tangerang", "Banten", -6.178, 106.630, 1.0),
    ("SRG-1", "Serang", "Banten", -6.120, 106.150, 0.5),
    ("BGR-1", "Bogor", "Jawa Barat", -6.595, 106.816, 0.8),
    ("BDG-1", "Bandung", "Jawa Barat", -6.917, 107.619, 1.2),
    ("CRB-1", "Cirebon", "Jawa Barat", -6.732, 108.552, 0.5),
    ("SMG-1", "Semarang", "Jawa Tengah", -6.966, 110.420, 0.9),
    ("SOL-1", "Surakarta", "Jawa Tengah", -7.575, 110.824, 0.6),
    ("PWT-1", "Purwokerto", "Jawa Tengah", -7.424, 109.234, 0.4),
    ("YOG-1", "Yogyakarta", "DI Yogyakarta", -7.797, 110.370, 0.8),
    ("SBY-1", "Surabaya", "Jawa Timur", -7.257, 112.752, 1.3),
    ("MLG-1", "Malang", "Jawa Timur", -7.978, 112.630, 0.7),
    ("BWI-1", "Banyuwangi", "Jawa Timur", -8.219, 114.369, 0.3),
]

# category, price per unit (IDR thousands), weekly units at size 1, yearly seasonality
CATEGORIES = [
    ("coffee", 45, 300, 0.10),
    ("tea", 30, 220, 0.05),
    ("pastry", 25, 180, 0.15),
    ("merch", 120, 25, 0.35),
]


def main():
    rnd = random.Random(20261115)
    with open(os.path.join(HERE, "stores.csv"), "w", newline="") as f:
        w = csv.writer(f)
        w.writerow(["store", "city", "province", "lat", "lon"])
        for s, city, prov, lat, lon, _ in STORES:
            w.writerow([s, city, prov, lat, lon])

    start = datetime.date(2026, 1, 5)  # a Monday
    growth = {s[0]: rnd.uniform(-0.004, 0.012) for s in STORES}  # weekly trend per store
    with open(os.path.join(HERE, "orders.csv"), "w", newline="") as f:
        w = csv.writer(f)
        w.writerow(["week", "store", "category", "units", "revenue"])
        for week in range(26):
            day = start + datetime.timedelta(weeks=week)
            for s, _, _, _, _, size in STORES:
                trend = (1 + growth[s]) ** week
                # A dip in the fasting month (weeks 8-11), a bump in the holidays after it (12-13).
                season = 0.85 if 8 <= week <= 11 else 1.2 if 12 <= week <= 13 else 1.0
                for cat, price, base, amp in CATEGORIES:
                    wave = 1 + amp * math.sin(2 * math.pi * week / 26)
                    units = max(0, round(base * size * trend * season * wave * rnd.uniform(0.85, 1.15)))
                    revenue = round(units * price * rnd.uniform(0.95, 1.05), 1)
                    w.writerow([day.isoformat(), s, cat, units, revenue])


if __name__ == "__main__":
    main()
