# Roadmap

Where nbsandbox is going. Items are roughly in priority order within each section; none has a date yet. Comment on or open an issue if one matters to you.

## Released in v0.1.0

The first release covers what was planned as v0.1 to v0.4, plus scaling out and Kubernetes. The [CHANGELOG](CHANGELOG.md) has the details:
- **Runs:** spaces, runs with parameters, live logs, results with artifacts and resource usage.
- **Functions:** notebooks published as versioned functions with JSON contracts, called with JSON, with streamed output.
- **Storage and scaling:** PostgreSQL and object storage (S3-compatible, Azure Blob) in every mode; API and worker roles, so one binary runs on a laptop or on many machines ([scaling.md](docs/scaling.md)).
- **Backends:**
  - **Docker**, with `runc` or gVisor ([gvisor.md](docs/gvisor.md));
  - **Kubernetes**, with a deny-all NetworkPolicy checked by a probe Pod and an optional sandbox RuntimeClass, plus a Helm chart and a Terraform example for AKS ([kubernetes.md](docs/kubernetes.md)).
- **Authentication:** OpenID Connect bearer tokens with scopes per endpoint, per-owner quotas, admin endpoints, and the calling app recorded on runs ([auth.md](docs/auth.md)).
- **Local web editor:** completion and documentation from the runner image's packages.
- **Isolation reports** for Docker, Linux with `runc` and gVisor, and Kubernetes with both runtimes.

## Released in v0.1.1

A patch release with two fixes (Kubernetes runs copied their files slowly; Go 1.27.4 for a `net/http` vulnerability) and these additions:

- **Check your deployment:** `nbsandbox-report check` produces the isolation report for any running deployment, Docker or Kubernetes ([check-deployment.md](docs/check-deployment.md)). Kubernetes 1.34 to 1.37 are supported, the ends tested weekly.
- **MCP server for AI agents** (`nbsandbox-mcp`): agents write, run and read notebooks as their user ([mcp.md](docs/mcp.md)).
- **Keycloak, end to end:** user tokens, scopes, audience and token exchange against a real Keycloak, weekly ([auth.md](docs/auth.md#keycloak)).

## Released in v0.1.2

A patch release with two fixes (tornado 6.6.1 in the runner image; gVisor installs again on the local Kubernetes cluster) and:

- **Faster results:** rendering `output.html` takes about 0.1 s instead of 1 s, so a small call on Docker takes 2.5 s instead of 2.9 s ([perf.md](docs/perf.md#starting-a-run)).
- **The reference reports at production sizes:** the weekly CI run tests Linux with Docker Engine (runc and gVisor, default 2 GiB per run) and Kubernetes 1.37 (runc and gVisor); the reports in `docs/` are rendered from a passing run by a person and reviewed like any change ([releasing.md](docs/releasing.md)).

## Released in v0.2.0

Security and operations for real deployments, and a space's own packages. The [CHANGELOG](CHANGELOG.md) has the details and upgrade notes:

- **Token revocation and login rate limiting:** introspection (RFC 7662) for tokens revoked at the provider, and a limit on failed basic-auth sign-ins per username and per client address ([auth.md](docs/auth.md#revoked-tokens)).
- **Custom packages per space (Docker):** a space's `requirements.txt` is built once into a package set its runs import; runs keep no network; published functions keep their exact versions ([packages.md](docs/packages.md)). Kubernetes is next.
- **Audit log and admin operations:** who did what, when and from where, for every change, download and administrative operation; administrators list and stop any run, and drain the workers before maintenance ([operations.md](docs/operations.md#audit-log)).
- **Web UI login:** the editor is usable beyond one's own machine. Users sign in with the identity provider (or `users.yaml`) and are the same caller as through the API; server-side sessions; served at the domain's root by Caddy or the Ingress ([auth.md](docs/auth.md#signing-in-to-the-web-ui)).

## Released in v0.2.1

A patch release: the runner images published on GHCR up to 0.2.0 had a broken manifest label, so nbsandboxd's Docker backend refused them; 0.2.1 fixes the release workflow and checks the published label. A weekly job now deploys the newest release from its published images, as [deployment.md](docs/deployment.md) does, and runs notebooks through it (`scripts/check-published.sh`).

## Released in v0.3.0

Interactive mode. The [CHANGELOG](CHANGELOG.md) has the details and upgrade notes:

- **Interactive sessions:** a notebook's live kernel with a run's isolation (no network, the run limits, the runtime, the space's packages). Run cells one by one and keep the variables, as in Jupyter, on Docker, Kubernetes and clusters ([sessions.md](docs/sessions.md)).
- **In the editor:** a session bar (run all, interrupt, restart, save files, end), outputs with tables and charts, completion, signatures and docs from the live kernel (`df.` lists the columns), and a variables panel.
- **Through the API and for agents:** REST, a WebSocket of Jupyter messages, gRPC's `SessionService` ([api.md](docs/api.md)), and MCP tools to open a session, run cells and read their output ([mcp.md](docs/mcp.md)).
- **Checked like runs:** sessions are in the isolation reports, `nbsandbox-report check`, the soak, and the weekly check of the published release.

## Released in v0.4.0

Data outside nbsandbox, the first phase of [plan-data-access.md](docs/plan-data-access.md), and a view of the compute. The [CHANGELOG](CHANGELOG.md) has the details and upgrade notes:

- **Notebooks read the user's data where it lives,** without copying it in and without secrets in notebook code: `nbsandbox.sql("SELECT … FROM crm.orders")` returns a DataFrame, `nbsandbox.stream(…)` Arrow batches. PostgreSQL, MySQL, SQL Server, and CSV and Parquet in S3-compatible storage, joined in one query if need be ([connections.md](docs/connections.md)).
- **Connections** are defined once per user and named per space, with environments (dev, prod); runs, interactive sessions and published functions pick one. Through the API, gRPC, MCP and the web UI.
- **Credentials** in Kubernetes Secrets (mounted into the fetcher), Vault or OpenBao (the fetcher logs in itself), or a built-in store for development; nbsandboxd never reads them when a run starts.
- **Each reading run gets a fetcher** (DuckDB, Arrow Flight) on a network of the run's own, locked to one `SELECT` on the run's sources, reaching them only through nbsandbox's egress gateway with a token for exactly their endpoints. On Docker and Kubernetes; the isolation report has a Connections section.
- **The compute view:** the workers and how busy each is (`GET /v1/workers`, the web UI's Compute page), and each run's memory, CPU and disk writes over its life, charted on its page (Docker's stats API; Kubernetes' metrics API, with metrics-server).
- **Fixed:** interactive sessions now survive a Kubernetes rolling upgrade (another worker takes them over), and signing in to the web UI with a password works in browsers.
- **Checked:** upgrades from 0.3.0 with Compose and with Helm, in the weekly run.

## Next
- **Shared spaces.** A space grants roles (read, write, owner) to other callers, users or apps; every member sees the space's runs, and each reads data with their own connections under the space's names, functions included ([plan](docs/plan-shared-spaces.md)).
- **HTTP APIs in notebooks.** A notebook's code calls the user's APIs (JSON, uploads, streaming SSE) with any HTTP client, through its run's fetcher, which adds the credential (a header token, basic auth, or OAuth 2.0 client credentials): the secret never enters the run ([plan](docs/plan-data-access.md#http-apis-through-the-fetcher)).
- **Authentication against Entra ID, end to end.** Keycloak is tested end to end (tokens, scopes, audience, token exchange); Entra ID is unit-tested against a local issuer and documented, and needs the same end-to-end test with a real tenant.
- **Sandboxes on real clusters.** Run the suite under GKE Sandbox (gVisor) and AKS Pod Sandboxing (Kata), and add a GKE Terraform example beside the AKS one. `nbsandbox-report check` makes each one a deploy, a check and a teardown.

## Later

| Item | What it adds | Where it lands |
|---|---|---|
| Fast starts (under 1 s) | A small call takes about 2.5 s, which suits batch work and most function calls. What remains is CPU work on the run's one CPU (the Jupyter client libraries and the kernel's imports, about 0.6 s each; [perf.md](docs/perf.md#starting-a-run)): starting the kernel earlier doesn't help on one CPU, so going under 1 s takes **a pool of single-use pre-started containers** per published function version (created and kernel started ahead of time, used once, then removed), and later restoring a booted kernel from a snapshot (gVisor checkpoint/restore, Firecracker snapshots). Waits for a caller that needs it | isolation, runs, runner image |
| Deeper lint in the editor | ruff (as WebAssembly, in the browser): undefined names, unused imports, common mistakes across all cells | web UI |
| Bring your own image | A space or function names its own image (any language or package set), pinned by digest from an allowed registry, with the runner contract installed. Only with gVisor or a sandbox RuntimeClass: a foreign image must not get `runc`'s level of trust | runner images, isolation, runs |
| Custom packages on Kubernetes | Package sets on Kubernetes: build pods with an egress policy of their own, and a per-node cache of sets mounted read-only into run pods (a copy per run would be slow for large sets) | isolation, runs, Helm |
| Other notebook languages | R first, then Julia if asked for. One runner image per language, chosen from the notebook's kernel against an allow-list (smaller and patched separately, rather than one image with every language): the language's kernel, parameter injection (papermill supports R and Julia), a `nbsandbox.output()` equivalent writing the same status file, its package list for the missing-import check, and editor highlighting. Until then, non-Python notebooks are refused | runner images, runs, web UI |
| Resource tiers | e.g. small and large runs, chosen from a configured list; with data access, a choice of runner sizes and counts per run ([plan](docs/plan-data-access.md#a-run-with-several-runners)) | isolation, runs |
| Distributed runs | One run over several machines: an orchestrator and workers on the run's private network, each reading its share of the data; DuckDB per worker first, a distributed engine (DataFusion with Ballista, or Daft on Ray) when a workload needs one ([plan](docs/plan-data-access.md#distributed-mode)) | isolation, runs, Helm |
| Injected credentials | A connection's raw credential given to the notebook's own code, for clients that need it themselves (an SDK that signs requests or runs its own OAuth flow, a database driver in the notebook), redacted from logs, outputs and results, with the run's network open to that endpoint only, through the gateway ([plan](docs/plan-data-access.md#two-modes-mediated-and-injected)). Waits for a client that needs it | runs, runner image, gateway |
| Iceberg and Azure Blob | Iceberg tables and their catalogs, and Azure Blob Storage, read like the other sources | fetcher |
| Writes to external sources | Results written back to a database or object store, through the same connections; today a run writes artifacts | fetcher, runs |
| Scheduled runs | Run a notebook on a schedule with fixed parameters | runs |
| Filesystem quotas | Quotas enforced by the filesystem, not just checked every few seconds | host setup, spaces |
| Firecracker backend | Same `Backend` interface; must pass `internal/isolation/backendtest` | isolation |

## Won't do

| Item | Why not |
|---|---|
| Completion webhooks | The status streams (`GET .../runs/{id}/events` and `GET .../events`) and long-polling (`?wait=`) already tell a caller when a run finishes, without nbsandbox having to call out to other systems. Webhooks would also need outbound network access from nbsandboxd, request signing, retries and a delivery log. |

## Good first issues

Look for the `good first issue` label. Typical ones:
- web UI polish;
- more workload notebooks for the integration tests;
- documentation fixes found while following [docs/deployment.md](docs/deployment.md) or the [tour](docs/tour.md).
