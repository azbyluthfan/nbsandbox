# nbsandbox Isolation Report: Linux with Docker Engine

Generated 2026-11-26 15:28 UTC by `make report in the weekly CI run (GitHub's Linux runner, Docker Engine, 2 GiB per run)` from the integration tests' findings. Each row is a test that **ran**; the observed column is what the test saw, not what it assumed.

## Summary

| Runtime | Platform | Passed | Failed | Required tests missing |
|---|---|---|---|---|
| runc | Linux with Docker Engine — reference platform | 56 | 0 | none |
| runsc | Linux with Docker Engine — reference platform | 56 | 0 | none |

## Environment

**runc**

- Docker: 28.0.4 (Ubuntu 24.04.5 LTS); `docker info`: "28.0.4" "Ubuntu 24.04.5 LTS" "7.0.0-1014-aws" "2" 4 16464289792 "x86_64"
- Runner image: `nbsandbox-runner:dev` `sha256:c2a9b507ffae1fb46a208180683e09b1482efa3667e4f460321c94fed3ef8311`, Python 3.13.18
- Limits per run: `{"concurrency":2,"cpus":1,"log_bytes":10485760,"max_file_bytes":52428800,"max_space_bytes":524288000,"memory_bytes":2147483648,"pids":256,"timeout_seconds":60,"tmp_bytes":268435456}`
- Facts recorded: 2026-11-26T13:21:06Z

**runsc**

- Docker: 28.0.4 (Ubuntu 24.04.5 LTS); `docker info`: "28.0.4" "Ubuntu 24.04.5 LTS" "7.0.0-1014-aws" "2" 4 16464302080 "x86_64"
- Runner image: `nbsandbox-runner:dev` `sha256:85bbfa3537570d367a06338371af29653ca9bd3a405281fb7f1e67c95e0e5b1a`, Python 3.13.18
- Limits per run: `{"concurrency":2,"cpus":1,"log_bytes":10485760,"max_file_bytes":52428800,"max_space_bytes":524288000,"memory_bytes":2147483648,"pids":256,"timeout_seconds":60,"tmp_bytes":268435456}`
- Facts recorded: 2026-11-26T13:21:28Z

## Isolation

| Check | Attempted | Expected | Observed (runc) | runc | Observed (runsc) | runsc |
|---|---|---|---|---|---|---|
| space-paths | read space B by absolute host path, by ../, and by listing / and /space/..; reach this space's other runs and published versions; list /run | none of those paths exist; /run holds only this run's folder | paths exist map[/home/runner/work/nbsandbox/nbsandbox-data/spaces:false /home/runner/work/nbsandbox/nbsandbox-data/spaces/13ccc310-4d8f-4f70-88c3-5c4c8df3c1f1/functions:false /home/runner/work/nbsandbox/nbsandbox-data/spaces/13ccc310-4d8f-4f70-88c3-5c4c8df3c1f1/runs:false /home/runner/work/nbsandbox/nbsandbox-data/spaces/3c6087bd-0dd8-4385-9890-af14854936f8/files:false /run/../3c6087bd-0dd8-4385-9890-af14854936f8:false /run/../runs:false /space/../3c6087bd-0dd8-4385-9890-af14854936f8:false /space/../3c6087bd-0dd8-4385-9890-af14854936f8/canary.txt:false /space/../functions:false /space/../runs:false /spaces:false /srv/nbsandbox:false]; / has 21 entries, none is space B | ✅ pass | paths exist map[/home/runner/work/nbsandbox/nbsandbox-data/spaces:false /home/runner/work/nbsandbox/nbsandbox-data/spaces/0f1060ee-fb59-4287-8644-28ae784bd507/functions:false /home/runner/work/nbsandbox/nbsandbox-data/spaces/0f1060ee-fb59-4287-8644-28ae784bd507/runs:false /home/runner/work/nbsandbox/nbsandbox-data/spaces/c74f8283-c54d-4b98-894a-8781bc3d934f/files:false /run/../c74f8283-c54d-4b98-894a-8781bc3d934f:false /run/../runs:false /space/../c74f8283-c54d-4b98-894a-8781bc3d934f:false /space/../c74f8283-c54d-4b98-894a-8781bc3d934f/canary.txt:false /space/../functions:false /space/../runs:false /spaces:false /srv/nbsandbox:false]; / has 21 entries, none is space B | ✅ pass |
| canary-search | search the container filesystem for bob's canary | not found | found in []; 578 files scanned in 0s | ✅ pass | found in []; 578 files scanned in 0.1s | ✅ pass |
| proc-list | list /proc | only alice's own processes | 3 processes visible, foreign [] | ✅ pass | 3 processes visible, foreign [] | ✅ pass |
| memory-secret | search readable /proc/*/environ, cmdline and mem for the holder's in-memory secret | not found | hits []; readable map[cmdline:3 environ:3 mem:1]; 2.54861312e+08 bytes of memory searched | ✅ pass | hits []; readable map[cmdline:3 environ:3 mem:1]; 1.69828352e+08 bytes of memory searched | ✅ pass |
| shared-memory | read /dev/shm | holder's shared-memory secret absent | /dev/shm entries [] | ✅ pass | /dev/shm entries [] | ✅ pass |
| socket-env-network | look for the Docker socket; list environment variables; try the network | no socket; only allowlisted variables; no network (on Kubernetes: nothing reachable, the API server, worker, kubelet and metadata service included) | sockets []; forbidden env []; up interfaces besides lo []; connections map[1.1.1.1:443:OSError: [Errno 101] Network is unreachable 169.254.169.254:80:OSError: [Errno 101] Network is unreachable 8.8.8.8:53:OSError: [Errno 101] Network is unreachable host.docker.internal:8080:gaierror: [Errno -3] Temporary failure in name resolution nbsandboxd:8080:gaierror: [Errno -3] Temporary failure in name resolution socket-proxy:2375:gaierror: [Errno -3] Temporary failure in name resolution] | ✅ pass | sockets []; forbidden env []; up interfaces besides lo []; connections map[1.1.1.1:443:OSError: [Errno 101] Network is unreachable 169.254.169.254:80:OSError: [Errno 101] Network is unreachable 8.8.8.8:53:OSError: [Errno 101] Network is unreachable host.docker.internal:8080:gaierror: [Errno -3] Temporary failure in name resolution nbsandboxd:8080:gaierror: [Errno -3] Temporary failure in name resolution socket-proxy:2375:gaierror: [Errno -3] Temporary failure in name resolution] | ✅ pass |
| privileges | id, CapEff, write to /usr, chmod u+s | uid 1000; CapEff 0; NoNewPrivs; writes fail; setuid ineffective (nosuid /tmp, or no-new-privileges on Kubernetes) | uid 1000, CapEff 0000000000000000, NoNewPrivs 1, Seccomp 2, write /usr: OSError, chmod u+s: set on /tmp (rw,nosuid,nodev,noexec,relatime,size=262144k,inode64), AppArmor: docker-default (enforce) | ✅ pass | uid 1000, CapEff 0000000000000000, NoNewPrivs 1, Seccomp 0, write /usr: PermissionError, chmod u+s: set on /tmp (rw,nosuid,noexec,size=262144k), AppArmor: [Errno 2] No such file or directory: '/proc/self/attr/current' | ✅ pass |
| cross-user-api | bob streams and downloads alice's run log, outputs and artifacts | 404 on every call | 404 404 404 404 404 404 404 404 | ✅ pass | 404 404 404 404 404 404 404 404 | ✅ pass |
| neighbour-unaffected | the holder's run after the probes | completed normally, secrets intact | succeeded | ✅ pass | succeeded | ✅ pass |

## Resources

| Check | Attempted | Expected | Observed (runc) | runc | Observed (runsc) | runsc |
|---|---|---|---|---|---|---|
| memory-limit | alice allocates beyond the memory limit while bob runs a normal notebook | alice oom (not kernel_died/failed) with peak memory reported (not on Kubernetes, which kills the whole container); bob succeeded | alice oom (the notebook ran out of memory: peak 2.0 GiB of the 2.0 GiB limit (files in /tmp count toward it)), peak 2048 MiB; bob succeeded | ✅ pass | alice oom (the notebook ran out of memory (limit 2.0 GiB)), peak 2048 MiB; bob succeeded | ✅ pass |
| cpu-fairness | alice spins all cores while bob runs a fixed CPU workload | bob within 1.5× of his solo baseline | bob alone 3.27s, with the hog 3.63s: 1.11× (runs alone [3.271 3.309 3.094], contended [3.613 3.663 3.633]; report-only on this platform) | ✅ pass | bob alone 3.29s, with the hog 3.89s: 1.18× (runs alone [3.311 3.289 3.247], contended [3.808 3.886 3.998]; report-only on this platform) | ✅ pass |
| process-limit | alice spawns processes until failure | failed/resource_pids (on Kubernetes: failed on the refused fork); bob unaffected; service responsive | alice failed/resource_pids; bob succeeded; slowest /readyz 5ms (0 not ready) | ✅ pass | alice failed/resource_pids; bob succeeded; slowest /readyz 3ms (0 not ready) | ✅ pass |
| time-limit | alice runs an infinite loop | timeout; container removed; partial output kept | timeout after 60s; partial true; containers left [] | ✅ pass | timeout after 60s; partial true; containers left [] | ✅ pass |
| tmp-limit | alice fills /tmp | failed/resource_tmp; others unaffected | alice failed/resource_tmp; bob succeeded | ✅ pass | alice failed/resource_tmp; bob succeeded | ✅ pass |
| endless-output | alice prints endlessly (200-byte lines for 25 s) | log stops at the cap with a notice; Docker's own log within its cap; host disk unaffected | succeeded; log.txt 10485673 bytes (cap 10485760), last line "[nbsandbox] log truncated at 10485581 by"; Docker's log 2564560 bytes; output.ipynb 4775065 bytes (stored output capped at 5 MiB) | ✅ pass | succeeded; log.txt 10485674 bytes (cap 10485760), last line "[nbsandbox] log truncated at 10485582 by"; Docker's log 2564560 bytes; output.ipynb 5442430 bytes (stored output capped at 5 MiB) | ✅ pass |

## Cancellation and errors

| Check | Attempted | Expected | Observed (runc) | runc | Observed (runsc) | runsc |
|---|---|---|---|---|---|---|
| cancel-running | cancel during a long cell | container gone within grace + 5s; cancelled; partial output | cancelled in 0.3s, partial true, container gone true | ✅ pass | cancelled in 0.3s, partial true, container gone true | ✅ pass |
| cancel-queued | cancel a queued run | never starts; cancelled | was queued true; cancelled; started false | ✅ pass | was queued true; cancelled; started false | ✅ pass |
| cancel-at-completion | cancel right as the last cell finishes (5 rounds; 200 in the unit test) | exactly one terminal status, the same in API, result.json and log | map[cancelled:5], consistent true | ✅ pass | map[cancelled:5], consistent true | ✅ pass |
| caller-disconnect | caller disconnects right after submitting | run completes normally | succeeded | ✅ pass | succeeded | ✅ pass |
| kernel-crash | crash in a C extension (ctypes) | kernel_died | kernel_died | ✅ pass | kernel_died | ✅ pass |
| cell-exception | exception raised in cell 3 | failed, cell 3, ValueError | failed, cell 3, ValueError: boom in cell 3 | ✅ pass | failed, cell 3, ValueError: boom in cell 3 | ✅ pass |
| docker-lost-mid-run | Docker proxy stopped mid-run | platform_error or interrupted, never a notebook fault; container cleaned up | interrupted (docker_lost, fault platform); readyz down true; submit while down 503 | ✅ pass | interrupted (docker_lost, fault platform); readyz down true; submit while down 503 | ✅ pass |
| render-failure | HTML rendering fails on a successful run | succeeded, rendered false | succeeded, rendered false | ✅ pass | succeeded, rendered false | ✅ pass |

## Functional and lifecycle

| Check | Attempted | Expected | Observed (runc) | runc | Observed (runsc) | runsc |
|---|---|---|---|---|---|---|
| parameters-and-artifacts | run with parameters on an uploaded CSV | succeeded; parameters applied; summary.csv artifact | succeeded; artifact "region,value\njakarta,30\n" | ✅ pass | succeeded; artifact "region,value\njakarta,30\n" | ✅ pass |
| runtime-info | GET /v1/runtime | image id and package list match the image | id match true, 70 packages, 0 mismatches | ✅ pass | id match true, 70 packages, 0 mismatches | ✅ pass |
| queue-concurrency | three spaces start runs at once, limit 2 | two run, one queued at position 1, all complete | 2 running, 1 queued (position 1), 3 succeeded | ✅ pass | 2 running, 1 queued (position 1), 3 succeeded | ✅ pass |
| restart-mid-run | kill and restart nbsandboxd while a run is active | orphan container removed; run interrupted | interrupted (orphaned) | ✅ pass | interrupted (orphaned) | ✅ pass |
| graceful-shutdown | SIGTERM to nbsandboxd during a run | run stopped as interrupted (shutdown); no containers left | interrupted/shutdown after 5.3s; containers left [] | ✅ pass | interrupted/shutdown after 5.3s; containers left [] | ✅ pass |
| delete-space | delete a space with a finished run | space, run and artifacts return 404 | delete 204, then [404 404 404] | ✅ pass | delete 204, then [404 404 404] | ✅ pass |

## Log and event streaming

| Check | Attempted | Expected | Observed (runc) | runc | Observed (runsc) | runsc |
|---|---|---|---|---|---|---|
| live-lines | notebook prints one line a second for 10 s | first line long before the run ends; order preserved | first line 10.3s before done; lines [0 1 2 3 4 5 6 7 8 9]; {"last_event_id":17,"status":"succeeded"} | ✅ pass | first line 10.4s before done; lines [0 1 2 3 4 5 6 7 8 9]; {"last_event_id":17,"status":"succeeded"} | ✅ pass |
| resume | disconnect after 5 lines, reconnect with Last-Event-ID | remaining lines, no gaps or duplicates | 17 lines total; sequence true; matches log.txt true | ✅ pass | 17 lines total; sequence true; matches log.txt true | ✅ pass |
| join-mid-run | connect mid-run (after line 4) | earlier lines replayed, then live lines, no gaps | 17 lines; sequence true; matches log.txt true | ✅ pass | 17 lines; sequence true; matches log.txt true | ✅ pass |
| replay-after-finish | connect after the run finished | full log replayed, then done with the final status | 15 lines in 0.00s; done failed; matches log.txt true | ✅ pass | 15 lines in 0.00s; done failed; matches log.txt true | ✅ pass |
| slow-viewer | five viewers on a 20,000-line run, one stops reading | run duration unaffected; fast viewers complete; the slow viewer isn't dropped and gets every line once it reads again | run 2.8s (alone 2.7s); fast viewers complete true ([20007 20007 20007 20007]); slow dropped false, complete true | ✅ pass | run 4.1s (alone 3.9s); fast viewers complete true ([20008 20008 20008 20008]); slow dropped false, complete true | ✅ pass |
| streamed-output | a notebook emits five items a second apart; follow the result stream, then read the result as JSON | items arrive live (ids 1 to 5, the last at least 2 s after the first), then the result with the same list; JSON gives the same list; the status stream has only status | events [item progress item progress item progress item progress item progress result]; first item 5.1s before the end, last 4.0s after the first; JSON output [{"step":0},{"step":1},{"step":2},{"step":3},{"step":4}]; 3 status events | ✅ pass | events [item progress item progress item progress item progress item progress result]; first item 5.4s before the end, last 4.2s after the first; JSON output [{"step":0},{"step":1},{"step":2},{"step":3},{"step":4}]; 3 status events | ✅ pass |

## Interactive sessions

| Check | Attempted | Expected | Observed (runc) | runc | Observed (runsc) | runsc |
|---|---|---|---|---|---|---|
| session-network | a session's kernel connects to the internet, the cloud metadata address, nbsandboxd and the cluster API, and resolves a name | every connection and lookup fails | 1.1.1.1:blocked 169.254.169.254:blocked nbsandboxd:blocked host.docker.internal:blocked kubernetes.default:blocked dns:none | ✅ pass | 1.1.1.1:blocked 169.254.169.254:blocked nbsandboxd:blocked host.docker.internal:blocked kubernetes.default:blocked dns:none | ✅ pass |
| session-isolation | a session looks at its user, capabilities, root filesystem, /space, other spaces' files and nbsandboxd's settings | uid 1000, no capabilities, a read-only system, only its own space's files, nothing of nbsandboxd's | 1000 0000000000000000 read-only ['mine.txt', 'nb.ipynb'] [] [] | ✅ pass | 1000 0000000000000000 read-only ['mine.txt', 'nb.ipynb'] [] [] | ✅ pass |
| session-memory-limit | a session's cell allocates memory without bound | the kernel is killed at the limit (runc on Docker, gVisor on Kubernetes: started again, the session goes on; gVisor on Docker, Kubernetes otherwise: the whole container, so the session ends, saying why); nbsandboxd goes on | kernel dead, ready again after 2s; next cell printed "alive"; nbsandboxd ready 200 | ✅ pass | the session ended after 9s (the session's container stopped (out of memory, or killed)); nbsandboxd ready 200 | ✅ pass |
| session-read-only | a read-only session changes, deletes and creates files in /space | every write fails; the space is unchanged; a writing run may start meanwhile | OSError OSError OSError; a writing run meanwhile: 201; keep.txt: "original" | ✅ pass | OSError OSError OSError; a writing run meanwhile: 201; keep.txt: "original" | ✅ pass |
| session-reattach | restart nbsandboxd while a session holds a variable; reconnect | the session's kernel kept running and is reached again, with its variables | after the restart, the variable is "42" | ✅ pass | after the restart, the variable is "42" | ✅ pass |

## Web UI

| Check | Attempted | Expected | Observed (runc) | runc | Observed (runsc) | runsc |
|---|---|---|---|---|---|---|
| web-ui | run a notebook and follow its log through the local web UI port | live lines, then done; other users still get 404 | 17 lines, first line 10.3s before done, {"last_event_id":17,"status":"succeeded"} | ✅ pass | 17 lines, first line 10.4s before done, {"last_event_id":17,"status":"succeeded"} | ✅ pass |

## Compute view

| Check | Attempted | Expected | Observed (runc) | runc | Observed (runsc) | runsc |
|---|---|---|---|---|---|---|
| compute-workers | list the workers as a user, run a notebook, look again | every worker listed with its role and capacity; the run counts on the worker that holds it, which it names, also once it ended; only administrators filter runs by worker | 1 up (want 1, role all): b0e7bbaea1a7; the run's worker "b0e7bbaea1a7" counted: true; still named once it ended: true; the admin filter refused to a user: true | ✅ pass | 1 up (want 1, role all): 15f52c89f74a; the run's worker "15f52c89f74a" counted: true; still named once it ended: true; the admin filter refused to a user: true | ✅ pass |
| compute-run-usage | a notebook that grows by 6 MiB every half second and computes; read its usage during and after | samples every few seconds follow its memory up to below its limit and show its CPU; its worker's totals include it while it runs; another user gets 404 | 5 samples, memory 200 → 387 MiB (limit 2048 MiB), CPU seen: true; its worker's totals while it ran: true; another user: 404 | ✅ pass | 6 samples, memory 181 → 417 MiB (limit 2048 MiB), CPU seen: true; its worker's totals while it ran: true; another user: 404 | ✅ pass |

## Notebook functions

| Check | Attempted | Expected | Observed (runc) | runc | Observed (runsc) | runsc |
|---|---|---|---|---|---|---|
| function-invoke | invoke a published notebook with nested JSON and wait | succeeded; output computed from the input and the bundle | 200 succeeded; model "m1", forecast [1.5 3 4.5 6], echo {"big":123456789012345678901234567890,"cfg":{"include":["sales",null],"months":4},"region":"bandung"} | ✅ pass | 200 succeeded; model "m1", forecast [1.5 3 4.5 6], echo {"big":123456789012345678901234567890,"cfg":{"include":["sales",null],"months":4},"region":"bandung"} | ✅ pass |
| function-immutable | edit the model file, publish v2, call latest and v1 | latest sees m2; v1 still sees m1 | latest v2 "m2"; v1 "m1" | ✅ pass | latest v2 "m2"; v1 "m1" | ✅ pass |
| function-errors | invalid input; a notebook that raises; an output that breaks the contract | 400 invalid_parameters, no run; failed with cell and exception; failed output_invalid | input 400 invalid_parameters (2 problems, 0 runs); raise failed/&{execution_error notebook false cell 1 raised ValueError: asked to fail 0x3cc8c5f9bd38 ValueError}; output failed/&{output_invalid notebook false the output doesn't match the notebook's output contract: (the whole value): missing properties 'region', 'forecast' <nil> } | ✅ pass | input 400 invalid_parameters (2 problems, 0 runs); raise failed/&{execution_error notebook false cell 1 raised ValueError: asked to fail 0x27d1d876f808 ValueError}; output failed/&{output_invalid notebook false the output doesn't match the notebook's output contract: (the whole value): missing properties 'region', 'forecast' <nil> } | ✅ pass |
| read-only-invocation | a function writes into /space; the same notebook as a normal run | invocation fails on the read-only file system; the normal run writes the file | invocation failed (&{execution_error notebook false cell 0 raised OSError: [Errno 30] Read-only file system: 'written.txt' 0x3cc8c65e45c0 OSError}); run succeeded; file listed true | ✅ pass | invocation failed (&{execution_error notebook false cell 0 raised OSError: [Errno 30] Read-only file system: 'written.txt' 0x27d1d6d66458 OSError}); run succeeded; file listed true | ✅ pass |
| parallel-invocations | two invocations of one space at once | both succeed, and their runs overlap in time | succeeded, succeeded in 2.9s; overlap true | ✅ pass | succeeded, succeeded in 4.2s; overlap true | ✅ pass |
| status-stream | follow the space's status stream, drop the connection after one event, resume | queued, running, succeeded: each once, none lost | queued,running,succeeded | ✅ pass | queued,running,succeeded | ✅ pass |
| start-latency | three small invocations, request to result; where the time went (median seconds per phase) | recorded (target: under 1 s of start-up; see the roadmap) | [2.821109361s 2.860459441s 2.820635865s]; queued 0.00, prepare 0.00, container_start 0.01, runner_start 0.79, kernel_start 1.26, execute 0.55, render 0.08, exit 0.10, teardown 0.03 | ✅ pass | [4.013425721s 3.943533582s 3.976328501s]; queued 0.00, prepare 0.00, container_start 0.01, runner_start 1.13, kernel_start 1.89, execute 0.70, render 0.09, exit 0.11, teardown 0.03 | ✅ pass |

## Notes

- **cpu-fairness** gates only on the Linux reference host (`NBSANDBOX_CPU_FAIRNESS_GATE=1`); elsewhere the ratio is recorded and the row passes if the test ran.
- **cancel-at-completion** runs 5 rounds with real containers; the 200-round race with exact timing control is a unit test (`internal/runs` TestCancelCompletionRace).
- **Retention** is a unit test with a fake clock (the minimum retention is one hour), so it has no row here.
- Known limitations are in [security/threat-model.md](security/threat-model.md).
