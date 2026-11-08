# gVisor (`runsc`)

gVisor is an optional runtime for the Docker backend that isolates runs more strongly. With the default runtime (`runc`), a notebook's system calls go to the host's Linux kernel, and Docker's controls (no network, no capabilities, seccomp, a read-only root, limits) restrict what they can do. With gVisor, they go to gVisor's own kernel, the Sentry, which runs as an ordinary process under a strict seccomp filter of its own. A bug in the host kernel is then much harder to reach from a notebook.

**When to use it:** whenever the people running notebooks are less trusted than your own team, and before letting spaces bring their own runner images (see the roadmap). The cost is about a second more per run and slower small-file work (below); computation runs at the same speed.

**How to switch it on:** set `docker.runtime: runsc` in `settings.yaml` (or `NBSANDBOX_DOCKER_RUNTIME=runsc`). It applies to every run on that server: a request can't choose it, and there is never a fallback to `runc`. If `runsc` is missing, runs fail to start with a clear error. On Kubernetes, use a RuntimeClass instead (`runs.runtimeClass`, docs/kubernetes.md).

## Install on the Docker host

gVisor needs Linux with Docker Engine (x86_64 or arm64). Docker Desktop is not a target. The default platform (systrap) needs no KVM, so a cloud VM works. The tested release is in `deploy/gvisor.version`.

1. Install `runsc` from gVisor's apt repository or release binaries. Follow gVisor's installation guide, and pin the version in `deploy/gvisor.version`.
2. Register it with Docker, and restart Docker:

   ```bash
   sudo runsc install        # adds "runsc" to /etc/docker/daemon.json
   sudo systemctl restart docker
   ```

3. Check it:

   ```bash
   docker run --rm --runtime runsc alpine dmesg | head -1   # "Starting gVisor..."
   ```

4. Set `docker.runtime: runsc` and restart nbsandboxd. `GET /v1/runtime` then reports the runtime.

## Results (2026-11-08)

`make report-linux` ran the whole integration suite on Linux with Docker Engine, once with `runc` and once with `runsc`: **49 of 49 checks pass with each** ([isolation-report-linux.md](isolation-report-linux.md)). Setup:
- Ubuntu 26.04, Docker 29.9, gVisor 20261102.0;
- a Lima VM with 2 CPUs and 2 GB, on an Apple-silicon Mac;
- runs limited to 512 MiB, 1 CPU and 256 processes.

**What it costs**, measured on the same VM, the same runner image and the same limits (best of several runs):

| Work | runc | runsc | runsc / runc |
|---|---|---|---|
| Container create, start, exit, remove | 125 ms | 120 ms | same |
| Small function call, request to result | 3.3–3.5 s | 4.2–4.4 s | about +0.9 s |
| Two calls of one function at once | 3.5 s | 6.5 s | 1.9× |
| Start Python | 11 ms | 20 ms | 1.8× |
| `import pandas` | 0.46 s | 0.69 s | 1.5× |
| 2,000 small files: write, read, delete | 0.07 s | 0.51 s | 7.4× |
| 200 MiB file: write, read | 0.11 s | 0.18 s | 1.6× |
| `pandas.read_csv`, 500,000 rows | 0.09 s | 0.14 s | 1.6× |
| numpy 1500×1500 matrix product | 0.14 s | 0.17 s | 1.2× |
| Pure-Python loop | 0.25 s | 0.23 s | same |
| CPU fairness (a neighbour spinning every core) | 1.05× | 1.14× | both within 1.5× |

- **Computation runs at the same speed.**
- **Opening files costs more.** Every file system call goes through gVisor, so work with many small files is several times slower, and large reads and writes about 1.6×.
- **Start-up is slower.** Python and Jupyter read thousands of small files to start, which is where the extra second per run comes from. The container start itself costs the same.
- **On a busy small host,** two runs at once slow each other more under gVisor (1.9× for two calls vs about 1× with runc on 2 CPUs).

## How runs differ under gVisor

The suite found these differences. nbsandbox handles each of them, so results and statuses are the same under both runtimes.

| Difference | What nbsandbox does |
|---|---|
| A container created with Docker's `NetworkDisabled` has no loopback interface either, and Jupyter's ZeroMQ aborts the kernel at start. | Runs use network mode `none` only, which still means no network at all (loopback only). Under `runc` the two are the same. |
| Writes to the container's stdout or stderr can return after 64 KiB, and Python's unbuffered streams don't retry: most of a large log write was lost. | The runner writes its log through a stream that writes everything. |
| The cgroup files are gVisor's cgroup v1 emulation: current memory use, the limit, the process count and CPU time, but no peak memory, OOM or process-limit counters. | The runner falls back to the v1 files, and **samples** memory every 0.2 s for the peak; the status says it was sampled (`memory_peak_sampled`), so a spike shorter than that can be missed. |
| An out-of-memory kill takes the whole sandbox, runner included, so nothing reports the peak. | An OOM-killed run's peak is reported as its limit (it reached it; same on Kubernetes). |
| Docker's process limit (`--pids-limit`) counts gVisor's own threads, more than one per notebook process. About 45% of the limit was usable, a refused fork failed with "Cannot allocate memory", and sometimes gVisor itself was refused a thread and the run died with no status. | The notebook's limit becomes `RLIMIT_NPROC`, which gVisor enforces inside the sandbox (processes and threads of this run only), failing with "Resource temporarily unavailable" like `runc`. The host limit stays as a backstop with room for gVisor (4 × the limit + 256). Such a run is `failed/resource_pids`. |
| No `/sys/class/net`; no AppArmor attribute; the notebook's processes show `Seccomp: 0`. | Nothing to do. The isolation probe reads interfaces another way. The seccomp filter and the host's AppArmor profile apply to gVisor's Sentry on the host, not inside. |

## Reproduce

```bash
make report-linux                          # runc and runsc, writes docs/isolation-report-linux.md
RUNTIMES=runsc make report-linux           # one runtime (the other's findings are kept)
LIMA_VM=myvm RUN_MEMORY=2GiB make report-linux
```

`make report-linux` runs from a Mac, using a Lima VM named `gvisor` by default:
- **VM setup (once):** Ubuntu with Docker Engine and gVisor installed as above. The Mac's home folder is mounted read-only (Lima's default).
- **Build:** it builds the images and a Linux build of the suite on the Mac, then copies them into the VM.
- **Run:** it runs the suite once per runtime as root (`scripts/report-linux-vm.sh`), keeping its state in `/var/lib/nbsandbox-report` in the VM, then copies the findings back.
- **On a Linux server instead of a VM:** run `scripts/report-linux-vm.sh` there directly.

**Kernel note (2026-11-08):** on Ubuntu's kernel 7.0.0-42 the Postgres image's start-up helper (`gosu`) hung when changing user, so the stack in the VM never started; 7.0.0-38 works. If that happens, boot the previous kernel once: `sudo grub-reboot "Advanced options for Ubuntu>Ubuntu, with Linux 7.0.0-38-generic"`, then restart the VM.

Use about 4 GB of RAM for the VM if you can. With 2 GB, runs get 512 MiB, and the VM swaps under the heaviest tests, which makes timings noisier.
