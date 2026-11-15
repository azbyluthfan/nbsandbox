# Plan: audit log and admin operations

**Status (2026-11-15):** built as below.

## Today

- Administrators (`nbsandbox.admin`) can list every owner's spaces, give a space to another owner, and delete any space. Each change goes to the service log, and nowhere else.
- Nothing records who uploaded, ran, downloaded or deleted what.
- An operator can't see or stop other people's runs.
- An operator can't stop new runs from starting before maintenance. Shutting a worker down stops it claiming runs, but other workers keep going.

## Audit log

**What is recorded.** One row per audited operation, whether it succeeded or was refused:

| Kind | Operations |
|---|---|
| Changes | create, rename or delete a space; upload, write, move or delete a file; create a folder; publish or delete a function; invoke a function; start, cancel or delete a run; promote an artifact |
| Downloads (`audit.downloads`, on by default) | a file, a run's notebook, HTML, artifact or log |
| Administration | every admin operation, including reading the audit log |
| Web UI | sign-in and sign-out |

Listing and streaming are not recorded: they are frequent and reveal little. A request refused before the caller is known (`401`) is not recorded either. The service log already has those, with their reasons.

**Each row:**
- `at`;
- `actor` (the caller ID), `method` (basic or oidc), `client` (the token's application);
- `via` (rest, grpc or ui);
- `action` (the operation's name, as in gRPC), `space_id`, `target` (a path, run, function or version), `detail` (for example the new owner);
- `status`: the HTTP status, with gRPC codes mapped to their HTTP equivalent;
- `address`: the client's address, through trusted proxies.

**Where.** A recorder in `internal/audit` is called from one place per transport: the REST route wrapper, which also covers the UI, and the gRPC interceptor. Both see every operation, so no handler can forget to record itself. Handlers only add what the path doesn't say, such as the ID of the run they created.

**Stored** in PostgreSQL (`audit_events`), synchronously, after the handler. A row that can't be written is logged as an error and counted (`nbsandbox_audit_failures_total`); the request still succeeds. Each row also goes to the service log (`msg=audit`), for log shippers.

**Kept** for `audit.retention`, 90 days by default, then removed by the cleanup loop.

**Read** by administrators:
- REST: `GET /v1/admin/audit?actor=&space=&action=&since=&until=&cursor=&limit=`, newest first.
- gRPC: `AdminListAudit`.

## Admin operations

| | |
|---|---|
| `GET /v1/admin/runs?status=&owner=&space=` | every owner's runs, newest first, with the owner (metadata only: no outputs) |
| `POST /v1/admin/runs/{run_id}/cancel` `{"reason": ""}` | stop any run; `cancelled_by` is the administrator |
| `GET /v1/admin/drain` | whether workers are drained, why, since when, by whom, and how many runs are queued and running |
| `PUT /v1/admin/drain` `{"draining": true, "reason": ""}` | drain or resume |

**Drain**, before maintenance:
- No worker starts a queued run. Running runs finish.
- The API keeps accepting runs, which wait in the queue, so clients see `queued`, not errors.
- Resuming wakes the workers.
- The state is one database row, checked by the claim query itself, so every worker obeys it at once and it survives restarts.
- When `GET /v1/admin/drain` shows no running runs, maintenance can start.

All four are in REST and gRPC (`AdminService`) with `nbsandbox.admin`, and are audited.

## Settings

```yaml
audit:
  enabled: true
  retention: 2160h     # 90 days
  downloads: true      # also record downloads
```

## Not now

- An admin page in the web UI. The API, gRPC and `curl` cover it for now.
- Exporting to an external system other than through the service log.
