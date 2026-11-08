# Security Review

**2026-11-08, before the first v0.x release.** What was checked, what was found, and what was done about each finding. Repeat it before each release (the commands are at the end). The [threat model](threat-model.md) describes what nbsandbox protects against; this page is the evidence that it was checked.

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
make runner-image && docker build -q -f deploy/nbsandboxd.Dockerfile -t nbsandboxd:dev .
docker save nbsandboxd:dev -o /tmp/nbsandboxd.tar && docker save nbsandbox-runner:dev -o /tmp/runner.tar
docker run --rm -v /tmp:/in:ro aquasec/trivy:0.58.1 image --input /in/runner.tar --severity HIGH,CRITICAL
docker run --rm -v "$PWD/runner-image":/w:ro python:3.13-slim sh -c 'pip install -q pip-audit && pip-audit -r /w/requirements.txt --disable-pip --no-deps'
docker run --rm -v "$PWD":/repo:ro zricethezav/gitleaks:v8.21.2 dir /repo --redact
go test ./internal/tarfs -run '^$' -fuzz '^FuzzReadTar$' -fuzztime 2m   # and each target above
```
