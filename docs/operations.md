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
  `deploy/observability/alerts.yml` alerts on nbsandboxd being down, Docker unreachable, a queue backlog, platform errors, and waves of out-of-memory runs. `deploy/observability/grafana-dashboard.json` is an example dashboard: import it into Grafana with a Prometheus data source.
- **Logs:** `compose logs -f nbsandboxd` gives JSON lines. Every run line carries `run_id` and `space_id`; credentials and parameter values are never logged.

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

1. Read the release notes and [CHANGELOG.md](../CHANGELOG.md).
2. **Back up first** (see above).
3. Check out the new release and update `NBSANDBOX_IMAGE` and `NBSANDBOX_RUNNER_IMAGE` in `deploy/.env`.
4. Pull and restart:
   ```bash
   sudo docker pull <new nbsandboxd and runner images>
   compose up -d --wait
   ```
   - **Database migrations** run automatically at startup, inside a transaction each.
   - **Active runs:** nbsandboxd is stopped gracefully (`shutdown_grace`, 60 s by default). Runs still active after that end as `interrupted`, and queued runs stay queued and start after the upgrade.
   - **A new runner image** applies to runs started after the restart. Runs never switch images half-way.
5. If the new nbsandboxd reports `runner_image` as not ready, the runner image's status format isn't supported by this nbsandboxd. Use the runner image from the same release.

## Runner image

The runner image is the only environment notebooks run in. It is rebuilt monthly for security updates (Renovate opens the update):
- **patch:** rebuild only;
- **minor:** packages added;
- **major:** status format or Python major version.

To add packages for your own deployment:
1. Edit `runner-image/requirements.in`.
2. Lock it with `make runner-lock`, which pins exact versions with hashes.
3. Build with `make runner-image RUNNER_IMAGE=nbsandbox-runner:custom`; this also writes the package manifest label nbsandboxd needs.
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
- **Check:** `df -h /srv/nbsandbox` (working copies of active runs) and `sudo docker system df -v | grep -E 'nbsandbox_(objects|pgdata)'` (the object store and the database). `GET /v1/admin/spaces` lists every owner's spaces; `GET /v1/spaces/{id}` shows a space's usage to its owner.
- **Fix:** lower `runs.retention` (old runs and their outputs go, and contents nothing refers to any more are removed an hour later), ask users to delete old runs or spaces, or grow the disk. Docker images can be pruned with `docker image prune` (not the runner image in use).

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
With basic auth, edit `/etc/nbsandbox/users.yaml` to add a user or change a password (hashes from `nbsandboxd hash-password`) and restart nbsandboxd: `compose restart nbsandboxd`. With an identity provider there is nothing to do here: access follows the provider ([auth.md](auth.md)). Either way, removing someone doesn't delete their spaces: an administrator can give them to someone else or delete them (`/v1/admin/spaces`).

### TLS
With Let's Encrypt, Caddy renews certificates automatically; its state lives in the `caddy-data` volume. To change the host name, edit `NBSANDBOX_DOMAIN` and `compose up -d caddy`.
