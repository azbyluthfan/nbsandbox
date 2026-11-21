# Changelog

All notable changes. The format follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and versions follow [SemVer](https://semver.org/) (see [docs/releasing.md](docs/releasing.md)).

## [Unreleased]

### Upgrading

- **Database migrations 11 and 12** run at startup: interactive sessions, and where API nodes reach a worker's sessions.
- **The Docker socket proxy must allow attach** (`POST /containers/{id}/attach`) for interactive sessions: the Compose files in `deploy/` do. A proxy configured by hand needs it added, or set `sessions.enabled: false`.
- **Kubernetes:** the workers' Role needs `pods/attach` in the runs namespace (the Helm chart adds it).
- **With API and worker processes,** sessions need `sessions.secret` (`NBSANDBOX_SESSIONS_SECRET`; Helm: `sessions.secretName`), the same on every process, and API nodes able to reach the workers' port 8091. Without it, sessions stay off there, and nbsandboxd says so at startup.
- **The web UI's page policy** allows `frame-src data:`, for a session's HTML outputs.

### Fixed

- **Kubernetes: a run could stay `running` for good,** past its time limit and any cancel, with its Pod left behind. Copying a run's files into its Pod ignored the run's end, and an exec into the Pod could block forever while client-go closed a stalled connection. Both now end with the run (abandoning a stuck exec), so the run stops and its Pod is removed. Seen once under the weekly CPU-fairness check.

### Added

- **Interactive sessions** ([docs/sessions.md](docs/sessions.md)): a notebook's live kernel, with a run's isolation (no network, the run limits, the runtime, the space's package set). Run cells one by one and keep the variables, as in Jupyter.
  - **In the web editor:** a session bar on each notebook (start, Run all, Interrupt, Restart, Save files, Variables, End), the variables panel listing the kernel's variables (type, shape, a DataFrame's columns), refreshed after every cell; run a cell with its button, Shift-Enter or Ctrl-Enter; outputs stream in, tables and charts included, and are saved with the notebook. Completion merges the live kernel's answers (`df.` lists a DataFrame's columns, `df["` its column names) with the static index, with a 300 ms deadline; docs on hover and for the highlighted item, and a signature line after `(`.
  - **Through the API:** `POST/GET /v1/spaces/{id}/sessions`, `GET/DELETE …/sessions/{session_id}`, `POST …/interrupt`, `…/restart`, `…/sync`, and `GET …/channel`, a WebSocket of Jupyter messages (filtered both ways). gRPC: `SessionService`, the channel as a bidirectional stream. Administrators: `GET /v1/admin/sessions`, `POST /v1/admin/sessions/{session_id}/end`.
  - **Files:** a read-write session holds the space's write lock with writing runs, and its changes are written back every 5 minutes, on request and at the end, with the runs' conflict rules. Otherwise a session is read-only.
  - **Limits:** a session ends after 30 minutes with no cell running and nobody connected, or 8 hours after it started; 2 per user, 4 per worker (`sessions.*`).
  - **Docker, Kubernetes and clusters:** sessions survive a restart of nbsandboxd (re-attached). With API and worker processes, API nodes relay to the worker holding a session, authenticated with `sessions.secret`.
  - **Isolation report:** a new section, "Interactive sessions": no network, the same isolation as a run, the memory limit, read-only sessions, re-attaching.
- **`nbsandbox-report check` checks interactive sessions** through the API: no network, the same isolation as a run, read-only sessions, the memory limit ([docs/check-deployment.md](docs/check-deployment.md)).
- **A check of the published release** (`scripts/check-published.sh`): deploys a release from GHCR as `docs/deployment.md` does and runs notebooks through it. Weekly for the newest release, and part of the release checklist.

## [0.2.1] - 2026-11-16

### Fixed

- **The published runner images work with the Docker backend.** Every runner image on GHCR up to 0.2.0 (`ghcr.io/azbyluthfan/nbsandbox-runner:0.1.0` to `0.2.0`) had a broken `io.nbsandbox.manifest` label: the release workflow passed it through an input that is parsed as CSV, which stripped quotes inside the JSON. nbsandboxd refused those images (`/readyz`: `runner image manifest is not valid JSON`), so a deployment that pulled the published runner image never became ready. Images built with `make runner-image`, and the Kubernetes backend (which reads the manifest from the image itself), weren't affected. The release workflow now sets the label directly and checks it on the published image for both platforms.

### Upgrading from 0.2.0

- Nothing to change besides the version: use the 0.2.1 images (nbsandboxd, the runner and the builder) and chart. The code is the same as 0.2.0.

## [0.2.0] - 2026-11-15

### Upgrading from 0.1.2

- **Database migrations 8 to 10** run at startup: web UI sessions, the audit log, the drain flag and package sets.
- **The web UI without sign-in now listens on loopback only** (see Changed). Deployments that served it to a network must set `ui.auth` to `oidc` or `basic`.
- **A new image,** `ghcr.io/azbyluthfan/nbsandbox-runner-builder`, is needed only with `packages.enabled`.
- **The Compose stack's socket proxy** now also allows bind mounts from `<data_dir>/packages`. Take the new `deploy/docker-compose.yml`.
- **New settings,** all off or unchanged by default: `auth.login_limit`, `auth.trusted_proxies`, `auth.oidc.introspection`, `ui.auth` and its sign-in settings, `audit.*` (on), `packages.*`.

### Added

- **Token revocation:** with `auth.oidc.introspection`, nbsandboxd asks the identity provider (RFC 7662) whether a token is still active, so a token revoked there stops working within 30 s instead of at its expiry. Fails closed; only tokens that pass the local checks are sent. Tested against Keycloak; Entra ID has no introspection endpoint ([auth.md](docs/auth.md#revoked-tokens)).
- **Failed sign-in limit:** basic auth allows 5 failed sign-ins per username, then one a minute (`429` with `Retry-After`; gRPC `RESOURCE_EXHAUSTED`), and per client address when the proxies in front are named in `auth.trusted_proxies`. The Compose stack names Caddy; the Helm chart has `auth.loginLimit` and `auth.trustedProxies` ([auth.md](docs/auth.md#failed-sign-ins)).
- The API's error codes gain `too_many_requests`, and every authenticated operation documents its `429` response.
- **Signing in to the web UI:** with `ui.auth: oidc`, people sign in with the identity provider (authorization code with PKCE) and are the same caller as through the API; with `ui.auth: basic`, with a `users.yaml` password. Sessions are server-side (tokens never reach the browser), end after 1 h idle or 12 h, follow revocation at the provider, and sign out of the provider too. The Compose stack serves the UI at the domain's root with `NBSANDBOX_UI_ENABLED=true`; the Helm chart with `ui.enabled` ([auth.md](docs/auth.md#signing-in-to-the-web-ui)).

- **Audit log:** every change, download and administrative operation (refused ones included) and every web UI sign-in and sign-out is recorded with who, what, when, how it ended and from where, in REST, gRPC and the UI alike. Administrators read it with `GET /v1/admin/audit` (gRPC `AdminListAudit`); each row also goes to the service log. Kept 90 days (`audit.retention`) ([operations.md](docs/operations.md#audit-log)).
- **Admin operations:** list every owner's runs (`GET /v1/admin/runs`), stop any run (`POST /v1/admin/runs/{run_id}/cancel`), and drain the workers before maintenance (`PUT /v1/admin/drain`: no queued run starts, running ones finish, new ones queue). Also in gRPC's `AdminService` ([auth.md](docs/auth.md#administration)).
- Metric `nbsandbox_audit_failures_total`.
- **Custom packages per space (Docker):** with `packages.enabled`, a space's `requirements.txt` is installed into a package set its runs import: built once per requirements and runner image by a build container (the new `nbsandbox-runner-builder` image: the runner's packages plus pip; wheels only; network, but no space files), shared between spaces, mounted read-only. A run waits, queued, for its set (`waiting_for: packages`); a failed build fails it (`error_kind: packages`), and later runs get `422 packages_failed` with the build log. Published versions keep their exact packages. `GET`/`POST /v1/spaces/{id}/packages` (gRPC `GetSpacePackages`, `BuildSpacePackages`) and the web UI's Packages section show and build them. Off by default ([packages.md](docs/packages.md)).

- **Two richer example notebooks:** `sales_dashboard.ipynb` (charts: weekly trends with rolling averages, stacked categories, store growth, small multiples) and `sales_map.ipynb` (a map of Java with matplotlib alone: provinces shaded by a metric, stores as bubbles, labels, scale bar; a GeoJSON artifact), on synthetic data from `examples/notebooks/data/make_orders.py` and Natural Earth's public-domain province shapes ([examples/README.md](examples/README.md)).
- **`make seed`** fills a dev stack with example spaces, notebooks (the two above included), data, runs and a function (as the demo users; safe to run again).

### Changed

- **The Compose stack's Docker socket proxy** allows bind mounts from `<data_dir>/packages` (package sets and their builds) besides the spaces.
- **The web UI without sign-in (`ui.auth: none`, the default) refuses to listen beyond loopback**; it used to start with a warning. To serve the UI to a network, set `ui.auth` to `oidc` or `basic`. A container whose port is published only on the host's loopback can set `ui.no_login_beyond_loopback: true` (the dev Compose stack does).

## [0.1.2] - 2026-11-15

### Security

- **tornado 6.6.1 in the runner image** (was 6.6): an incomplete fix for an open redirect in tornado's `RedirectHandler` ([GHSA-pw9p-v7r6-gx4p](https://github.com/advisories/GHSA-pw9p-v7r6-gx4p) and its follow-up). The runner doesn't serve HTTP (tornado is there for the Jupyter kernel, and runs have no network), so it wasn't reachable; updated anyway.

### Fixed

- **gVisor on the local Kubernetes cluster:** `make k8s-up K8S_RUNTIME_CLASS=gvisor` (and the weekly CI job) failed with "gVisor … not found": gVisor's apt repository only lists its newest release, so the pinned one dropped out. The pinned release now comes from gVisor's release bucket, checked against its SHA-512, unpacked on the host (the kind node has no bzip2) and copied into the node.

### Changed

- **Faster results: rendering `output.html` takes about 0.1 s instead of 1 s.** The runner renders with nbconvert's library, warmed up in the background while the kernel starts, instead of starting a new `python -m nbconvert` (same HTML, same 5-minute limit; a failure still leaves `rendered: false`). A small call on Docker: 2.9 s → 2.5 s ([docs/perf.md](docs/perf.md#starting-a-run)).
- **The reference reports come from the weekly CI run's results:** it runs the whole suite on Linux with Docker Engine with `runc` and gVisor at the default limits (2 GiB per run, where the last Linux report used 512 MiB), and on Kubernetes 1.37 (kind, Cilium) with both runtimes, and attaches the findings to the run. CI never writes to the repository: a person renders the reports from a passing run with `scripts/render-reports.sh` and commits them, usually in the release's pull request. `make report-linux` and `make report-k8s` still run them on your own machine.

## [0.1.1] - 2026-11-15

### Added

- **Check your deployment:** `nbsandbox-report check` runs the isolation, resource and failure checks against a running deployment (Docker or any Kubernetes cluster) through its API, as two users, and writes the isolation report for that deployment. With a kubeconfig it also checks that run Pods are gone, probes the API server, a worker and the kubelet, and records the cluster version and the runs' NetworkPolicies. It never restarts or stops anything; see [docs/check-deployment.md](docs/check-deployment.md). The integration suite runs the same checks, from one copy of the code.

- **Where a run's time goes:** each run records its phases (queued, preparing files, container start, runner start, kernel start, the cells, rendering, teardown) in its result (`usage.phases_seconds`, REST and gRPC), the `nbsandbox_run_phase_seconds` metric (with a dashboard panel) and its log line; the runner's status file has the matching `timings`. The isolation report's start-latency row shows the breakdown ([docs/perf.md](docs/perf.md#starting-a-run)).

- **Keycloak, end to end:** `make test-keycloak` runs a real Keycloak (26.8) beside the dev stack and checks that a user's token runs a notebook, that scopes and the audience are enforced, and that a backend calls on behalf of its user with token exchange (RFC 8693), the run recording the backend. Also in the weekly CI run. [docs/auth.md](docs/auth.md#keycloak) describes the tested setup; [deploy/keycloak/realm.json](deploy/keycloak/realm.json) is a complete example.

- **MCP server for AI agents:** `nbsandbox-mcp` gives agents (Claude Code, Claude Desktop, others) 17 tools over the REST API: spaces, files, running a notebook and waiting for its result, logs, artifacts, functions and the runtime. Over stdio it acts as one user from the environment; over HTTP (`-http`) it holds no credentials and passes each caller's own `Authorization` header to nbsandbox. Released with the other binaries; see [docs/mcp.md](docs/mcp.md).

### Changed

- **Kubernetes 1.34 to 1.37 are supported** ([docs/kubernetes.md](docs/kubernetes.md#supported-versions)). The weekly CI run tests 1.37 with both runtimes and 1.34 with the default one; the local cluster is kind 0.33 with Cilium 1.20 (`K8S_VERSION=1.34` for the oldest, `K8S_RUN_MEMORY` for small Docker hosts).

### Security

- **Go 1.27.4** for the binaries and the server image: Go 1.27.3's `net/http` server could crash on a header race with `MaxBytesReader` ([GO-2026-6859](https://pkg.go.dev/vuln/GO-2026-6859)).

### Fixed

- **Kubernetes: copying a run's files in and out took 10–90 s** (since 0.1.0): the files container of each run Pod had 10 millicores of CPU, and the events follower started a process four times a second, so the container was throttled in nearly every CPU period. The follower now starts no process while it waits, and the files container gets 250 millicores (requests equal limits, so run Pods stay Guaranteed). On kind, a small call is back to about 5 s and a cancel to about 1 s.
- **Kubernetes: a space whose owner has a larger quota** (`quotas.owners[].max_space`) could be evicted at twice the server's default: a run Pod's `/space` and `/run`, and what is copied back, are now sized from the run's own allowance.
- **Kubernetes:** on a busy cluster, the first items of a streamed result could reach the caller together, about a second late, instead of as they were emitted.

## [0.1.0] - 2026-11-08

The first release. Everything below is new. A guided tour: [docs/tour.md](docs/tour.md).

### Running notebooks

- **Spaces and files:** create, list, rename and delete spaces; upload files (all or nothing, with size limits and a per-space quota), organise them in folders, rename, move, download and delete them. Saving a file checks the version it replaces (`If-Match`), so concurrent edits never overwrite each other silently. Another user's space is always `404`.
- **Runs:** start a notebook with parameters (any JSON, up to 1 MiB); a queue with a configured concurrency; one writing run per space, read-only runs beside it; cancel; time limit; idempotent submission (`Idempotency-Key`). Each run executes a snapshot of the notebook taken when it was submitted, on its own copy of the space's files; its changes are written back when it ends, and a conflicting edit made meanwhile is kept beside them.
- **Results:** status (the notebook's fault or the platform's, and whether a retry may help), the failing cell and exception, the value returned with `nbsandbox.output(...)`, resource usage, the executed notebook, a rendered HTML copy, the log, and artifacts (files written to the output folder).
- **Live logs and status:** Server-Sent Events for a run's log, its status, its result and every run in a space, each resumable with `Last-Event-ID`; long-polling with `?wait=`. A slow viewer never slows a run and is never disconnected.
- **Python notebooks only, checked:** a notebook for another kernel is refused with a clear message. A dependency check warns (or, optionally, refuses) when a notebook imports a package the runner image doesn't have; `GET /v1/runtime` lists the image's packages, modules and limits.

### Notebook functions

- **Publish a notebook as a function:** an immutable, numbered version of the notebook and the files it needs; other services call it with JSON and get JSON back ([docs/functions.md](docs/functions.md)).
- **Contracts:** optional JSON Schemas for input and output in the notebook's metadata or the publish request. Bad input is `400 invalid_parameters` with JSON pointers, before anything runs; an output that breaks the contract fails the call.
- **Streamed output:** a notebook can send its result as items with `nbsandbox.emit(...)`, which callers receive as they are made; progress (`nbsandbox.progress`) and cell starts and ends on request. Items are never dropped silently.
- **Calls run side by side,** each in its own fresh, read-only container.

### APIs

- **REST** under `/v1`, described by an OpenAPI spec (`api/openapi.yaml`, checked against the routes by a test), with a compatibility policy ([docs/api.md](docs/api.md)).
- **gRPC, gRPC-Web and Connect** on the same port, with the same operations, scopes and errors (`api/proto`); uploads, downloads and streams are streamed. Go clients can import the generated package.
- **Examples:** complete clients in Python and Go (`examples/`).

### Isolation

- **Every run in a fresh container or Pod:** no network, a read-only image, uid 1000 with no capabilities and no privilege escalation, only its own files mounted, and limits on memory, CPU, processes, `/tmp`, time and log size.
- **Docker backend:** nbsandboxd reaches Docker only through a socket proxy that allows a fixed list of calls and bind mounts from the spaces folder only. Runtime `runc` (the default) or **gVisor** (`docker.runtime: runsc`), which handles system calls in user space ([docs/gvisor.md](docs/gvisor.md)).
- **Kubernetes backend** (`backend: kubernetes`, [docs/kubernetes.md](docs/kubernetes.md)): runs as hardened Pods (meeting the `restricted` Pod Security Standard, no service-account token, no DNS, Guaranteed resources) in a namespace with a deny-all NetworkPolicy. A probe Pod checks at startup and every 10 minutes that nothing is reachable, and workers refuse to run notebooks if anything is (tested with Cilium and Calico, and with kind's own plugin, which doesn't enforce the policy and is refused). Files go in and out through the Kubernetes API, so run Pods need no shared storage or credentials. An optional sandbox RuntimeClass (gVisor, Kata).
- **Isolation reports** generated from the integration suite: [Docker](docs/isolation-report.md) 49/49; [Linux with Docker Engine](docs/isolation-report-linux.md), `runc` and gVisor, 49/49 each; [Kubernetes](docs/isolation-report-kubernetes.md), `runc` and gVisor, 45/45 each.

### Storage and scaling

- **PostgreSQL** for spaces, the file index, runs, events and functions, and as the queue.
- **Object storage** for file contents, run outputs and published versions: S3-compatible stores (RustFS, Ceph, AWS S3, ...) or Azure Blob Storage (Workload Identity, or a connection string). Each content is stored once; renames and moves are index updates; unreferenced contents are cleaned up.
- **API and worker roles** (`role: all | api | worker`, [docs/scaling.md](docs/scaling.md)): any number of each share PostgreSQL and object storage. Workers claim runs with leases and fencing tokens; a worker that goes away has its runs queued again or ended as `interrupted`; cancels, live logs and status streams work across nodes. Tested by killing workers, API processes and PostgreSQL mid-run.

### Authentication

- **OpenID Connect bearer tokens** (Entra ID, Keycloak, Okta, Auth0, ...): nbsandbox keeps no user accounts; spaces belong to the caller's ID. Tokens are checked against the issuer's keys (asymmetric algorithms only), issuer, audience and expiry ([docs/auth.md](docs/auth.md)).
- **Scopes per endpoint** (`nbsandbox.read`, `.spaces.write`, `.files.write`, `.runs`, `.functions.invoke`, `.functions.publish`, `.admin`); calls on behalf of users, with the calling application recorded on runs.
- **Quotas per owner** (spaces per caller, space size) and **administration** endpoints to list, reassign and delete any owner's spaces.
- **Basic auth** from a users file, for development and small private hosts; nbsandboxd warns when the repository's demo users are loaded.

### Web editor (local, for development)

- Spaces, files and folders; notebooks edited cell by cell and text files with syntax highlighting, syntax checks, and **completion and hover documentation** from the runner image's packages; runs with parameters and a live log; results; publishing and calling functions from a form built from the contract. Light and dark, down to phone width. Off by default, and with no login: never expose it ([CONTRIBUTING.md](CONTRIBUTING.md#local-web-ui)).

### Operations

- **Deployment:** production Docker Compose with Caddy (automatic TLS), PostgreSQL and RustFS ([docs/deployment.md](docs/deployment.md)), every service with a read-only root, no capabilities and `no-new-privileges` (checked by `scripts/compose-lint.sh`); a Helm chart and Terraform examples for AKS and existing clusters.
- **Health, readiness and Prometheus metrics**, with example alerts and a Grafana dashboard; graceful shutdown; recovery after Docker or nbsandboxd restarts.
- **Backup and restore** of the database and the object store (`scripts/backup.sh`, [docs/operations.md](docs/operations.md)).
- **Releases** with signed multi-arch images, SBOMs, a signed Helm chart, and the isolation report for the tagged commit ([docs/releasing.md](docs/releasing.md)).

### Evidence

- Unit, integration, conformance and fuzz tests; soak tests on one process, a Compose cluster and Kubernetes ([docs/perf.md](docs/perf.md)); a security review with dependency, image and secret scans ([docs/security/review.md](docs/security/review.md)); a threat model ([docs/security/threat-model.md](docs/security/threat-model.md)).

### Known limitations

- **Trusted users:** runs are hardened and tested, but with `runc` they share the host's kernel. Use gVisor or a sandbox RuntimeClass for less trusted users.
- **The web UI has no login** and is a local development tool.
- **No sharing of spaces** between users; no token revocation before expiry; no rate limiting of failed basic-auth logins.
- **Storage quotas are checked** every few seconds while a run writes, not enforced by the filesystem.
- **Starting a run takes 3–5 s** (a fresh container and Python kernel).
- Docker Desktop and kind work for development but aren't reference isolation platforms.
