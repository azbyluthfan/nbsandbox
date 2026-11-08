# Performance Notes

Measured numbers that capacity guidance and test tolerances rely on. Add a row whenever something is re-measured, with the date and platform.

## Log hub fan-out

`go test ./internal/logs/ -run '^$' -bench HubFanOut -benchtime 200000x`: one 64-byte line appended and delivered to N viewers that read as fast as they can. Writing to log.txt is excluded (a discarding writer).

| Date | Platform | 0 viewers | 1 viewer | 10 viewers | 100 viewers |
|---|---|---|---|---|---|
| 2026-11-06 | macOS, Apple silicon (8 cores), Go 1.27.3 | 201 ns/line (~5.0 M lines/s) | 283 ns (~3.5 M/s) | 1.7 µs (~590 k/s) | 16 µs (~62 k/s) |

One allocation per line (169 B) regardless of viewers. A notebook printing as fast as Python can (the 20,000-line burst workload, about 4,700 lines/s) is two to three orders of magnitude below these limits.

## Streaming under load (`slow-viewer` integration test)

| Date | Platform | Burst run alone | Same run with 5 viewers (one stalled) | Notes |
|---|---|---|---|---|
| 2026-11-06 | Docker Desktop (macOS), dev stack | 4.3 s | 6.4 s | Within the test's tolerance (1.5× + 2 s). The run's output reaches Docker's log driver whether or not anyone reads it, so the hub can't block the run; the extra time is most likely CPU contention in Docker Desktop's VM while nbsandboxd and Docker Desktop's port forwarding move about 20 MB of stream data. **Re-measure on the Linux reference host.** |

## Runs (smoke notebook)

| Date | Platform | Smoke notebook end to end | Peak memory | CPU |
|---|---|---|---|---|
| 2026-11-06 | Docker Desktop (macOS) | about 3.7 s container time, 4.9 s submit to finished | about 300 MB | about 3.0 s |

## Function call latency (`start-latency` integration test)

Three small function calls in a row, timed from the request to the result (`?wait=`), on Docker Desktop with the dev stack, 2026-11-07:

| Call | Request → result |
|---|---|
| 1 | 4.28 s |
| 2 | 4.27 s |
| 3 | 4.32 s |

**Baseline: about 4.3 s**, almost all of it starting a fresh container and Python kernel (the notebook itself takes milliseconds). This is the number the "Fast starts" roadmap item works against (target: under 1 s of start-up); measure again on Linux, where container start is usually faster than on Docker Desktop.

## CPU fairness (`cpu-fairness` calibration)

`cpu_fixed.ipynb` (30 M loop iterations, single-threaded) run three times alone, then three times while another space's `cpu_hog.ipynb` starts two busy processes per core. Both runs are capped at 1 CPU.

| Date | Platform | Alone (median) | With the hog (median) | Ratio | Gate |
|---|---|---|---|---|---|
| 2026-11-06 | Docker Desktop (macOS), 8 vCPU VM | 5.17 s | 5.44 s | 1.05× | report-only (non-reference) |

The 1.5× tolerance holds with a wide margin here. It gates only on the Linux reference host (`NBSANDBOX_CPU_FAIRNESS_GATE=1`); GitHub's shared runners record it without gating. Re-measure on the reference host and adjust the tolerance with those numbers.

## Soak (first pass)

| Date | Platform | Duration | Runs | Goroutines | Open files | RSS | Leftover containers |
|---|---|---|---|---|---|---|---|
| 2026-11-06 | Docker Desktop, dev stack | 4 min | 40 (mixed: success, failure, kernel crash, streamed and cancelled) | 21 → 20 | 18 → 18 | 25 → 29 MiB | 0 |
| 2026-11-06/07 | Docker Desktop, dev stack | 13 h (stopped by choice) | 9,413: 3,766 succeeded, 1,883 failed, 1,882 kernel crashes, 1,882 cancelled after a streamed log | 26 at 6 h → 20 idle at the end | 21 at 6 h → 18 idle | ≈ 35 MiB at 6 h → 35 MiB | 0 |

**The 13-hour soak is the baseline** (22:58 on 2026-11-06 to 12:02 on 2026-11-07, about 720 runs an hour, one at a time). Nothing grew: at the end, with nothing running, goroutines, open files and memory were back at the idle values seen on the first day; no run container was left; `/readyz` was 200; the log had no errors and one warning (the expected notice that the dev stack's web UI listens beyond loopback inside its container). It was stopped after 13 hours instead of 24, so the test didn't print its own verdict; the numbers above were read from `/metrics` and Docker afterwards. Not exercised: the first pruning of status events (24 h) and of idempotency keys (24 h).

**First `make soak`, 2026-11-07** (45 min, 4 spaces at once, cleanup timers at 10 min / every 2 min, Docker Desktop): **passed.** 1,522 runs (about 2,000 an hour, 2.8 times the baseline's rate). Per-hour trends after the first quarter: goroutines −3.6, open files −2.4, memory −0.4 MiB; the data folder levelled off at about 90 MiB once cleanup started (+19.5 MiB/h over the settled samples, within the 50 MiB/h limit). End: 23 goroutines, 22 open files, 36 MiB memory, no run containers. Cleanup: the oldest finished run left was 11.8 minutes old (limit 15), resuming from event 1 reported `pruned`, and the idempotency key expired after its window. Compared with the 13-hour baseline: same idle values (about 20–25 goroutines, 18–22 files, 35 MiB), at a higher run rate, with every cleanup path exercised.

**From now on, the faster soak** replaces the 24-hour one (to build after this baseline): several spaces running at once up to the concurrency limit, metrics sampled every minute with a fitted trend (fail on steady growth rather than comparing two snapshots), and cleanup timers shortened for the test (event and idempotency retention 10 min, cleanup every 2 min) so every cleanup path runs many times. Target: 30–60 min nightly, 3–4 h before a release, at least 20,000 runs and 10 cleanup cycles. Its results are compared with this baseline.

### Soaks on the cluster stacks

`make soak-cluster` and `make soak-k8s` run the same soak through two API processes and two workers. Each process's `/metrics` is sampled on its own (through the load balancer's container, or the Kubernetes API's Pod proxy), and the worst slope of each figure is judged.

**Kubernetes (kind with Cilium, Docker Desktop, 4 GB), 2026-11-08: passed.** 45 min, 4 spaces, 70 runs: run Pods reserve their memory, so two run at a time, and each takes about 25 s to start. Worst per-hour trends: goroutines +4.1 (a worker), open files +1.3, memory −34.6 MiB. End: 16–21 goroutines, 13–15 open files and 34–44 MiB per process; no run Pods left. Cleanup: the oldest finished run was 10 minutes old (limit 15), resuming from event 1 reported `pruned`, the idempotency key expired. Few runs for a leak check: rerun it on a real cluster with more runs at once.

**Cluster stack (Docker Compose: two API processes, two workers with 2 runs each, Docker Desktop), 2026-11-08: passed.** 45 min, 4 spaces, 1,792 runs. Worst per-hour trends: goroutines +0.4, open files +0.7, memory +1.3 MiB (API processes); the shared scratch folder stayed empty between runs (−0.1 MiB/h). End: 18 goroutines and 55 MiB per API process, 25–27 goroutines and 54–56 MiB per worker; no run containers left. Cleanup: the oldest finished run was 11.8 minutes old (limit 15), resuming from event 1 reported `pruned`, the idempotency key expired.

**Single node again, 2026-11-08 (after object storage, roles and the Kubernetes work): passed.** 45 min, 4 spaces, 1,676 runs. Per-hour trends: goroutines −8.0, open files −3.0, memory +0.8 MiB; the data folder held about 1 MiB (contents are in object storage now). End: 29 goroutines, 19 open files, 59 MiB; no run containers. Cleanup checks all passed (oldest finished run 11.5 minutes old, limit 15).
