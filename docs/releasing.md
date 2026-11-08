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

1. **Main is green:** PR, main and nightly pipelines; the last nightly isolation report has no failures.
2. **Soak:** for a minor or major release, a 24 h soak on the reference host (Linux with Docker Engine):
   ```bash
   SOAK_DURATION=24h make test-integration TESTFLAGS="-v -run TestSoak -timeout 25h"
   ```
3. **Changelog:** move "Unreleased" in [CHANGELOG.md](../CHANGELOG.md) to the new version, with today's date.
4. **Versions in files:** `deploy/.env.example`, the defaults in `deploy/docker-compose.yml`, `deploy/helm/nbsandbox/Chart.yaml` (`version`, `appVersion`) and `values.yaml` (`runnerImage`), and `api/openapi.yaml` `info.version`.
5. **Scans** (the [security review](security/review.md) lists the commands): `govulncheck`, Trivy on both images, `pip-audit` on the runner's packages, gitleaks on the tree. Fix what has a fix; note what doesn't.
6. **Isolation reports on every platform** (the release workflow repeats the Docker one on Linux):
   - `make report-linux`: Linux with Docker Engine, `runc` and `runsc`;
   - `make report-k8s` and `make report-k8s K8S_RUNTIME_CLASS=gvisor`: Kubernetes, both runtimes.
7. **Tag** (signed off, like every commit):
   ```bash
   git tag -s vX.Y.Z -m "nbsandbox vX.Y.Z"
   git push origin vX.Y.Z
   ```
8. **The release workflow** (`.github/workflows/release.yml`) then:
   - runs `make report`: the full integration suite. The release stops if a required test is missing or failing;
   - builds and pushes multi-arch (amd64, arm64) images for nbsandboxd and the runner, with SBOMs and provenance, and signs them with cosign (keyless);
   - packages the Helm chart and pushes it to `oci://ghcr.io/azbyluthfan/charts`, signed with cosign;
   - builds the binaries with GoReleaser, with checksums, SBOMs and a signed checksum file;
   - creates the GitHub release with the changelog and **this commit's isolation report** attached.
9. **Check the release:** verify the signatures (the same for `nbsandbox-runner` and `charts/nbsandbox`):
   ```bash
   cosign verify ghcr.io/azbyluthfan/nbsandboxd:X.Y.Z \
     --certificate-identity-regexp 'https://github.com/azbyluthfan/nbsandbox/' \
     --certificate-oidc-issuer https://token.actions.githubusercontent.com
   ```
   Then deploy to a test host following [deployment.md](deployment.md).

## Security fixes

A fix for a reported vulnerability ([SECURITY.md](../SECURITY.md)) goes out as a patch release, with a GitHub security advisory published once the release is available.
