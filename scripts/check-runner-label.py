# Copyright 2026 The nbsandbox Authors
# SPDX-License-Identifier: Apache-2.0
"""Checks a runner image's io.nbsandbox.manifest label the way nbsandboxd
reads it (internal/deps): valid JSON, the status format nbsandboxd supports,
and the same content as the manifest the image exported. The release
workflow runs it on the published image.

    python3 scripts/check-runner-label.py LABEL_FILE MANIFEST_FILE
"""

import json
import sys

SUPPORTED_STATUS_SCHEMA = 2  # deps.SupportedStatusSchema


def main(label_path, manifest_path):
    with open(label_path) as f:
        raw = f.read().strip()
    try:
        label = json.loads(raw)
    except ValueError as e:
        sys.exit(f"runner label is not valid JSON: {e}: ...{raw[max(0, e.pos - 40):e.pos + 40]}...")
    with open(manifest_path) as f:
        manifest = json.load(f)
    if label.get("status_schema_version") != SUPPORTED_STATUS_SCHEMA:
        sys.exit(f"runner label reports status format {label.get('status_schema_version')}; nbsandboxd supports {SUPPORTED_STATUS_SCHEMA}")
    if label != manifest:
        sys.exit("runner label differs from the manifest the image exported")
    print(f"runner label ok: Python {label['python_version']}, {len(label['distributions'])} distributions")


if __name__ == "__main__":
    if len(sys.argv) != 3:
        sys.exit(__doc__)
    main(sys.argv[1], sys.argv[2])
