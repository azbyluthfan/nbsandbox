# Changelog

All notable changes. The format follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and versions follow [SemVer](https://semver.org/) (see [docs/releasing.md](docs/releasing.md)).

## [Unreleased]

Nothing is tagged yet: the first release includes everything below.

### PostgreSQL instead of SQLite

- **Every mode uses PostgreSQL** (setting `database_url`, env `NBSANDBOX_DATABASE_URL`, never logged): the first step towards several nbsandboxd processes sharing one database. Docker Compose (dev and production) has a `postgres` service with its data in a named volume; `make test-db` starts a throwaway one for unit tests, where each test gets its own schema.
- **What SQLite's single writer gave for free is now explicit:** status events become visible strictly in sequence order (a transaction lock on insert), so a stream resuming after event N never misses an earlier one; queue order comes from a sequence; one writing run per space stays a unique index; two identical idempotent requests at once still create one run. Each has a concurrency test.
- **Backups** use `pg_dump` (`scripts/backup.sh`); the `nbsandboxd backup` command is gone.

### Faster soak

- `make soak` (45 minutes; 3 hours before a release): several spaces at once, sampled every minute and judged on fitted trends, with cleanup timers shortened so every cleanup path runs many times. New settings `runs.cleanup_every` and `runs.idempotency_window`; run and event retention can now be as short as 1 minute. First run: 1,522 runs, nothing grew, all cleanup checks passed (docs/perf.md).

### Object storage (in progress)

- `internal/blob`: the storage interface, an in-memory store, and the shared test suite; adapters for S3-compatible stores (RustFS, Ceph, AWS S3, ...) on the AWS SDK for Go v2 and Azure Blob Storage (Workload Identity, or a connection string for Azurite). `make test-blob` runs both against RustFS and Azurite.
- **Space files live in object storage**, with the file index in PostgreSQL (paths, sizes, content hashes): contents are stored once however many files use them; renames and folder moves are one transaction. File versions are now the SHA-256 of the contents. Settings `storage.kind` (`s3` or `azure`) with `storage.s3.*` / `storage.azure.*` and `NBSANDBOX_STORAGE`, `NBSANDBOX_S3_*`, `NBSANDBOX_AZURE_*`; Docker Compose has a RustFS service; `/readyz` checks `storage`.
- **Runs work on a copy of the space** (`work/<run>/` on the host, mounted at `/space`). A writing run's changes are written back when it ends, each checked against the version it started from: an edit made meanwhile wins and the run's version is kept beside it (`name.conflict-<run>.ext`), and the log says so. Changes over the quota are not stored at all (before, the oversized file stayed in the space).
- **Cleanup** removes contents nothing refers to, once they're older than an hour, with the periodic retention.
- **Run outputs and published function versions are in object storage too.** A run's snapshot and parameters are stored at submission; the log, executed notebook, result and artifacts when it ends, before its final status is visible. Publishing a version copies nothing: it records the contents it holds. The host keeps only the folders of active runs, so any node can serve any finished run's files.

### Security review (2026-11-08)

Findings and how each was handled: docs/security/review.md.
- **Dependencies:** upgraded the Go modules with known vulnerabilities our code reaches (klauspost/compress, the AWS SDK's S3 client and event-stream decoder, moby/spdystream) and golang.org/x/net. The runner image applies Debian's security updates at build and no longer contains pip (runs have no network; pip's bundled urllib3, msgpack and setuptools were flagged).
- **Hardening:** a tar stream from a run Pod can't overflow the size budget with a forged entry size; `ParseSize` refuses `NaN`, infinities and sizes beyond int64; nbsandboxd warns at startup when the repository's demo users (public passwords) are loaded; isolation reports show the home folder as `~` in host paths.
- **Fuzz tests** for every parser of outside input that had none: tar streams from Pods, run events, notebook contracts and schemas, uploaded notebooks, basic credentials, the stream resume point, storage keys, sizes.

### gVisor and the Linux reference report

- **`docker.runtime: runsc` works end to end:** every integration test passes under gVisor on Linux with Docker Engine. Guide, costs and differences: docs/gvisor.md.
- **`make report-linux`** runs the suite in a Linux VM (Lima) with `runc` and `runsc` and writes docs/isolation-report-linux.md: 49/49 with each.
- **gVisor on Kubernetes:** with `runs.runtimeClass` set (gVisor, Kata), the run's process limit is `RLIMIT_NPROC`, set by the runner inside the sandbox (the node's pids limit counts gVisor's own threads, and gVisor died when refused one). `make k8s-up K8S_RUNTIME_CLASS=gvisor` installs gVisor in the kind node (`scripts/kind-gvisor.sh`) and runs notebooks with it; `make report-k8s` shows its results beside the default runtime's.
- **Fixed for gVisor:**
  - Runs no longer set Docker's `NetworkDisabled` (with it, gVisor gave no loopback and the kernel aborted at start). Network mode `none` still means no network.
  - The runner writes its log in full (gVisor returns short writes on stdout/stderr, and most of a large log write was lost).
  - Peak memory comes from cgroup v1 files or is sampled (`memory_peak_sampled`). An OOM-killed run reports its limit as the peak.
  - The process limit is `RLIMIT_NPROC` under gVisor, with the host's pids limit as a backstop. Before, gVisor's own threads counted against it, and the sandbox could die at the limit. Refused forks and threads are `resource_pids`.
- **Fixed on Kubernetes:** a run's events (items, progress, cells) now reach callers live: the worker copies new ones out of the Pod twice a second. Before, they arrived only when the run ended, and the streamed-output test was too weak to notice (it now requires the items to arrive spread out).
- **Fixed for every runtime:**
  - **Nonstop printing.** A notebook printing without pause ran out of memory: IPython 9 keeps a copy of everything a cell prints. The kernel now keeps no output history, and stops sending stdout/stderr past twice the log cap (`NBSANDBOX_LOG_CAP`).
  - **Log viewers.** They read a run's lines at their own pace and are never disconnected for being slow. Before, a burst of output dropped even fast viewers on a busy 2-CPU host. Lines go out in batches.

### Kubernetes backend

- **`backend: kubernetes`:** workers start each run as a Pod in a runs namespace, instead of a Docker container. Docker stays the default; both run in cluster mode. Guide: docs/kubernetes.md.
- **Files without shared storage.** The worker copies the run's files into the Pod and back out through the Kubernetes API (`pods/exec`), so run Pods need no shared volumes, host paths or credentials.
- **Run Pods are hardened:**
  - non-root, all capabilities dropped, read-only root filesystem, seccomp;
  - no service-account token, no service variables, no DNS;
  - Guaranteed resources; in-memory `/tmp` and `/dev/shm`; an ephemeral-storage limit (eviction ends the run as `resource_disk`);
  - an optional RuntimeClass (Kata, gVisor).
- **Its own isolation report** (`make report-k8s`, docs/isolation-report-kubernetes.md): the full integration suite on kind with Cilium, 45 checks passed. From a run Pod, the API server, a worker, the node's kubelet, the metadata address and the internet are all unreachable.
- **Not tied to one network plugin.** The local cluster runs with Cilium (default) or Calico (`make k8s-up K8S_CNI=calico`); both pass the isolation tests. On kind's own plugin, which doesn't enforce NetworkPolicy, the workers refuse to run notebooks (`make test-k8s-cni-refuses`): the probe found the internet, the kubelet, the worker and the API server reachable, and no run was started.
- **A network probe guards it all.** Each worker starts a probe Pod labelled like a run, at startup and every 10 minutes, and refuses runs if it reaches anything: the API server, the internet, the worker itself. It caught kind's default network plugin letting Pods reach the API server; the local cluster uses Cilium.
- **Helm chart** (`deploy/helm/nbsandbox`):
  - API and worker Deployments with separate service accounts; only workers may manage Pods, and only in the runs namespace;
  - the runs namespace with the `restricted` Pod Security Standard and a deny-all NetworkPolicy;
  - PodDisruptionBudgets and an optional Ingress.
- **Terraform examples** (`deploy/terraform/aks`, `deploy/terraform/existing-cluster`):
  - AKS with Cilium, Workload Identity, a tainted node pool for runs, private PostgreSQL, key-less storage, and optional Pod Sandboxing;
  - any existing cluster with your own Postgres and S3-compatible storage.

  Validated with OpenTofu.
- **Locally:**
  - `make k8s-up` runs everything on kind inside Docker (kind and Helm through `go run`);
  - `make test-k8s-conformance` passes the backend conformance suite against real Pods;
  - `make test-k8s` runs the integration suite through it.

### Streamed output and progress

- **A notebook returns one value or a stream of items.** `nbsandbox.output(value)` as before, or `nbsandbox.emit(item, total=None)` once per item, as each is ready. The contract decides which: `output` is a JSON Schema (one value) or `{"mode": "stream", "item": <schema>}`. Without one, the mode is inferred from the calls the notebook makes. Publishing and starting a run scan the notebook's cells, and refuse one that calls both or contradicts its contract, naming the cells; the run enforces it too. `GET …/contract` shows the `mode` and whether it was inferred. A contract can also be given in the publish request, replacing the notebook's. New setting `functions.require_schemas`.
- **One result endpoint, two forms.** `GET …/runs/{id}/result` (and `POST …/invoke`) answer as JSON, or as Server-Sent Events with `Accept: text/event-stream` or `?stream=1`: an `item` event per item as soon as it is recorded (id = its number, resumable), then the `result`. For a streamed run, the JSON `output` is exactly the list of items the stream sent. A run that fails after sending items keeps them: `output` holds the items so far, with `"partial": true`. gRPC: `FollowRunResult`, `InvokeFunctionStream`.
- **Items are never dropped:** an item over 64 KiB, beyond 1,000, past `runs.max_output` in total, or not matching its schema fails the run. `nbsandbox.emit` paces itself to 10 a second.
- **`nbsandbox.progress(fraction, message)`**, and the runner's `cell` events (each code cell starting and finishing), are sent on request: `?include=progress,cell` on the event streams, `?include=progress` on the result stream. The event streams send only status changes by default.

### gRPC

- **The whole API over gRPC, gRPC-Web and Connect**, on the same port as REST (`api/proto/nbsandbox/v1/nbsandbox.proto`): the same operations, scopes, errors and data, with uploads and downloads streamed and the log and event streams as server streams. A test checks both transports cover every operation. Go clients import `github.com/azbyluthfan/nbsandbox/gen/...`; `examples/go/grpcclient` runs a notebook with it. nbsandboxd accepts HTTP/2 without TLS for this; the Caddy configuration forwards it.

### Object storage client

- **minio-go replaced by the AWS SDK for Go v2** for S3-compatible storage (RustFS by default; path-style requests). Same settings; the shared storage suite passes against RustFS.

### API and worker roles

- **`role: all | api | worker`** (`NBSANDBOX_ROLE`). `all` stays the default single process. `api` processes serve the API and UI and never touch Docker; `worker` processes run notebooks and serve only health and metrics. Any number of each share PostgreSQL and object storage. Guide: docs/scaling.md.
- **The queue is the database:** workers claim runs with a lease (`runs.lease`, default 30 s) that they renew; every write a worker makes for a run is fenced by the lease's token. A worker that goes away has its claimed runs queued again and its running ones ended as `interrupted` (retryable).
- **Across nodes:** cancels reach the run's worker, live log lines are streamed by any API node (resuming with `Last-Event-ID` works on any of them), and status streams wake through PostgreSQL notifications with a polling fallback. Workers publish their runner image and limits for `GET /v1/runtime`; an API node answers `503` to new runs while no worker can take them. Periodic cleanup runs on one node at a time.
- **Failure tests** (`make test-cluster-failures`): a worker killed or stopped mid-stream, each API process killed under a streaming client, PostgreSQL killed mid-run. All pass; see docs/scaling.md. Writes that must not be lost (an item, a run's final status) now retry through a database outage of up to 20 s instead of failing the run.
- **`make cluster-up` / `make test-cluster`:** two API processes behind a load balancer and two workers on one machine, and the integration suite through them.

### Handlers only translate

- The HTTP handlers parse requests and write answers; following a log, following status events, waiting for a result and listing runs are run-manager methods (`FollowLog`, `FollowEvents`, `WaitResult`, `ListFilter`), so another transport can use them as they are. Authentication takes an Authorization value whatever carried it, and each operation's scope is in one table that both transports use.

### Renamed to nbsandbox

The project was called notebox, which is already in use (notebox.cloud, and a PyPI package). Everything is renamed: the Go module (`github.com/azbyluthfan/nbsandbox`), the server (`nbsandboxd`), the runner image (`nbsandbox-runner`), the module notebooks import (`import nbsandbox`, `nbsandbox.output(...)`), the notebook metadata key (`metadata.nbsandbox.contract`), settings (`NBSANDBOX_*`), token scopes (`nbsandbox.*`), metrics (`nbsandbox_*`), alerts, image labels, the data folder and the docs.

### Authentication with an identity provider

- **Bearer tokens** from any OpenID Connect provider (Entra ID, Keycloak, Okta, Auth0, ...): nbsandbox is a resource server and keeps no user accounts. Tokens are checked against the issuer's published keys (RS/PS/ES algorithms only), exact issuer and audience, expiry with 60 s leeway and a maximum lifetime. Spaces belong to the caller ID (e.g. `entra:<tid>:<oid>`). Basic auth from `users.yaml` stays, on by default, for development. Guide: docs/auth.md.
- **Scopes per endpoint** (`nbsandbox.read`, `.spaces.write`, `.files.write`, `.runs`, `.functions.invoke`, `.functions.publish`), listed per operation in the OpenAPI spec; a token without one gets `403 forbidden` with `WWW-Authenticate: Bearer error="insufficient_scope"`.
- **Settings:** `auth.basic`, `auth.oidc.*`, and `NBSANDBOX_AUTH_BASIC`, `NBSANDBOX_OIDC_*`.
- **Quotas per owner:** `quotas.max_spaces` caps spaces per caller ID (it holds under concurrent creates), and `quotas.owners` gives particular IDs their own cap and space quota.
- **Administration** (new scope `nbsandbox.admin`, never implied; basic users need `admin: true` in users.yaml): `GET /v1/admin/spaces`, `PATCH /v1/admin/spaces/{id}` to give a space to another owner, `DELETE /v1/admin/spaces/{id}`; gRPC `AdminService`. Each change is logged with the administrator's ID.
- **Runs record the calling application** (`client`: the token's `azp`, `appid` or `client_id`).

### Fixed

- **The failing cell's number** (`error.cell_index` and the message) was one too high when the notebook had a `parameters` cell: the runner didn't skip the error cells papermill adds to the executed notebook. Found by the function integration tests.

### Web UI redesign

- A new layout: top bar with the space switcher, runs and functions; a file explorer that can be hidden; drawers for runs and functions; a spaces list; a space overview with notebook cards, recent runs, functions and a files table; folder pages with breadcrumbs; a sticky notebook header with a ⋯ menu; a function page with a generated call form, recent calls and a copyable example. Light and dark, down to phone width.

### Editor in the web UI (planned as 0.3.0)

- **Edit in the browser:** notebooks cell by cell (add, move, delete, code or markdown, the `parameters` tag, the contract) and text files, with syntax highlighting (CodeMirror 6, vendored and checksummed, no CDN) in light and dark mode; read-only views use the same highlighting.
- **Checks while typing:** Python syntax errors, and imports the runner image doesn't provide (`GET /v1/runtime` now lists `modules`).
- **Completion and hover docs from the runner image:** `pd.` lists pandas' public names with their signatures and docs, `import mat` completes module names, and hovering `pd.read_csv` shows its signature and summary, with no kernel running. Imports and definitions in any cell count in the others. The index is built with the runner image (`export_completions.py`, about 150 KB gzipped for 227 modules), read out of the image once by a worker (a one-off container or Pod), stored in object storage under its hash, and served by `GET /v1/runtime/completions` (gRPC `GetCompletions`), cached by the browser until the image changes. Variables' types aren't followed (`df.` after `df = pd.read_csv(...)`).
- **File versions in the editor:** a notice when the file changes elsewhere; saving over a newer version asks to reload, save a copy, or overwrite; a guard against leaving unsaved edits.
- **File management in the UI:** upload (button and drag and drop), folders, rename, move, delete.
- **Functions in the UI:** publish a notebook with chosen files, see versions and the contract, call it from a form built from the input schema, follow it live, see the result.
- The UI page now carries a fresh CSP nonce on every load (for the editor's styles); no `'unsafe-inline'`. `make ui-preview` runs the UI without Docker.

### Notebook functions (planned as 0.2.0)

- **Notebook functions:** publish a notebook, with the files it needs, as an immutable version; other services invoke it with JSON parameters and get JSON back ([docs/functions.md](docs/functions.md)).
- **Structured input and output:** parameters may be any JSON (nested objects, arrays, `null`; default limit 1 MiB, `runs.max_parameters`); notebooks return a value with `nbsandbox.output(...)` (numpy, pandas and dates converted); `GET .../runs/{id}/result` returns the output, or an error object with the reasons and `retryable`.
- **Contracts:** optional JSON Schemas for input and output in the notebook's metadata; bad input is `400 invalid_parameters` with JSON pointers, a bad output fails the run (`output_invalid`).
- **Status for callers:** `?wait=` long-polling; Server-Sent Events status streams per run and per space, resumable with `Last-Event-ID` (`runs.event_retention`, 24 h); `cancelled_by` and an optional cancel reason.
- **File management:** save a file with `PUT` and version checks (`If-Match`, `412`/`428`), rename and move files and folders, create folders, delete folders (`?recursive=true`), copy an artifact into the space (`promote`); listings include folders and each file's version.

- **Each run executes a snapshot** of the notebook taken when it was submitted.
- **Python notebooks only, stated and checked:** running or publishing a notebook whose kernel is another language (R, Julia, …) is now `400` with a clear message, instead of running it as Python and failing in its first cell.
- **What a run can reach:** run outputs and published versions moved out of the space's files; a run mounts only the space's files (`/space`) and its own folder (`/run`). Function calls and runs started with `"space": "read-only"` see the files read-only and may run alongside other runs of the space.
- **Disk:** a run that takes its space over quota is stopped (`resource_disk`).
- **Storage layout:** `spaces/<id>/files`, `runs`, `functions`. The runner image's status format is now 2; use the runner image from the same release.

### First release (planned as 0.1.0)

First release: run Jupyter notebooks headless in isolated spaces, with live logs.

- **Spaces and files:** create, list, rename and delete spaces; upload (all or nothing, size limits and quota, `?folder=` for the destination), list, download and delete files. Another user's space is always 404.
- **Runs:** start a notebook with parameters; FIFO queue with global concurrency; one active run per space; cancel; time limit; idempotent submission (`Idempotency-Key`); results with status, failing cell, usage, artifacts, executed notebook and rendered HTML; retention.
- **Isolation:** each run in a fresh container with no network, a read-only image, only its own space mounted, no capabilities, uid 1000, and memory, CPU, process and `/tmp` limits. Docker access goes through an allowlist socket proxy.
- **Live logs:** Server-Sent Events with replay, resume (`Last-Event-ID`) and a log cap; slow viewers can't slow a run.
- **Dependency check:** warns (or, optionally, rejects) when a notebook imports packages the runner image doesn't have.
- **Optional local web UI** (off by default): browse spaces, view notebooks read-only, run them, follow the live log.
- **Operations:** health and readiness checks, Prometheus metrics with example alerts and dashboard, graceful shutdown, recovery after Docker restarts, backup and restore, production Compose with Caddy (TLS).
- **Evidence:** isolation report generated from 35+ automated checks (`make report`); backend conformance suite; fuzzing; soak test.

**Known limitations:**
See [docs/security/threat-model.md](docs/security/threat-model.md): trusted users; storage quotas are checked by the service, not enforced by the filesystem; the web UI has no login; no login rate limiting; Docker Desktop isn't a reference isolation platform.
