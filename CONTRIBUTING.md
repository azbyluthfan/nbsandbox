# Contributing to nbsandbox

Thanks for your interest. nbsandbox is early (pre-release); what's next is in [ROADMAP.md](ROADMAP.md). Users and operators start with the [README](README.md), [docs/user-guide.md](docs/user-guide.md), [docs/deployment.md](docs/deployment.md) and [docs/operations.md](docs/operations.md). Before starting anything large, open an issue so we can agree on the approach.

## Sign your commits (DCO)

Every commit must be signed off under the [Developer Certificate of Origin](https://developercertificate.org/). Signing off certifies that you wrote the change, or otherwise have the right to submit it under the project's licence (Apache-2.0). There is no CLA.

Add the sign-off with `-s`:

```bash
git commit -s -m "feat(runs): add queue position"
```

This appends a line like:

```
Signed-off-by: Your Name <you@example.com>
```

**Your sign-off email is public.** To keep your personal address private, use your GitHub no-reply address for both the commit author and the sign-off. Find it under GitHub *Settings → Emails*; it looks like `12345678+username@users.noreply.github.com`. Set it for this repo:

```bash
git config user.email "12345678+username@users.noreply.github.com"
```

Forgot to sign off? Fix the commits on your branch and force-push:

```bash
git rebase --signoff main
```

## Tools

Install these yourself; the project doesn't download them for you.

| Tool | Needed for | Install |
|---|---|---|
| Go 1.27+ | everything | https://go.dev/dl/ |
| golangci-lint v2 | `make lint` | https://golangci-lint.run/welcome/install/ (release binary or Homebrew; not `go install`) |
| govulncheck | `make vuln` | `go install golang.org/x/vuln/cmd/govulncheck@latest` |
| Docker Desktop (macOS) or Docker Engine (Linux) | integration tests | https://docs.docker.com/ |

`make tools` shows which of these are installed.

### Docker Desktop (macOS)

Docker Desktop is supported for development and local use; isolation reports from it are labelled "non-reference" (Linux is the reference platform).

- **Socket:** Desktop's socket is `~/.docker/run/docker.sock`, and Go programs don't read docker CLI contexts. When running nbsandboxd natively, set `DOCKER_HOST=unix://$HOME/.docker/run/docker.sock` (or `docker.host` in `settings.yaml`).
- **Data directory:** keep it inside a folder Desktop shares with containers. `/Users/...` is shared by default.
- **File ownership:** files the containers write show up owned by your macOS user, and folder permissions don't restrict the container the way they do on Linux. Permission-related tests adapt on Desktop.
- **Desktop shut down:** nbsandboxd keeps running and reports not-ready until Docker is back.

```bash
make runner-image   # build nbsandbox-runner:dev
make workloads      # regenerate tests/workloads/*.ipynb
```

## Everyday commands

```bash
make help        # list targets
make build       # build bin/nbsandboxd
make test        # unit tests, no Docker needed
make cover       # unit tests with the race detector, plus the coverage gate
make runner-test # runner image checks and wrapper unit tests (Docker)
make check       # licence headers, vet, lint, race tests, coverage gate: what a PR must pass
```

## Running nbsandboxd locally

Both ways use `~/.local/share/nbsandbox` as the data directory (override with `NBSANDBOX_DATA_DIR`) and install the demo users from `config/users.yaml`: **alice / alice-dev-password** and **bob / bob-dev-password**. These are for local development only.

```bash
make runner-image   # once: the image notebooks run in
make up             # nbsandboxd + Docker socket proxy in containers, on 127.0.0.1:8080
make down
```

```bash
make run-native     # nbsandboxd directly on your Mac against Docker Desktop (no proxy)
```

```bash
curl -s localhost:8080/readyz          # which checks pass
make preflight                         # host prerequisites
export NBSANDBOX_PASSWORD=alice-dev-password
scripts/nbsandbox.sh spaces create demo  # example client; run it without arguments for help
scripts/nbsandbox.sh files upload <space-id> tests/workloads/smoke.ipynb
scripts/nbsandbox.sh runs start <space-id> smoke.ipynb '{"region": "jakarta", "months": 4}'
scripts/nbsandbox.sh runs follow <space-id> <run-id>   # live log (Server-Sent Events) until the run ends
scripts/nbsandbox.sh runs log <space-id> <run-id>      # the log so far
```

### Two modes, both on Docker alone

| Mode | Start / stop | Logs | Wipe its database and object storage | Data folder |
|---|---|---|---|---|
| **Single process** (`role: all`): one nbsandboxd, web UI on :8090 | `make up` / `make down` | `make logs` | `make wipe-dev` | `~/.local/share/nbsandbox` |
| **Cluster** (`role: api` ×2 behind a load balancer, `role: worker` ×2), see [docs/scaling.md](docs/scaling.md) | `make cluster-up` / `make cluster-down` | `make cluster-logs` | `make wipe-cluster` | `~/.local/share/nbsandbox-cluster` |

| **Kubernetes** (the same cluster mode, with runs as Pods), see [docs/kubernetes.md](docs/kubernetes.md) | `make k8s-up` / `make k8s-down` | `make k8s-logs` | `make k8s-down` (the cluster and its data go together) | inside the kind cluster |

All three serve the API on 127.0.0.1:8080, so starting one stops the others. Each has its own database and object storage. Only single-process mode has the web UI. The Kubernetes mode is kind running inside Docker: still nothing to install beyond Docker. Runs there are slower (Docker Desktop starts containers in nested Kubernetes slowly); use it for the backend, not everyday work.

The runner image must be built with `make runner-image` (not a plain `docker build`): it adds the `io.nbsandbox.manifest` label nbsandboxd needs, and `/readyz` reports `runner_image` as not ready without it.

### Local web UI

`make up` and `make run-native` also start a small web UI at **http://127.0.0.1:8090**: pick a user and a space, browse files, view notebooks read-only, run them, and watch the live log.

**It has no login: it acts as whichever user you pick.** Keep it on your machine. It is off by default (`ui.enabled` in `settings.yaml`), listens on `127.0.0.1` unless configured otherwise (nbsandboxd warns at startup if not), and must never be exposed through a reverse proxy. The main API on port 8080 still requires a password. The UI's files are plain HTML, CSS and JavaScript in `internal/ui/web/`, embedded in the binary; after changing them, rebuild (`make up`) and repeat the check in [docs/ui-check.md](docs/ui-check.md). `make ui-preview` runs a native nbsandboxd with the UI on http://127.0.0.1:18090, its own data folder and no Docker (runs can't start), to work on the UI without touching the dev stack.

The editor is CodeMirror 6, vendored as one bundle in `internal/ui/web/vendor/` (with its licences and `SHA256SUMS`, which a test checks). To update it, change versions in `internal/ui/vendor-src/package.json`, delete `package-lock.json`, and run `make ui-vendor` (Docker and network needed); commit the new lockfile and bundle together.

The API is described in [api/openapi.yaml](api/openapi.yaml). When you add a route, add it to the route table in `internal/api/routes.go` **and** to `api/openapi.yaml`: a test fails if they differ, and space-scoped routes are automatically covered by the "other user gets 404" test.

`make up` is closest to production: nbsandboxd reaches Docker only through the socket proxy, which allows a fixed list of API calls. `make run-native` is quicker to iterate on.

## Testing

| Level | What | Command | Needs Docker |
|---|---|---|---|
| Unit | one package, with fakes for what it depends on | `make test` / `make cover` | no |
| Integration | the API against the dev stack: runs, cancellation, restarts, Docker loss | `make test-integration` (restarts the stack); `make test-quick` skips the slow limit and isolation tests (about 6 min) | yes |
| Cluster | the same suite through the cluster stack, plus failure tests (killing workers, API processes, Postgres) | `make test-cluster`; `make test-cluster-failures` for only the failure tests | yes |
| Kubernetes | the backend conformance suite against real Pods; the integration suite through the kind stack | `make test-k8s-conformance`; `make test-k8s` | yes |
| Isolation | probe notebooks and the isolation report | `make report` | yes, Linux for authoritative results |

Coverage thresholds: at least 80 % for core packages (`runs`, `logs`, `spaces`, `store`, `isolation/docker`) and at least 70 % overall. `make cover` enforces both.

## Code layout

| Package | What it does |
|---|---|
| `cmd/nbsandboxd` | the service binary: `serve`, `hash-password`, `health`, `backup`, `version` |
| `cmd/nbsandbox-report` | turns integration-test findings into `docs/isolation-report.md` |
| `internal/api` | HTTP routes (one route table), auth middleware, errors, SSE log stream, the web UI's API path |
| `internal/auth` | users file, bcrypt, credential cache |
| `internal/config` | settings file and environment overrides, validated at startup |
| `internal/spaces` | spaces and files on disk: path checks, uploads, quotas |
| `internal/runs` | run lifecycle: submission, queue, cancel, classification, results, retention |
| `internal/logs` | the per-run log hub that live viewers subscribe to |
| `internal/isolation` | the `Backend` interface; `docker` implements it, `backendtest` is its conformance suite |
| `internal/deps` | the runner image's package manifest and the notebook import check |
| `internal/store` | PostgreSQL: migrations, spaces, runs, functions, status events, idempotency keys (tests: `make test-db`, one schema per test via `storetest`) |
| `internal/obs` | logging, metrics, health and readiness |
| `internal/ui` | the optional local web UI's static files |
| `runner-image/` | the image notebooks run in, and its wrapper around papermill |

Packages talk to each other through small interfaces so each one can be unit-tested with fakes; only integration tests need Docker.

## Conventions

- **Licence header** at the top of every Go, Python and shell file (`make headers` checks this):
  ```go
  // Copyright 2026 The nbsandbox Authors
  // SPDX-License-Identifier: Apache-2.0
  ```
- **Commit messages** follow [Conventional Commits](https://www.conventionalcommits.org/): `feat:`, `fix:`, `docs:`, `test:`, `refactor:`, `chore:`, optionally with a scope, e.g. `fix(logs): …`.
- **Tests** come with the change, at the level that fits it (unit first; integration when Docker behaviour matters).
- **API spec:** every response in the API and integration tests is checked against `api/openapi.yaml` (status, content type, JSON schema), so change the spec in the same PR as the handler.
- **Docs** are updated in the same PR: the API spec if an endpoint changes, the user guide or operations guide if behaviour or a setting changes, and `CHANGELOG.md` under Unreleased.

## Pull requests

1. Fork and branch from `main`.
2. Make the change with tests; run `make check`.
3. Open a PR and fill in the template. CI must pass and every commit must be signed off.

## Reporting security issues

Never in a public issue. See [SECURITY.md](SECURITY.md).

## Code of Conduct

This project follows the [Contributor Covenant](CODE_OF_CONDUCT.md). Be kind and constructive.
