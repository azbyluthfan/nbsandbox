# Contributing to nbsandbox

Thanks for your interest. nbsandbox is young (v0.x: expect changes between minor versions); what's next is in [ROADMAP.md](ROADMAP.md). Users and operators start with the [README](README.md), [docs/user-guide.md](docs/user-guide.md), [docs/deployment.md](docs/deployment.md) and [docs/operations.md](docs/operations.md). Before starting anything large, open an issue so we can agree on the approach.

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

Install these yourself. Tools used only by some targets (kind and Helm for `make k8s-up`, buf for `make proto`) are fetched at pinned versions through `go run`, so there is nothing else to install for them.

| Tool | Needed for | Install |
|---|---|---|
| Go 1.27.4+ | everything | https://go.dev/dl/ |
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
make up             # nbsandboxd, PostgreSQL, RustFS and the Docker socket proxy in containers, on 127.0.0.1:8080
make down
```

```bash
make run-native     # nbsandboxd directly on your Mac against Docker Desktop (no proxy), with the test PostgreSQL and RustFS
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
| **Single process** (`role: all`): one nbsandboxd, web UI on :8090 | `make up` / `make down` | `make logs` | `make wipe-dev` (asks first) | `~/.local/share/nbsandbox` |
| **Cluster** (`role: api` ×2 behind a load balancer, `role: worker` ×2), see [docs/scaling.md](docs/scaling.md) | `make cluster-up` / `make cluster-down` | `make cluster-logs` | `make wipe-cluster` | `~/.local/share/nbsandbox-cluster` |
| **Kubernetes** (the same cluster mode, with runs as Pods), see [docs/kubernetes.md](docs/kubernetes.md) | `make k8s-up` / `make k8s-down` | `make k8s-logs` | `make k8s-down` (the cluster and its data go together) | inside the kind cluster |

All three serve the API on 127.0.0.1:8080, so starting one stops the others. Each has its own database and object storage. Only single-process mode has the web UI. The Kubernetes mode is kind running inside Docker: still nothing to install beyond Docker. Runs there are slower (Docker Desktop starts containers in nested Kubernetes slowly); use it for the backend, not everyday work.

The runner image must be built with `make runner-image` (not a plain `docker build`): it adds the `io.nbsandbox.manifest` label nbsandboxd needs, and `/readyz` reports `runner_image` as not ready without it.

### Web UI

`make seed` fills a dev stack with example spaces for `alice` and `bob` (the sales notebooks, a charts dashboard and a map, with their data and finished runs, and a published function); it is safe to run again, and refills after `make wipe-dev`. `make up` and `make run-native` also start the web UI at **http://127.0.0.1:8090**: pick a user, then manage spaces and files, edit notebooks with completion from the runner image, run them and follow the live log, and publish and call functions ([docs/tour.md](docs/tour.md) shows each screen). `make ui-preview` runs it without Docker, for work on the UI itself (runs can't start there).

**In the dev stack it has no sign-in: it acts as whichever user you pick.** That mode (`ui.auth: none`) listens on loopback only; the dev Compose stack allows its container port with `NBSANDBOX_UI_NO_LOGIN_BEYOND_LOOPBACK`, published on the host's `127.0.0.1` only. Deployments sign people in (`ui.auth: oidc` or `basic`, [docs/auth.md](docs/auth.md#signing-in-to-the-web-ui)); the sign-in code is in `internal/uiauth`, and `make test-keycloak` signs in to the UI through Keycloak's real login page. The main API on port 8080 always requires credentials. The UI's files are plain HTML, CSS and JavaScript in `internal/ui/web/`, embedded in the binary; after changing them, rebuild (`make up`) and check it in a browser, and run `make tour`: it drives the main screens in a headless browser and retakes the screenshots of [docs/tour.md](docs/tour.md), so a broken screen shows up as a failed step or a wrong picture. `make ui-preview` runs a native nbsandboxd with the UI on http://127.0.0.1:18090, its own data folder and no Docker (runs can't start), to work on the UI without touching the dev stack.

The editor is CodeMirror 6, vendored as one bundle in `internal/ui/web/vendor/` (with its licences and `SHA256SUMS`, which a test checks). To update it, change versions in `internal/ui/vendor-src/package.json`, delete `package-lock.json`, and run `make ui-vendor` (Docker and network needed); commit the new lockfile and bundle together.

The API is described in [api/openapi.yaml](api/openapi.yaml). When you add a route, add it to the route table in `internal/api/routes.go` **and** to `api/openapi.yaml`: a test fails if they differ, and space-scoped routes are automatically covered by the "other user gets 404" test.

`make up` is closest to production: nbsandboxd reaches Docker only through the socket proxy, which allows a fixed list of API calls. `make run-native` is quicker to iterate on.

## Testing

| Level | What | Command | Needs Docker |
|---|---|---|---|
| Unit | one package, with fakes for what it depends on | `make test` / `make cover` | no |
| Integration | the API against the dev stack: runs, cancellation, restarts, Docker loss | `make test-integration` (restarts the stack); `make test-quick` skips the slow limit and isolation tests (about 6 min) | yes |
| Cluster | the same suite through the cluster stack, plus failure tests (killing workers, API processes, Postgres) | `make test-cluster`; `make test-cluster-failures` for only the failure tests | yes |
| Kubernetes | the backend conformance suite against real Pods; the integration suite through the kind stack | `make test-k8s-conformance`; `make test-k8s` (`K8S_VERSION=1.34` for the oldest supported Kubernetes; `K8S_RUN_MEMORY=512MiB` with Docker under 4 GB) | yes |
| Authentication | a real Keycloak beside the dev stack: user tokens, scopes, audience, token exchange | `make test-keycloak` | yes |
| Isolation | probe notebooks and the isolation reports | `make report` (this machine's Docker); `make report-linux` (Linux with Docker Engine, `runc` and gVisor, in a Lima VM: [docs/gvisor.md](docs/gvisor.md#reproduce)); `make report-k8s` (kind; `K8S_RUNTIME_CLASS=gvisor` adds gVisor); `nbsandbox-report check` against any running deployment ([docs/check-deployment.md](docs/check-deployment.md)) | yes, Linux for authoritative results |
| Connections | reading external data end to end: test sources (Postgres, MySQL, SQL Server, S3, with and without TLS) beside the dev stack, failures of a fetcher, a source, the gateway | `make test-connections`; `make load-connections` for reading under load (`LOAD_RUNS`, default 16) | yes |
| Upgrade | an earlier release's dev stack (its published image) makes spaces, files, runs, a function, an open session and active runs; this checkout then starts on the same database and object store and checks it all survived | `make test-upgrade` (from the newest release, `UPGRADE_FROM=<tag>` for another with interactive sessions, 0.3.0 or later; stops the dev stack, keeps its data); `make test-helm-upgrade` does the same with the Helm chart on kind (it resets the kind stack's database) | yes |
| Soak | a long mix of runs, failing on any steady growth | `make soak` (45 min), `make soak-cluster`, `make soak-k8s`; `make soak-connections` adds reading runs, function calls and sessions | yes |
| Web UI | the main screens in a headless browser, retaking the tour's screenshots | `make tour` (with `make up` running) | yes |
| Fuzzing | the parsers of outside input | `make fuzz` (every target, 30 s each; `FUZZTIME=3m` for longer) or one target with `go test ./internal/<pkg> -run '^$' -fuzz '^FuzzName$'` ([docs/security/review.md](docs/security/review.md#fuzzing) lists them) | no |

**In CI:** every pull request runs `make check`, govulncheck, the Compose and workflow lints and shellcheck (`pr`); every push to `main` runs the integration suite on Linux (`main`). Once a week, and on demand from the Actions tab, `weekly` runs every scenario side by side in about 1.5 hours: Docker with runc and gVisor (conformance, isolation report, the API check, soak), Kubernetes on kind (1.37 with both runtimes, 1.34 with runc), Keycloak end to end, all fuzz targets, and a security scan of the code and the three images (nbsandboxd, the runner and its builder), and the latest release as published (deployed from its GHCR images with `scripts/check-published.sh`); each job attaches its findings and report to the run, and a failure opens an issue. CI never changes the repository: a person renders the reference reports from a passing run (`scripts/render-reports.sh`, see [docs/releasing.md](docs/releasing.md)) and commits them.

Coverage thresholds: at least 80 % for core packages (`runs`, `logs`, `spaces`, `store`, `isolation/docker`) and at least 70 % overall. `make cover` enforces both.

## Code layout

| Package | What it does |
|---|---|
| `cmd/nbsandboxd` | the service binary: `serve`, `hash-password`, `health`, `backup`, `version` |
| `cmd/nbsandbox-report` | turns integration-test findings into `docs/isolation-report.md`; `check` runs the checks against a deployment |
| `cmd/nbsandbox-mcp` | the MCP server for AI agents ([docs/mcp.md](docs/mcp.md)) |
| `internal/api` | HTTP routes (one route table), auth middleware, errors, SSE log stream, the web UI's API path |
| `internal/grpcapi` | the gRPC (Connect) services, the same operations as REST |
| `internal/apiclient` | a small REST client, shared by `check` and the MCP server |
| `internal/check` | the isolation, resource and failure checks, run through the API (by the integration suite and by `nbsandbox-report check`) |
| `internal/mcpserver` | the MCP tools |
| `internal/auth` | users file, bcrypt, credential cache |
| `internal/config` | settings file and environment overrides, validated at startup |
| `internal/spaces` | spaces and files: path checks, uploads, quotas, published versions; contents in object storage (`internal/blob`) |
| `internal/runs` | run lifecycle: submission, queue, cancel, classification, results, retention |
| `internal/logs` | the per-run log hub that live viewers subscribe to |
| `internal/isolation` | the `Backend` interface; `docker` and `kubernetes` implement it, `backendtest` is its conformance suite |
| `internal/deps` | the runner image's package manifest and the notebook import check |
| `internal/store` | PostgreSQL: migrations, spaces, runs, functions, status events, idempotency keys (tests: `make test-db`, one schema per test via `storetest`) |
| `internal/obs` | logging, metrics, health and readiness |
| `internal/ui` | the optional web UI's static files |
| `internal/uiauth` | signing in to the web UI: sessions, the provider's sign-in, CSRF checks |
| `runner-image/` | the image notebooks run in, and its wrapper around papermill (cell events, step timings, rendering) |

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
