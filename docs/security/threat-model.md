# Threat Model

Status: **reviewed for the first v0.x release** (2026-11-08; see the [security review](review.md)). Every mitigation below names the automated check behind it. The check names match the rows of the isolation reports ([Docker](../isolation-report.md), [Linux with runc and gVisor](../isolation-report-linux.md), [Kubernetes](../isolation-report-kubernetes.md)), or are Go test and fuzz target names. Revisit this when the trust model changes: less trusted users, shared spaces, or network policy.

## What we protect

| Asset | Where it lives |
|---|---|
| A space's files: notebooks, data, outputs, artifacts | object storage (contents), PostgreSQL (the index); a run's working copy on the worker while it runs |
| Secrets in a running notebook's memory | the run container's processes |
| Fair share of CPU, memory, PIDs and disk for each run | host kernel cgroups, host disk |
| The host and the Docker daemon | outside every container |
| User credentials and service configuration | `users.yaml`, `settings.yaml`, nbsandboxd's environment |

## Who might cause harm

| Actor | Trust | Typical harm |
|---|---|---|
| A trusted user, by mistake | trusted | notebook reads the wrong files, runs away with memory or CPU, prints forever |
| Notebook code (the user's own, or code the user copied from elsewhere) | **untrusted at run time** | tries to read other spaces, escape the container, reach the network or Docker, exhaust resources |
| Another authenticated user | trusted, but must not see other users' spaces | guesses space or run IDs through the API |
| Someone on the network without credentials | untrusted | brute-forces credentials, abuses the API |
| A compromised nbsandboxd | out of scope for v1 | can create any container through the proxy |

## Trust boundaries

```
client ──HTTPS──▶ Caddy ──▶ nbsandboxd ──▶ socket proxy ──▶ Docker daemon ──▶ run container
      network → service   user → other users' data   nbsandboxd → Docker     notebook → everything else
```
- **Network → service:** TLS and basic auth.
- **User → another user's data:** ownership checks in nbsandboxd.
- **nbsandboxd → Docker:** the proxy's allowlist of Docker API calls.
- **Notebook → everything else:** the run container's settings (no network, read-only image, no capabilities, uid 1000, limits).

## Threats and mitigations

### Notebook → everything else

| Threat | Mitigation | Checked by |
|---|---|---|
| Notebook reads another space's files | Only its own space is mounted; others don't exist in the container | `space-paths`, `canary-search` (whole container searched) |
| Notebook changes its space's earlier runs, their results, or published function versions | Run outputs and published versions live outside the space's files: a run mounts only the files (`/space`) and its own folder (`/run`); nbsandboxd reads a run's folder through a root opened at that folder, so links planted there lead nowhere | `space-paths` (probes the space's own `runs/` and `functions/` and lists `/run`), spaces unit tests |
| One function call changes what the next one sees | Calls mount the published bundle read-only; a call can write only its own `/run` and `/tmp` | `read-only-invocation`, `parallel-invocations` |
| Notebook reads another run's processes or memory | Private PID namespace; no `ptrace` capability; separate containers | `proc-list`, `memory-secret` (every readable `/proc/*/mem` searched while bob's holder runs) |
| Notebook reads another run's shared memory | Private `/dev/shm` per container | `shared-memory` |
| Notebook controls Docker | No socket mounted; network `none`, so the proxy is unreachable | `socket-env-network` (socket absent; connections to the proxy, nbsandboxd, the host and the internet all fail) |
| Notebook sees service secrets | Environment allowlist | `socket-env-network` (no forbidden variables) |
| Notebook gains privileges | uid 1000; all capabilities dropped (`CapEff` 0); `no-new-privileges`; read-only root file system; `/tmp` `nosuid,noexec` (Docker's default for tmpfs, kept); no setuid binaries in the image | `privileges`, runner image tests |
| Notebook escapes through a kernel bug | Docker's default seccomp profile (`Seccomp: 2` observed); AppArmor where the host has it (not on Docker Desktop); for less trusted users, gVisor (`runsc`): system calls go to gVisor's own kernel, and only a filtered few reach the host's ([gvisor.md](../gvisor.md)); on Kubernetes, a sandbox RuntimeClass (gVisor, Kata) | container-settings unit tests, `privileges`; the whole suite under `runsc` ([Linux report](../isolation-report-linux.md)) and under a gVisor RuntimeClass ([Kubernetes report](../isolation-report-kubernetes.md)) |
| Notebook exhausts memory, CPU, PIDs or `/tmp` | cgroup limits per container; `/tmp` tmpfs size (counts toward memory); under gVisor the process limit is `RLIMIT_NPROC` inside the sandbox, with the host's pids limit as a backstop (the host limit counts gVisor's own threads) | `memory-limit`, `cpu-fairness`, `process-limit`, `time-limit`, `tmp-limit`, each with another user's run checked unaffected |
| Notebook fills the host disk or nbsandboxd's memory with output | Log cap in nbsandboxd; capped Docker log driver; **stored notebook output capped at 5 MiB**; in the kernel, no output history (IPython keeps a copy of everything a cell prints) and no stdout/stderr sent past twice the log cap (without these, a notebook printing nonstop ran out of memory) | `endless-output` |
| Notebook fills the host disk with files in its space or its run folder | Quota checked at upload, publish and submit, and every few seconds while a run writes; a run over quota is stopped (`resource_disk`). Between checks a run can overshoot (**soft**, known limitation) | `TestDiskQuotaStopsRun`; filesystem quotas are on the roadmap |
| Notebook reaches the network | Docker: network mode `none` (loopback only). Kubernetes: a deny-all NetworkPolicy on the runs namespace, and a probe Pod at start and periodically: if it reaches anything, workers refuse to run notebooks | `socket-env-network` (the internet, the metadata service, nbsandboxd, the proxy; on Kubernetes also the API server, a worker and the kubelet), `cni-refuses` |
| User forges their run's status | The status file lives in the user's own space, so they can only mislead *their own* result (**known limitation**) | documented |
| Notebook or file content runs as script in the web UI | Content inserted as text only (the editor and the highlighter too); the rendered HTML opens in its own tab under a `sandbox` CSP; strict page CSP (`script-src 'self'`, `frame-src 'none'`); styles only from the page or carrying that load's random nonce (for the editor's own styles), never `'unsafe-inline'`; the vendored editor bundle is checksummed and contains no `eval`, `new Function` or HTML injection | UI static tests (`TestNoExternalOrInlineCode`, `TestVendoredBundle`, nonce per load) |

### User → another user's data

| Threat | Mitigation | Checked by |
|---|---|---|
| User reaches another user's space through the API | Ownership check on every route; not-yours = 404 | `TestAuthorizationMatrix` (generated from the route table), `cross-user-api` |
| User escapes the space with file paths | Lexical checks plus `os.Root`; fuzzing | path traversal and symlink tests, `FuzzValidatePath` |
| Malformed input breaks parsing (request parameters, notebooks, output, run events, tar streams from Pods, credentials, sizes) | Strict parsing; size caps on every read; fuzzing (it found trailing data after the parameters object being accepted, and sizes such as `NaN GiB` parsing; both fixed) | `FuzzNormalizeParameters`, `FuzzScanners`, `FuzzLineWriter`, `FuzzReadTar`, `FuzzParseRunEvent`, `FuzzFromNotebook`, `FuzzValidateNotebook`, `FuzzParseBasic`, `FuzzResumePoint`, `FuzzValidKey`, `FuzzParseSize`, `FuzzVerify` |
| Costly input: huge or deeply nested parameters, outputs or contract schemas | Parameters ≤ `runs.max_parameters` (1 MiB) and 32 levels; outputs ≤ `runs.max_output` (1 MiB); schemas ≤ 256 KiB; schemas may not load anything (no remote or file `$ref`); request bodies capped | contract and parameter unit tests |
| A user's file edit silently overwrites another's | Saves need the version being replaced (`If-Match`); `412` otherwise | file-editing API tests |
| Someone reaches the optional web UI and acts as any user (it has no login) | Off by default; its own listener on `127.0.0.1` by default, with a startup warning otherwise; host-only publish in Compose; never proxied by Caddy; the API's rules (ownership, limits) still apply to whoever is chosen. Since the editor, the UI can **change and delete** any user's files, not only run notebooks (**known limitation**: local tool only) | UI handler tests, `web-ui` |

### Network → service

| Threat | Mitigation | Checked by |
|---|---|---|
| Credential brute force or leaking | bcrypt; timing-safe check for unknown users; TLS through Caddy; users-file permission check; logs redacted; a startup warning when the repository's demo users (public passwords) are loaded; OIDC tokens checked for issuer, audience, expiry and an asymmetric algorithm, keys only from the issuer's key set. **No rate limiting in v0.x** (known limitation: trusted users, small deployments; to come with real authentication) | auth tests, log redaction tests, users-file permission tests |
| A calling service's credentials do more than invoke | With OIDC, give the calling service a token with only `nbsandbox.functions.invoke nbsandbox.read`: it can call functions and read their results, nothing else. Basic-auth users (development) hold every scope but admin (**known limitation** of basic auth) | `TestScopeMatrix` (generated from the route table: each route refused without its scope); [auth.md](../auth.md) |

### nbsandboxd → Docker

| Threat | Mitigation | Checked by |
|---|---|---|
| nbsandboxd makes Docker calls it shouldn't | The proxy allows only the needed endpoints, and **bind mounts from the spaces folder only**. It can't restrict other create options such as privileged mode (**known limitation**) | proxy probe during development; compose lint |

### Everywhere

| Threat | Mitigation | Checked by |
|---|---|---|
| Vulnerable dependency in the image or service | Pinned with hashes; scans; Renovate; monthly image rebuild; pip removed from the runner image (runs have no network); Debian security updates applied at build | `govulncheck` in CI; Trivy and `pip-audit` before each release ([review](review.md)) |
| A leak in nbsandboxd degrades the service over time | Soak test watching goroutines, open files, memory and leftover containers | `TestSoak` (weekly 45 min; 3 h before releases) |

## Platform notes

- **Docker Desktop** is not a reference isolation platform. Ownership and permission behaviour differs from Linux, and reports from Desktop are labelled as such.
- **gVisor** handles system calls in user space. It reduces exposure to kernel bugs; it passes every isolation check, and costs about a second more per run and slower small-file work ([gvisor.md](../gvisor.md)).
- **Kubernetes:** isolation depends on the cluster's network plugin enforcing NetworkPolicy; nbsandbox checks it with a probe and refuses to run notebooks otherwise. Run the suite in each target cluster for its own report.

## Decisions on the draft's open items

- **Rate limiting of failed logins:** not in v0.x. It is a known limitation, to be revisited with real authentication.
- **`/tmp` `noexec`:** kept. Docker applies it by default to the tmpfs, and the runner image doesn't need exec in `/tmp`.

## Not covered by the tests (yet)

- **Kernel exploits.** No test attempts a kernel escape; protection relies on seccomp and, optionally, gVisor.
- **A compromised nbsandboxd** (out of scope; see the proxy limitation above).
- **Egress allowlists.** Runs have no network, so there are no tests of partial network access.
