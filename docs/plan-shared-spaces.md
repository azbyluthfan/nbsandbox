# Plan: shared spaces

**Status (2026-11-28):** built as below, in S1 to S4 (user documentation: [sharing.md](sharing.md)).

## Today

- A space has one owner (`spaces.owner`, the caller ID that created it). Every request names its caller, and every query filters by owner: someone else's space answers `404`, exactly like a missing one.
- Runs record who started them (`runs.owner`), always the space's owner today. Sessions are their opener's.
- A space's names for connections (`space_environments`, `space_bindings`) bind the owner's own connections. A published function is called by its space's owner, with the space's bindings ([plan-data-access.md](plan-data-access.md#functions-and-shared-spaces-the-callers-connections)).
- Quotas are per owner: spaces, bytes per space, runs and sessions at once.

Teams can't work in one space, and a function can't be called by anyone but its publisher.

## Goal

Several callers work in one space, each with a role, and each reading data with **their own connections** under the space's names. A function can be called by anyone the space grants it to, apps included, with the caller's connections.

## Roles

A space grants a role to a **principal**: a caller ID, exactly as nbsandbox records callers (`alice`, or `kc:3f2a…` for an OIDC subject or client). No check that the principal exists: nbsandbox has no user directory, and a grant waits for its caller's first request. OIDC groups come later, as another kind of principal in the same grants.

| | read | write | owner |
|---|---|---|---|
| See the space, its files, members and names; download files | ✓ | ✓ | ✓ |
| See every run in the space (who started it, status, log, output, artifacts, usage); follow the space's events | ✓ | ✓ | ✓ |
| Call its published functions (with their own connections) | ✓ | ✓ | ✓ |
| Bind their own connections to the space's names | ✓ | ✓ | ✓ |
| Upload, edit, move and delete files; create folders | | ✓ | ✓ |
| Start runs; cancel their own; promote their own run's outputs into the space | | ✓ | ✓ |
| Open interactive sessions (theirs alone) | | ✓ | ✓ |
| Publish and retire functions; change the space's packages | | ✓ | ✓ |
| Declare the space's names, their types and its environments | | | ✓ |
| Grant, change and revoke roles; rename and delete the space | | | ✓ |
| Cancel anyone's run; end anyone's session (without seeing into it) | | | ✓ |

- **The creator** (`spaces.owner`) is always an owner, and the space's **account**: its files count toward the creator's quotas. Another owner can't remove the creator's role; the creator can **hand the space over** to another owner, who becomes its account (an administrator can too, for someone who left).
- **Several owners** are allowed. The last owner can't be removed.
- **Runs and sessions count toward whoever starts them** (runs and sessions at once), not the space's account.
- **No access answers `404`,** as today, so a space's existence isn't revealed; a role too low for the request answers `403`, saying which role it needs.
- **Scopes still apply:** a token's scopes limit what its caller may do anywhere; a role limits what they may do in a space. Both must allow a request.
- **Sessions stay private:** listed, opened and attached only by their opener. Owners see that a session is open (who, since when) and can end it, not attach to it or read its cells.
- **Files:** concurrent edits already need the version the editor read (`If-Match`), so two members can't overwrite each other's changes unnoticed.

## Names, per member

Owners **declare** the space's names and their types, per environment; each member **binds** their own connections to them:

```
space "forecast"   declared (owners)           alice's bindings          bob's bindings
                   crm: postgresql, dev+prod   dev:  crm → crm-dev       dev:  crm → bob-crm
                   events: http, dev+prod      prod: crm → crm-prod      prod: (none)
                                               events → events-api       events → bob-events
```

- **A run uses the bindings of whoever starts it,** a session its opener's, a function call its caller's. A name the caller hasn't bound in the chosen environment refuses the run, session or call when it is submitted (`400`), naming the names to bind; a connection deleted since then fails it at start, as today.
- **A binding must match the declared type,** checked when it is saved.
- **Functions:** a version keeps declaring the names it reads and their types; a caller needs bindings for those names, in the environment called, to connections of those types.
- **Members' bindings are their own:** others see which names a member has bound (to explain a refused run), never which connection.
- **Upgrade:** today's bindings become the space's declarations (each name with its connection's type, in each environment) and the owner's bindings. Nothing changes for a space nobody else is in.

## API

| | |
|---|---|
| `GET /v1/spaces` | the caller's spaces and those shared with them, each with `"role"`; `?role=owner` to filter |
| `GET /v1/spaces/{id}/members` | every member: principal, role, granted by and when (any role) |
| `PUT /v1/spaces/{id}/members/{principal}` | `{"role": "read" \| "write" \| "owner"}`: grant or change (owners) |
| `DELETE /v1/spaces/{id}/members/{principal}` | revoke (owners; anyone, for themselves: leave) |
| `POST /v1/spaces/{id}/transfer` | `{"to": "<principal>"}`: hand the space over to another owner (the creator, or an administrator) |
| `GET`/`PUT /v1/spaces/{id}/names` | the declared names, types and environments (`PUT`: owners) |
| `GET`/`PUT /v1/spaces/{id}/connections` | the caller's own bindings (any role); the response lists the declared names each environment still needs |

- Runs, sessions and functions keep their endpoints; who may call them follows the table above. Each run's `"owner"` is who started it.
- gRPC gains the same (`SpaceService`: members, transfer, names); MCP's `list_spaces` shows the role, and its other tools act with the caller's role.
- Revoking a role doesn't stop runs already started; a revoked member's queued runs still run, as their own.
- The audit log records each grant, change, revocation, transfer and change of declared names.

## Web UI

- **Spaces:** shared spaces beside the caller's own, with the role and the space's account.
- **A space's Members section** (everyone sees it; owners change it): principal, role, granted by.
- **Runs** show who started each one, with a filter for the caller's own.
- **Connections section:** owners edit the declared names and environments; every member sets their own bindings and sees which names they still need.
- Buttons a role doesn't allow aren't shown; the server checks regardless.

## Order of work

1. **S1, roles:** the members table, one access check (caller, space → role) used by every space endpoint on REST and gRPC, the members and transfer endpoints, quotas by account and by caller, the audit entries; tests for each role against each endpoint group, and that no access stays `404`.
2. **S2, names per member:** declared names, bindings per member, resolution by the caller for runs, sessions and function calls, the upgrade of existing bindings.
3. **S3, the web UI:** spaces list, Members, runs by whom, the Connections section's two parts.
4. **S4, the rest:** docs (a "Sharing a space" guide; API, gRPC and MCP), the isolation report's access rows, the tour, a soak space with two members.

## Decisions so far

- Roles read, write and owner (2026-11-28); principals are caller IDs, groups later.
- Every member sees every run in the space; sessions stay their opener's, which owners can end.
- Owners declare names and types per environment; each member binds their own connections; a run, session or function call resolves with its caller's bindings.
- The creator is the space's account for storage quotas and can hand it over; runs and sessions count toward whoever starts them.
- No access is `404`; too low a role is `403`.
- One open session per notebook per caller (two members each get their own); still one read-write session per space.
- An owner's `PUT /v1/spaces/{id}/connections` declares the names (as in 0.4) when it sends `default_environment` or nothing is declared yet; otherwise it sets the caller's bindings only, partly if they like.
- Revoking a member, or their leaving, drops their bindings in the space.
