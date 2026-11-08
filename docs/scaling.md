# Scaling out: API and worker roles

One nbsandboxd process can do everything (`role: all`, the default). To run more notebooks at once than one machine allows, or to keep the API up while workers come and go, run several processes with different roles. They share one PostgreSQL database and one object store and need nothing else in common: no shared filesystem, no sticky sessions.

| Role | Serves | Needs | Scale by |
|---|---|---|---|
| `api` | REST API, SSE streams, web UI (if enabled), health, metrics | PostgreSQL, object storage | adding replicas behind a load balancer |
| `worker` | health and metrics only | PostgreSQL, object storage, Docker (or later Kubernetes) | adding replicas; each runs `runs.concurrency` notebooks |
| `all` | everything | all of the above | not at all: one process |

Set it with `role` in `settings.yaml` or `NBSANDBOX_ROLE`.

## How the pieces talk

- **The queue is the `runs` table.** A worker claims the oldest queued run with `FOR UPDATE SKIP LOCKED`, so two workers never take the same run and never wait for each other. The claim is a **lease** (`runs.lease`, default 30 s) that the worker renews every third of it.
- **Fencing.** Every claim gets a new token. Everything a worker writes for a run (starting it, writing its changes back to the space, recording its outputs and its final status) only applies while the run still has that token. A worker that was cut off and comes back can't overwrite what another node decided.
- **A worker that goes away.** Any node notices an expired lease. A run the worker had claimed but not started goes back to the queue; a running one ends as `interrupted` (retryable, never the notebook's fault). Its container is removed when that worker returns, or by the next sweep of a worker on the same Docker host.
- **Notifications.** PostgreSQL `LISTEN/NOTIFY` wakes nodes early: a run was queued, a cancel was requested, a status event or live log lines were written. Every node also re-checks on a timer (2 s), so a lost notification costs a delay, never a missed run or event.
- **Cancel.** The API records the request, notifies, and finishes a still-queued run itself; the worker running a run stops it (on the notification, or at its next check).
- **Live logs.** A worker writes log lines to the database in small batches (every 200 ms or 64 KiB). Any API node streams them, and a client resuming with `Last-Event-ID` can reconnect to any API node. After the run, `log.txt` in object storage is the source and the batches are deleted.
- **What workers run.** Each worker publishes its runner image, packages and limits in the database. API nodes use the freshest one for `GET /v1/runtime` and the dependency check, and answer `503` to new runs while no worker can take them.
- **Periodic cleanup** (retention, unreferenced contents) runs on one node at a time.

## Tested failures

`make test-cluster-failures` runs these against the cluster stack (two API processes, two workers, 10 s leases); `make test-cluster` runs them with the whole integration suite:

| What fails | What happens |
|---|---|
| A worker is killed (SIGKILL) while both workers stream a run | Its run ends `interrupted` (retryable) within about one lease (7–10 s), keeping the items it had streamed (`partial`). The other worker's run finishes. The dead worker's container is removed, and the worker takes runs again when it's back. |
| A worker is stopped (SIGTERM) with a run queued behind both | Its run ends `interrupted` (`shutdown`) with its items. The queued run starts on the other worker. |
| Each API process is killed in turn while a client follows a streamed result through the load balancer | The client reconnects with `Last-Event-ID` and gets every item exactly once, then the result. |
| PostgreSQL is killed for about 3 s mid-run | The run finishes normally with every item and its complete status events. Writes that must not be lost (an item, the final status) retry for up to 20 s. New runs work as soon as it's back. |

Retention and the cleanup of unreferenced contents run on one node at a time (an advisory lock), checked by unit tests.

An outage longer than the lease ends the affected runs as `interrupted`: their workers can't prove they still hold them. Size `runs.lease` to the longest database failover you expect (a managed PostgreSQL failover can take a minute or more: check your provider's figure; the default lease is 30 s).

## Requirements

- `worker_id` must be unique per worker. The default, the host name, is unique per container and per Kubernetes pod. A worker restarted with the same ID takes back its own abandoned runs at once instead of waiting for the lease to expire.
- Workers on one Docker host may share a data folder: each run has its own folders in it. The path must be the same inside the worker container and on the host (Docker resolves bind mounts on the host).
- API nodes keep no run files; their data folder can be ephemeral.
- Keep `runs.lease` well above the database's worst failover time. A worker that can't renew its leases stops its runs, since nothing it does afterwards would be recorded.

## Trying it on one machine

```bash
make cluster-up
```

Two API processes behind a load balancer on `http://127.0.0.1:8080`, two workers, PostgreSQL, RustFS and the Docker socket proxy (`deploy/docker-compose.cluster.yml`). It uses its own data folder and volumes, and stops the dev stack, which uses the same port. `make test-cluster` runs the integration suite through it; `make cluster-down` stops it.

## Kubernetes

Same shape: an `api` Deployment behind a Service, a `worker` Deployment (or StatefulSet) with access to a container runtime, PostgreSQL, and S3-compatible storage or Azure Blob Storage. A Kubernetes backend for workers, which starts runs as pods instead of Docker containers, is on the roadmap.
