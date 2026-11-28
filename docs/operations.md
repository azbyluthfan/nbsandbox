# Operating nbsandbox

Monitoring, backups, upgrades, capacity and runbooks. The commands assume the production Compose stack from [deployment.md](deployment.md), run from the repository folder, with this shell function defined (it works in bash and zsh):

```bash
compose() { sudo docker compose -f deploy/docker-compose.yml --env-file deploy/.env "$@"; }
```

## Monitoring

- **Health:** nbsandboxd's container healthcheck runs `nbsandboxd health`, which asks `/readyz`. `compose ps` shows `healthy` or `unhealthy`.
- **Readiness details:** each failing check names its reason.
  ```bash
  compose exec nbsandboxd /nbsandboxd health   # exit 0 = ready; otherwise prints the failing checks
  ```
- **Metrics:** nbsandboxd's `/metrics` is internal (Caddy doesn't forward it). The optional Prometheus profile scrapes it and loads the example alerts:
  ```bash
  compose --profile monitoring up -d prometheus      # then http://127.0.0.1:9090 on the host
  ```
  `deploy/observability/alerts.yml` alerts on nbsandboxd being down, Docker unreachable, a queue backlog, platform errors, and waves of out-of-memory runs. `deploy/observability/grafana-dashboard.json` is an example dashboard: import it into Grafana with a Prometheus data source. `nbsandbox_run_phase_seconds` breaks each run's time down by phase (queued, prepare, container start, kernel start, execute, render, teardown, record); a run's own breakdown is in its result's `usage.phases_seconds`.
- **Logs:** `compose logs -f nbsandboxd` gives JSON lines. Every run line carries `run_id` and `space_id`; credentials and parameter values are never logged.
- **Audit log failures:** `nbsandbox_audit_failures_total` counts audit rows that couldn't be written (each is also logged as an error). It should stay at 0.

## Audit log

nbsandboxd records who did what, when, and how it ended:
- **every change:** spaces, files, folders, functions, invocations, runs, promoted artifacts, connections and a space's names, and a shared space's members (grants, changes, revocations, hand-overs);
- **every download:** files, and a run's notebook, HTML, artifacts and log (`audit.downloads`);
- **every administrative operation,** reading the audit log included;
- **web UI sign-ins and sign-outs.**

Refused attempts are recorded too (a `403`, including a role too low in a shared space, or a `404` on a space the caller has no role in), so probing shows up. Listings and live streams are not recorded. Neither are requests without valid credentials: the service log has those, with the reason.

Each row has the time, the caller ID, how they signed in (`basic` or `oidc`), the token's application, the transport (`rest`, `grpc` or `ui`), the operation, the space, what it acted on (a path, a run, `function@version`), a detail (a created run's notebook, a move's destination, a new owner), the HTTP status (gRPC codes mapped), and the client's address.

```bash
curl -su root "https://$HOST/v1/admin/audit?actor=alice&since=2026-11-01T00:00:00Z" | jq '.items[] | [.at, .action, .space_id, .target, .status] | @tsv'
curl -su root "https://$HOST/v1/admin/audit?space=<space id>&action=DeleteFile"
```

Filters: `actor`, `space`, `action`, `since`, `until` (RFC 3339); newest first, with `limit` and `cursor`. gRPC: `AdminService.AdminListAudit`. Each row also goes to the service log as a JSON line with `"msg":"audit"`, for a log shipper (Loki, Elasticsearch, a SIEM) that keeps it longer.

Rows are kept for `audit.retention` (90 days by default) and removed by the hourly cleanup. Turn recording off with `audit.enabled: false`; the rows already kept stay readable until they expire. A row that can't be written never fails the request: it is logged as an error and counted (`nbsandbox_audit_failures_total`).

## Backup

Everything worth keeping is in two places: the **database** (spaces, files and their versions, runs, functions) and the **object store** (the contents of every file, run output and published version, each stored once). The host data folder holds only the working copies of runs that are active right now, so it isn't backed up.

With the bundled PostgreSQL and RustFS:

```bash
sudo scripts/backup.sh /var/backups/nbsandbox
```

It writes two files: `nbsandbox-<time>.dump`, a consistent snapshot of the database (`pg_dump`), and `objects-<time>.tar.gz`, the object store's volume. It's safe while nbsandbox runs. Copy the backups off the host.

With your own services instead, back up each with its own tools: a managed PostgreSQL with its snapshots or `pg_dump`, an S3 bucket with versioning or replication, Azure Blob Storage with soft delete or object replication.

### Restore

This replaces the current data. Back it up first if you might want it back (the command above).

1. Stop nbsandbox, and remove its database and object store volumes:
   ```bash
   compose down
   sudo docker volume rm nbsandbox_pgdata nbsandbox_objects
   ```
2. Create the stack without starting it (this makes empty volumes), and put the object store's files back:
   ```bash
   compose create
   sudo docker run --rm -i -v nbsandbox_objects:/data --entrypoint tar \
     postgres:17-alpine@sha256:3540c0362c809df084ffac0ae69fe794515f9de92692f02178bcdce0ef0302ac \
     -C /data -xzf - < /var/backups/nbsandbox/objects-<time>.tar.gz
   ```
3. Start PostgreSQL alone and restore the database:
   ```bash
   compose up -d --wait postgres
   compose exec -T postgres pg_restore -U nbsandbox -d nbsandbox --clean --if-exists < /var/backups/nbsandbox/nbsandbox-<time>.dump
   ```
4. Start nbsandbox: `compose up -d --wait`.

Runs that were active when the backup was taken end as `interrupted` when nbsandbox starts; everything else is as it was.

**Drilled on 2026-11-08** with the production Compose stack (`make prod-local`): a space with files in folders, a finished run with an artifact, a published function and a call; a backup; then a file deleted, another added and a run started; then the restore above. Afterwards every file (version and bytes), run (status, output, artifact, log, rendered notebook) and function version matched the backup exactly, the changes made after it were gone, and new runs, calls and uploads worked. Rehearse it on your own test host before relying on it.

## Upgrade

Each release is tested as an upgrade from the one before it, with sessions open and runs active: data, results, published functions and open sessions survive, as described in step 5 (`make test-upgrade`, also in the weekly run).

1. Read the release notes and [CHANGELOG.md](../CHANGELOG.md).
2. **Back up first** (see above).
3. **Optional: drain**, so no run is interrupted: no queued run starts, running ones finish, and new runs still queue. No interactive session starts either; open ones keep running through the upgrade and are re-attached ([sessions.md](sessions.md#for-operators)). Wait until `running_runs` is 0. The drain is stored in the database, so it holds across the restart; resume afterwards (step 7). A long drain may set off the queue-backlog alert.
   ```bash
   curl -su root -X PUT "https://$HOST/v1/admin/drain" -H 'Content-Type: application/json' -d '{"draining":true,"reason":"upgrade to 0.5.0"}'
   curl -su root "https://$HOST/v1/admin/drain"     # {"draining":true, ..., "queued_runs":3,"running_runs":0}
   ```
4. Check out the new release and update `NBSANDBOX_IMAGE` and `NBSANDBOX_RUNNER_IMAGE` in `deploy/.env`.
5. Pull and restart:
   ```bash
   sudo docker pull <new nbsandboxd and runner images>
   compose up -d --wait
   ```
   - **Database migrations** run automatically at startup, inside a transaction each.
   - **Active runs:** nbsandboxd is stopped gracefully (`shutdown_grace`, 60 s by default). Runs still active after that end as `interrupted`, and queued runs stay queued and start after the upgrade.
   - **A new runner image** applies to runs started after the restart. Runs never switch images half-way.
6. If the new nbsandboxd reports `runner_image` as not ready, the runner image's status format isn't supported by this nbsandboxd. Use the runner image from the same release.
7. **If you drained, resume:** `curl -su root -X PUT "https://$HOST/v1/admin/drain" -H 'Content-Type: application/json' -d '{"draining":false}'`. The queued runs start.

## Connections

With connections on ([connections.md](connections.md#for-operators)):
- **Back up the master key** (`connections.secrets.master_key`, built-in store) with the backups above, but apart from them: the stored secrets are encrypted with it, so a backup without the key can't restore them, and anyone with both can read them. With the `kubernetes` or `vault` provider, the secrets live in Secrets or Vault: back those up as you do the rest of the cluster's.
- **The keys can't be rotated in place yet.** A new gateway key must go to nbsandboxd and the gateway together; runs and sessions reading at the time lose their fetcher's access (drain first). A new master key makes the stored secrets unreadable: users enter their connections' secrets again (or move to the `kubernetes` or `vault` provider, which keeps no key here).
- **The fetcher image** (`connections.fetcher_image`) is updated like the runner image: pin the release's, and new reading runs use it.
- **The gateway** keeps no state: restarting it is safe (fetchers' queries work again once it's back). Its log records each tunnel and request it allows or refuses (run, endpoint, bytes, duration); a refusal says why. It has no metrics of its own yet.
- **HTTP APIs:** each fetcher logs the requests it forwards (name, method, path without the query string, status, bytes, time) to its run's log, never bodies or credentials. An `oauth2` connection's token URL must be allowed (`connections.allowed`) as well as its API.
- **nbsandboxd warns** at startup when given the repository's development keys, which are public.
- **On Kubernetes,** the usage charts need metrics-server ([kubernetes.md](kubernetes.md#usage-while-runs-run)).

## Runner image

The runner image is the only environment notebooks run in. It is rebuilt monthly for security updates (Renovate opens the update):
- **patch:** rebuild only;
- **minor:** packages added;
- **major:** status format or Python major version.

To add packages for every space of your deployment (for one space's own, see [packages.md](packages.md)):
1. Edit `runner-image/requirements.in`.
2. Lock it with `make runner-lock`, which pins exact versions with hashes.
3. Build with `make runner-image RUNNER_IMAGE=nbsandbox-runner:custom BUILDER_IMAGE=nbsandbox-runner-builder:custom`. This also writes the package manifest label nbsandboxd needs, and builds the matching builder image. With custom packages on, set `packages.builder_image` to it: package sets must be built against the runner's exact packages.
4. Check with `make runner-test RUNNER_IMAGE=nbsandbox-runner:custom`.

## Capacity

| Resource | Rule of thumb | Basis |
|---|---|---|
| Memory | `runs.concurrency × limits.memory` + 1 GB for the host and nbsandboxd | default 2 × 2 GB → 5 GB, so 8 GB hosts |
| CPU | `runs.concurrency × limits.cpus` + 1 | each run is capped; a neighbour spinning every core slowed a run by at most 1.22× on Docker, 1.11× on Linux ([perf.md](perf.md)) |
| Disk: object store | `number of spaces × uploads.max_space` (500 MB default; includes run outputs and published function versions) at most; identical files are stored once | quota checked at upload, publish and submit, and every few seconds while a run writes (a run over quota is stopped) |
| Disk: host | `runs.concurrency × uploads.max_space` for the working copies of active runs, + images (~1.5 GB) | a run's copy is removed when it ends |
| Log volume | ≤ `limits.log_cap` (10 MB) per run in `log.txt`, plus at most 5 MB of output stored in `output.ipynb` | `endless-output` test |
| Live viewers | thousands of lines per second per viewer; 100 viewers on one run were measured at about 60 k lines/s | [perf.md](perf.md) |

Retention (`runs.retention`, 30 days) deletes old runs automatically. Raise `runs.concurrency` only together with memory.

## Runbooks

### nbsandboxd down
- **Symptom:** alert `NbsandboxDown`, or `compose ps` not healthy.
- **Check:** `compose logs --tail 100 nbsandboxd`. The usual causes are a bad setting (startup prints every problem at once), an unreadable users file, or the data directory's permissions.
- **Fix** the cause, then `compose up -d --wait`. Active runs at the time of a crash end as `interrupted` when nbsandboxd comes back, and their containers are removed.

### Docker down
- **Symptom:** alert `NbsandboxDockerDown`; `/readyz` says `docker unreachable`; new runs get `503 docker_unavailable`.
- **Check:** `systemctl status docker` and `compose ps socket-proxy`.
- **Fix:** restart Docker or the proxy. nbsandboxd reconnects on its own (with backoff up to 30 s), cleans up leftover containers, and resumes the queue. Runs that were active end as `interrupted` (a platform fault, not the notebook's).

### Disk full
- **Symptom:** `/readyz` says `data_dir: only N MiB free`; uploads or runs fail.
- **Check:** `df -h /srv/nbsandbox` (working copies of active runs, and the package sets in `packages/`, bounded by `packages.cache_size`) and `sudo docker system df -v | grep -E 'nbsandbox_(objects|pgdata)'` (the object store and the database). `GET /v1/admin/spaces` lists every owner's spaces; `GET /v1/spaces/{id}` shows a space's usage to its owner.
- **Fix:** lower `runs.retention` (old runs and their outputs go, and contents nothing refers to any more are removed an hour later), ask users to delete old runs or spaces, or grow the disk. Docker images can be pruned with `docker image prune` (not the runner image in use).

### A runaway run

- **Find it:** `GET /v1/admin/runs?status=running` lists every owner's running runs, with the owner and start time (metadata only: administrators don't see results or outputs).
- **Stop it:** `POST /v1/admin/runs/{run_id}/cancel` with `{"reason": "..."}`. The owner sees `cancelled_by` (the administrator) and the reason on the run. The cancel is in the audit log.

### A session holding resources

An interactive session ([sessions.md](sessions.md)) holds a container (or Pod) and its run limits for as long as it is open: until its user ends it, after `sessions.idle` with no cell running and nobody connected (30 minutes), or `sessions.max` after it started (8 hours).
- **Find it:** `GET /v1/admin/sessions` lists every open session: owner, worker, notebook, mode, since when.
- **End it:** `POST /v1/admin/sessions/{session_id}/end`, optionally with `{"reason": "..."}`. A read-write session's changes are written to the space first; its user sees who ended it. In the audit log.

### Queue backlog
- **Symptom:** alert `NbsandboxQueueBacklog`.
- **Check** `nbsandbox_active_runs` and whether runs are stuck: a run is bounded by `limits.timeout`, so nothing can run forever.
- **Fix:** raise `runs.concurrency` (with memory, see Capacity) or shorten `limits.timeout`.

### Platform errors
- **Symptom:** alert `NbsandboxPlatformErrors`; runs end as `platform_error` or `interrupted`.
- **Check:** the `message` of those runs, `compose logs nbsandboxd | grep run_id`, and that the runner image exists and is supported (`/readyz`).
- **Usual causes:** a missing or replaced runner image, Docker restarts, or a full disk.

### Leftover run containers
They shouldn't exist, because nbsandboxd removes them after every run and at reconnect. To look:
```bash
sudo docker ps -a --filter label=nbsandbox.run
```
Restarting nbsandboxd reconciles them. Never remove containers of runs that are still active.

### Users
With basic auth, edit `/etc/nbsandbox/users.yaml` to add a user or change a password (hashes from `nbsandboxd hash-password`) and restart nbsandboxd: `compose restart nbsandboxd`. With an identity provider there is nothing to do here: access follows the provider ([auth.md](auth.md)). Either way, removing someone doesn't delete their spaces: an administrator can give them to someone else or delete them (`/v1/admin/spaces`). Their roles in spaces others share stay until an owner revokes them; their bindings there go with the role. Owners of a shared space can hand it over themselves before they leave ([sharing.md](sharing.md)).

### TLS
With Let's Encrypt, Caddy renews certificates automatically; its state lives in the `caddy-data` volume. To change the host name, edit `NBSANDBOX_DOMAIN` and `compose up -d caddy`.
