# Releasing nbsandbox

How a version gets from `main` to a GitHub release with signed images and a signed Helm chart.

## Versions

| What | Scheme | Where it shows |
|---|---|---|
| nbsandboxd | SemVer, tags `vX.Y.Z` | `nbsandboxd version`, image `ghcr.io/azbyluthfan/nbsandboxd:X.Y.Z` |
| Runner image | Same version as the release that built it; contents change by the rules below | `ghcr.io/azbyluthfan/nbsandbox-runner:X.Y.Z`, `GET /v1/runtime` |
| Helm chart | Same version as the release (`version` and `appVersion` in `Chart.yaml`) | `oci://ghcr.io/azbyluthfan/charts/nbsandbox --version X.Y.Z` |
| API | `/v1`, additive changes only after v1.0 | [api.md](api.md) |
| Status file between runner and nbsandboxd | `status_schema_version` (now 2) | nbsandboxd refuses a runner image with a version it doesn't support (`/readyz`: `runner_image`) |

Before v1.0, a minor version may change behaviour; changes are listed in the changelog. The runner image follows these rules: a patch release only rebuilds it (security updates); a minor release may add packages; a major release changes the status format or the Python major version.

## Checklist

1. **Main is green:** the PR and main pipelines, and the weekly one (start it from the Actions tab: "weekly" → "Run workflow"), which runs every scenario: Docker with runc and gVisor, Kubernetes with both, fuzzing and the security scan.
2. **Soak:** for a minor or major release, a 12 h soak (6 h at least) on the reference host (Linux with Docker Engine):
   ```bash
   SOAK_DURATION=12h make test-integration TESTFLAGS="-v -run TestSoak -timeout 13h"
   ```
3. **Changelog:** move "Unreleased" in [CHANGELOG.md](../CHANGELOG.md) to the new version, with today's date.
4. **Versions in files:** `deploy/.env.example`, the defaults in `deploy/docker-compose.yml`, `deploy/helm/nbsandbox/Chart.yaml` (`version`, `appVersion`) and `values.yaml` (`runnerImage`), and `api/openapi.yaml` `info.version`.
5. **Scans** (the [security review](security/review.md) lists the commands): `govulncheck`, Trivy on the four images (nbsandboxd, the runner, its builder and the fetcher), `pip-audit` on the runner's and the fetcher's packages (the weekly run repeats both), gitleaks on the tree. Fix what has a fix; note what doesn't.
6. **Isolation reports on every platform** (the release workflow repeats the Docker one on Linux):
   - Linux with Docker Engine and Kubernetes 1.37 (kind), each with `runc` and gVisor: from the latest weekly run on `main` that passed (start one from the Actions tab if the code changed since). Download its `isolation-report-*` artifacts, unzip each into a folder of its own name, run `scripts/render-reports.sh <folder>`, review the diff, and commit the two reports in the release's pull request. CI never writes them itself;
   - or on your own machines: `make report-linux`, and `make report-k8s` then `make report-k8s K8S_RUNTIME_CLASS=gvisor`.
7. **Tag** (signed off, like every commit):
   ```bash
   git tag -s vX.Y.Z -m "nbsandbox vX.Y.Z"
   git push origin vX.Y.Z
   ```
8. **The release workflow** (`.github/workflows/release.yml`) then:
   - runs `make report`: the full integration suite. The release stops if a required test is missing or failing;
   - builds and pushes multi-arch (amd64, arm64) images for nbsandboxd, the runner and its builder, with SBOMs and provenance, checks the published runner image's manifest label for both platforms (`scripts/check-runner-label.py`), and signs them with cosign (keyless);
   - packages the Helm chart and pushes it to `oci://ghcr.io/azbyluthfan/charts`, signed with cosign;
   - builds the binaries with GoReleaser, with checksums, SBOMs and a signed checksum file;
   - creates the GitHub release with the changelog and **this commit's isolation report** attached.
9. **Check the release:** verify the signatures (the same for `nbsandbox-runner`, `nbsandbox-runner-builder`, `nbsandbox-fetcher` and `charts/nbsandbox`):
   ```bash
   cosign verify ghcr.io/azbyluthfan/nbsandboxd:X.Y.Z \
     --certificate-identity-regexp 'https://github.com/azbyluthfan/nbsandbox/' \
     --certificate-oidc-issuer https://token.actions.githubusercontent.com
   ```
   Then check the published release itself: the tests before it all use images built from source, so this is the only check of what users pull. On any Docker host:
   ```bash
   scripts/check-published.sh X.Y.Z
   ```
   It deploys that release from its tag with the images on GHCR, as [deployment.md](deployment.md) does (pulled without a login, so they must be public), and runs the smoke notebook and a package set build through Caddy; for a release with interactive sessions, also the release's own session example ([examples/python/interactive_session.py](../examples/python/interactive_session.py)) over a WebSocket through Caddy (needs `python3` with `venv`). For a release with connections, it turns them on as [connections.md](connections.md) says, starts the gateway, and reads a PostgreSQL table beside the stack through the published fetcher. `CHECK_LOCAL_IMAGES=1` tries it before a release with local builds tagged as the release's images. The weekly workflow repeats it for the newest release.

## Security fixes

A fix for a reported vulnerability ([SECURITY.md](../SECURITY.md)) goes out as a patch release, with a GitHub security advisory published once the release is available.
