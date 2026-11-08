#!/bin/sh
# Copyright 2026 The nbsandbox Authors
# SPDX-License-Identifier: Apache-2.0
#
# Builds web/vendor/codemirror.js from entry.js with the exact versions in
# package-lock.json. Runs inside a pinned Node container (make ui-vendor).
set -eu
# Work on a copy: node_modules never lands in the repository.
mkdir -p /work && cp /src/package.json /src/entry.js /work/
[ -f /src/package-lock.json ] && cp /src/package-lock.json /work/
cd /work
if [ -f package-lock.json ]; then npm ci --no-audit --no-fund; else npm install --no-audit --no-fund && cp package-lock.json /src/; fi
npx --no-install esbuild entry.js --bundle --format=esm --minify --legal-comments=none \
  --target=es2020 --outfile=/out/codemirror.js
# Licences of everything bundled, for NOTICE purposes.
node -e '
const lock = require("./package-lock.json"), fs = require("fs");
const out = [];
for (const [p, m] of Object.entries(lock.packages)) {
  if (!p || m.dev && p !== "node_modules/esbuild") continue;
  if (!p.startsWith("node_modules/") || p.includes("esbuild")) continue;
  out.push(`${p.slice(13)} ${m.version} ${m.license || "?"}`);
}
fs.writeFileSync("/out/LICENSES.txt", "Packages installed to build codemirror.js (name version licence; unused ones are left out of the bundle):\n" + out.sort().join("\n") + "\n");
'
cd /out && sha256sum codemirror.js LICENSES.txt > SHA256SUMS
