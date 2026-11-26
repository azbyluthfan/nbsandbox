# Security Review

**2026-11-08, before the first v0.x release.** What was checked, what was found, and what was done about each finding. Repeat it before each release (the commands are at the end). The [threat model](threat-model.md) describes what nbsandbox protects against; this page is the evidence that it was checked.

## The review for v0.3.0 (2026-11-21)

Interactive sessions are the new surface: a notebook's live kernel, reached over a WebSocket.

| Area | Result |
|---|---|
| Go | `govulncheck`: none reached by our code (one in a required module, not called). New direct dependency: `gorilla/websocket` (already in the module graph through client-go) |
| Runner's Python packages | `pip-audit`: none |
| Container images | Trivy, HIGH and CRITICAL with a fix: nbsandboxd 0, runner 0, builder 0 (the accepted CVE-2025-47273 still listed in `.trivyignore`). The runner and builder have 50 HIGH in Debian packages with no fix published yet; they are taken up by the weekly rebuild once fixed |
| Secrets in the source tree | `gitleaks`: four, all known test values: the two from before (a `YOUR_PASSWORD` placeholder, the demo users' published password), and two new fixed test secrets for sessions in a cluster (`internal/config/config_test.go`, `internal/sessions/remote_test.go`). The cluster Compose stack's `nbsandbox-dev-sessions-secret` is a development default, overridable, like its other dev values |
| The WebSocket channel | The owner check runs before the upgrade; the token needs `nbsandbox.runs`. The UI's CSRF header can't cover a WebSocket (a GET), so the channel checks `Origin` against the host: another site's page is refused (`TestSessionChannelRefusals`). Client messages: at most 1 MiB, one of six types, compacted to one line before they reach the bridge; on an API node they are checked before being relayed |
| What a session sends back | Everything the bridge sends comes from the user's own code: it can forge messages for its own clients (bridge states, outputs), as `display()` already lets it show anything. nbsandboxd treats it as untrusted: lines over `sessions.max_message` are dropped, it reads only the channel, type and execution state (for the idle rule: a session that claims to be busy still ends at `sessions.max`), and the browser shows HTML only in sandboxed frames with no network |
| **Found and fixed: a slow viewer's backlog** | A client that stopped reading could be 1,024 messages behind, each up to 2 MiB: one viewer of a session printing large outputs could make nbsandboxd hold about 2 GiB. Each client now has a 32 MiB budget too, and is disconnected past it (it reconnects) (`TestSlowClientIsDisconnectedByBytes`) |
| **Found and fixed: a session's disk on Docker** | A read-write session's working copy is on the worker's disk, and nothing checked its size while the session ran: a kernel could fill the host's disk. Like a writing run, it is now checked against the space's quota every tick, and the session ends over it, its changes not stored (`TestSessionOverQuotaEnds`). On Kubernetes the Pod's disk limit bounds it |
| The workers' internal listener (clusters) | Serves only with `sessions.secret` (constant-time compare; `TestInternalHandlerNeedsTheSecret`); without a secret, sessions stay off on API and worker processes. It trusts the API node for ownership, so it belongs on the internal network: plain HTTP, port 8091. On Kubernetes, add a NetworkPolicy allowing only API Pods to reach it if other workloads share the cluster |
| Attach | Docker: the socket proxy now allows `attach` for any container, as it allowed stop and kill: a compromised nbsandboxd could talk to another container's stdin (it is out of scope, as before). Kubernetes: `pods/attach` only in the runs namespace |
| Session isolation | A session is a run's container or Pod, with the same settings pinned by the same tests, and passes the report's new checks on Docker and on kind: `session-network`, `session-isolation`, `session-memory-limit`, `session-read-only`, `session-reattach` |

**Before the tag (2026-11-22).** Changed since the review: MCP tools for sessions (`nbsandbox-mcp`: the same session API, as the agent's user, with the same scopes), and two fixes in the session bridge (a shell channel that could go silent; an interrupt sent as a cell started was lost). No new dependency: `go.mod`, the runner's `requirements.txt` and its Dockerfile are as reviewed. Repeated: `govulncheck` none reached (the same one in a required module, not called); `pip-audit` none; `gitleaks` the same four test values. Trivy on nbsandboxd and the runner: no HIGH or CRITICAL with a fix, in the weekly run on `13b5440` (2026-11-22), the code this release ships; the builder's inputs are unchanged since the review.

## v0.2.1 (2026-11-16)

A release workflow fix only (the runner image's manifest label); the code, the dependencies and the pinned base images are those of 0.2.0, so the v0.2.0 review below applies.

## The review for v0.2.0 (2026-11-15)

| Area | Result |
|---|---|
| Go | `govulncheck`: none reached by our code (one in a required module, not called) |
| Runner's Python packages | `pip-audit`: none |
| Container images | Trivy, HIGH and CRITICAL with a fix: nbsandboxd 0, runner 0. The new **builder image** (`nbsandbox-runner-builder`, package set builds) keeps pip, so it had pip's bundled copies of urllib3 2.7.0 and msgpack 1.1.2: **fixed** by pinning pip 26.3 (with hashes), which bundles urllib3 2.8.0 and msgpack 1.2.4. One remains, **accepted**: CVE-2025-47273, a path traversal in setuptools' package downloader, reported for the `pkg_resources` that pip still bundles from setuptools 70.3.0; pip bundles only `pkg_resources`, not the downloader, and builds install wheels from the configured index only. Listed, with the reason, in `.trivyignore` |
| Secrets in the source tree | `gitleaks`: the same two as before, a `YOUR_PASSWORD` placeholder and the demo users' published dev password in the tour. The Keycloak test realm's new client secrets (`nbsandbox-introspection-test-secret`, `nbsandbox-ui-test-secret`) are public test values, like its others |
| New surfaces | token introspection (fails closed), the failed sign-in limit, the web UI sign-in (PKCE, nonce, browser-bound state, server-side sessions, CSRF), the audit log, admin operations, and package set builds (the only containers with network): each in the [threat model](threat-model.md), with its tests |

## Since v0.1.0: the review for v0.1.2 (2026-11-15)

| Area | Result |
|---|---|
| Go | Go 1.27.3's `net/http` server could crash on a header race (GO-2026-6859), reachable from the API: **fixed** in 0.1.1 by building with Go 1.27.4. `govulncheck` now: none reached by our code |
| Runner's Python packages | tornado ≤ 6.6.0's incomplete open-redirect fix (`RedirectHandler`), not reachable (the runner serves no HTTP and runs have no network): **fixed** with tornado 6.6.1. `pip-audit` now: none |
| Container images | Trivy in the weekly CI run on both images built from this code: no fixable HIGH or CRITICAL |
| Secrets in the source tree | `gitleaks`: the same `YOUR_PASSWORD` placeholder, and the demo users' published dev passwords in the tour; the Keycloak test realm's fixed test secrets (`deploy/keycloak/realm.json`) are marked as such and used only by `make test-keycloak` |
| New surfaces | the MCP server, the deployment check and the Keycloak test setup: see the [threat model](threat-model.md#network--service) |

## Summary

| Area | Result |
|---|---|
| Known vulnerabilities in Go dependencies | 3 reached by our code (all denial of service), **fixed** by upgrading; 1 more in a package we don't import, no tagged fix yet |
| Container images | nbsandboxd: none. Runner: 5 in Python packages bundled with pip and 2 in zlib, **fixed**; 47 in Debian base packages with no fix published, **accepted** (see below) |
| Python packages notebooks use | none (`pip-audit`) |
| Secrets in the source tree | none; one false positive (a `YOUR_PASSWORD` placeholder); the demo users' published passwords now cause a startup warning |
| Memory safety ("buffer overflows") | no `unsafe` or cgo in our code; every read of outside input is size-capped; one size check that could overflow, **fixed** |
| Input handling | paths, uploads, tar streams, SQL, shell commands, HTML, streams, URLs and tokens reviewed: no exploitable issue found; hardening applied |
| Fuzzing | 13 targets over every parser of outside input, 90 seconds each (31 million inputs): 2 bugs found, **fixed** (one earlier) |

## Dependencies and images

**Go** (`govulncheck`):

| Advisory | Module | Effect | Done |
|---|---|---|---|
| GO-2026-6865 | klauspost/compress 1.20.1 | panic on crafted compressed data (denial of service) | upgraded to 1.20.2 |
| GO-2026-5764 | AWS SDK event-stream decoder and S3 client | panic in the decoder (denial of service) | upgraded (event stream 1.7.8, S3 1.97.3) |
| GO-2026-4958 | moby/spdystream 0.5.0 (Kubernetes exec) | unbounded memory parsing SPDY frames | upgraded to 0.5.1 |
| GO-2026-6866, GO-2026-6860 | golang.org/x/net 0.61.0 | not reached by our code | upgraded to 0.62.0 anyway |
| GO-2026-5932 | golang.org/x/crypto 0.58.0 | in a package we don't import; the fix exists only as an untagged commit | **accepted**; take the next tagged release |

**Images** (Trivy, HIGH and CRITICAL):

| Image | Finding | Done |
|---|---|---|
| nbsandboxd (distroless) | the Go modules above | fixed with the upgrades; now 0 |
| runner | urllib3, msgpack and setuptools bundled inside pip | **pip removed** from the runner image: runs have no network, so it had no use there |
| runner | zlib (2 advisories) | Debian's security updates applied at build |
| runner | 47 advisories in Debian base packages (util-linux, ncurses, login, perl-base, sqlite, systemd libraries, acl) with **no fix published** | **accepted**. Runs are unprivileged (uid 1000, no capabilities, `no-new-privileges`) on a read-only root, the image has no setuid binaries, and nothing in a run uses mount helpers or logins. Rebuilt when Debian publishes fixes |

## Memory safety

In the classic sense, a buffer overflow needs memory-unsafe code. nbsandbox's own code is Go and Python, which check every access: an out-of-bounds index is a crash, not memory corruption.

- **No escape hatches:** no `unsafe` and no cgo in our Go code (outside generated protobuf code); release builds use `CGO_ENABLED=0`.
- **Every read of outside input is capped:** request bodies (`http.MaxBytesReader` on every route), uploads (`uploads.max_file`, `uploads.max_space`), parameters (1 MiB, 32 levels), outputs, logs (the log cap), tar streams from Pods (twice the space quota), run events (64 KiB a line, 1,000 items), OIDC discovery and key documents (1 MiB), tokens (16 KiB), the completion index (4 MiB).
- **Integer overflow:** sizes are compared before they are added. One place compared after adding: a tar entry from a run Pod with a forged size near the int64 maximum could wrap the running total past the check (the copy then failed at the end of the stream). **Fixed**, and covered by `FuzzReadTar`.
- **Panics from outside input:** the HTTP server recovers from a panic in a handler (500, logged), and the fuzz targets below run every parser on arbitrary input.
- **Native code that does run:** numpy, pyzmq, Jupyter and gVisor are written in memory-unsafe languages, and a notebook can call them however it likes. That is exactly why each run is isolated; their known vulnerabilities are covered by the image scans.

## Input handling

| Risk | Where | Finding |
|---|---|---|
| Path traversal | file and folder names, renames, artifacts, run folders | lexical checks, then every file operation through `os.Root` (can't leave the folder, even through links); `FuzzValidatePath` |
| Path traversal and special files in archives | tar streams copied out of run Pods | `os.Root`, `fs.ValidPath`, regular files and folders only (links and devices skipped), size budget; `FuzzReadTar` |
| Command injection | `pods/exec` commands, Docker API calls | commands are fixed strings with values passed as separate arguments (`sh -c '…' arg1 arg2`); Docker calls are structured API requests; nothing is built from user text |
| SQL injection | PostgreSQL | every query is parameterised; none is built from strings |
| Cross-site scripting | web UI, rendered notebooks, downloads | the UI never inserts HTML (text only) and has a strict Content-Security-Policy (`script-src 'self'`, no inline scripts); a run's `output.html` is served with `sandbox; default-src 'none'`, so its scripts can't run; downloads are attachments with `nosniff` |
| Stream injection | Server-Sent Events | log lines are split at newlines and carriage returns are removed, so a line can't start a forged event; events are JSON |
| Server-side request forgery | anything nbsandboxd fetches | only configured URLs: the OIDC issuer (HTTPS required, the discovery document's issuer must match) and storage endpoints. Contract schemas in notebooks can't load anything (no loader) |
| Token forgery | OIDC bearer tokens | asymmetric algorithms only (no `none`, no HMAC), one signature, the key only from the issuer's key set (an embedded key is ignored), issuer, audience and expiry checked, key refresh rate-limited; `FuzzVerify` |
| Credentials | basic auth | bcrypt, timing-safe for unknown users; the users file refused if others can read it; **new:** a startup warning when the repository's demo users are loaded |
| Costly input | regular expressions, JSON, YAML, schemas | Go's regular expressions run in linear time; JSON depth limits; YAML only from the operator; schemas capped at 256 KiB |
| Leaks in published evidence | isolation reports | **new:** the report generator replaces the home folder with `~`, so host paths in findings don't name whoever ran the tests |

## Fuzzing

Go's built-in fuzzer; each target runs in CI on its seed inputs, and for a longer time before a release. For this review each ran 90 seconds on 8 cores (inputs tried in brackets).

| Target | What it feeds | Result |
|---|---|---|
| `FuzzValidatePath` | file paths | no issue (471,105) |
| `FuzzNormalizeParameters` | run parameters | found (earlier): trailing data after the JSON object accepted; fixed. Now no issue (960,270) |
| `FuzzScanners` | the dependency scanner | no issue (1,216,426) |
| `FuzzLineWriter` | Docker's log stream | no issue (784,560) |
| `FuzzVerify` | OIDC tokens | no issue (1,066,296) |
| `FuzzReadTar` | tar streams from run Pods | no issue (271,727, without coverage guidance: the file work per input made the guided mode too slow to be useful); also 20,000 random mutations of a valid stream |
| `FuzzParseRunEvent` | lines of the run events file | no issue (1,678,483) |
| `FuzzFromNotebook` | notebook contracts and schemas | no issue (5,428,935) |
| `FuzzValidateNotebook` | uploaded notebooks | no issue (1,183,452) |
| `FuzzParseBasic` | basic credentials | no issue (2,831,856) |
| `FuzzResumePoint` | `Last-Event-ID` and `after=` | no issue (479,846) |
| `FuzzValidKey` | object-storage keys | no issue (11,207,143) |
| `FuzzParseSize` | sizes in settings | found: `NaN GiB`, `Inf MiB` and `1e30GiB` parsed to meaningless sizes; fixed. Now no issue (2,744,699) |

## Repeat it

```bash
go run golang.org/x/vuln/cmd/govulncheck@latest ./...
make runner-image fetcher-image && docker build -q -f deploy/nbsandboxd.Dockerfile -t nbsandboxd:dev .
docker save nbsandboxd:dev -o /tmp/nbsandboxd.tar && docker save nbsandbox-runner:dev -o /tmp/runner.tar && docker save nbsandbox-runner-builder:dev -o /tmp/builder.tar && docker save nbsandbox-fetcher:dev -o /tmp/fetcher.tar
docker run --rm -v /tmp:/in:ro -v "$PWD/.trivyignore:/.trivyignore:ro" aquasec/trivy:0.58.1 image --input /in/runner.tar --severity HIGH,CRITICAL --ignorefile /.trivyignore   # and nbsandboxd.tar, builder.tar, fetcher.tar
for d in runner-image fetcher-image; do docker run --rm -v "$PWD/$d":/w:ro python:3.13-slim sh -c 'pip install -q pip-audit && pip-audit -r /w/requirements.txt --disable-pip --no-deps'; done
docker run --rm -v "$PWD":/repo:ro zricethezav/gitleaks:v8.21.2 dir /repo --redact
go test ./internal/tarfs -run '^$' -fuzz '^FuzzReadTar$' -fuzztime 2m   # and each target above
```
