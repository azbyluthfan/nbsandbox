# nbsandbox on Kubernetes

Workers can start each run as a **Pod** instead of a Docker container (`backend: kubernetes`). Everything else is the same as in [cluster mode](scaling.md): API processes behind a Service, workers claiming runs from PostgreSQL, file contents in object storage. Docker stays the default backend; local development and Docker Compose deployments don't change.

## What a run Pod looks like

| | |
|---|---|
| **Files** | Its own `emptyDir` volumes for `/space` and `/run`. The worker copies the run's files in before it starts, and back out after it ends, through the Kubernetes API (`pods/exec`); while it works, the worker copies new events (items, progress, cells) out twice a second, so callers get them live. No shared storage, no host paths, no storage credentials in the Pod. Links and special files the notebook creates are never copied back. |
| **Identity** | No service-account token, no service environment variables, no host namespaces, no DNS. |
| **Privileges** | uid 1000 (non-root), all capabilities dropped, no privilege escalation, read-only root filesystem, seccomp `RuntimeDefault`. It meets the `restricted` Pod Security Standard, which the chart enforces on the runs namespace. |
| **Network** | None: a deny-all NetworkPolicy covers the whole runs namespace. |
| **Resources** | Memory and CPU requests equal the limits (Guaranteed), so a node holds only as many runs as its memory allows: size the runs node pool for `worker.replicas × worker.concurrency` runs at `runs.memory` each. An out-of-memory run ends as `oom` (with cgroup v2 the whole container is killed, so its peak memory isn't reported). `/tmp` and `/dev/shm` are size-limited and in memory. `/space` and `/run` have an ephemeral-storage limit; a run that goes over is evicted and ends as `failed` / `resource_disk`. |
| **Processes** | Limited **per node** in Kubernetes (kubelet `podPidsLimit`, e.g. 300), not per run. Set it on the nodes that run notebooks: the AKS example does. With a sandbox RuntimeClass, see below. |
| **Optional sandbox** | `runs.runtimeClass` (gVisor; Kata through AKS Pod Sandboxing). It must exist and is never silently replaced by the default. |

### With a sandbox RuntimeClass (gVisor, Kata)

A sandbox with its own kernel changes two things:
- **The process limit:** the node's `podPidsLimit` counts the sandbox's own threads too (gVisor uses more than one host thread per process inside, and dies if it is refused one). So with a RuntimeClass the run's limit is `RLIMIT_NPROC` (`limits.pids`, 256 by default), which the runner sets inside the sandbox, and `podPidsLimit` on those nodes is only a backstop: set it to about 4 × `limits.pids` + 256 (1,280).
- **Peak memory** is sampled inside gVisor (it emulates the cgroup files without a peak counter).

Everything else is the same; the whole suite passes under a gVisor RuntimeClass ([report](isolation-report-kubernetes.md)). The costs are those measured on Docker ([gvisor.md](gvisor.md)). On GKE, a GKE Sandbox node pool provides the `gvisor` RuntimeClass; elsewhere, install gVisor's containerd shim on the nodes (as `scripts/kind-gvisor.sh` does for kind).

A run Pod has two containers:
- **`runner`**: waits until the worker has copied the files in, then runs the notebook;
- **`files`**: idles so the worker can copy files in before, and the outputs out after the runner ends.

Cancelling or a timeout stops the runner (`SIGTERM`; if it hasn't ended after `runs.stop_grace`, the Pod is deleted), and the partial outputs are still copied out. The worker also checks `/space` and `/run` every 5 seconds and stops a run that goes over the space quota (`resource_disk`), before the node's eviction would.

## Which clusters: any network plugin that enforces NetworkPolicy

nbsandbox doesn't depend on a particular network plugin (CNI). The chart creates a standard `NetworkPolicy`, and any plugin that enforces NetworkPolicy, including egress, works. It's usually a choice made when the cluster is created:

| Platform | Enforcing options | Note |
|---|---|---|
| AKS | Azure CNI powered by Cilium, Calico, Azure Network Policy Manager | choose one at cluster creation (the Terraform example's `network_policy`) |
| GKE | Dataplane V2; legacy Calico network policy enforcement | Dataplane V2 is the default on Autopilot |
| EKS | the VPC CNI's network policy support, Calico, Cilium | the VPC CNI's is off until enabled |
| OpenShift | OVN-Kubernetes | enforces by default |
| On premises | Cilium, Calico, Antrea, k3s's built-in policy controller | Flannel alone doesn't enforce (Canal does) |

Plugins differ in how they treat traffic to host-network endpoints (the API server, nodes, the cloud metadata service). That's why the workers check the result for themselves, whichever plugin it is.

**Tested locally, on kind (2026-11-08):**

| Plugin | Result |
|---|---|
| Cilium 1.17 | the full integration suite and its isolation report pass; nothing is reachable from a run Pod |
| Calico 3.29 | the isolation suite, memory, cancel, disk-quota and runtime tests pass; nothing is reachable from a run Pod |
| kindnet (kind's own) | doesn't enforce the policy: the probe Pod reached the internet, the node's kubelet, the worker and the API server. Both workers said so, stayed not ready and took no runs; submitting a run got 503 and no run Pod was created |

So a cluster whose plugin doesn't enforce NetworkPolicy fails closed: nbsandbox stops running notebooks instead of running them with network access.

## The network probe

The deny-all policy only works if it's really enforced: some plugins accept policies and silently ignore them, or skip some destinations (kind's default plugin lets Pods reach the API server; the local cluster uses Cilium instead).

So at startup, and every 10 minutes, each worker starts a **probe Pod** labelled like a run. It tries to reach:
- the Kubernetes API server;
- the cloud metadata service (169.254.169.254: on every cloud it hands out credentials);
- the internet (1.1.1.1, 8.8.8.8);
- the worker itself and its node's kubelet.

If anything answers, the worker refuses runs and reports it on `/readyz` (`runtime`), so a misconfigured cluster never runs a notebook with network access.

## Install with Helm

The chart is in [deploy/helm/nbsandbox](../deploy/helm/nbsandbox). It doesn't install PostgreSQL or object storage: bring a managed database and storage account, or your own.

```bash
kubectl create namespace nbsandbox
kubectl -n nbsandbox create secret generic nbsandbox-db --from-literal=database-url='postgres://…?sslmode=require'
helm upgrade --install nbsandbox oci://ghcr.io/azbyluthfan/charts/nbsandbox --version 0.1.0 -n nbsandbox -f my-values.yaml
```

Releases publish the chart signed, on GHCR ([releasing.md](releasing.md) shows how to verify it); from a checkout, use `deploy/helm/nbsandbox` instead of the `oci://` address.

What it creates:
- **API Deployment and Service:** no Kubernetes permissions.
- **Worker Deployment:** a Role allowing only Pods, Pod logs and `pods/exec`, only in the runs namespace.
- **The runs namespace:** the `restricted` Pod Security Standard and the deny-all NetworkPolicy.
- **PodDisruptionBudgets**, and an optional Ingress (only `/v1/` and the gRPC services; health and metrics stay internal).

The values file documents every option: images, replicas, worker concurrency, run limits, storage (S3 or Azure with Workload Identity), and auth (OIDC). gRPC clients need HTTP/2 through your ingress: terminate TLS there and forward h2c.

## Terraform examples

- **[deploy/terraform/aks](../deploy/terraform/aks):** AKS with Azure CNI powered by Cilium and Workload Identity, a tainted, autoscaled node pool only for runs (with `pod_max_pid`), PostgreSQL Flexible Server on a private network, a storage account with shared keys disabled (nbsandbox uses its managed identity), and the Helm release.
  - `pod_sandboxing = true` runs each notebook in its own Kata VM (AKS Pod Sandboxing). Check the current AKS documentation for the workload runtime and RuntimeClass names and the supported VM sizes in your region: they have changed while the feature matured.
- **[deploy/terraform/existing-cluster](../deploy/terraform/existing-cluster):** any cluster you already run, on premises or elsewhere, with an existing PostgreSQL and S3-compatible store (RustFS, Ceph RGW, ...). Its network plugin must enforce NetworkPolicy; the probe will tell you if it doesn't.

Both are starting points, validated with OpenTofu (`tofu validate`) but not applied by our tests: review the sizes, regions and network ranges before use.

## Trying it locally

kind runs a Kubernetes cluster inside Docker. `make k8s-up` creates one, fetching kind and Helm through `go run`, with nothing to install. It then:
1. loads the locally built images;
2. starts PostgreSQL and RustFS;
3. installs the chart with [deploy/kubernetes/kind/values.yaml](../deploy/kubernetes/kind/values.yaml).

The API is on http://127.0.0.1:8080 (the demo users, as with `make up`).

| | |
|---|---|
| `make k8s-up` / `make k8s-down` | start the stack (stopping the other two, which use the same port) / delete the cluster. `K8S_CNI=calico` uses Calico instead of Cilium, in its own cluster. `K8S_RUNTIME_CLASS=gvisor` installs gVisor in the node (the release in `deploy/gvisor.version`, from gVisor's apt repository, checked against its SHA-256), adds a `gvisor` RuntimeClass and runs notebooks with it |
| `make test-k8s-cni-refuses` | on kind's own plugin (kindnet), which doesn't enforce NetworkPolicy: checks that the workers refuse to run notebooks |
| `make k8s-logs` | follow the API processes' and workers' logs |
| `make test-k8s-conformance` | the backend conformance suite (lifecycle, live output, cancel, OOM, orphans) against real Pods |
| `make test-k8s` | the integration suite through the Kubernetes stack |
| `make report-k8s` | the same, writing [isolation-report-kubernetes.md](isolation-report-kubernetes.md): every isolation, resource and failure check, observed on run Pods. With `K8S_RUNTIME_CLASS=gvisor` it adds gVisor's results beside the default runtime's |

On kind, runs get 1 GiB (run Pods reserve their whole limit, and the node has what Docker Desktop gives it). Starting a run takes longer than with Docker (8–30 s on a laptop, [perf.md](perf.md#starting-a-run)): the Pod is scheduled, and the files are copied in through the API. Cancelling takes up to about 10 s more for the same reason.
