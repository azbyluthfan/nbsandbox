# A space's own packages

Every run uses the runner image: Python 3.13 and a fixed set of packages (`GET /v1/runtime` lists them). A space that needs more lists them in a `requirements.txt` at its top level. nbsandbox installs them once into a **package set**, and that space's runs import them as if they were in the image.

Runs still have no network: the packages are installed by a separate build step, never by a run.

This is off unless the operator turns it on ([below](#for-operators)). While it's off, `requirements.txt` is only checked: runs warn about what the image doesn't have.

## For users

1. **Add `requirements.txt`** to the space, with one requirement per line:
   ```
   humanize
   tabulate>=0.9
   rich[jupyter] ~= 13.9 ; python_version >= "3.12"
   ```
   - Accepted: names, extras, version specifiers, environment markers, and `--hash=sha256:...`.
   - Not accepted: index options (`-i`, `--extra-index-url`, `-f`), URLs, paths, `-e`, `-r` and other options. The package index is the operator's setting.
2. **Run a notebook.** The first run after `requirements.txt` changes waits, queued, while the packages are built (`waiting_for: "packages"`; the run page says so). Every later run starts at once.
   - **Build ahead:** `POST /v1/spaces/{id}/packages`, or **Build now** on the space's page in the web UI.
3. **See what was installed:** `GET /v1/spaces/{id}/packages`, or the **Packages** section of the space's page. It shows the status, each package with its version, the size, and the build log.

**What can be installed:**
- **Wheels only.** A package published only as source can't be installed (pip says "No matching distribution").
- **Not a different version of a package the runner image has.** For example `pandas==2.0` when the image has another pandas is refused with pip's explanation. Packages the image already has are skipped (`pandas` alone is fine).
- **Only what the operator allows,** if they set an allow-list.

**If the build fails:**
- The runs waiting for it fail with `error_kind: packages`.
- Later runs are refused at once (`422 packages_failed`, with `build_log`) until `requirements.txt` changes.
- After a few minutes, an unchanged `requirements.txt` is tried again (for when the package index was down).

**Versions stay put.** Requirements without exact versions (`humanize`) resolve to the newest version *when first built*. After that, the space keeps that set; a rebuild of the same `requirements.txt` (after a runner image upgrade) installs the same versions again if they still fit. Pin versions (`humanize==4.17.0`) to choose them yourself.

**Functions keep their packages.** Publishing waits for the space's packages and records their exact versions and hashes. Every invocation of that version gets exactly those, whatever `requirements.txt` says later.

**Not yet:** the editor's completions don't include a space's own packages.

## How it works

```
requirements.txt ──► key = hash(requirements, runner image, platform)
                        │
                        ├─ already built (by any space)? ──► use it
                        └─ otherwise a worker builds it once:
                              build container: builder image (the runner's packages + pip)
                              pip install --only-binary=:all: -c <the runner's versions>
                              network: yes · space files: none · same limits and hardening as a run
                           ──► package set (only the packages the image lacks) in object storage
run ──► the worker unpacks the set once into its cache ──► mounted read-only at /opt/nbsandbox/packages
                                                         the kernel adds it after the image's packages
```

- **Cost:**
  - **The first build** of a `requirements.txt`: a few seconds for small sets, minutes for large ones (machine-learning frameworks).
  - **The first run on each worker:** downloading and unpacking the set.
  - **Every later run:** nothing measurable; it is a read-only mount. Numbers are in [perf.md](perf.md#package-sets).
- **Shared:** spaces with the same requirements share a set.
- **One build per set:** several runs waiting for the same set wait for one build.
- **Clean-up:** sets nobody used for `packages.retention` (30 days) are removed, and rebuilt if asked for again.

## For operators

```yaml
packages:
  enabled: true
  index_url: https://pypi.org/simple    # or your mirror (Artifactory, Nexus, devpi): https only
  allowed: []                           # e.g. [humanize, tabulate, polars]; empty = any
  max_size: 1GiB                        # a set, unpacked
  build_timeout: 10m
  retention: 720h
  cache_size: 20GiB                     # sets a worker keeps unpacked (least recently used go first)
  builder_image: ghcr.io/azbyluthfan/nbsandbox-runner-builder:<version>   # the same release as the runner image
  network: bridge                       # the Docker network of build containers
```

- **Compose:** `NBSANDBOX_PACKAGES_ENABLED=true` in `.env` (plus `NBSANDBOX_PACKAGES_INDEX_URL` and `NBSANDBOX_PACKAGES_ALLOWED` as needed).
- **Locally:** `make runner-image` builds the builder image next to the runner image; then `NBSANDBOX_PACKAGES_ENABLED=true make up`.
- **Docker only for now.** On Kubernetes, nbsandboxd refuses `packages.enabled`.

**Security.** The build container is the only container nbsandbox starts with network access. Pip is the only program that runs in it:
- **Wheels only:** installing a wheel unpacks it; no package code runs.
- **No space files:** it gets `requirements.txt`, nothing else, and no credentials.
- **No redirection:** `requirements.txt` can't send pip anywhere but `index_url`.
- **The runner's packages can't be replaced:** they are constraints, and come first on `sys.path`.

The packages' code then runs inside runs, in the normal sandbox: no network, limits, gVisor if configured. That is the same trust as the notebook's own code.

What remains is **supply chain**: a user can install any package from the index, including a malicious or typo-squatted one. Use `allowed` (an allow-list, checked against the requirements and again against everything resolved, dependencies included) or a mirror with only the packages you approve.

**Locked-down hosts.** Build containers use `packages.network`, Docker's `bridge` by default, which reaches whatever the host reaches. To allow nothing but your package mirror:
1. Create an internal network, which has no route out: `docker network create --internal nbsandbox-builds`.
2. Attach the mirror's container to it: `docker network connect nbsandbox-builds <mirror>`.
3. Set `packages.network: nbsandbox-builds` and `packages.index_url: https://<mirror>/simple`.

**Audit:** builds asked for through the API are in the audit log (`BuildSpacePackages`).
