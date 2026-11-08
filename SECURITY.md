# Security Policy

nbsandbox runs user-supplied notebook code, so isolation bugs are taken seriously.

## Reporting a vulnerability

**Please do not open a public issue.** Report privately through GitHub's private vulnerability reporting:

https://github.com/azbyluthfan/nbsandbox/security/advisories/new

(On the repository page: *Security → Report a vulnerability*.) The maintainer is [azbyluthfan](https://github.com/azbyluthfan) on GitHub.

Please include:
- what an attacker can do (e.g. read another space's files, escape the container, exhaust host resources);
- steps or a notebook that reproduce it;
- platform (Linux, Docker Desktop or Kubernetes), Docker or Kubernetes version, runtime (`runc`, `runsc` or the RuntimeClass) and nbsandbox version.

## What to expect

nbsandbox is maintained by one person, so these are targets, not guarantees:

| Step | Target |
|---|---|
| Acknowledge the report | within 7 days |
| Initial assessment | within 14 days |
| Fix or mitigation for confirmed issues | within 90 days, sooner for severe ones |

You'll be credited in the advisory unless you prefer not to be.

## Supported versions

Before v1.0, only the latest release receives security fixes.

## Scope and known limitations

nbsandbox v0.x assumes **trusted users**: isolation protects spaces from accidental access and resource contention between each other, and every control is tested. With the default runtime (`runc`) runs share the host's kernel; for less trusted users, run them under gVisor or a sandbox RuntimeClass on Kubernetes ([docs/gvisor.md](docs/gvisor.md)). Some limitations are by design and documented in [docs/security/threat-model.md](docs/security/threat-model.md), for example: the Docker socket proxy limits which API calls exist but not the options inside them, and storage quotas are checked by the service rather than enforced by the filesystem. Reports that show these limits being worse than documented are welcome.

Isolation results from Docker Desktop and kind are not authoritative; the reference platforms are Linux with Docker Engine and the Kubernetes cluster you deploy to.
