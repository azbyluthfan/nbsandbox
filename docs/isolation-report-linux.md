# nbsandbox Isolation Report: Linux with Docker Engine

Generated 2026-11-08 08:48 UTC by `make report-linux` from the integration tests' findings. Each row is a test that **ran**; the observed column is what the test saw, not what it assumed.

## Summary

| Runtime | Platform | Passed | Failed | Required tests missing |
|---|---|---|---|---|
| runc | Linux with Docker Engine — reference platform | 49 | 0 | none |
| runsc | Linux with Docker Engine — reference platform | 49 | 0 | none |

## Environment

**runc**

- Docker: 29.9.3 (Ubuntu 26.04.1 LTS); `docker info`: "29.9.3" "Ubuntu 26.04.1 LTS" "7.0.0-38-generic" "2" 2 2045415424 "aarch64"
- Runner image: `nbsandbox-runner:dev` `sha256:2026c12f1f6e0b45920dc0c7971a4320ec69f774d32f78ed50a884968e238648`, Python 3.13.18
- Limits per run: `{"concurrency":2,"cpus":1,"log_bytes":10485760,"max_file_bytes":52428800,"max_space_bytes":524288000,"memory_bytes":536870912,"pids":256,"timeout_seconds":60,"tmp_bytes":268435456}`
- Facts recorded: 2026-11-08T08:31:42Z

**runsc**

- Docker: 29.9.3 (Ubuntu 26.04.1 LTS); `docker info`: "29.9.3" "Ubuntu 26.04.1 LTS" "7.0.0-38-generic" "2" 2 2045415424 "aarch64"
- Runner image: `nbsandbox-runner:dev` `sha256:2026c12f1f6e0b45920dc0c7971a4320ec69f774d32f78ed50a884968e238648`, Python 3.13.18
- Limits per run: `{"concurrency":2,"cpus":1,"log_bytes":10485760,"max_file_bytes":52428800,"max_space_bytes":524288000,"memory_bytes":536870912,"pids":256,"timeout_seconds":60,"tmp_bytes":268435456}`
- Facts recorded: 2026-11-08T08:39:39Z

## Isolation

| Check | Attempted | Expected | Observed (runc) | runc | Observed (runsc) | runsc |
|---|---|---|---|---|---|---|
| space-paths | read space B by absolute host path, by ../, and by listing / and /space/..; reach this space's other runs and published versions; list /run | none of those paths exist; /run holds only this run's folder | paths exist map[/run/../14fcdbd3-8a17-4f5b-8564-34125b9b7034:false /run/../runs:false /space/../14fcdbd3-8a17-4f5b-8564-34125b9b7034:false /space/../14fcdbd3-8a17-4f5b-8564-34125b9b7034/canary.txt:false /space/../functions:false /space/../runs:false /spaces:false /srv/nbsandbox:false /var/lib/nbsandbox-report/data/spaces:false /var/lib/nbsandbox-report/data/spaces/14fcdbd3-8a17-4f5b-8564-34125b9b7034/files:false /var/lib/nbsandbox-report/data/spaces/691e7e93-a48a-4969-9ac3-ef07a0d36ce5/functions:false /var/lib/nbsandbox-report/data/spaces/691e7e93-a48a-4969-9ac3-ef07a0d36ce5/runs:false]; / has 20 entries, none is space B | ✅ pass | paths exist map[/run/../3c8b29a6-2287-4765-b3dd-682ff567385f:false /run/../runs:false /space/../3c8b29a6-2287-4765-b3dd-682ff567385f:false /space/../3c8b29a6-2287-4765-b3dd-682ff567385f/canary.txt:false /space/../functions:false /space/../runs:false /spaces:false /srv/nbsandbox:false /var/lib/nbsandbox-report/data/spaces:false /var/lib/nbsandbox-report/data/spaces/3c8b29a6-2287-4765-b3dd-682ff567385f/files:false /var/lib/nbsandbox-report/data/spaces/46561806-085e-4641-9ea3-f284a61c7a19/functions:false /var/lib/nbsandbox-report/data/spaces/46561806-085e-4641-9ea3-f284a61c7a19/runs:false]; / has 20 entries, none is space B | ✅ pass |
| canary-search | search the container filesystem for bob's canary | not found | found in []; 577 files scanned in 0.1s | ✅ pass | found in []; 577 files scanned in 0.2s | ✅ pass |
| proc-list | list /proc | only alice's own processes | 3 processes visible, foreign [] | ✅ pass | 3 processes visible, foreign [] | ✅ pass |
| memory-secret | search readable /proc/*/environ, cmdline and mem for the holder's in-memory secret | not found | hits []; readable map[cmdline:3 environ:3 mem:1]; 1.59219712e+08 bytes of memory searched | ✅ pass | hits []; readable map[cmdline:3 environ:3 mem:1]; 1.67337984e+08 bytes of memory searched | ✅ pass |
| shared-memory | read /dev/shm | holder's shared-memory secret absent | /dev/shm entries [] | ✅ pass | /dev/shm entries [] | ✅ pass |
| socket-env-network | look for the Docker socket; list environment variables; try the network | no socket; only allowlisted variables; no network (on Kubernetes: nothing reachable, the API server, worker, kubelet and metadata service included) | sockets []; forbidden env []; up interfaces besides lo []; connections map[1.1.1.1:443:OSError: [Errno 101] Network is unreachable 169.254.169.254:80:OSError: [Errno 101] Network is unreachable 8.8.8.8:53:OSError: [Errno 101] Network is unreachable host.docker.internal:8080:gaierror: [Errno -3] Temporary failure in name resolution nbsandboxd:8080:gaierror: [Errno -3] Temporary failure in name resolution socket-proxy:2375:gaierror: [Errno -3] Temporary failure in name resolution] | ✅ pass | sockets []; forbidden env []; up interfaces besides lo []; connections map[1.1.1.1:443:OSError: [Errno 101] Network is unreachable 169.254.169.254:80:OSError: [Errno 101] Network is unreachable 8.8.8.8:53:OSError: [Errno 101] Network is unreachable host.docker.internal:8080:gaierror: [Errno -3] Temporary failure in name resolution nbsandboxd:8080:gaierror: [Errno -3] Temporary failure in name resolution socket-proxy:2375:gaierror: [Errno -3] Temporary failure in name resolution] | ✅ pass |
| privileges | id, CapEff, write to /usr, chmod u+s | uid 1000; CapEff 0; NoNewPrivs; writes fail; setuid ineffective (nosuid /tmp, or no-new-privileges on Kubernetes) | uid 1000, CapEff 0000000000000000, NoNewPrivs 1, Seccomp 2, write /usr: OSError, chmod u+s: set on /tmp (rw,nosuid,nodev,noexec,relatime,size=262144k,inode64), AppArmor: docker-default (enforce) | ✅ pass | uid 1000, CapEff 0000000000000000, NoNewPrivs 1, Seccomp 0, write /usr: PermissionError, chmod u+s: set on /tmp (rw,nosuid,noexec,size=262144k), AppArmor: [Errno 2] No such file or directory: '/proc/self/attr/current' | ✅ pass |
| cross-user-api | bob streams and downloads alice's run log, outputs and artifacts | 404 on every call | 404 404 404 404 404 404 404 404 | ✅ pass | 404 404 404 404 404 404 404 404 | ✅ pass |
| neighbour-unaffected | the holder's run after the probes | completed normally, secrets intact | succeeded | ✅ pass | succeeded | ✅ pass |

## Resources

| Check | Attempted | Expected | Observed (runc) | runc | Observed (runsc) | runsc |
|---|---|---|---|---|---|---|
| memory-limit | alice allocates beyond the memory limit while bob runs a normal notebook | alice oom (not kernel_died/failed) with peak memory reported (not on Kubernetes, which kills the whole container); bob succeeded | alice oom (the notebook ran out of memory: peak 512 MiB of the 512 MiB limit (files in /tmp count toward it)), peak 512 MiB; bob succeeded | ✅ pass | alice oom (the notebook ran out of memory (limit 512 MiB)), peak 512 MiB; bob succeeded | ✅ pass |
| cpu-fairness | alice spins all cores while bob runs a fixed CPU workload | bob within 1.5× of his solo baseline | bob alone 3.62s, with the hog 3.50s: 0.97× (runs alone [3.392 3.623 3.637], contended [3.46 3.693 3.505]; gating) | ✅ pass | bob alone 3.29s, with the hog 3.65s: 1.11× (runs alone [3.422 3.294 3.051], contended [3.6 3.852 3.65]; gating) | ✅ pass |
| process-limit | alice spawns processes until failure | failed/resource_pids (on Kubernetes: failed on the refused fork); bob unaffected; service responsive | alice failed/resource_pids; bob succeeded; slowest /readyz 5ms | ✅ pass | alice failed/resource_pids; bob succeeded; slowest /readyz 21ms | ✅ pass |
| time-limit | alice runs an infinite loop | timeout; container removed; partial output kept | timeout after 60s; partial true; containers left [] | ✅ pass | timeout after 60s; partial true; containers left [] | ✅ pass |
| tmp-limit | alice fills /tmp | failed/resource_tmp; others unaffected | alice failed/resource_tmp; bob succeeded | ✅ pass | alice failed/resource_tmp; bob succeeded | ✅ pass |
| endless-output | alice prints endlessly (200-byte lines for 25 s) | log stops at the cap with a notice; Docker's own log within its cap; host disk unaffected | succeeded; log.txt 10485673 bytes (cap 10485760), last line "[nbsandbox] log truncated at 10485581 by"; Docker's log 2564559 bytes; output.ipynb 4164833 bytes (stored output capped at 5 MiB) | ✅ pass | succeeded; log.txt 10485672 bytes (cap 10485760), last line "[nbsandbox] log truncated at 10485580 by"; Docker's log 2564560 bytes; output.ipynb 5195962 bytes (stored output capped at 5 MiB) | ✅ pass |

## Cancellation and errors

| Check | Attempted | Expected | Observed (runc) | runc | Observed (runsc) | runsc |
|---|---|---|---|---|---|---|
| cancel-running | cancel during a long cell | container gone within grace + 5s; cancelled; partial output | cancelled in 0.3s, partial true, container gone true | ✅ pass | cancelled in 0.3s, partial true, container gone true | ✅ pass |
| cancel-queued | cancel a queued run | never starts; cancelled | was queued true; cancelled; started false | ✅ pass | was queued true; cancelled; started false | ✅ pass |
| cancel-at-completion | cancel right as the last cell finishes (5 rounds; 200 in the unit test) | exactly one terminal status, the same in API, result.json and log | map[cancelled:5], consistent true | ✅ pass | map[cancelled:5], consistent true | ✅ pass |
| caller-disconnect | caller disconnects right after submitting | run completes normally | succeeded | ✅ pass | succeeded | ✅ pass |
| kernel-crash | crash in a C extension (ctypes) | kernel_died | kernel_died | ✅ pass | kernel_died | ✅ pass |
| cell-exception | exception raised in cell 3 | failed, cell 3, ValueError | failed &{CellIndex:0x1466fe16c1b8 ExceptionType:ValueError Message:boom in cell 3} | ✅ pass | failed &{CellIndex:0x69cfea62a250 ExceptionType:ValueError Message:boom in cell 3} | ✅ pass |
| docker-lost-mid-run | Docker proxy stopped mid-run | platform_error or interrupted, never a notebook fault; container cleaned up | interrupted (docker_lost, fault platform); readyz down true; submit while down 503 | ✅ pass | interrupted (docker_lost, fault platform); readyz down true; submit while down 503 | ✅ pass |
| render-failure | HTML rendering fails on a successful run | succeeded, rendered false | succeeded, rendered false | ✅ pass | succeeded, rendered false | ✅ pass |

## Functional and lifecycle

| Check | Attempted | Expected | Observed (runc) | runc | Observed (runsc) | runsc |
|---|---|---|---|---|---|---|
| parameters-and-artifacts | run with parameters on an uploaded CSV | succeeded; parameters applied; summary.csv artifact | succeeded; artifact "region,value\njakarta,30\n" | ✅ pass | succeeded; artifact "region,value\njakarta,30\n" | ✅ pass |
| runtime-info | GET /v1/runtime | image id and package list match the image | id match true, 69 packages, 0 mismatches | ✅ pass | id match true, 69 packages, 0 mismatches | ✅ pass |
| queue-concurrency | three spaces start runs at once, limit 2 | two run, one queued at position 1, all complete | 2 running, 1 queued (position 1), 3 succeeded | ✅ pass | 2 running, 1 queued (position 1), 3 succeeded | ✅ pass |
| restart-mid-run | kill and restart nbsandboxd while a run is active | orphan container removed; run interrupted | interrupted (orphaned) | ✅ pass | interrupted (orphaned) | ✅ pass |
| graceful-shutdown | SIGTERM to nbsandboxd during a run | run stopped as interrupted (shutdown); no containers left | interrupted/shutdown after 5.5s; containers left [] | ✅ pass | interrupted/shutdown after 5.5s; containers left [] | ✅ pass |
| delete-space | delete a space with a finished run | space, run and artifacts return 404 | delete 204, then [404 404 404] | ✅ pass | delete 204, then [404 404 404] | ✅ pass |

## Log and event streaming

| Check | Attempted | Expected | Observed (runc) | runc | Observed (runsc) | runsc |
|---|---|---|---|---|---|---|
| live-lines | notebook prints one line a second for 10 s | first line long before the run ends; order preserved | first line 11.3s before done; lines [0 1 2 3 4 5 6 7 8 9]; {"last_event_id":17,"status":"succeeded"} | ✅ pass | first line 12.4s before done; lines [0 1 2 3 4 5 6 7 8 9]; {"last_event_id":17,"status":"succeeded"} | ✅ pass |
| resume | disconnect after 5 lines, reconnect with Last-Event-ID | remaining lines, no gaps or duplicates | 17 lines total; sequence true; matches log.txt true | ✅ pass | 17 lines total; sequence true; matches log.txt true | ✅ pass |
| join-mid-run | connect mid-run (after line 4) | earlier lines replayed, then live lines, no gaps | 17 lines; sequence true; matches log.txt true | ✅ pass | 17 lines; sequence true; matches log.txt true | ✅ pass |
| replay-after-finish | connect after the run finished | full log replayed, then done with the final status | 15 lines in 0.00s; done failed; matches log.txt true | ✅ pass | 15 lines in 0.00s; done failed; matches log.txt true | ✅ pass |
| slow-viewer | five viewers on a 20,000-line run, one stops reading | run duration unaffected; fast viewers complete; the slow viewer isn't dropped and gets every line once it reads again | run 3.3s (alone 2.8s); fast viewers complete true ([20007 20007 20007 20007]); slow dropped false, complete true | ✅ pass | run 4.7s (alone 4.5s); fast viewers complete true ([20007 20007 20007 20007]); slow dropped false, complete true | ✅ pass |
| streamed-output | a notebook emits three items a second apart; follow the result stream, then read the result as JSON | items arrive live (ids 1, 2, 3), then the result with the same list; JSON gives the same list; the status stream has only status | events [item progress item progress item progress result]; first item 4.1s before the end, last 2.0s after the first; JSON output [{"step":0},{"step":1},{"step":2}]; 3 status events | ✅ pass | events [item progress item progress item progress result]; first item 4.8s before the end, last 2.0s after the first; JSON output [{"step":0},{"step":1},{"step":2}]; 3 status events | ✅ pass |

## Web UI

| Check | Attempted | Expected | Observed (runc) | runc | Observed (runsc) | runsc |
|---|---|---|---|---|---|---|
| web-ui | run a notebook and follow its log through the local web UI port | live lines, then done; other users still get 404 | 17 lines, first line 11.3s before done, {"last_event_id":17,"status":"succeeded"} | ✅ pass | 17 lines, first line 12.1s before done, {"last_event_id":17,"status":"succeeded"} | ✅ pass |

## Notebook functions

| Check | Attempted | Expected | Observed (runc) | runc | Observed (runsc) | runsc |
|---|---|---|---|---|---|---|
| function-invoke | invoke a published notebook with nested JSON and wait | succeeded; output computed from the input and the bundle | 200 succeeded; model "m1", forecast [1.5 3 4.5 6], echo {"big":123456789012345678901234567890,"cfg":{"include":["sales",null],"months":4},"region":"bandung"} | ✅ pass | 200 succeeded; model "m1", forecast [1.5 3 4.5 6], echo {"big":123456789012345678901234567890,"cfg":{"include":["sales",null],"months":4},"region":"bandung"} | ✅ pass |
| function-immutable | edit the model file, publish v2, call latest and v1 | latest sees m2; v1 still sees m1 | latest v2 "m2"; v1 "m1" | ✅ pass | latest v2 "m2"; v1 "m1" | ✅ pass |
| function-errors | invalid input; a notebook that raises; an output that breaks the contract | 400 invalid_parameters, no run; failed with cell and exception; failed output_invalid | input 400 invalid_parameters (2 problems, 0 runs); raise failed/&{execution_error notebook false cell 1 raised ValueError: asked to fail 0x1466fdc05228 ValueError}; output failed/&{output_invalid notebook false the output doesn't match the notebook's output contract: (the whole value): missing properties 'region', 'forecast' <nil> } | ✅ pass | input 400 invalid_parameters (2 problems, 0 runs); raise failed/&{execution_error notebook false cell 1 raised ValueError: asked to fail 0x69cfe9f15958 ValueError}; output failed/&{output_invalid notebook false the output doesn't match the notebook's output contract: (the whole value): missing properties 'region', 'forecast' <nil> } | ✅ pass |
| read-only-invocation | a function writes into /space; the same notebook as a normal run | invocation fails on the read-only file system; the normal run writes the file | invocation failed (&{execution_error notebook false cell 0 raised OSError: [Errno 30] Read-only file system: 'written.txt' 0x1466fd92d930 OSError}); run succeeded; file listed true | ✅ pass | invocation failed (&{execution_error notebook false cell 0 raised OSError: [Errno 30] Read-only file system: 'written.txt' 0x69cfe9f86ea0 OSError}); run succeeded; file listed true | ✅ pass |
| parallel-invocations | two invocations of one space at once | both succeed, and their runs overlap in time | succeeded, succeeded in 3.8s; overlap true | ✅ pass | succeeded, succeeded in 6.2s; overlap true | ✅ pass |
| status-stream | follow the space's status stream, drop the connection after one event, resume | queued, running, succeeded: each once, none lost | queued,running,succeeded | ✅ pass | queued,running,succeeded | ✅ pass |
| start-latency | three small invocations, request to result | recorded (target: under 1 s of start-up; see the roadmap) | [7.476774846s 3.774789308s 3.566752845s] | ✅ pass | [4.27766287s 4.261141869s 4.218105272s] | ✅ pass |

## Notes

- **cpu-fairness** gates only on the Linux reference host (`NBSANDBOX_CPU_FAIRNESS_GATE=1`); elsewhere the ratio is recorded and the row passes if the test ran.
- **cancel-at-completion** runs 5 rounds with real containers; the 200-round race with exact timing control is a unit test (`internal/runs` TestCancelCompletionRace).
- **Retention** is a unit test with a fake clock (the minimum retention is one hour), so it has no row here.
- Known limitations are in [security/threat-model.md](security/threat-model.md).
