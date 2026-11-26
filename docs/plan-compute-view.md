# Plan: the compute view

**Status (2026-11-26):** built as below: workers, samples on Docker, and samples on Kubernetes from metrics-server.

## Today

- Each worker process publishes itself in the `workers` table every third of a lease: up or not, what it runs (`GET /v1/runtime` as it sees it), the runner image's manifest, where API nodes reach its sessions. Only processes with the `worker` role publish; a single process (`all`, the dev stack and the production Compose file) doesn't.
- Each run records the worker that holds it (`runs.worker`).
- A run's page shows its usage once it ends: duration, peak memory, CPU seconds.
- Nothing shows the workers, which worker ran a run, or a run's memory, CPU and disk writes while it runs. Prometheus metrics cover processes in aggregate (`deploy/observability`).

## What users see

**A Compute page** in the web UI, open to every signed-in user, listing the workers:

| Column | From |
|---|---|
| Name, up or not, since when, last seen | the worker's own record |
| Backend and runtime (Docker with `runc` or gVisor, Kubernetes with its runtime class), nbsandbox version | the worker's own record |
| Runs: active of its capacity (`runs.concurrency`) | the worker's own count |
| Sessions: open of its capacity (`sessions.per_worker`) | the worker's own count |
| Memory and CPU in use by its runs and sessions, against what they reserve (their limits); disk writes per second | the sum of its containers' latest samples |

Counts and totals only: no other user's runs, spaces or names.

**Administrators** (`nbsandbox.admin`) also see each worker's active runs and sessions (owner, space, notebook, since when), and can filter the admin run list by worker (`GET /v1/admin/runs?worker=`).

**A run's page** names its worker, and charts its memory (against its limit), CPU (against its limit) and disk writes over its life: live while it runs, kept once it ends. On Kubernetes, without disk writes (below).

Not in this plan: charts for sessions (their usage counts in their worker's totals), the fetcher's usage (its own limits, beside the run), the hosts' or nodes' own load, and a history of workers that have gone.

## Workers

- **Every process that runs notebooks publishes itself,** `all` included, so a single-process deployment shows one worker. API-only processes don't.
- **The record gains** its role, version, start time, capacities, and a usage summary (JSON): active runs and sessions, memory and CPU in use and reserved, disk writes per second. The worker computes it from its own samples (below) each time it publishes.
- **Gone workers:** as today, a worker deletes its record when it stops, and records not seen for a day are removed. The page shows those not seen within three leases as gone, greyed out, until then.

## Samples

- **Who samples:** each worker, for the runs and sessions it holds, every 5 seconds on Docker and every 15 seconds on Kubernetes (metrics-server's own resolution).
- **Docker** (`runc` and gVisor): the stats API, one reading at a time (`stream=false`), which the socket proxy already allows. Memory is the working set, as `docker stats` shows it (usage less inactive file pages). CPU is cores used over the interval. Disk writes are the cgroup's written bytes.
- **Kubernetes:** the metrics API (`metrics.k8s.io`, `PodMetrics` in the runs namespace), when the cluster has metrics-server. It gives memory (working set) and CPU, no disk writes. The workers' Role gains `get` on `pods` in `metrics.k8s.io`. Without metrics-server, a run's page says no usage was sampled, and the end-of-run usage stays. The kind stack installs metrics-server, so the tests cover it.
- **What is stored:** a run's samples in a new table (`run_samples`: run, time, memory bytes, CPU millicores, written bytes), removed with the run. A run is sampled every 5 seconds (Docker) for its first hour, then every minute: a 10-hour run keeps about 1,300 samples. Session samples aren't stored; they only feed their worker's totals.
- **Cost:** one stats call per active container per interval. A sample that fails is skipped, never retried, and never affects the run.

## API

| | REST | gRPC |
|---|---|---|
| The workers | `GET /v1/workers` (scope `nbsandbox.read`) | `RunService.ListWorkers` |
| A run's worker | `worker` on the run, for its owner and administrators | `Run.worker` |
| A run's samples | `GET /v1/spaces/{id}/runs/{run_id}/usage`: the limits, the interval, and the samples | `RunService.GetRunUsage` (returns `RunSamples`) |
| The runs on a worker (administrators) | `GET /v1/admin/runs?worker=` | `AdminListRunsRequest.worker` |
| The sessions on a worker (administrators) | `GET /v1/admin/sessions?worker=` | `AdminListSessionsRequest.worker` |

## Milestones

1. **Workers:** every executing process publishes itself with its counts; `GET /v1/workers` and `ListWorkers`; the Compute page; a run's worker on its page; the admin filters.
2. **Samples:** Docker sampling, `run_samples`, `GET …/usage` and `GetRunUsage`, the run page's charts, the workers' totals.
3. **Kubernetes:** sampling from the metrics API, the Role, metrics-server in the kind stack.

Each with unit tests, integration tests on the Compose cluster stack (two workers) and on kind, the docs (`docs/scaling.md`, `docs/kubernetes.md`, the API reference) and the tour's screenshots.
