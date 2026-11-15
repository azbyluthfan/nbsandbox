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

## Next

- **Interactive mode.** Designed and agreed: run cells live in the editor, with autocomplete, signatures and docs from the live kernel (`df.` lists the columns). A kernel per notebook in a long-lived container with a run's isolation (no network), reached over its stdio and proxied as Jupyter messages over a WebSocket. Docker first, Kubernetes after.
- **Authentication against Entra ID, end to end.** Keycloak is tested end to end (tokens, scopes, audience, token exchange); Entra ID is unit-tested against a local issuer and documented, and needs the same end-to-end test with a real tenant.
- **Sandboxes on real clusters.** Run the suite under GKE Sandbox (gVisor) and AKS Pod Sandboxing (Kata), and add a GKE Terraform example beside the AKS one. `nbsandbox-report check` makes each one a deploy, a check and a teardown.

## Later

| Item | What it adds | Where it lands |
|---|---|---|
| Fast starts (under 1 s) | A small call takes about 2.5 s, which suits batch work and most function calls. What remains is CPU work on the run's one CPU (the Jupyter client libraries and the kernel's imports, about 0.6 s each; [perf.md](docs/perf.md#starting-a-run)): starting the kernel earlier doesn't help on one CPU, so going under 1 s takes **a pool of single-use pre-started containers** per published function version (created and kernel started ahead of time, used once, then removed), and later restoring a booted kernel from a snapshot (gVisor checkpoint/restore, Firecracker snapshots). Waits for a caller that needs it | isolation, runs, runner image |
| Shared spaces | Grants keyed by caller ID (`space, principal, role`), read or read-write | auth, API, web UI |
| Completion through variables | `df.` after `df = pd.read_csv(...)`: follow return annotations in the completion index (today it knows modules, aliases and names across cells), or the kernel's own completion in interactive mode (next) | runner image, web UI |
| Deeper lint in the editor | ruff (as WebAssembly, in the browser): undefined names, unused imports, common mistakes across all cells | web UI |
| Bring your own image | A space or function names its own image (any language or package set), pinned by digest from an allowed registry, with the runner contract installed. Only with gVisor or a sandbox RuntimeClass: a foreign image must not get `runc`'s level of trust | runner images, isolation, runs |
| Custom packages on Kubernetes | Package sets on Kubernetes: build pods with an egress policy of their own, and a per-node cache of sets mounted read-only into run pods (a copy per run would be slow for large sets) | isolation, runs, Helm |
| Other notebook languages | R first, then Julia if asked for. One runner image per language, chosen from the notebook's kernel against an allow-list (smaller and patched separately, rather than one image with every language): the language's kernel, parameter injection (papermill supports R and Julia), a `nbsandbox.output()` equivalent writing the same status file, its package list for the missing-import check, and editor highlighting. Until then, non-Python notebooks are refused | runner images, runs, web UI |
| Network access to registered endpoints | Runs have no network today. When notebooks need external APIs: the space owner registers HTTPS endpoints (within a list the operator allows); runs with endpoints join an internal network whose only exit is an egress proxy that permits just those hosts; private addresses (host, other containers, cloud metadata) are always refused; no DNS inside the container; every attempt is logged and shown with the run; isolation tests prove unregistered hosts and bypasses fail. Comes with **space secrets** (encrypted API keys injected only into runs, never logged or returned) | host setup, isolation, runs, API, web UI |
| Resource tiers | e.g. small and large runs, chosen from a configured list | isolation, runs |
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
