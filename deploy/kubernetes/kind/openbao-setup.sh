#!/bin/sh
# Copyright 2026 The nbsandbox Authors
# SPDX-License-Identifier: Apache-2.0
#
# Configures the development OpenBao (openbao.yaml) as an operator would
# for nbsandbox (docs/connections.md#vault): Kubernetes auth, a role for
# nbsandboxd's API processes and one for the fetchers, each with a policy,
# and a secret under teams/ for references. Usage: openbao-setup.sh "<kubectl>"
set -eu
KUBECTL=$1
bao() { $KUBECTL -n vault exec -i deploy/openbao -- env BAO_ADDR=http://127.0.0.1:8200 BAO_TOKEN=dev-root bao "$@"; }
bao auth list | grep -q '^kubernetes/' || bao auth enable kubernetes
bao write auth/kubernetes/config kubernetes_host=https://kubernetes.default.svc:443 >/dev/null
bao policy write nbsandbox - <<'P'
path "secret/data/nbsandbox/connections/*"     { capabilities = ["create", "update"] }
path "secret/metadata/nbsandbox/connections/*" { capabilities = ["read", "delete"] }
path "secret/metadata/teams/*"                 { capabilities = ["read"] }
P
bao policy write nbsandbox-fetcher - <<'P'
path "secret/data/nbsandbox/connections/*" { capabilities = ["read"] }
path "secret/data/teams/*"                 { capabilities = ["read"] }
P
bao write auth/kubernetes/role/nbsandbox bound_service_account_names=nbsandbox-api \
	bound_service_account_namespaces=nbsandbox token_policies=nbsandbox token_ttl=1h >/dev/null
bao write auth/kubernetes/role/nbsandbox-fetcher bound_service_account_names=nbsandbox-fetcher \
	bound_service_account_namespaces=nbsandbox-runs audience=vault token_policies=nbsandbox-fetcher token_ttl=10m >/dev/null
bao kv put secret/teams/data/crm password=reader-SECRET-pw >/dev/null
echo "openbao: configured"
