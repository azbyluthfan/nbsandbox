# nbsandbox

Run Jupyter notebooks headless, each in its own isolated sandbox, with live logs and results over an API.

`nbsandboxd` is a self-hosted Go service:
- **Spaces:** you upload a notebook and its data files to a **space**.
- **Runs:** you start a **run**, with parameters if you like, and watch its console output live over Server-Sent Events.
- **Results:** when it finishes you get the status, the failing cell if any, resource usage, the executed notebook, a rendered HTML copy, and the files it wrote.
- **Functions:** publish a notebook as a versioned function that other services call with JSON and get JSON back, items streamed as they are produced ([docs/functions.md](docs/functions.md)).
- **Two APIs, one model:** REST with an OpenAPI spec, and gRPC (Connect) for the same operations.
- **For AI agents:** an MCP server (`nbsandbox-mcp`) lets Claude and other agents write notebooks, run them in the sandbox and read the results, as their user ([docs/mcp.md](docs/mcp.md)).
- **Web editor:** browse spaces, edit notebooks and files with completion from the runner's packages, run, follow the log, see charts and maps in the results. Users sign in with your identity provider (or a users file) ([docs/auth.md](docs/auth.md#signing-in-to-the-web-ui)).
- **Interactive sessions:** run a notebook's cells one by one in a live kernel, as in Jupyter, with a run's isolation. Completion comes from the real objects (`df.` lists a DataFrame's columns). In the editor, or over a WebSocket of Jupyter messages ([docs/sessions.md](docs/sessions.md)).
- **A space's own packages:** a `requirements.txt` is built once into a package set the space's runs import, with no network in the runs ([docs/packages.md](docs/packages.md)).
- **Data where it lives:** notebooks query PostgreSQL, MySQL, SQL Server and S3-compatible storage by names their space defines (`nbsandbox.sql("SELECT … FROM crm.orders")`), and call HTTP APIs (`nbsandbox.http("events").get("orders")`), per environment (dev, prod), never seeing a host or a password: a fetcher of the run's own reads for it and adds the credentials ([docs/connections.md](docs/connections.md)).
- **Shared spaces:** a space's creator grants others `read`, `write` or `owner`; every member sees the space's runs, and each reads data with their own connections under the space's names ([docs/sharing.md](docs/sharing.md)).
- **For operators:** an audit log of every change and download, administrators who can list and stop any run, and draining before maintenance ([docs/operations.md](docs/operations.md#audit-log)).

Every run executes in a **fresh, locked-down container or Pod**. Code in one space can't see another space's files, processes or memory, can't reach the network, and can't use up another run's resources. An automated test suite checks each of these and writes a report: Docker ([report](docs/isolation-report.md)), Linux with Docker Engine with `runc` and gVisor side by side ([report](docs/isolation-report-linux.md)), and Kubernetes ([report](docs/isolation-report-kubernetes.md)).

> **Status:** v0.4, pre-1.0: the API is versioned (`/v1`) and follows the compatibility policy in [docs/api.md](docs/api.md), but expect changes between minor versions. See [Limitations](#limitations).

**See it working:** [docs/tour.md](docs/tour.md) walks through the web editor and the API step by step, with a screenshot or the real output of each step.

## Why

Running other people's notebooks on a shared server usually means trusting them a lot or building a sandbox yourself. nbsandbox is that sandbox, kept small:
- **A Go service, PostgreSQL, and object storage.** No message queue. One process does everything on a laptop; the same binary splits into API and worker processes on several machines or in Kubernetes.
- **One model, two backends.** Runs are Docker containers (the default, with `runc` or gVisor) or Kubernetes Pods (with a deny-all NetworkPolicy and an optional sandbox RuntimeClass).
- **Isolation that is tested,** not just configured: every control has a check, and the reports show what each check observed.

## How it works

```
client ──HTTPS──> nbsandboxd (API) ───────┐
                    │                      │  PostgreSQL: spaces, runs, events, queue
                    │                      │  object storage (S3, Azure Blob): file contents and results
                    ▼                      │
              nbsandboxd (worker) ─────────┘
                    │
        Docker (through an allowlisting socket proxy)   or   Kubernetes API
                    │                                          │
        one container per run                       one Pod per run
        runc or gVisor (runsc)                      deny-all NetworkPolicy, optional RuntimeClass
        no network, read-only image, non-root, no capabilities, limits, only its own files
```

- **API and workers:** one process can be both (`role: all`), or run them separately and scale each ([docs/scaling.md](docs/scaling.md)). Workers claim queued runs from PostgreSQL, with leases so a run whose worker dies is taken over.
- **Runner image:** Python 3.13 with papermill and the data-science packages listed by `GET /v1/runtime`. A space can add its own packages ([docs/packages.md](docs/packages.md)), or you build your own image for everyone.
- **Files:** contents live in object storage (RustFS, Ceph, AWS S3, Azure Blob), each stored once. A run works on its own copy, and its changes are written back with conflict checks.
- **Reading data where it lives:** a space names your databases, buckets and APIs (PostgreSQL, MySQL, SQL Server, S3-compatible storage, HTTP), and notebooks query them with `nbsandbox.sql(...)` or call them with `nbsandbox.http(...)`. A run never sees a credential: a fetcher container or Pod beside it holds them and reads the sources through an egress gateway, only the endpoints that run may reach ([docs/connections.md](docs/connections.md)).
- **Interactive sessions** keep a notebook's kernel alive between cells, in the web editor or over the API ([docs/sessions.md](docs/sessions.md)); **published functions** let a service call a notebook ([docs/functions.md](docs/functions.md)).
- **Compute:** `GET /v1/workers` and the web UI's Compute page show the workers and how busy each is; a run's page charts its memory, CPU and disk writes ([docs/scaling.md](docs/scaling.md)).

## Quickstart (local, Docker Desktop or Linux)

Needs Docker, Go 1.27.4+, `make`, `curl` and `jq`.

```bash
git clone https://github.com/azbyluthfan/nbsandbox.git && cd nbsandbox
make runner-image      # build the runner image (a few minutes the first time)
make up                # API on http://127.0.0.1:8080, web UI on http://127.0.0.1:8090
```

On Linux, add `DOCKER_GID=$(getent group docker | cut -d: -f3)` to `make up`. Then try a run as the demo user:

```bash
export NBSANDBOX_URL=http://127.0.0.1:8080 NBSANDBOX_USER=alice NBSANDBOX_PASSWORD=alice-dev-password
ID=$(scripts/nbsandbox.sh spaces create demo | jq -r .id)
scripts/nbsandbox.sh files upload "$ID" tests/workloads/smoke.ipynb
RUN=$(scripts/nbsandbox.sh runs start "$ID" smoke.ipynb | jq -r .id)
scripts/nbsandbox.sh runs follow "$ID" "$RUN"
```

You can also run `make seed`, which fills the stack with example spaces, notebooks, data, runs and a published function, then open http://127.0.0.1:8090, pick `alice` and the **Sales analysis** space, open a notebook and press **Run**. In the dev stack the web UI has no sign-in (deployments sign people in: [docs/auth.md](docs/auth.md#signing-in-to-the-web-ui)). The demo users' passwords are public: nbsandboxd warns at startup when they are loaded.

Stop with `make down`. `make report` runs the isolation suite on your machine and writes `docs/isolation-report.md`.

**On Kubernetes, locally:** `make k8s-up` starts a kind cluster with Cilium inside Docker and deploys the Helm chart (API on the same port). Add `K8S_RUNTIME_CLASS=gvisor` to run notebooks under gVisor.

## Production

| Where | Guide |
|---|---|
| One Linux server, Docker Compose, Caddy for TLS | [docs/deployment.md](docs/deployment.md) |
| Kubernetes (Helm chart; Terraform example for AKS) | [docs/kubernetes.md](docs/kubernetes.md) |
| Several machines without Kubernetes | [docs/scaling.md](docs/scaling.md) |
| Stronger isolation with gVisor | [docs/gvisor.md](docs/gvisor.md) |
| Sign-in with your identity provider (OpenID Connect) | [docs/auth.md](docs/auth.md) |
| Monitoring, backups, upgrades, runbooks | [docs/operations.md](docs/operations.md) |
| More Python packages for a space (`requirements.txt`) | [docs/packages.md](docs/packages.md) |
| Reading databases and object storage (connections, Vault) | [docs/connections.md](docs/connections.md) |

## Limitations

Read [docs/security/threat-model.md](docs/security/threat-model.md) before deploying. In short:
- **Users are trusted** not to attack the platform on purpose. Runs are hardened and tested, but with `runc` they share the host's kernel. For less trusted users, run notebooks under gVisor ([docs/gvisor.md](docs/gvisor.md)) or another sandbox RuntimeClass on Kubernetes.
- **Python notebooks only.** Runs use the Python 3 kernel; a notebook written for another kernel (R, Julia, …) is refused when you run or publish it.
- **No network in runs.** Notebooks can't `pip install`, and reach outside data only through their connections ([docs/connections.md](docs/connections.md)): no HTTP APIs from notebook code yet. A space's `requirements.txt` can add packages (wheels from the package index, built once into a set the space's runs import) when the operator turns it on, on Docker for now ([docs/packages.md](docs/packages.md)); otherwise build a runner image with the packages you need.
- **Authentication:** bearer tokens from any OpenID Connect provider, or basic auth from a users file, with revocation, a limit on failed sign-ins, and a web UI sign-in. Not yet: sharing a space between users.
- **No widgets or `input()` in sessions:** an interactive session's kernel can't ask for input, and ipywidgets and other JavaScript outputs show as text.
- **Custom packages on Docker only** for now; Kubernetes is next.
- **Reference platforms:** the isolation reports that count are Linux with Docker Engine and your own Kubernetes cluster. Docker Desktop and kind work for development.

## Documentation

| Document | For |
|---|---|
| [docs/tour.md](docs/tour.md) | A guided tour, screen by screen |
| [docs/user-guide.md](docs/user-guide.md) | People running notebooks: spaces, files, parameters, statuses, logs, the editor |
| [docs/functions.md](docs/functions.md) | Calling a notebook from your service: contracts, publishing, invoking, streamed output |
| [docs/sessions.md](docs/sessions.md) | Interactive sessions: a notebook's live kernel, in the editor or through the API |
| [docs/packages.md](docs/packages.md) | A space's own Python packages (`requirements.txt`) |
| [docs/api.md](docs/api.md) and [api/openapi.yaml](api/openapi.yaml) | The REST and gRPC APIs and their compatibility policy |
| [docs/mcp.md](docs/mcp.md) | AI agents: the MCP server, its tools and how to connect Claude |
| [docs/auth.md](docs/auth.md) | Authentication: tokens from your identity provider, scopes, quotas, administration |
| [examples/](examples/) | Complete clients (Python, Go) that run a notebook and call a function |
| [docs/deployment.md](docs/deployment.md), [docs/kubernetes.md](docs/kubernetes.md), [docs/scaling.md](docs/scaling.md), [docs/gvisor.md](docs/gvisor.md) | Installing it |
| [docs/operations.md](docs/operations.md) | Running it: monitoring, backups, upgrades, runbooks |
| [docs/isolation-report.md](docs/isolation-report.md), [docs/isolation-report-linux.md](docs/isolation-report-linux.md), [docs/isolation-report-kubernetes.md](docs/isolation-report-kubernetes.md) | What the isolation tests found; [docs/check-deployment.md](docs/check-deployment.md) produces the same report for your own deployment |
| [docs/security/threat-model.md](docs/security/threat-model.md), [docs/security/review.md](docs/security/review.md) | What nbsandbox protects against, and the latest security review |
| [docs/perf.md](docs/perf.md) | Performance measurements and soak tests |
| [docs/releasing.md](docs/releasing.md), [CHANGELOG.md](CHANGELOG.md), [ROADMAP.md](ROADMAP.md) | Versions and what's next |

## Contributing

See [CONTRIBUTING.md](CONTRIBUTING.md). Every commit must be signed off (Developer Certificate of Origin).

## Security

Please report vulnerabilities privately. See [SECURITY.md](SECURITY.md).

## Licence

Apache License 2.0. See [LICENSE](LICENSE) and [NOTICE](NOTICE).
