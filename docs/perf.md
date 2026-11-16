# Performance

Measured numbers behind the capacity guidance in [operations.md](operations.md#capacity) and the tolerances of the tests. Most come from the integration suite, so `make report`, `make report-linux` and `make report-k8s` measure them again; the isolation reports have each run's figures. gVisor's costs are in [gvisor.md](gvisor.md#results-2026-11-08).

## Starting a run

A small function call, timed from the request to the result (`start-latency` in the reports, 2026-11-15, with 0.1.2's in-process rendering):

| Platform | Request → result |
|---|---|
| Docker Desktop (macOS, Apple silicon) | 2.5 s |
| Linux with Docker Engine, `runc` (GitHub's 4-CPU runner, weekly) | 3.4 s |
| Linux with Docker Engine, gVisor (same runner) | 3.4 s |
| Kubernetes in kind, one node (Cilium) | 5–6 s: the Pod is scheduled and the files are copied in through the Kubernetes API; not representative of a real cluster |

Almost all of it is starting a fresh container and Python kernel; the notebook itself takes milliseconds. The "Fast starts" item on the [roadmap](../ROADMAP.md) works against this (target: under 1 s).

**Where the time goes.** Every run records its phases (`usage.phases_seconds` in its result, the `nbsandbox_run_phase_seconds` metric). The same small call, median of three (2026-11-15), before and after rendering moved into the runner (it used to start a new Python for `nbconvert`):

| Phase | Docker Desktop, before | Docker Desktop, after | Kubernetes in kind, before |
|---|---|---|---|
| Container start | 0.04 s | 0.04 s | under 0.5 s |
| Runner start (Python, the wrapper's imports; on Kubernetes also the files copied in) | 0.54 s | 0.57 s | 2.0 s |
| Kernel start, to the first cell | 0.77 s | 1.13 s | 0.90 s |
| The notebook's cells | 0.39 s | 0.49 s | 0.62 s |
| Rendering `output.html` | 0.99 s | 0.08 s | 1.17 s |
| Teardown (outputs copied out, container removed) | 0.04 s | 0.04 s | 2.0 s |
| Request → result | 2.9 s | 2.5 s | 5.1–5.8 s |

The renderer now warms up in the background while the kernel starts; a run has one CPU, so the kernel's start takes a little longer, and the call is 0.4 s faster overall. What remains is mostly CPU work on the run's one CPU: the Jupyter client libraries (about 0.6 s) and the kernel's own imports (about 0.6 s). Starting the kernel earlier doesn't help on one CPU (measured: it made calls slower), and dropping papermill would save at most 0.13 s; going well below this takes pre-started containers (the [roadmap](../ROADMAP.md)'s "fast starts" item). On Kubernetes, copying files through the API adds about 4 s.

## Several runs at once

- **Two calls of one function at once** (`parallel-invocations`): 3.5 s on Docker Desktop, 3.8 s on Linux with `runc`, 6.2 s with gVisor on a 2-CPU VM. The calls overlap; under gVisor on a small host they slow each other more.
- **CPU fairness** (`cpu-fairness`): a run with a fixed CPU workload, alone and while another space's notebook spins every core, both capped at 1 CPU. The test allows 1.5× and gates on Linux. Measured: 0.97× and 1.11× on Linux (`runc`, gVisor), 1.22× on Docker Desktop, 1.12× and 1.37× on kind.

## Live logs

- **Log hub fan-out** (`go test ./internal/logs/ -run '^$' -bench HubFanOut -benchtime 200000x`): one 64-byte line appended and delivered to N viewers reading as fast as they can, without writing log.txt. Apple silicon (8 cores), Go 1.27: 201 ns a line with no viewers (about 5 M lines/s), 283 ns with one, 1.7 µs with 10 (about 590 k lines/s), 16 µs with 100 (about 62 k lines/s). One allocation per line. A notebook printing as fast as Python can is about 5,000 lines/s, orders of magnitude below.
- **Viewers under load** (`slow-viewer`): a 20,000-line burst followed by five viewers, one of which stops reading. The run takes as long as with nobody watching (3.1 s and 3.1 s on Docker Desktop; 3.3 s and 2.8 s on Linux), the four others get every line, and the stalled one isn't disconnected: it gets everything once it reads again.

## Package sets

A space's own packages ([packages.md](packages.md)), measured on Docker Desktop (macOS, Apple silicon) on 2026-11-15. Polling every 50 ms, from the request to the result, against PyPI:

| | Time |
|---|---|
| Build `humanize==4.16.0`, `tabulate==0.9.0` (691 KiB set), from "build now" to ready | 5.1 s |
| Build `polars` (polars and its runtime, 167 MiB set) | 10.6 s |
| A run without a set, median of 7 | 3.00 s |
| The same with the 691 KiB set mounted (importing both packages), median of 7 | 3.04 s |
| A run importing polars from its set: the first after the build, then later ones | 5.7 s, then 3.8 s |

- **Steady state:** a mounted set costs nothing measurable. The difference is importing the packages, as from the image; polars takes about 0.8 s to import.
- **The first run after a large set is built** takes longer, probably the file cache warming up through Docker Desktop's shared file system.
- **Builds are cached:** the same `requirements.txt` builds once for every space.
- **Not measured yet:** the first use on another worker downloads the set from object storage and unpacks it; this test stack has one worker.

## A run's footprint

The smoke notebook on Docker Desktop: about 3.7 s of container time, about 300 MB peak memory, 3 s of CPU. The tour's sales report (pandas, 4 months of data): 4.7 s, 124 MB peak, 4.3 s CPU ([tour.md](tour.md)).

## Soak tests

A soak runs a mix of notebooks for a long time (succeeding, failing, crashing the kernel, streaming, cancelled) and fails on steady growth of goroutines, open files, memory, disk or leftover containers; cleanup timers are shortened so every cleanup path runs many times. `make soak` (45 minutes, weekly; a 12 h soak before a minor or major release, [releasing.md](releasing.md)), `make soak-cluster` and `make soak-k8s` sample every process.

| Date | Stack | Duration | Runs | Result |
|---|---|---|---|---|
| 2026-11-06/07 | One process, Docker Desktop | 13 h | 9,413, one at a time | nothing grew: at the end, idle at the first day's values (about 20 goroutines, 18 open files, 35 MiB), no containers left |
| 2026-11-08 | One process, Docker Desktop | 45 min, 4 spaces at once | 1,676 | passed: goroutines −8.0/h, open files −3.0/h, memory +0.8 MiB/h; every cleanup check passed |
| 2026-11-08 | Two API processes and two workers, Docker Compose | 45 min | 1,792 | passed: worst trends goroutines +0.4/h, open files +0.7/h, memory +1.3 MiB/h |
| 2026-11-08 | Kubernetes in kind (Cilium), two API processes and two workers | 45 min | 70 (run Pods reserve their memory, so two at a time) | passed: worst trends goroutines +4.1/h, open files +1.3/h, memory −34.6 MiB/h; few runs for a leak check, so rerun it on a real cluster |
| 2026-11-15/16 (v0.2.0) | One process, Docker Desktop (4 CPUs, 3 GB) | 12 h, 4 spaces at once | 25,060 | passed: worst trends goroutines +0.0/h, open files −0.0/h, memory −2.6 MiB/h; at the end 31 goroutines, 21 open files, 54 MiB, no containers left; stored data flat at 174 MiB (not judged: the test judges data only past the retention plus two cleanups) |
