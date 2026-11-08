# Changelog

All notable changes. The format follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and versions follow [SemVer](https://semver.org/) (see [docs/releasing.md](docs/releasing.md)).

## [Unreleased]

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
