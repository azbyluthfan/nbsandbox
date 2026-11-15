# Plan: custom packages per space

**Status (2026-11-15):** agreed: a package set, wheels only, off by default, Docker first (Kubernetes in a follow-up PR; until then it refuses runs that need packages, with a clear message). Built (Docker); Kubernetes is the next PR. A set also records the top-level modules it provides, so the dependency check counts them.

## Today

- Every run uses the same runner image: Python 3.13, a fixed set of packages pinned with hashes, and no pip (runs have no network, so it is removed).
- A space's `requirements.txt` is only *checked*: the dependency check warns (or, with `runs.dependency_check: reject`, refuses) when a notebook imports, or `requirements.txt` names, something the image doesn't have.
- To get another package today, an operator rebuilds the runner image for everyone.

## Goal

A space lists the extra packages it needs in `requirements.txt`, and its runs can import them:
- **No operator involvement** per space, and no effect on other spaces.
- **Runs keep no network:** packages are fetched by a separate build step, never by a run.
- **Reproducible:** a published function keeps exactly the package versions it was published with.
- **Cached:** the same requirements are built once, whichever space asks.

## Design

### A package set, not a derived image

The roadmap said "a derived image". I propose a **package set** instead: a directory of installed packages, mounted read-only into the run at `/opt/nbsandbox/packages` and put on `sys.path` by the runner.

| | Package set (proposed) | Derived image |
|---|---|---|
| Backends | Docker and Kubernetes the same way | Kubernetes needs an image registry and an in-cluster builder (BuildKit or kaniko) |
| Storage | the object store nbsandbox already has | a registry to run and secure |
| Runner image upgrade | the runner image stays the one image; sets are rebuilt against it | every derived image is rebuilt |
| Cost | a mount (Docker) or a copy into the pod (Kubernetes) | none at run time |

### The build

1. **Input:** the space's `requirements.txt`. Only plain requirement lines are accepted: `name[extras] specifier ; markers`, and `--hash`. Everything else is refused before anything runs:
   - index options (`-i`, `--index-url`, `--extra-index-url`, `-f`, `--find-links`), which could point pip at internal addresses such as cloud metadata;
   - `-e`, `-r`, `-c`, file paths, URLs and `name @ url`;
   - any other option.
2. **Key:** the SHA-256 of (the normalised requirements, the runner image's digest, the platform). The same key anywhere means the same set, built once and shared across spaces.
3. **Builder:** a **build container** from a new image, `nbsandbox-runner-builder`. It is the runner image's own build stage, before pip is removed, so it has exactly the runner's packages plus pip. It runs:
   ```
   pip install --only-binary=:all: --prefix /out -c /constraints.txt -r /requirements.txt --report /out/report.json
   ```
   - **`--only-binary=:all:`:** wheels only. Installing a wheel is unpacking it: no package code runs during the build. A package published only as source is refused, with a message naming it.
   - **`-c constraints.txt`:** the runner image's own versions. A requirement that conflicts with them (for example another pandas) fails with pip's explanation. The runner's packages can't be replaced, so nbsandbox's own runner keeps working.
   - **The report** records the exact resolved versions and hashes: the set's **lock**.
4. **Hardening:** the build container gets the same hardening as a run:
   - non-root, no capabilities, read-only root, seccomp, gVisor when configured;
   - limits: memory, CPU, `packages.build_timeout` (10 min), and `packages.max_size` (1 GiB unpacked);
   - **no space files mounted:** only `requirements.txt` and the constraints.

   Unlike a run, **it has network**, to reach the package index (`packages.index_url`; a private mirror is supported). Pip is the only code that runs in it.
5. **Allow-list (optional):** `packages.allowed` lists the distributions spaces may ask for. Empty means any. Checked before the build, against the names in `requirements.txt` and again against the resolved lock (dependencies included).
6. **Result:**
   - the set as a tarball in object storage (`packages/<key>.tar`);
   - a row in `package_sets`: key, status (building, ready, failed), lock, size, build log, created, last used;
   - workers unpack a set once into a local cache (`<data_dir>/packages/<key>`) and reuse it.

### When it is built

- **On a run:** if the space has a `requirements.txt` whose set isn't ready, the run waits for the build.
  - It stays `queued`, with a `packages` phase in its events and the build's output in its log.
  - A failed build fails the run: `error_kind: packages`, fault caller, with pip's message.
  - Concurrent runs needing the same set wait for one build.
- **Ahead of time:**
  - `POST /v1/spaces/{id}/packages` builds now;
  - `GET /v1/spaces/{id}/packages` shows the status, the lock, the size and the build log;
  - the web UI shows the same on the space's overview page.
- **On publish:** a function version records its set's **key and lock**, and publishing waits for the build. Invocations always use that set. After a runner image upgrade, the set is rebuilt from the **lock** (the exact versions), not from the loose requirements, so the function keeps exactly its versions.
- **No `requirements.txt`:** nothing changes; the run uses the image alone.

### Running with a set

- **Docker:** the unpacked set is bind-mounted read-only at `/opt/nbsandbox/packages`.
- **Kubernetes:** the set is copied into the pod beside the space's files, like those files. A large set makes the start slower (measured and documented). An image volume, which needs a registry, can come later.
- **Inside the run:** the runner adds the set's `site-packages` to `sys.path` before the kernel starts. The runner's own packages come first and can't be shadowed: the constraints already keep them out of the set, and the path order makes sure.
- **The dependency check** counts the set's packages as provided.
- **The editor's completions** don't cover a space's own packages yet (noted in the docs).

### Settings

```yaml
packages:
  enabled: false                       # off: requirements.txt is only checked, as today
  index_url: https://pypi.org/simple   # or a private mirror
  allowed: []                          # distribution names spaces may ask for; empty = any
  max_size: 1GiB                       # unpacked
  build_timeout: 10m
  retention: 720h                      # sets unused this long are removed (unless a function version uses them)
```

### Security

| Risk | Answer |
|---|---|
| A package's code runs during the build, with network | Wheels only: installing runs no package code. The only code in the build container is pip. |
| `requirements.txt` points pip at an internal address (cloud metadata, internal services) | Index and URL options are refused; the index is the operator's setting only. |
| The build container reads other spaces' data | It mounts no space; only `requirements.txt` and the constraints are copied in. |
| A package replaces nbsandbox's runner code or its dependencies | Constraints pin the runner's packages; the path order puts the image first. |
| A malicious package does harm at run time | It runs in the normal run sandbox: no network, no credentials, limits, gVisor if configured. That is the same trust a notebook's own code gets. |
| Disk exhaustion | `max_size`, build limits, retention, and a cache size limit per worker. |
| Supply chain (a compromised package on PyPI) | The operator's choice: a private mirror (`index_url`) and/or `allowed`. The lock records exact hashes, so a set never changes after it is built. |

**Isolation report:** new checks, that a build container can't reach the host, other containers or cloud metadata beyond the index, and that a run can't import a set from another space.

## Order of work

1. Parse and validate `requirements.txt`; the key; the `package_sets` table; settings.
2. The builder image (a target in the runner Dockerfile), `make runner-image`, and the release pipeline.
3. The build on Docker: build container, report, tarball, cache, limits, timeout.
4. Runs and publish: wait for the build, mount, `sys.path`, dependency check, errors, events and log.
5. API (REST, gRPC, OpenAPI) and the web UI's packages panel.
6. Kubernetes: build pods (with an egress NetworkPolicy of their own), copying the set into run pods.
7. Isolation report checks, docs, `make test-quick` and the report.

## Decisions (agreed 2026-11-15)

1. A **package set** (mounted), not a derived image.
2. **Wheels only**: no source builds.
3. **Off by default** (`packages.enabled: false`).
4. **Docker in this milestone**, Kubernetes in the next PR (step 6).
5. **Open when on:** any package from the index (with a wheel, not conflicting with the runner's packages) may be installed. `packages.allowed` is an optional allow-list for operators who want approval; empty by default.
6. **A run waits for its build** (queued, holding no run slot; builds have their own concurrency, one per worker); publishing waits for its build; "build now" in the API and the UI.
7. **No silent drift:** a space keeps its last lock, and a rebuild of unchanged requirements (after a runner upgrade, or after the set expired) tries that lock first, resolving afresh only if it no longer fits the image (said in the build log).
8. **Building needs `nbsandbox.runs`.**
9. **`packages.network`** chooses the Docker network of build containers (default: bridge); a proxy or mirror recipe for locked-down hosts.
10. **Kubernetes (next PR):** a per-node cache of sets, mounted read-only, not a copy per run.
