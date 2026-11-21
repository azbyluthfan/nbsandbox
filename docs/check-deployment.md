# Check your deployment

`nbsandbox-report check` runs the isolation, resource and failure checks against **your** running deployment, through its public API, and writes the isolation report for it. The reports in this repository show the controls work on the reference platforms; this one shows they work where you run nbsandbox: your cluster's network plugin, RuntimeClass, node settings and limits.

It works the same for a Docker host and any Kubernetes cluster.

## What it needs

- **Two ordinary user accounts**, different users (the checks probe from one and put secrets in the other's space). Basic-auth users or OIDC tokens with the `nbsandbox.read`, `nbsandbox.write` and `nbsandbox.runs` scopes.
- **The deployment's URL.** Over `https` (plain `http` only to localhost).
- **Optional, Kubernetes:** a kubeconfig that can read services, pods, nodes and network policies. It adds the API server, a worker Pod and the node's kubelet to what a run must not reach, checks that the run's Pod is gone after cancel and timeout, and records the cluster's version and its NetworkPolicies.

The binary is in each release's archive (`nbsandbox_<version>_<os>_<arch>.tar.gz`), or build it from a checkout: `go build ./cmd/nbsandbox-report`.

## Run it

```bash
export NBSANDBOX_CHECK_A_PASSWORD='…'   # or NBSANDBOX_CHECK_A_TOKEN
export NBSANDBOX_CHECK_B_PASSWORD='…'   # or NBSANDBOX_CHECK_B_TOKEN
nbsandbox-report check -url https://nbsandbox.example.com -user-a check-a -user-b check-b \
  -kubeconfig ~/.kube/config -out isolation-report.md
```

It prints each finding as it goes and exits 0 only if every check ran and passed. A full check takes about 10 minutes plus the deployment's run timeout (`-skip-slow` leaves out the time-limit check, which waits for it). `-only isolation` runs one check; `-help` lists them.

Useful flags:
- `-target host:port` (repeatable): more addresses a run must not reach, for example a database or an internal service. Without a kubeconfig, add the Kubernetes API server here.
- `-ca-file ca.pem`: trust a private CA for the deployment's TLS.
- `-findings dir`: keep the raw findings, in the layout `nbsandbox-report -in` renders again.

## What it does to the deployment

- It creates its own spaces, named `nbsandbox-check-…`, under the two users, and deletes them at the end, also on Ctrl-C.
- It opens two interactive sessions, one after the other, read-only (they hold no write lock), and ends them; one fills its memory to the limit. A deployment with sessions turned off gets findings saying so.
- It runs up to two notebooks at a time (a "noisy" one and a neighbour), so it takes up to two run slots for about 10 minutes. One of them spins its CPU for 50 seconds, another fills its own `/tmp`; both stay within their run's limits.
- It never restarts or stops anything. The checks that kill services (worker crashes, PostgreSQL blips, nbsandboxd restarts) and the queue check that needs every run slot busy stay in the integration suite; the report lists them under "Not checked here".

## Reading the report

The report has the same sections as [isolation-report.md](isolation-report.md), with this deployment's observations:
- **Isolation:** the probe tries to read the other user's space, canary file, processes, memory and shared memory; looks for the Docker socket, secrets in its environment and any network route; and checks it is an unprivileged user with no capabilities. Then the second user tries to read the probe's run through the API.
- **Resources:** memory, processes, `/tmp`, output size and the time limit stop the noisy run and leave the neighbour unaffected. CPU fairness is recorded, not judged: shared nodes vary too much.
- **Cancellation and errors:** cancels, a caller that disconnects, a crashing kernel and a failing cell end with the right status.
- **Interactive sessions:** a session's kernel reaches nothing (the internet, the metadata address, nbsandbox itself, your `-target`s, DNS), is an unprivileged user seeing only its own space, can't write when read-only, and dies at the memory limit without taking nbsandbox with it. The other user gets `404` for it.

A failure in **Isolation** means a run could reach something it must not: don't run untrusted notebooks on that deployment until it's fixed. On Kubernetes, the usual cause is a network plugin that doesn't enforce the deny-all NetworkPolicy ([kubernetes.md](kubernetes.md#which-clusters-any-network-plugin-that-enforces-networkpolicy)); the worker's own probe would normally have refused runs already.
