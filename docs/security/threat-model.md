# Threat Model

Status: **reviewed for v0.2.0** (2026-11-15: sign-in, revocation, the web UI login, the audit log and package sets added; interactive sessions added 2026-11-21, for v0.3.0; first reviewed for v0.1.0 on 2026-11-08; see the [security review](review.md)). Every mitigation below names the automated check behind it. The check names match the rows of the isolation reports ([Docker](../isolation-report.md), [Linux with runc and gVisor](../isolation-report-linux.md), [Kubernetes](../isolation-report-kubernetes.md)), or are Go test and fuzz target names. Revisit this when the trust model changes: less trusted users, shared spaces, or network policy.

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
| Notebook or file content runs as script in the web UI | Content inserted as text only (the editor and the highlighter too); the rendered HTML opens in its own tab under a `sandbox` CSP; strict page CSP (`script-src 'self'`, `frame-src data:` only); styles only from the page or carrying that load's random nonce (for the editor's own styles), never `'unsafe-inline'`; the vendored editor bundle is checksummed and contains no `eval`, `new Function` or HTML injection. A session's HTML outputs (pandas tables) go in a `data:` frame with an empty `sandbox` (no scripts, a unique origin: it can't reach the page, its cookies or the API) and a policy of its own allowing no network (`default-src 'none'`), on top of the page's, which it inherits; SVG outputs are shown as images (their scripts don't run) | UI static tests (`TestNoExternalOrInlineCode`, `TestVendoredBundle`, nonce per load); checked in the browser |
| An interactive session's kernel does more than a run could | A session is a run's container (or Pod) for longer, with the same settings, pinned by the same tests: no network, uid 1000, no capabilities, a read-only system, the run limits, the runtime. Its main process (the session bridge) passes only some Jupyter messages both ways: no `input()`, comms (widgets) or history; restart only on nbsandboxd's own request. It holds no credentials. It ends when idle (no cell running, nobody connected) or past `sessions.max`; users hold at most `sessions.per_user` | `session-network`, `session-isolation`, `session-memory-limit`, `session-read-only`; session container and Pod spec tests; bridge tests (refused messages) |
| A session writes over the space while someone else does | A read-write session holds the space's write lock with writing runs (checked under the space's row lock in the database, both ways); its file changes are written back checked against the version it started from, edits made meanwhile kept | store tests (`TestOpenSessionShareTheWriteLock`), `TestSessionEndToEnd` |
| Another site opens a signed-in user's session WebSocket (cross-site WebSocket hijacking) | The channel checks the `Origin`: only this host's pages (a browser always sends it); programs send none. The UI's CSRF header can't cover a WebSocket's GET, so this check does | `TestSessionChannelRefusals` |

### User → another user's data

| Threat | Mitigation | Checked by |
|---|---|---|
| User reaches another user's space through the API | Ownership check on every route; not-yours = 404 | `TestAuthorizationMatrix` (generated from the route table), `cross-user-api` |
| User escapes the space with file paths | Lexical checks plus `os.Root`; fuzzing | path traversal and symlink tests, `FuzzValidatePath` |
| Malformed input breaks parsing (request parameters, notebooks, output, run events, tar streams from Pods, credentials, sizes) | Strict parsing; size caps on every read; fuzzing (it found trailing data after the parameters object being accepted, and sizes such as `NaN GiB` parsing; both fixed) | `FuzzNormalizeParameters`, `FuzzScanners`, `FuzzLineWriter`, `FuzzReadTar`, `FuzzParseRunEvent`, `FuzzFromNotebook`, `FuzzValidateNotebook`, `FuzzParseBasic`, `FuzzResumePoint`, `FuzzValidKey`, `FuzzParseSize`, `FuzzVerify` |
| Costly input: huge or deeply nested parameters, outputs or contract schemas | Parameters ≤ `runs.max_parameters` (1 MiB) and 32 levels; outputs ≤ `runs.max_output` (1 MiB); schemas ≤ 256 KiB; schemas may not load anything (no remote or file `$ref`); request bodies capped | contract and parameter unit tests |
| A user's file edit silently overwrites another's | Saves need the version being replaced (`If-Match`); `412` otherwise | file-editing API tests |
| Someone reaches the web UI without sign-in (`ui.auth: none`) and acts as any user | Off by default; refused beyond loopback (the dev Compose stack opts in, its port published on the host's loopback only); deployments use `oidc` or `basic` sign-in instead | config tests, UI handler tests, `web-ui` |
| A space's `requirements.txt` (custom packages) attacks the build, other spaces or the host | Off by default. Requirement lines only (no index options, URLs or paths: pip can't be pointed at internal addresses); wheels only, so no package code runs while building; the build container gets no space files and no credentials, a run's hardening and limits, and is the only container with network (`packages.network`, e.g. an internal network with a mirror); the runner's packages are constraints and come first on `sys.path`; sets are read-only in runs. Supply chain (a malicious package on the index) is the operator's choice: `packages.allowed` (checked against everything resolved) or a mirror | `internal/packages` tests, Docker build container spec tests |
| A user denies an action, or someone's probing or misuse goes unnoticed (repudiation) | The audit log records every change, download and administrative operation with the caller ID, the token's application, the client's address and the outcome, refused attempts included; written by the transport (not by each handler); readable by administrators only, and reading it is recorded too; also in the service log for an external store. Rows that can't be written are counted and logged (the request is not failed: availability first) | `internal/audit`, API and gRPC audit tests |
| A signed-in UI session is stolen or misused | Tokens never reach the browser (server-side, AES-GCM encrypted, key from the client secret); the cookie is a random ID (only its hash stored), `__Host-`, HttpOnly, SameSite=Lax; sessions end after 1 h idle or 12 h; the access token is checked like an API token on every request and refreshed at the provider, so revocation there ends the session | `internal/uiauth` tests, `make test-keycloak` |
| Another site makes a signed-in browser change something (CSRF), or signs a victim in to the attacker's account | Changes need the UI's header and origin (a cross-site page can send neither without CORS, which the UI never allows); the main API ignores the cookie; the sign-in's state is bound to the browser by a cookie, used once, with PKCE and a nonce; return paths are local only | `internal/uiauth` tests |

### Network → service

| Threat | Mitigation | Checked by |
|---|---|---|
| Credential brute force or leaking | bcrypt; timing-safe check for unknown users; TLS through Caddy; users-file permission check; logs redacted; a startup warning when the repository's demo users (public passwords) are loaded; OIDC tokens checked for issuer, audience, expiry and an asymmetric algorithm, keys only from the issuer's key set. Failed basic-auth sign-ins limited per username (5, then one a minute, password not checked while limited) and per client address when the proxies in front are named (`trusted_proxies`; `X-Forwarded-For` believed only from them, so a client can't pick its own address) | auth tests, log redaction tests, users-file permission tests, login limiter tests |
| A revoked token (user signed out or disabled) keeps working until it expires | Short-lived access tokens; optional introspection (RFC 7662) at the provider, failing closed, cached 30 s, only for tokens that passed the local checks. Entra ID has no introspection: tokens there live until they expire | introspection tests, `make test-keycloak` (revoked token refused) |
| A calling service's credentials do more than invoke | With OIDC, give the calling service a token with only `nbsandbox.functions.invoke nbsandbox.read`: it can call functions and read their results, nothing else. Basic-auth users (development) hold every scope but admin (**known limitation** of basic auth) | `TestScopeMatrix` (generated from the route table: each route refused without its scope); [auth.md](../auth.md) |
| An AI agent (through `nbsandbox-mcp`) does more than its user meant | The MCP server is only a client of the API: every tool call is checked by nbsandboxd with the agent's own credentials, so an agent can do exactly what its token's scopes allow, in its user's own spaces. Its runs are ordinary runs (no network, the run's limits). Give agents a token with only the scopes they need, and a space of their own | `internal/mcpserver` tests: credentials passed through, errors from nbsandbox shown, tools marked read-only or changing |
| A shared MCP server (`nbsandbox-mcp -http`) holds or leaks credentials | It has none of its own: each request's `Authorization` header goes to nbsandbox unchanged, and requests without one get `401`; it refuses a server-wide token over HTTP; plain `http` to nbsandbox only on loopback, private or in-cluster addresses | `cmd/nbsandbox-mcp` tests (`requireAuth`, settings refused) |
| The deployment check (`nbsandbox-report check`) exposes its users' credentials, or leaves data behind | Credentials only from the environment (never flags, so not in the process list); plain `http` only to localhost; its spaces deleted at the end, also on Ctrl-C; nothing restarted or stopped | `cmd/nbsandbox-report` and `internal/check` tests |
| Test credentials end up in a deployment | The Keycloak test realm's passwords and secrets are fixed public values in `deploy/keycloak/realm.json`, used only by `make test-keycloak` with Keycloak in development mode; the demo users in `config/users.yaml` cause a startup warning | review |

### nbsandboxd → Docker

| Threat | Mitigation | Checked by |
|---|---|---|
| nbsandboxd makes Docker calls it shouldn't | The proxy allows only the needed endpoints, and **bind mounts from the spaces folder only**. It can't restrict other create options such as privileged mode (**known limitation**). Attach (for interactive sessions) is allowed for any container, as stop and kill already were: a compromised nbsandboxd could talk to another container's standard input | proxy probe during development; compose lint |
| An API node or anything on the internal network drives a worker's sessions | A worker's internal session listener (cluster mode only) refuses requests without `sessions.secret` (compared in constant time); it is meant for the internal network only. On Kubernetes the workers' Role allows `pods/attach` in the runs namespace only | `TestInternalHandlerNeedsTheSecret` |

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
