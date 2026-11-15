# nbsandbox Isolation Report: Kubernetes backend

Generated 2026-11-15 15:14 UTC by `make report-k8s in the weekly CI run (kind with Kubernetes 1.37 and Cilium, on GitHub's Linux runner)` from the integration tests' findings. Each row is a test that **ran**; the observed column is what the test saw, not what it assumed.

## Summary

| Runtime | Platform | Passed | Failed | Required tests missing |
|---|---|---|---|---|
| gvisor | Kubernetes v1.37.0 | 45 | 0 | none |
| runc | Kubernetes v1.37.0 | 45 | 0 | none |

## Environment

**gvisor**

- Kubernetes: v1.37.0 (Kubernetes linux/amd64); the Docker underneath: "28.0.4" "Ubuntu 24.04.5 LTS" "7.0.0-1014-aws" "2" 4 16464306176 "x86_64"
- Not applicable: docker-lost-mid-run (stops Docker Compose services)
- Runner image: `nbsandbox-runner:dev` `sha256:4a43c6cabe4c7f4d54dabd3bd4ac1bd0bccf3bd6150ed4ea6aa6a7876bfb7375`, Python 3.13.18
- Limits per run: `{"concurrency":1,"cpus":1,"log_bytes":10485760,"max_file_bytes":104857600,"max_space_bytes":536870912,"memory_bytes":1073741824,"pids":256,"timeout_seconds":150,"tmp_bytes":268435456}`
- Facts recorded: 2026-11-15T13:59:13Z

**runc**

- Kubernetes: v1.37.0 (Kubernetes linux/amd64); the Docker underneath: "28.0.4" "Ubuntu 24.04.5 LTS" "7.0.0-1017-azure" "2" 4 16759308288 "x86_64"
- Not applicable: docker-lost-mid-run (stops Docker Compose services)
- Runner image: `nbsandbox-runner:dev` `sha256:972662ec3a6f394d3abbee30683fe41b92533e779ddbcee280e220d79785f803`, Python 3.13.18
- Limits per run: `{"concurrency":1,"cpus":1,"log_bytes":10485760,"max_file_bytes":104857600,"max_space_bytes":536870912,"memory_bytes":1073741824,"pids":256,"timeout_seconds":150,"tmp_bytes":268435456}`
- Facts recorded: 2026-11-15T14:00:23Z

## Isolation

| Check | Attempted | Expected | Observed (gvisor) | gvisor | Observed (runc) | runc |
|---|---|---|---|---|---|---|
| space-paths | read space B by absolute host path, by ../, and by listing / and /space/..; reach this space's other runs and published versions; list /run | none of those paths exist; /run holds only this run's folder | paths exist map[/home/runner/.local/share/nbsandbox/spaces:false /home/runner/.local/share/nbsandbox/spaces/4306fa47-1990-4cfc-a588-134d893c2a12/files:false /home/runner/.local/share/nbsandbox/spaces/7073c7d7-1ef8-4c4a-8e69-9189e899a826/functions:false /home/runner/.local/share/nbsandbox/spaces/7073c7d7-1ef8-4c4a-8e69-9189e899a826/runs:false /run/../4306fa47-1990-4cfc-a588-134d893c2a12:false /run/../runs:false /space/../4306fa47-1990-4cfc-a588-134d893c2a12:false /space/../4306fa47-1990-4cfc-a588-134d893c2a12/canary.txt:false /space/../functions:false /space/../runs:false /spaces:false /srv/nbsandbox:false]; / has 20 entries, none is space B | ✅ pass | paths exist map[/home/runner/.local/share/nbsandbox/spaces:false /home/runner/.local/share/nbsandbox/spaces/6bd8af71-95fe-4c88-931b-4cc1633fa947/files:false /home/runner/.local/share/nbsandbox/spaces/edafb6ac-68e5-4180-aa34-5e07bf12a9b0/functions:false /home/runner/.local/share/nbsandbox/spaces/edafb6ac-68e5-4180-aa34-5e07bf12a9b0/runs:false /run/../6bd8af71-95fe-4c88-931b-4cc1633fa947:false /run/../runs:false /space/../6bd8af71-95fe-4c88-931b-4cc1633fa947:false /space/../6bd8af71-95fe-4c88-931b-4cc1633fa947/canary.txt:false /space/../functions:false /space/../runs:false /spaces:false /srv/nbsandbox:false]; / has 22 entries, none is space B | ✅ pass |
| canary-search | search the container filesystem for bob's canary | not found | found in []; 575 files scanned in 0.1s | ✅ pass | found in []; 577 files scanned in 0s | ✅ pass |
| proc-list | list /proc | only alice's own processes | 2 processes visible, foreign [] | ✅ pass | 2 processes visible, foreign [] | ✅ pass |
| memory-secret | search readable /proc/*/environ, cmdline and mem for the holder's in-memory secret | not found | hits []; readable map[cmdline:2 environ:2 mem:1]; 1.61251328e+08 bytes of memory searched | ✅ pass | hits []; readable map[cmdline:2 environ:2 mem:1]; 2.36867584e+08 bytes of memory searched | ✅ pass |
| shared-memory | read /dev/shm | holder's shared-memory secret absent | /dev/shm entries [] | ✅ pass | /dev/shm entries [] | ✅ pass |
| socket-env-network | look for the Docker socket; list environment variables; try the network | no socket; only allowlisted variables; no network (on Kubernetes: nothing reachable, the API server, worker, kubelet and metadata service included) | sockets []; forbidden env []; up interfaces besides lo [eth0]; connections map[1.1.1.1:443:TimeoutError: timed out 10.244.0.195:8080:TimeoutError: timed out 10.96.0.1:443:TimeoutError: timed out 169.254.169.254:80:TimeoutError: timed out 172.18.0.2:10250:TimeoutError: timed out 8.8.8.8:53:TimeoutError: timed out host.docker.internal:8080:gaierror: [Errno -3] Temporary failure in name resolution nbsandboxd:8080:gaierror: [Errno -3] Temporary failure in name resolution socket-proxy:2375:gaierror: [Errno -3] Temporary failure in name resolution] | ✅ pass | sockets []; forbidden env []; up interfaces besides lo [eth0]; connections map[1.1.1.1:443:TimeoutError: timed out 10.244.0.78:8080:TimeoutError: timed out 10.96.0.1:443:TimeoutError: timed out 169.254.169.254:80:TimeoutError: timed out 172.18.0.2:10250:TimeoutError: timed out 8.8.8.8:53:TimeoutError: timed out host.docker.internal:8080:gaierror: [Errno -3] Temporary failure in name resolution nbsandboxd:8080:gaierror: [Errno -3] Temporary failure in name resolution socket-proxy:2375:gaierror: [Errno -3] Temporary failure in name resolution] | ✅ pass |
| privileges | id, CapEff, write to /usr, chmod u+s | uid 1000; CapEff 0; NoNewPrivs; writes fail; setuid ineffective (nosuid /tmp, or no-new-privileges on Kubernetes) | uid 1000, CapEff 0000000000000000, NoNewPrivs 1, Seccomp 0, write /usr: PermissionError, chmod u+s: set on /tmp (rw,trans=fd,rfdno=49,wfdno=49,aname=/,dfltuid=4294967294,dfltgid=4294967294,dcache=1000,cache=remote_revalidating,disable_fifo_open,directfs), AppArmor: [Errno 2] No such file or directory: '/proc/self/attr/current' | ✅ pass | uid 1000, CapEff 0000000000000000, NoNewPrivs 1, Seccomp 2, write /usr: OSError, chmod u+s: set on /tmp (rw,relatime,size=262144k,inode64,noswap), AppArmor: unconfined | ✅ pass |
| cross-user-api | bob streams and downloads alice's run log, outputs and artifacts | 404 on every call | 404 404 404 404 404 404 404 404 | ✅ pass | 404 404 404 404 404 404 404 404 | ✅ pass |
| neighbour-unaffected | the holder's run after the probes | completed normally, secrets intact | succeeded | ✅ pass | succeeded | ✅ pass |

## Resources

| Check | Attempted | Expected | Observed (gvisor) | gvisor | Observed (runc) | runc |
|---|---|---|---|---|---|---|
| memory-limit | alice allocates beyond the memory limit while bob runs a normal notebook | alice oom (not kernel_died/failed) with peak memory reported (not on Kubernetes, which kills the whole container); bob succeeded | alice oom (the notebook ran out of memory: peak 1.0 GiB of the 1.0 GiB limit (files in /tmp count toward it)), peak 1024 MiB; bob succeeded | ✅ pass | alice oom (the notebook ran out of memory (limit 1.0 GiB)), peak 1024 MiB; bob succeeded | ✅ pass |
| cpu-fairness | alice spins all cores while bob runs a fixed CPU workload | bob within 1.5× of his solo baseline | bob alone 3.28s, with the hog 4.19s: 1.28× (runs alone [3.278 3.522 3.273], contended [3.952 4.194 4.685]; report-only on this platform) | ✅ pass | bob alone 3.75s, with the hog 4.63s: 1.23× (runs alone [3.556 3.755 3.802], contended [4.748 4.484 4.632]; report-only on this platform) | ✅ pass |
| process-limit | alice spawns processes until failure | failed/resource_pids (on Kubernetes: failed on the refused fork); bob unaffected; service responsive | alice failed/resource_pids; bob succeeded; slowest /readyz 3ms (0 not ready) | ✅ pass | alice failed/execution_error; bob succeeded; slowest /readyz 3ms (0 not ready) | ✅ pass |
| time-limit | alice runs an infinite loop | timeout; container removed; partial output kept | timeout after 151s; partial true; containers left [] | ✅ pass | timeout after 152s; partial true; containers left [] | ✅ pass |
| tmp-limit | alice fills /tmp | failed/resource_tmp; others unaffected | alice failed/resource_tmp; bob succeeded | ✅ pass | alice failed/resource_tmp; bob succeeded | ✅ pass |
| endless-output | alice prints endlessly (200-byte lines for 25 s) | log stops at the cap with a notice; Docker's own log within its cap; host disk unaffected | succeeded; log.txt 10485674 bytes (cap 10485760), last line "[nbsandbox] log truncated at 10485582 by"; Docker's log not measured here (kubelet or the host rotates container logs); output.ipynb 4679780 bytes (stored output capped at 5 MiB) | ✅ pass | succeeded; log.txt 10485673 bytes (cap 10485760), last line "[nbsandbox] log truncated at 10485581 by"; Docker's log not measured here (kubelet or the host rotates container logs); output.ipynb 3988758 bytes (stored output capped at 5 MiB) | ✅ pass |

## Cancellation and errors

| Check | Attempted | Expected | Observed (gvisor) | gvisor | Observed (runc) | runc |
|---|---|---|---|---|---|---|
| cancel-running | cancel during a long cell | container gone within grace + 15s; cancelled; partial output | cancelled in 1.0s, partial true, container gone true | ✅ pass | cancelled in 1.0s, partial true, container gone true | ✅ pass |
| cancel-queued | cancel a queued run | never starts; cancelled | was queued true; cancelled; started false | ✅ pass | was queued true; cancelled; started false | ✅ pass |
| cancel-at-completion | cancel right as the last cell finishes (5 rounds; 200 in the unit test) | exactly one terminal status, the same in API, result.json and log | map[cancelled:5], consistent true | ✅ pass | map[cancelled:5], consistent true | ✅ pass |
| caller-disconnect | caller disconnects right after submitting | run completes normally | succeeded | ✅ pass | succeeded | ✅ pass |
| kernel-crash | crash in a C extension (ctypes) | kernel_died | kernel_died | ✅ pass | kernel_died | ✅ pass |
| cell-exception | exception raised in cell 3 | failed, cell 3, ValueError | failed, cell 3, ValueError: boom in cell 3 | ✅ pass | failed, cell 3, ValueError: boom in cell 3 | ✅ pass |
| render-failure | HTML rendering fails on a successful run | succeeded, rendered false | succeeded, rendered false | ✅ pass | succeeded, rendered false | ✅ pass |

## Functional and lifecycle

| Check | Attempted | Expected | Observed (gvisor) | gvisor | Observed (runc) | runc |
|---|---|---|---|---|---|---|
| parameters-and-artifacts | run with parameters on an uploaded CSV | succeeded; parameters applied; summary.csv artifact | succeeded; artifact "region,value\njakarta,30\n" | ✅ pass | succeeded; artifact "region,value\njakarta,30\n" | ✅ pass |
| runtime-info | GET /v1/runtime | image id and package list match the image | id match true, 69 packages, 0 mismatches | ✅ pass | id match true, 69 packages, 0 mismatches | ✅ pass |
| queue-concurrency | three spaces start runs at once, limit 2 | two run, one queued at position 1, all complete | 2 running, 1 queued (position 1), 3 succeeded | ✅ pass | 2 running, 1 queued (position 1), 3 succeeded | ✅ pass |
| delete-space | delete a space with a finished run | space, run and artifacts return 404 | delete 204, then [404 404 404] | ✅ pass | delete 204, then [404 404 404] | ✅ pass |

## Log and event streaming

| Check | Attempted | Expected | Observed (gvisor) | gvisor | Observed (runc) | runc |
|---|---|---|---|---|---|---|
| live-lines | notebook prints one line a second for 10 s | first line long before the run ends; order preserved | first line 11.1s before done; lines [0 1 2 3 4 5 6 7 8 9]; {"last_event_id":17,"status":"succeeded"} | ✅ pass | first line 11.4s before done; lines [0 1 2 3 4 5 6 7 8 9]; {"last_event_id":17,"status":"succeeded"} | ✅ pass |
| resume | disconnect after 5 lines, reconnect with Last-Event-ID | remaining lines, no gaps or duplicates | 17 lines total; sequence true; matches log.txt true | ✅ pass | 17 lines total; sequence true; matches log.txt true | ✅ pass |
| join-mid-run | connect mid-run (after line 4) | earlier lines replayed, then live lines, no gaps | 17 lines; sequence true; matches log.txt true | ✅ pass | 17 lines; sequence true; matches log.txt true | ✅ pass |
| replay-after-finish | connect after the run finished | full log replayed, then done with the final status | 15 lines in 0.00s; done failed; matches log.txt true | ✅ pass | 15 lines in 0.00s; done failed; matches log.txt true | ✅ pass |
| slow-viewer | five viewers on a 20,000-line run, one stops reading | run duration unaffected; fast viewers complete; the slow viewer isn't dropped and gets every line once it reads again | run 4.0s (alone 4.0s); fast viewers complete true ([20007 20007 20007 20007]); slow dropped false, complete true | ✅ pass | run 4.0s (alone 4.0s); fast viewers complete true ([20007 20007 20007 20007]); slow dropped false, complete true | ✅ pass |
| streamed-output | a notebook emits five items a second apart; follow the result stream, then read the result as JSON | items arrive live (ids 1 to 5, the last at least 2 s after the first), then the result with the same list; JSON gives the same list; the status stream has only status | events [item progress item progress item progress item progress item progress result]; first item 6.0s before the end, last 4.0s after the first; JSON output [{"step":0},{"step":1},{"step":2},{"step":3},{"step":4}]; 3 status events | ✅ pass | events [item progress item progress item progress item progress item progress result]; first item 6.2s before the end, last 4.0s after the first; JSON output [{"step":0},{"step":1},{"step":2},{"step":3},{"step":4}]; 3 status events | ✅ pass |

## Notebook functions

| Check | Attempted | Expected | Observed (gvisor) | gvisor | Observed (runc) | runc |
|---|---|---|---|---|---|---|
| function-invoke | invoke a published notebook with nested JSON and wait | succeeded; output computed from the input and the bundle | 200 succeeded; model "m1", forecast [1.5 3 4.5 6], echo {"big":123456789012345678901234567890,"cfg":{"include":["sales",null],"months":4},"region":"bandung"} | ✅ pass | 200 succeeded; model "m1", forecast [1.5 3 4.5 6], echo {"big":123456789012345678901234567890,"cfg":{"include":["sales",null],"months":4},"region":"bandung"} | ✅ pass |
| function-immutable | edit the model file, publish v2, call latest and v1 | latest sees m2; v1 still sees m1 | latest v2 "m2"; v1 "m1" | ✅ pass | latest v2 "m2"; v1 "m1" | ✅ pass |
| function-errors | invalid input; a notebook that raises; an output that breaks the contract | 400 invalid_parameters, no run; failed with cell and exception; failed output_invalid | input 400 invalid_parameters (2 problems, 0 runs); raise failed/&{execution_error notebook false cell 1 raised ValueError: asked to fail 0x296a12e0a548 ValueError}; output failed/&{output_invalid notebook false the output doesn't match the notebook's output contract: (the whole value): missing properties 'region', 'forecast' <nil> } | ✅ pass | input 400 invalid_parameters (2 problems, 0 runs); raise failed/&{execution_error notebook false cell 1 raised ValueError: asked to fail 0x34d09c06ca48 ValueError}; output failed/&{output_invalid notebook false the output doesn't match the notebook's output contract: (the whole value): missing properties 'region', 'forecast' <nil> } | ✅ pass |
| read-only-invocation | a function writes into /space; the same notebook as a normal run | invocation fails on the read-only file system; the normal run writes the file | invocation failed (&{execution_error notebook false cell 0 raised OSError: [Errno 30] Read-only file system: 'written.txt' 0x296a12a70608 OSError}); run succeeded; file listed true | ✅ pass | invocation failed (&{execution_error notebook false cell 0 raised OSError: [Errno 30] Read-only file system: 'written.txt' 0x34d09c06d5c8 OSError}); run succeeded; file listed true | ✅ pass |
| parallel-invocations | two invocations of one space at once | both succeed, and their runs overlap in time | succeeded, succeeded in 5.8s; overlap true | ✅ pass | succeeded, succeeded in 6.0s; overlap true | ✅ pass |
| status-stream | follow the space's status stream, drop the connection after one event, resume | queued, running, succeeded: each once, none lost | queued,running,succeeded | ✅ pass | queued,running,succeeded | ✅ pass |
| start-latency | three small invocations, request to result; where the time went (median seconds per phase) | recorded (target: under 1 s of start-up; see the roadmap) | [4.895543462s 4.970698008s 4.951140597s]; queued 0.00, prepare 0.00, container_start 0.14, runner_start 1.85, kernel_start 1.51, execute 0.58, render 0.09, exit 0.00, teardown 0.79 | ✅ pass | [4.956355714s 4.979021139s 4.826582982s]; queued 0.00, prepare 0.01, container_start 0.48, runner_start 1.48, kernel_start 1.40, execute 0.58, render 0.09, teardown 1.41 | ✅ pass |

## Notes

- **cpu-fairness** gates only on the Linux reference host (`NBSANDBOX_CPU_FAIRNESS_GATE=1`); elsewhere the ratio is recorded and the row passes if the test ran.
- **cancel-at-completion** runs 5 rounds with real containers; the 200-round race with exact timing control is a unit test (`internal/runs` TestCancelCompletionRace).
- **Retention** is a unit test with a fake clock (the minimum retention is one hour), so it has no row here.
- Known limitations are in [security/threat-model.md](security/threat-model.md).
