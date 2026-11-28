# Copyright 2026 The nbsandbox Authors
# SPDX-License-Identifier: Apache-2.0
"""The tour's space: the caller's own space with this exact name, reused
from an earlier tour (its finished runs removed, so the screenshots look the
same every time), or a new one. Prints its ID.

    python3 scripts/tour/space.py http://127.0.0.1:8080 alice:alice-dev-password "sales analysis"
"""

import base64
import json
import sys
import urllib.parse
import urllib.request

ACTIVE = {"queued", "running"}


def main():
    api, auth, name = sys.argv[1:4]
    user = auth.split(":", 1)[0]
    headers = {"Authorization": "Basic " + base64.b64encode(auth.encode()).decode()}

    def call(method, path, body=None):
        data = json.dumps(body).encode() if body is not None else None
        req = urllib.request.Request(api + path, data=data, method=method,
                                     headers={**headers, **({"Content-Type": "application/json"} if data else {})})
        with urllib.request.urlopen(req, timeout=30) as r:
            raw = r.read()
        return json.loads(raw) if raw else None

    def pages(path):
        cursor = ""
        while True:
            sep = "&" if "?" in path else "?"
            page = call("GET", f"{path}{sep}limit=100" + (f"&cursor={urllib.parse.quote(cursor)}" if cursor else ""))
            yield from page["items"]
            cursor = page.get("next_cursor") or ""
            if not cursor:
                return

    # Newest first: an earlier tour's space, if there are several.
    mine = [s for s in pages("/v1/spaces?role=owner") if s["name"] == name and s.get("owner") == user]
    if not mine:
        print(call("POST", "/v1/spaces", {"name": name})["id"])
        return
    space = mine[0]["id"]
    for r in list(pages(f"/v1/spaces/{space}/runs")):
        if r["status"] not in ACTIVE:
            call("DELETE", f"/v1/spaces/{space}/runs/{r['id']}")
    print(space)


if __name__ == "__main__":
    main()
