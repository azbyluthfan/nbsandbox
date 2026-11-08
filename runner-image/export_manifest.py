# Copyright 2026 The nbsandbox Authors
# SPDX-License-Identifier: Apache-2.0
"""Prints the runner image manifest as compact JSON.

`make runner-image` stores it as the image label io.nbsandbox.manifest, so it is
tied to the image digest. nbsandboxd reads it for GET /v1/runtime and for the
dependency check at submit time.
"""

import importlib.metadata as md
import json
import sys

# Must match STATUS_SCHEMA_VERSION in nbsandbox_runner.py.
STATUS_SCHEMA_VERSION = 2


def main():
    dists = {}
    for d in md.distributions():
        name = (d.metadata["Name"] or "").lower().replace("_", "-")
        if name:
            dists[name] = d.version
    imports = {}
    for module, owners in md.packages_distributions().items():
        if module.startswith("_") or not module.isidentifier():
            continue
        imports[module] = sorted({o.lower().replace("_", "-") for o in owners})
    manifest = {
        "schema_version": 1,
        "status_schema_version": STATUS_SCHEMA_VERSION,
        "python_version": sys.version.split()[0],
        "distributions": dict(sorted(dists.items())),
        "import_names": dict(sorted(imports.items())),
        # Importable without a distribution: the standard library, plus the
        # nbsandbox module this image provides (nbsandbox.output, nbsandbox.parameters).
        "stdlib": sorted([m for m in sys.stdlib_module_names if not m.startswith("_")] + ["nbsandbox"]),
    }
    json.dump(manifest, sys.stdout, separators=(",", ":"))


if __name__ == "__main__":
    main()
