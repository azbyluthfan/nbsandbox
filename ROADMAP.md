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

## Next

- **Authentication against real identity providers.** Token checking, scopes and on-behalf-of calls are built and unit-tested against a local issuer; next, end-to-end tests with Entra ID and Keycloak, and a guide for each.
- **"Check this cluster".** A command that runs the network probe and the isolation checks inside a target Kubernetes cluster and writes its own report, so every deployment, not only kind, has evidence. With it, decide which Kubernetes versions are supported.
- **Sandboxes on real clusters.** Run the suite under GKE Sandbox (gVisor) and AKS Pod Sandboxing (Kata), and add a GKE Terraform example beside the AKS one.
- **Fast starts (≤ 1 s).** Today a run spends about 3–5 s (about a second more under gVisor) before its first cell, almost all of it starting a fresh container and Python kernel. Cut it while keeping a fresh container per run:
  - skip work at start (render HTML after the result, lighter imports, run nbclient directly instead of papermill);
  - keep a small pool of single-use pre-started containers per published function version (created and kernel started ahead of time, used once, then removed);
  - later, restore a booted kernel from a snapshot (gVisor checkpoint/restore, Firecracker snapshots).
- **Reference report at production sizes.** The Linux reference report ran with 512 MiB per run on a 2 GB VM; repeat it with the default 2 GiB.

## Later

| Item | What it adds | Where it lands |
|---|---|---|
| Web UI login | The editor becomes usable beyond one's own machine: authorization code with PKCE against the identity provider, and a server-side session cookie (a browser's `EventSource` can't send a bearer token) | web UI, auth |
| Shared spaces | Grants keyed by caller ID (`space, principal, role`), read or read-write | auth, API, web UI |
| Token revocation and login rate limiting | Token introspection (RFC 7662) for instant revocation; rate limiting of failed basic-auth logins | auth |
| Interactive mode | Run cells live from the editor, without JupyterHub: nbsandboxd starts a long-lived **session** container from the runner image (same limits and mounts as a run, still no network) with a kernel server listening on a Unix socket in the session's folder, and proxies the Jupyter kernel protocol over a WebSocket. A session that can write holds the space's write lock like a writing run; read-only sessions run beside others. Idle timeout, maximum length, and a limit on open sessions. The kernel also answers completion and inspection with live values (`df.` lists the columns). Needs the web UI login first | isolation, runs, API, web UI, runner image |
| Completion through variables | `df.` after `df = pd.read_csv(...)`: follow return annotations in the completion index (today it knows modules, aliases and names across cells), or the kernel's own completion in interactive mode | runner image, web UI |
| Deeper lint in the editor | ruff (as WebAssembly, in the browser): undefined names, unused imports, common mistakes across all cells | web UI |
| Bring your own image | A space or function names its own image (any language or package set), pinned by digest from an allowed registry, with the runner contract installed. Only with gVisor or a sandbox RuntimeClass: a foreign image must not get `runc`'s level of trust | runner images, isolation, runs |
| Custom packages per space | A space's requirements built into a derived, cached image (today's dependency check becomes the input) | runner image, runs |
| Other notebook languages | R first, then Julia if asked for. One runner image per language, chosen from the notebook's kernel against an allow-list (smaller and patched separately, rather than one image with every language): the language's kernel, parameter injection (papermill supports R and Julia), a `nbsandbox.output()` equivalent writing the same status file, its package list for the missing-import check, and editor highlighting. Until then, non-Python notebooks are refused | runner images, runs, web UI |
| Network access to registered endpoints | Runs have no network today. When notebooks need external APIs: the space owner registers HTTPS endpoints (within a list the operator allows); runs with endpoints join an internal network whose only exit is an egress proxy that permits just those hosts; private addresses (host, other containers, cloud metadata) are always refused; no DNS inside the container; every attempt is logged and shown with the run; isolation tests prove unregistered hosts and bypasses fail. Comes with **space secrets** (encrypted API keys injected only into runs, never logged or returned) | host setup, isolation, runs, API, web UI |
| Audit log | Who uploaded, ran, downloaded or deleted what, and when | store, runs, observability |
| Admin operations | List and stop any run; drain mode before maintenance | runs, API |
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
