# Operating nbsandbox

Monitoring, backups, upgrades, capacity and runbooks. The commands assume the production Compose stack from [deployment.md](deployment.md), run from the repository folder:

```bash
COMPOSE="sudo docker compose -f deploy/docker-compose.yml --env-file deploy/.env"
```

## Monitoring

- **Health:** nbsandboxd's container healthcheck runs `nbsandboxd health`, which asks `/readyz`. `$COMPOSE ps` shows `healthy` or `unhealthy`.
- **Readiness details:** each failing check names its reason.
  ```bash
  $COMPOSE exec nbsandboxd /nbsandboxd health   # exit 0 = ready; otherwise prints the failing checks
  ```
- **Metrics:** nbsandboxd's `/metrics` is internal (Caddy doesn't forward it). The optional Prometheus profile scrapes it and loads the example alerts:
  ```bash
  $COMPOSE --profile monitoring up -d prometheus      # then http://127.0.0.1:9090 on the host
  ```
  `deploy/observability/alerts.yml` alerts on nbsandboxd being down, Docker unreachable, a queue backlog, platform errors, and waves of out-of-memory runs. `deploy/observability/grafana-dashboard.json` is an example dashboard: import it into Grafana with a Prometheus data source.
- **Logs:** `$COMPOSE logs -f nbsandboxd` gives JSON lines. Every run line carries `run_id` and `space_id`; credentials and parameter values are never logged.

## Backup

```bash
sudo NBSANDBOX_DATA_DIR=/srv/nbsandbox scripts/backup.sh /var/backups/nbsandbox
```

It writes `nbsandbox-<time>.dump`, a consistent snapshot of the PostgreSQL database (`pg_dump` in the `postgres` container), and `spaces-<time>.tar.gz`, the spaces with their files, run outputs and published function versions. It's safe while nbsandbox runs. A run writing at the same moment may be copied half-way, but only that run's own files are affected. Copy the backups off the host.

With your own PostgreSQL (a managed service, say) instead of the bundled one, back the database up with that service's tools and keep only step 2 of the script (the spaces archive).

### Restore

1. Stop nbsandbox:
   ```bash
   $COMPOSE down
   ```
2. Move the current data aside instead of deleting it:
   ```bash
   sudo mv /srv/nbsandbox /srv/nbsandbox.before-restore
   ```
3. Restore the spaces:
   ```bash
   sudo install -d -m 700 -o 1000 -g 1000 /srv/nbsandbox
   sudo tar -C /srv/nbsandbox -xzf /var/backups/nbsandbox/spaces-<time>.tar.gz
   sudo chown -R 1000:1000 /srv/nbsandbox
   ```
4. Restore the database into a fresh volume (the old one is kept until you remove it):
   ```bash
   docker volume rename nbsandbox_pgdata nbsandbox_pgdata_before_restore  # or docker volume create + copy, on older Docker
   $COMPOSE up -d --wait postgres
   $COMPOSE exec -T postgres pg_restore -U nbsandbox -d nbsandbox --clean --if-exists < /var/backups/nbsandbox/nbsandbox-<time>.dump
   ```
5. Start nbsandbox: `$COMPOSE up -d --wait`.

At startup nbsandboxd reconciles the two halves. Space folders without a database row are removed; rows without a folder are logged as errors. Runs that were active at backup time end as `interrupted`.

The SQLite version of this procedure was drilled on 2026-11-06; the PostgreSQL version still needs a drill (`pg_restore` into a fresh volume).

## Upgrade

1. Read the release notes and [CHANGELOG.md](../CHANGELOG.md).
2. **Back up first** (see above).
3. Check out the new release and update `NBSANDBOX_IMAGE` and `NBSANDBOX_RUNNER_IMAGE` in `deploy/.env`.
4. Pull and restart:
   ```bash
   sudo docker pull <new nbsandboxd and runner images>
   $COMPOSE up -d --wait
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
| CPU | `runs.concurrency × limits.cpus` + 1 | each run is capped; a neighbour's CPU hog slowed a run by 1.05× ([perf.md](perf.md)) |
| Disk | `number of active spaces × uploads.max_space` (500 MB default; includes run outputs and published function versions) + images (~1 GB) | checked at upload, publish and submit, and every few seconds while a run writes (a run over quota is stopped) |
| Log volume | ≤ `limits.log_cap` (10 MB) per run in `log.txt`, plus at most 5 MB of output stored in `output.ipynb` | `endless-output` test |
| Live viewers | thousands of lines per second per viewer; 100 viewers on one run were measured at about 60 k lines/s | [perf.md](perf.md) |

Retention (`runs.retention`, 30 days) deletes old runs automatically. Raise `runs.concurrency` only together with memory.

## Runbooks

### nbsandboxd down
- **Symptom:** alert `NbsandboxDown`, or `$COMPOSE ps` not healthy.
- **Check:** `$COMPOSE logs --tail 100 nbsandboxd`. The usual causes are a bad setting (startup prints every problem at once), an unreadable users file, or the data directory's permissions.
- **Fix** the cause, then `$COMPOSE up -d --wait`. Active runs at the time of a crash end as `interrupted` when nbsandboxd comes back, and their containers are removed.

### Docker down
- **Symptom:** alert `NbsandboxDockerDown`; `/readyz` says `docker unreachable`; new runs get `503 docker_unavailable`.
- **Check:** `systemctl status docker` and `$COMPOSE ps socket-proxy`.
- **Fix:** restart Docker or the proxy. nbsandboxd reconnects on its own (with backoff up to 30 s), cleans up leftover containers, and resumes the queue. Runs that were active end as `interrupted` (a platform fault, not the notebook's).

### Disk full
- **Symptom:** `/readyz` says `data_dir: only N MiB free`; uploads or runs fail.
- **Check:** `df -h /srv/nbsandbox` and `sudo du -sh /srv/nbsandbox/spaces/* | sort -h | tail`.
- **Fix:** lower `runs.retention`, ask users to delete old runs (`DELETE .../runs/{id}`), or grow the disk. Docker images can be pruned with `docker image prune` (not the runner image in use).

### Queue backlog
- **Symptom:** alert `NbsandboxQueueBacklog`.
- **Check** `nbsandbox_active_runs` and whether runs are stuck: a run is bounded by `limits.timeout`, so nothing can run forever.
- **Fix:** raise `runs.concurrency` (with memory, see Capacity) or shorten `limits.timeout`.

### Platform errors
- **Symptom:** alert `NbsandboxPlatformErrors`; runs end as `platform_error` or `interrupted`.
- **Check:** the `message` of those runs, `$COMPOSE logs nbsandboxd | grep run_id`, and that the runner image exists and is supported (`/readyz`).
- **Usual causes:** a missing or replaced runner image, Docker restarts, or a full disk.

### Leftover run containers
They shouldn't exist, because nbsandboxd removes them after every run and at reconnect. To look:
```bash
sudo docker ps -a --filter label=nbsandbox.run
```
Restarting nbsandboxd reconciles them. Never remove containers of runs that are still active.

### Users
To add a user or change a password, edit `/etc/nbsandbox/users.yaml` (hashes from `nbsandboxd hash-password`) and restart nbsandboxd: `$COMPOSE restart nbsandboxd`. Removing a user doesn't delete their spaces; they stay on disk until deleted.

### TLS
With Let's Encrypt, Caddy renews certificates automatically; its state lives in the `caddy-data` volume. To change the host name, edit `NBSANDBOX_DOMAIN` and `$COMPOSE up -d caddy`.
