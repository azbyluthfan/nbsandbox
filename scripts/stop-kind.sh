#!/bin/sh
# Copyright 2026 The nbsandbox Authors
# SPDX-License-Identifier: Apache-2.0
#
# Stops every local nbsandbox kind cluster (nbsandbox, nbsandbox-calico,
# nbsandbox-kindnet): they publish 127.0.0.1:8080 like the Compose stacks.
# They keep their state; `make k8s-up` starts one again.
for id in $(docker ps -q --filter 'label=io.x-k8s.kind.role=control-plane' --filter 'name=^nbsandbox'); do
	docker stop "$id" >/dev/null
done
exit 0
