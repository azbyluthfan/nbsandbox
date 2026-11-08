# nbsandbox Isolation Report: Kubernetes backend

Generated 2026-11-08 09:26 UTC by `make report-k8s` from the integration tests' findings. Each row is a test that **ran**; the observed column is what the test saw, not what it assumed.

## Summary

| Runtime | Platform | Passed | Failed | Required tests missing |
|---|---|---|---|---|
| gvisor | Kubernetes in kind on Docker Desktop, network policy by Cilium — **local cluster**: the controls work here; run the suite in each target cluster for its own report | 45 | 0 | none |
| runc | Kubernetes in kind on Docker Desktop, network policy by Cilium — **local cluster**: the controls work here; run the suite in each target cluster for its own report | 45 | 0 | none |

## Environment

**gvisor**

- Kubernetes: v1.32.2 (Kubernetes linux/arm64); the Docker underneath: "29.9.2" "Docker Desktop" "7.0.14-linuxkit" "2" 4 4094517248 "aarch64"
- Not applicable: docker-lost-mid-run (stops Docker Compose services)
- Runner image: `nbsandbox-runner:dev` `sha256:94adc3281e14cef04728d545e916703aaf17c48b4526f7bdbad1e8436a561ce1`, Python 3.13.18
- Limits per run: `{"concurrency":1,"cpus":1,"log_bytes":10485760,"max_file_bytes":104857600,"max_space_bytes":536870912,"memory_bytes":1073741824,"pids":256,"timeout_seconds":150,"tmp_bytes":268435456}`
- Facts recorded: 2026-11-08T07:38:28Z

**runc**

- Kubernetes: v1.32.2 (Kubernetes linux/arm64); the Docker underneath: "29.9.2" "Docker Desktop" "7.0.14-linuxkit" "2" 4 4094517248 "aarch64"
- Not applicable: docker-lost-mid-run (stops Docker Compose services)
- Runner image: `nbsandbox-runner:dev` `sha256:94adc3281e14cef04728d545e916703aaf17c48b4526f7bdbad1e8436a561ce1`, Python 3.13.18
- Limits per run: `{"concurrency":1,"cpus":1,"log_bytes":10485760,"max_file_bytes":104857600,"max_space_bytes":536870912,"memory_bytes":1073741824,"pids":256,"timeout_seconds":150,"tmp_bytes":268435456}`
- Facts recorded: 2026-11-08T08:51:01Z

## Isolation

| Check | Attempted | Expected | Observed (gvisor) | gvisor | Observed (runc) | runc |
|---|---|---|---|---|---|---|
| space-paths | read space B by absolute host path, by ../, and by listing / and /space/..; reach this space's other runs and published versions; list /run | none of those paths exist; /run holds only this run's folder | paths exist map[~/.local/share/nbsandbox/spaces:false ~/.local/share/nbsandbox/spaces/3b125aa2-0fa4-46bb-8d3a-84ccb3bd283e/files:false ~/.local/share/nbsandbox/spaces/6c15d99c-4705-4dca-a102-a9bf1a422065/functions:false ~/.local/share/nbsandbox/spaces/6c15d99c-4705-4dca-a102-a9bf1a422065/runs:false /run/../3b125aa2-0fa4-46bb-8d3a-84ccb3bd283e:false /run/../runs:false /space/../3b125aa2-0fa4-46bb-8d3a-84ccb3bd283e:false /space/../3b125aa2-0fa4-46bb-8d3a-84ccb3bd283e/canary.txt:false /space/../functions:false /space/../runs:false /spaces:false /srv/nbsandbox:false]; / has 19 entries, none is space B | ✅ pass | paths exist map[~/.local/share/nbsandbox/spaces:false ~/.local/share/nbsandbox/spaces/1122011a-a72c-4eb1-8c81-ac7fdc7951ae/files:false ~/.local/share/nbsandbox/spaces/68fa4007-1e79-45d0-858e-b25ce9ab35e3/functions:false ~/.local/share/nbsandbox/spaces/68fa4007-1e79-45d0-858e-b25ce9ab35e3/runs:false /run/../1122011a-a72c-4eb1-8c81-ac7fdc7951ae:false /run/../runs:false /space/../1122011a-a72c-4eb1-8c81-ac7fdc7951ae:false /space/../1122011a-a72c-4eb1-8c81-ac7fdc7951ae/canary.txt:false /space/../functions:false /space/../runs:false /spaces:false /srv/nbsandbox:false]; / has 20 entries, none is space B | ✅ pass |
| canary-search | search the container filesystem for bob's canary | not found | found in []; 575 files scanned in 0.2s | ✅ pass | found in []; 576 files scanned in 0.1s | ✅ pass |
| proc-list | list /proc | only alice's own processes | 2 processes visible, foreign [] | ✅ pass | 2 processes visible, foreign [] | ✅ pass |
| memory-secret | search readable /proc/*/environ, cmdline and mem for the holder's in-memory secret | not found | hits []; readable map[cmdline:2 environ:2 mem:1]; 1.58765056e+08 bytes of memory searched | ✅ pass | hits []; readable map[cmdline:2 environ:2 mem:2]; 2.8186624e+08 bytes of memory searched | ✅ pass |
| shared-memory | read /dev/shm | holder's shared-memory secret absent | /dev/shm entries [] | ✅ pass | /dev/shm entries [] | ✅ pass |
| socket-env-network | look for the Docker socket; list environment variables; try the network | no socket; only allowlisted variables; no network (on Kubernetes: nothing reachable, the API server, worker, kubelet and metadata service included) | sockets []; forbidden env []; up interfaces besides lo [eth0]; connections map[1.1.1.1:443:TimeoutError: timed out 10.244.0.109:8080:TimeoutError: timed out 10.96.0.1:443:TimeoutError: timed out 169.254.169.254:80:TimeoutError: timed out 172.18.0.2:10250:TimeoutError: timed out 8.8.8.8:53:TimeoutError: timed out host.docker.internal:8080:gaierror: [Errno -3] Temporary failure in name resolution nbsandboxd:8080:gaierror: [Errno -3] Temporary failure in name resolution socket-proxy:2375:gaierror: [Errno -3] Temporary failure in name resolution] | ✅ pass | sockets []; forbidden env []; up interfaces besides lo [eth0]; connections map[1.1.1.1:443:TimeoutError: timed out 10.244.0.122:8080:TimeoutError: timed out 10.96.0.1:443:TimeoutError: timed out 169.254.169.254:80:TimeoutError: timed out 172.18.0.2:10250:TimeoutError: timed out 8.8.8.8:53:TimeoutError: timed out host.docker.internal:8080:gaierror: [Errno -3] Temporary failure in name resolution nbsandboxd:8080:gaierror: [Errno -3] Temporary failure in name resolution socket-proxy:2375:gaierror: [Errno -3] Temporary failure in name resolution] | ✅ pass |
| privileges | id, CapEff, write to /usr, chmod u+s | uid 1000; CapEff 0; NoNewPrivs; writes fail; setuid ineffective (nosuid /tmp, or no-new-privileges on Kubernetes) | uid 1000, CapEff 0000000000000000, NoNewPrivs 1, Seccomp 0, write /usr: PermissionError, chmod u+s: set on /tmp (rw,trans=fd,rfdno=49,wfdno=49,aname=/,dfltuid=4294967294,dfltgid=4294967294,dcache=1000,cache=remote_revalidating,disable_fifo_open,directfs), AppArmor: [Errno 2] No such file or directory: '/proc/self/attr/current' | ✅ pass | uid 1000, CapEff 0000000000000000, NoNewPrivs 1, Seccomp 2, write /usr: OSError, chmod u+s: set on /tmp (rw,relatime,size=262144k,noswap), AppArmor: [Errno 22] Invalid argument | ✅ pass |
| cross-user-api | bob streams and downloads alice's run log, outputs and artifacts | 404 on every call | 404 404 404 404 404 404 404 404 | ✅ pass | 404 404 404 404 404 404 404 404 | ✅ pass |
| neighbour-unaffected | the holder's run after the probes | completed normally, secrets intact | succeeded | ✅ pass | succeeded | ✅ pass |

## Resources

| Check | Attempted | Expected | Observed (gvisor) | gvisor | Observed (runc) | runc |
|---|---|---|---|---|---|---|
| memory-limit | alice allocates beyond the memory limit while bob runs a normal notebook | alice oom (not kernel_died/failed) with peak memory reported (not on Kubernetes, which kills the whole container); bob succeeded | alice oom (the notebook ran out of memory (limit 1.0 GiB)), peak 1024 MiB; bob succeeded | ✅ pass | alice oom (the notebook ran out of memory (limit 1.0 GiB)), peak 1024 MiB; bob succeeded | ✅ pass |
| cpu-fairness | alice spins all cores while bob runs a fixed CPU workload | bob within 1.5× of his solo baseline | bob alone 5.45s, with the hog 7.49s: 1.37× (runs alone [5.142 5.453 5.678], contended [7.39 7.596 7.49]; report-only on this platform) | ✅ pass | bob alone 3.20s, with the hog 3.59s: 1.12× (runs alone [3.194 3.198 3.349], contended [3.592 3.646 3.342]; report-only on this platform) | ✅ pass |
| process-limit | alice spawns processes until failure | failed/resource_pids (on Kubernetes: failed on the refused fork); bob unaffected; service responsive | alice failed/resource_pids; bob succeeded; slowest /readyz 12ms | ✅ pass | alice failed/execution_error; bob succeeded; slowest /readyz 5ms | ✅ pass |
| time-limit | alice runs an infinite loop | timeout; container removed; partial output kept | timeout after 152s; partial true; containers left [] | ✅ pass | timeout after 161s; partial true; containers left [] | ✅ pass |
| tmp-limit | alice fills /tmp | failed/resource_tmp; others unaffected | alice failed/resource_tmp; bob succeeded | ✅ pass | alice failed/resource_tmp; bob succeeded | ✅ pass |
| endless-output | alice prints endlessly (200-byte lines for 25 s) | log stops at the cap with a notice; Docker's own log within its cap; host disk unaffected | succeeded; log.txt 10485678 bytes (cap 10485760), last line "[nbsandbox] log truncated at 10485586 by"; Docker's log 0 bytes; output.ipynb 5458395 bytes (stored output capped at 5 MiB) | ✅ pass | succeeded; log.txt 10485672 bytes (cap 10485760), last line "[nbsandbox] log truncated at 10485580 by"; Docker's log 0 bytes; output.ipynb 4459935 bytes (stored output capped at 5 MiB) | ✅ pass |

## Cancellation and errors

| Check | Attempted | Expected | Observed (gvisor) | gvisor | Observed (runc) | runc |
|---|---|---|---|---|---|---|
| cancel-running | cancel during a long cell | container gone within grace + 15s; cancelled; partial output | cancelled in 1.5s, partial true, container gone true | ✅ pass | cancelled in 11.8s, partial true, container gone true | ✅ pass |
| cancel-queued | cancel a queued run | never starts; cancelled | was queued true; cancelled; started false | ✅ pass | was queued true; cancelled; started false | ✅ pass |
| cancel-at-completion | cancel right as the last cell finishes (5 rounds; 200 in the unit test) | exactly one terminal status, the same in API, result.json and log | map[cancelled:5], consistent true | ✅ pass | map[cancelled:5], consistent true | ✅ pass |
| caller-disconnect | caller disconnects right after submitting | run completes normally | succeeded | ✅ pass | succeeded | ✅ pass |
| kernel-crash | crash in a C extension (ctypes) | kernel_died | kernel_died | ✅ pass | kernel_died | ✅ pass |
| cell-exception | exception raised in cell 3 | failed, cell 3, ValueError | failed &{CellIndex:0x770d5bd790f8 ExceptionType:ValueError Message:boom in cell 3} | ✅ pass | failed &{CellIndex:0x6358396ca368 ExceptionType:ValueError Message:boom in cell 3} | ✅ pass |
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
| live-lines | notebook prints one line a second for 10 s | first line long before the run ends; order preserved | first line 13.8s before done; lines [0 1 2 3 4 5 6 7 8 9]; {"last_event_id":17,"status":"succeeded"} | ✅ pass | first line 22.7s before done; lines [0 1 2 3 4 5 6 7 8 9]; {"last_event_id":17,"status":"succeeded"} | ✅ pass |
| resume | disconnect after 5 lines, reconnect with Last-Event-ID | remaining lines, no gaps or duplicates | 17 lines total; sequence true; matches log.txt true | ✅ pass | 17 lines total; sequence true; matches log.txt true | ✅ pass |
| join-mid-run | connect mid-run (after line 4) | earlier lines replayed, then live lines, no gaps | 17 lines; sequence true; matches log.txt true | ✅ pass | 17 lines; sequence true; matches log.txt true | ✅ pass |
| replay-after-finish | connect after the run finished | full log replayed, then done with the final status | 15 lines in 0.01s; done failed; matches log.txt true | ✅ pass | 15 lines in 0.01s; done failed; matches log.txt true | ✅ pass |
| slow-viewer | five viewers on a 20,000-line run, one stops reading | run duration unaffected; fast viewers complete; the slow viewer isn't dropped and gets every line once it reads again | run 9.0s (alone 9.0s); fast viewers complete true ([20007 20007 20007 20007]); slow dropped false, complete true | ✅ pass | run 23.0s (alone 22.0s); fast viewers complete true ([20007 20007 20007 20007]); slow dropped false, complete true | ✅ pass |
| streamed-output | a notebook emits three items a second apart; follow the result stream, then read the result as JSON | items arrive live (ids 1, 2, 3), then the result with the same list; JSON gives the same list; the status stream has only status | events [item progress item progress item progress result]; first item 5.3s before the end, last 5.2s after the first; JSON output [{"step":0},{"step":1},{"step":2}]; 3 status events | ✅ pass | events [item progress item progress item progress result]; first item 16.0s before the end, last 2.0s after the first; JSON output [{"step":0},{"step":1},{"step":2}]; 3 status events | ✅ pass |

## Notebook functions

| Check | Attempted | Expected | Observed (gvisor) | gvisor | Observed (runc) | runc |
|---|---|---|---|---|---|---|
| function-invoke | invoke a published notebook with nested JSON and wait | succeeded; output computed from the input and the bundle | 200 succeeded; model "m1", forecast [1.5 3 4.5 6], echo {"big":123456789012345678901234567890,"cfg":{"include":["sales",null],"months":4},"region":"bandung"} | ✅ pass | 200 succeeded; model "m1", forecast [1.5 3 4.5 6], echo {"big":123456789012345678901234567890,"cfg":{"include":["sales",null],"months":4},"region":"bandung"} | ✅ pass |
| function-immutable | edit the model file, publish v2, call latest and v1 | latest sees m2; v1 still sees m1 | latest v2 "m2"; v1 "m1" | ✅ pass | latest v2 "m2"; v1 "m1" | ✅ pass |
| function-errors | invalid input; a notebook that raises; an output that breaks the contract | 400 invalid_parameters, no run; failed with cell and exception; failed output_invalid | input 400 invalid_parameters (2 problems, 0 runs); raise failed/&{execution_error notebook false cell 1 raised ValueError: asked to fail 0x770d5ab23e88 ValueError}; output failed/&{output_invalid notebook false the output doesn't match the notebook's output contract: (the whole value): missing properties 'region', 'forecast' <nil> } | ✅ pass | input 400 invalid_parameters (2 problems, 0 runs); raise failed/&{execution_error notebook false cell 1 raised ValueError: asked to fail 0x6358396cb160 ValueError}; output failed/&{output_invalid notebook false the output doesn't match the notebook's output contract: (the whole value): missing properties 'region', 'forecast' <nil> } | ✅ pass |
| read-only-invocation | a function writes into /space; the same notebook as a normal run | invocation fails on the read-only file system; the normal run writes the file | invocation failed (&{execution_error notebook false cell 0 raised OSError: [Errno 30] Read-only file system: 'written.txt' 0x770d5acb4888 OSError}); run succeeded; file listed true | ✅ pass | invocation failed (&{execution_error notebook false cell 0 raised OSError: [Errno 30] Read-only file system: 'written.txt' 0x6358394f4ba8 OSError}); run succeeded; file listed true | ✅ pass |
| parallel-invocations | two invocations of one space at once | both succeed, and their runs overlap in time | succeeded, succeeded in 10.4s; overlap true | ✅ pass | succeeded, succeeded in 27.3s; overlap true | ✅ pass |
| status-stream | follow the space's status stream, drop the connection after one event, resume | queued, running, succeeded: each once, none lost | queued,running,succeeded | ✅ pass | queued,running,succeeded | ✅ pass |
| start-latency | three small invocations, request to result | recorded (target: under 1 s of start-up; see the roadmap) | [7.93855025s 8.298994125s 7.940699125s] | ✅ pass | [32.695944125s 31.930437208s 31.970200375s] | ✅ pass |

## Notes

- **cpu-fairness** gates only on the Linux reference host (`NBSANDBOX_CPU_FAIRNESS_GATE=1`); elsewhere the ratio is recorded and the row passes if the test ran.
- **cancel-at-completion** runs 5 rounds with real containers; the 200-round race with exact timing control is a unit test (`internal/runs` TestCancelCompletionRace).
- **Retention** is a unit test with a fake clock (the minimum retention is one hour), so it has no row here.
- Known limitations are in [security/threat-model.md](security/threat-model.md).
