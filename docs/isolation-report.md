# nbsandbox Isolation Report

Generated 2026-11-08 07:34 UTC by `make report` from the integration tests' findings. Each row is a test that **ran**; the observed column is what the test saw, not what it assumed.

## Summary

| Runtime | Platform | Passed | Failed | Required tests missing |
|---|---|---|---|---|
| runc | Docker Desktop — **non-reference platform**: results show the controls work here, but the authoritative report comes from Linux with Docker Engine | 49 | 0 | none |

## Environment

**runc**

- Docker: 29.9.2 (Docker Desktop); `docker info`: "29.9.2" "Docker Desktop" "7.0.14-linuxkit" "2" 4 4094517248 "aarch64"
- Runner image: `nbsandbox-runner:dev` `sha256:3939222422831b155d3fca0dfc2f353911ef811c4b17ab1a68d6c44ef26802e2`, Python 3.13.18
- Limits per run: `{"concurrency":2,"cpus":1,"log_bytes":10485760,"max_file_bytes":52428800,"max_space_bytes":524288000,"memory_bytes":2147483648,"pids":256,"timeout_seconds":60,"tmp_bytes":268435456}`
- Facts recorded: 2026-11-08T07:26:42Z

## Isolation

| Check | Attempted | Expected | Observed (runc) | runc |
|---|---|---|---|---|
| space-paths | read space B by absolute host path, by ../, and by listing / and /space/..; reach this space's other runs and published versions; list /run | none of those paths exist; /run holds only this run's folder | paths exist map[~/.local/share/nbsandbox/spaces:false ~/.local/share/nbsandbox/spaces/749655a1-baa8-4620-98f5-57f794bb6afc/files:false ~/.local/share/nbsandbox/spaces/d0281830-5154-4d3e-85d3-ad680a56829d/functions:false ~/.local/share/nbsandbox/spaces/d0281830-5154-4d3e-85d3-ad680a56829d/runs:false /run/../749655a1-baa8-4620-98f5-57f794bb6afc:false /run/../runs:false /space/../749655a1-baa8-4620-98f5-57f794bb6afc:false /space/../749655a1-baa8-4620-98f5-57f794bb6afc/canary.txt:false /space/../functions:false /space/../runs:false /spaces:false /srv/nbsandbox:false]; / has 20 entries, none is space B | ✅ pass |
| canary-search | search the container filesystem for bob's canary | not found | found in []; 577 files scanned in 0.1s | ✅ pass |
| proc-list | list /proc | only alice's own processes | 3 processes visible, foreign [] | ✅ pass |
| memory-secret | search readable /proc/*/environ, cmdline and mem for the holder's in-memory secret | not found | hits []; readable map[cmdline:3 environ:3 mem:3]; 2.91262464e+08 bytes of memory searched | ✅ pass |
| shared-memory | read /dev/shm | holder's shared-memory secret absent | /dev/shm entries [] | ✅ pass |
| socket-env-network | look for the Docker socket; list environment variables; try the network | no socket; only allowlisted variables; no network (on Kubernetes: nothing reachable, the API server, worker, kubelet and metadata service included) | sockets []; forbidden env []; up interfaces besides lo []; connections map[1.1.1.1:443:OSError: [Errno 101] Network is unreachable 169.254.169.254:80:OSError: [Errno 101] Network is unreachable 8.8.8.8:53:OSError: [Errno 101] Network is unreachable host.docker.internal:8080:gaierror: [Errno -3] Temporary failure in name resolution nbsandboxd:8080:gaierror: [Errno -3] Temporary failure in name resolution socket-proxy:2375:gaierror: [Errno -3] Temporary failure in name resolution] | ✅ pass |
| privileges | id, CapEff, write to /usr, chmod u+s | uid 1000; CapEff 0; NoNewPrivs; writes fail; setuid ineffective (nosuid /tmp, or no-new-privileges on Kubernetes) | uid 1000, CapEff 0000000000000000, NoNewPrivs 1, Seccomp 2, write /usr: OSError, chmod u+s: set on /tmp (rw,nosuid,nodev,noexec,relatime,size=262144k), AppArmor: [Errno 22] Invalid argument | ✅ pass |
| cross-user-api | bob streams and downloads alice's run log, outputs and artifacts | 404 on every call | 404 404 404 404 404 404 404 404 | ✅ pass |
| neighbour-unaffected | the holder's run after the probes | completed normally, secrets intact | succeeded | ✅ pass |

## Resources

| Check | Attempted | Expected | Observed (runc) | runc |
|---|---|---|---|---|
| memory-limit | alice allocates beyond the memory limit while bob runs a normal notebook | alice oom (not kernel_died/failed) with peak memory reported (not on Kubernetes, which kills the whole container); bob succeeded | alice oom (the notebook ran out of memory: peak 2.0 GiB of the 2.0 GiB limit (files in /tmp count toward it)), peak 2048 MiB; bob succeeded | ✅ pass |
| cpu-fairness | alice spins all cores while bob runs a fixed CPU workload | bob within 1.5× of his solo baseline | bob alone 3.37s, with the hog 4.12s: 1.22× (runs alone [3.265 3.45 3.372], contended [3.959 4.124 4.374]; report-only on this platform) | ✅ pass |
| process-limit | alice spawns processes until failure | failed/resource_pids (on Kubernetes: failed on the refused fork); bob unaffected; service responsive | alice failed/resource_pids; bob succeeded; slowest /readyz 3ms | ✅ pass |
| time-limit | alice runs an infinite loop | timeout; container removed; partial output kept | timeout after 60s; partial true; containers left [] | ✅ pass |
| tmp-limit | alice fills /tmp | failed/resource_tmp; others unaffected | alice failed/resource_tmp; bob succeeded | ✅ pass |
| endless-output | alice prints endlessly (200-byte lines for 25 s) | log stops at the cap with a notice; Docker's own log within its cap; host disk unaffected | succeeded; log.txt 10485672 bytes (cap 10485760), last line "[nbsandbox] log truncated at 10485580 by"; Docker's log 2564559 bytes; output.ipynb 5203209 bytes (stored output capped at 5 MiB) | ✅ pass |

## Cancellation and errors

| Check | Attempted | Expected | Observed (runc) | runc |
|---|---|---|---|---|
| cancel-running | cancel during a long cell | container gone within grace + 5s; cancelled; partial output | cancelled in 0.3s, partial true, container gone true | ✅ pass |
| cancel-queued | cancel a queued run | never starts; cancelled | was queued true; cancelled; started false | ✅ pass |
| cancel-at-completion | cancel right as the last cell finishes (5 rounds; 200 in the unit test) | exactly one terminal status, the same in API, result.json and log | map[cancelled:5], consistent true | ✅ pass |
| caller-disconnect | caller disconnects right after submitting | run completes normally | succeeded | ✅ pass |
| kernel-crash | crash in a C extension (ctypes) | kernel_died | kernel_died | ✅ pass |
| cell-exception | exception raised in cell 3 | failed, cell 3, ValueError | failed &{CellIndex:0x56fc257176e8 ExceptionType:ValueError Message:boom in cell 3} | ✅ pass |
| docker-lost-mid-run | Docker proxy stopped mid-run | platform_error or interrupted, never a notebook fault; container cleaned up | interrupted (docker_lost, fault platform); readyz down true; submit while down 503 | ✅ pass |
| render-failure | HTML rendering fails on a successful run | succeeded, rendered false | succeeded, rendered false | ✅ pass |

## Functional and lifecycle

| Check | Attempted | Expected | Observed (runc) | runc |
|---|---|---|---|---|
| parameters-and-artifacts | run with parameters on an uploaded CSV | succeeded; parameters applied; summary.csv artifact | succeeded; artifact "region,value\njakarta,30\n" | ✅ pass |
| runtime-info | GET /v1/runtime | image id and package list match the image | id match true, 69 packages, 0 mismatches | ✅ pass |
| queue-concurrency | three spaces start runs at once, limit 2 | two run, one queued at position 1, all complete | 2 running, 1 queued (position 1), 3 succeeded | ✅ pass |
| restart-mid-run | kill and restart nbsandboxd while a run is active | orphan container removed; run interrupted | interrupted (orphaned) | ✅ pass |
| graceful-shutdown | SIGTERM to nbsandboxd during a run | run stopped as interrupted (shutdown); no containers left | interrupted/shutdown after 5.4s; containers left [] | ✅ pass |
| delete-space | delete a space with a finished run | space, run and artifacts return 404 | delete 204, then [404 404 404] | ✅ pass |

## Log and event streaming

| Check | Attempted | Expected | Observed (runc) | runc |
|---|---|---|---|---|
| live-lines | notebook prints one line a second for 10 s | first line long before the run ends; order preserved | first line 11.3s before done; lines [0 1 2 3 4 5 6 7 8 9]; {"last_event_id":17,"status":"succeeded"} | ✅ pass |
| resume | disconnect after 5 lines, reconnect with Last-Event-ID | remaining lines, no gaps or duplicates | 17 lines total; sequence true; matches log.txt true | ✅ pass |
| join-mid-run | connect mid-run (after line 4) | earlier lines replayed, then live lines, no gaps | 17 lines; sequence true; matches log.txt true | ✅ pass |
| replay-after-finish | connect after the run finished | full log replayed, then done with the final status | 15 lines in 0.01s; done failed; matches log.txt true | ✅ pass |
| slow-viewer | five viewers on a 20,000-line run, one stops reading | run duration unaffected; fast viewers complete; the slow viewer isn't dropped and gets every line once it reads again | run 3.1s (alone 3.1s); fast viewers complete true ([20007 20007 20007 20007]); slow dropped false, complete true | ✅ pass |
| streamed-output | a notebook emits three items a second apart; follow the result stream, then read the result as JSON | items arrive live (ids 1, 2, 3), then the result with the same list; JSON gives the same list; the status stream has only status | events [item progress item progress item progress result]; first item 4.4s before the end, last 2.3s after the first; JSON output [{"step":0},{"step":1},{"step":2}]; 3 status events | ✅ pass |

## Web UI

| Check | Attempted | Expected | Observed (runc) | runc |
|---|---|---|---|---|
| web-ui | run a notebook and follow its log through the local web UI port | live lines, then done; other users still get 404 | 17 lines, first line 11.3s before done, {"last_event_id":17,"status":"succeeded"} | ✅ pass |

## Notebook functions

| Check | Attempted | Expected | Observed (runc) | runc |
|---|---|---|---|---|
| function-invoke | invoke a published notebook with nested JSON and wait | succeeded; output computed from the input and the bundle | 200 succeeded; model "m1", forecast [1.5 3 4.5 6], echo {"big":123456789012345678901234567890,"cfg":{"include":["sales",null],"months":4},"region":"bandung"} | ✅ pass |
| function-immutable | edit the model file, publish v2, call latest and v1 | latest sees m2; v1 still sees m1 | latest v2 "m2"; v1 "m1" | ✅ pass |
| function-errors | invalid input; a notebook that raises; an output that breaks the contract | 400 invalid_parameters, no run; failed with cell and exception; failed output_invalid | input 400 invalid_parameters (2 problems, 0 runs); raise failed/&{execution_error notebook false cell 1 raised ValueError: asked to fail 0x56fc24893688 ValueError}; output failed/&{output_invalid notebook false the output doesn't match the notebook's output contract: (the whole value): missing properties 'region', 'forecast' <nil> } | ✅ pass |
| read-only-invocation | a function writes into /space; the same notebook as a normal run | invocation fails on the read-only file system; the normal run writes the file | invocation failed (&{execution_error notebook false cell 0 raised OSError: [Errno 30] Read-only file system: 'written.txt' 0x56fc24893cf0 OSError}); run succeeded; file listed true | ✅ pass |
| parallel-invocations | two invocations of one space at once | both succeed, and their runs overlap in time | succeeded, succeeded in 3.5s; overlap true | ✅ pass |
| status-stream | follow the space's status stream, drop the connection after one event, resume | queued, running, succeeded: each once, none lost | queued,running,succeeded | ✅ pass |
| start-latency | three small invocations, request to result | recorded (target: under 1 s of start-up; see the roadmap) | [3.183242709s 3.178558792s 3.177044666s] | ✅ pass |

## Notes

- **cpu-fairness** gates only on the Linux reference host (`NBSANDBOX_CPU_FAIRNESS_GATE=1`); elsewhere the ratio is recorded and the row passes if the test ran.
- **cancel-at-completion** runs 5 rounds with real containers; the 200-round race with exact timing control is a unit test (`internal/runs` TestCancelCompletionRace).
- **Retention** is a unit test with a fake clock (the minimum retention is one hour), so it has no row here.
- Known limitations are in [security/threat-model.md](security/threat-model.md).
