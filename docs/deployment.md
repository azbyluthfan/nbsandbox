# Deploying nbsandbox

From a fresh Linux server to a working nbsandbox behind HTTPS, with Docker Compose. For Kubernetes, see [kubernetes.md](kubernetes.md); for several machines without it, [scaling.md](scaling.md). Every command is meant to be copied as is. If a step doesn't work as written, that's a documentation bug: please open an issue.

**Reference host:** Ubuntu 24.04 LTS, x86_64 or arm64, cgroup v2 (the default). Sizing is in [operations.md](operations.md#capacity); a start is 4 CPUs, 8 GB RAM and 50 GB disk for the default two concurrent runs of 2 GB each.

**What runs:** Caddy (TLS) → nbsandboxd → a Docker socket proxy → Docker, which starts one locked-down container per run. nbsandboxd keeps its records in PostgreSQL and file contents in an S3-compatible object store (RustFS), both bundled. Only Caddy is reachable from outside.

## 1. Get the release

```bash
sudo apt-get update && sudo apt-get install -y git make
git clone https://github.com/azbyluthfan/nbsandbox.git
cd nbsandbox
git checkout v0.1.0
```

## 2. Prepare the host

```bash
sudo deploy/provision/provision.sh
```

It installs Docker Engine, sets Docker's log defaults, makes sure uid 1000 exists, and creates `/srv/nbsandbox` (uid 1000, mode 700) and `/etc/nbsandbox` with the example settings. It only adds what's missing, so it's safe to run again.

## 3. Add users

In production, sign people in with your identity provider (Entra ID, Keycloak, Okta and so on): see [auth.md](auth.md), then turn basic auth off. For a first check, or a small private host, basic-auth users are enough. Each user needs a bcrypt hash. Run this once per user and type the password (at least 8 characters) twice:

```bash
sudo docker run --rm -it ghcr.io/azbyluthfan/nbsandboxd:0.1.0 hash-password
```

Then edit the users file:

```bash
sudo nano /etc/nbsandbox/users.yaml
```

```yaml
users:
  - username: alice
    password_hash: "$2a$10$..."   # the line hash-password printed
```

nbsandboxd refuses a users file that other accounts can read. The provisioning script already set it to `600`, owned by uid 1000.

## 4. Configure

```bash
cp deploy/.env.example deploy/.env
getent group docker | cut -d: -f3        # put this number in DOCKER_GID
nano deploy/.env
```

Set at least the following:
- **`NBSANDBOX_DOMAIN`:** the server's host name. Its DNS must point to this server.
- **`NBSANDBOX_TLS`:** your e-mail address, for Let's Encrypt (ports 80 and 443 must be reachable from the internet). For a private host without public DNS, use `internal`: Caddy then uses its own certificate authority, and clients need `-k` or Caddy's root certificate.
- **`DOCKER_GID`.**
- **`NBSANDBOX_DB_PASSWORD`:** the password of the bundled PostgreSQL, long and random (`openssl rand -hex 24`; hex, because it goes into a URL). The database lives in the Docker volume `nbsandbox_pgdata`. To use your own PostgreSQL instead, remove the `postgres` service and set `NBSANDBOX_DATABASE_URL` for nbsandboxd.
- **`NBSANDBOX_S3_SECRET_KEY`:** the secret key of the bundled object store, long and random (`openssl rand -hex 24`). File contents live in the Docker volume `nbsandbox_objects`. To use your own S3 store or Azure Blob Storage instead, remove the `rustfs` service and set `NBSANDBOX_STORAGE` and its settings for nbsandboxd (`storage:` in `settings.yaml` documents them).

Per-run limits, concurrency, retention and the rest are in `/etc/nbsandbox/settings.yaml`, which documents each setting.

## 5. Runner image

Releases publish the runner image on GHCR, so pulling it is enough:

```bash
sudo docker pull ghcr.io/azbyluthfan/nbsandbox-runner:0.1.0
```

To build it yourself instead (for example, to change the packages), use `make runner-image RUNNER_IMAGE=nbsandbox-runner:custom`, set `NBSANDBOX_RUNNER_IMAGE=nbsandbox-runner:custom` in `deploy/.env`, and see [operations.md](operations.md#runner-image).

## 6. Preflight

```bash
sudo NBSANDBOX_DATA_DIR=/srv/nbsandbox NBSANDBOX_USERS_FILE=/etc/nbsandbox/users.yaml \
  NBSANDBOX_RUNNER_IMAGE=ghcr.io/azbyluthfan/nbsandbox-runner:0.1.0 scripts/preflight.sh
```

Every line should say `ok`. It checks Docker, cgroup v2, the runner image, the data directory's owner and mode, free disk, and the users file's permissions.

## 7. Start

```bash
sudo docker compose -f deploy/docker-compose.yml --env-file deploy/.env up -d --wait
```

`--wait` returns once nbsandboxd reports ready, which includes Docker, the runner image, the database and the data directory.

## 8. Check

From any machine:

```bash
curl -s -u alice:YOUR_PASSWORD https://nbsandbox.example.com/v1/runtime | head -c 300
```

A full round trip with the example client:

```bash
export NBSANDBOX_URL=https://nbsandbox.example.com NBSANDBOX_USER=alice NBSANDBOX_PASSWORD=YOUR_PASSWORD
ID=$(scripts/nbsandbox.sh spaces create first | sed 's/.*"id":"\([^"]*\)".*/\1/')
scripts/nbsandbox.sh files upload "$ID" tests/workloads/smoke.ipynb
RUN=$(scripts/nbsandbox.sh runs start "$ID" smoke.ipynb | sed 's/.*"id":"\([^"]*\)".*/\1/')
scripts/nbsandbox.sh runs follow "$ID" "$RUN"     # the live log, ending with [done] {"status":"succeeded"}
```

## 9. Verify the isolation on this host (recommended)

The integration suite, including the isolation report, runs against a separate dev stack on the same host. It doesn't touch the production stack or its data:

```bash
sudo apt-get install -y golang-go     # or Go 1.27.3+ from go.dev
sudo make runner-image
sudo NBSANDBOX_DATA_DIR=/srv/nbsandbox-test DOCKER_GID=$(getent group docker | cut -d: -f3) make dev-data
sudo chown -R 1000:1000 /srv/nbsandbox-test
sudo NBSANDBOX_DATA_DIR=/srv/nbsandbox-test NBSANDBOX_PORT=18080 NBSANDBOX_UI_PORT=18090 NBSANDBOX_URL=http://127.0.0.1:18080 \
  NBSANDBOX_UI_URL=http://127.0.0.1:18090 DOCKER_GID=$(getent group docker | cut -d: -f3) NBSANDBOX_CPU_FAIRNESS_GATE=1 make report
```

`docs/isolation-report.md` is then the reference-platform report for this host. Stop the test stack afterwards:

```bash
sudo NBSANDBOX_DATA_DIR=/srv/nbsandbox-test make down
```

## Next

- **Operations:** monitoring, backups, upgrades and runbooks are in [operations.md](operations.md).
- **Using nbsandbox:** see [user-guide.md](user-guide.md).
- **gVisor**, for stronger isolation: install `runsc` and set `docker.runtime: runsc` ([gvisor.md](gvisor.md)). It costs about a second more per run and slower small-file work, and passes the same isolation report.

## Not for production

`make up`, `make run-native` and the local web UI are development tools. The web UI acts as any user without a password, and the production Compose file never enables it.
