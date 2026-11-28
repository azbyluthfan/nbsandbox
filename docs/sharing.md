# Sharing a space

A space can be shared: its creator grants other callers a **role** in it, and they work in it beside them, each reading data with **their own connections**. The design is in [plan-shared-spaces.md](plan-shared-spaces.md).

## Roles

A role goes to a **caller ID**, exactly as nbsandbox records callers: a username from `users.yaml` (`bob`), or `<prefix>:<claims>` for an identity provider's token (`kc:3f2a…`, for a user or an app; [auth.md](auth.md)). Nothing checks that the caller exists: the grant waits for their first request. Groups aren't grantable yet.

| | read | write | owner |
|---|---|---|---|
| See the space, its files, members and names; download files | ✓ | ✓ | ✓ |
| See every run in the space (who started it, its log, output, artifacts and usage); follow its events | ✓ | ✓ | ✓ |
| Call its published functions, with their own connections | ✓ | ✓ | ✓ |
| Bind their own connections to the space's names | ✓ | ✓ | ✓ |
| Upload, edit, move and delete files; create folders | | ✓ | ✓ |
| Start runs; cancel, delete or copy files from their own | | ✓ | ✓ |
| Open interactive sessions (theirs alone) | | ✓ | ✓ |
| Publish and delete functions; build the space's packages | | ✓ | ✓ |
| Declare the space's names, their types and its environments | | | ✓ |
| Grant, change and revoke roles; rename and delete the space | | | ✓ |
| Cancel or delete anyone's run; end anyone's session (without seeing into it) | | | ✓ |

- **The creator** is always an owner and the space's **account**: its files count toward the creator's quota. They can **hand the space over** to another owner, who becomes its creator; they stay an owner. An administrator can hand over any space (`PATCH /v1/admin/spaces/{id}`).
- **Runs and sessions count toward whoever starts them** (`runs.per_user`, `sessions.per_user`), not the space's creator.
- **Without a role,** the space doesn't exist for you: `404`, as for a missing one. **With a role too low,** `403`, saying which role is needed.
- **A token's scopes still apply:** a reader with `nbsandbox.runs` still can't start runs in a space where they only read, and a writer whose token lacks it can't either.
- **Sessions stay private** to whoever opened them: two members opening the same notebook each get their own (still one read-write session per space at a time). Owners see who has one open and can end it.
- **Files** are saved with the version the editor read (`If-Match`), so two members can't overwrite each other's changes unnoticed.
- **Leaving** or being removed doesn't stop runs already started. Your bindings in the space go with you.

## Through the API

```sh
# alice grants bob write, and carol (signed in with Keycloak) read:
curl -u alice:… -X PUT https://nbsandbox.example/v1/spaces/$SPACE/members/bob -H 'Content-Type: application/json' -d '{"role": "write"}'
curl -u alice:… -X PUT "https://nbsandbox.example/v1/spaces/$SPACE/members/kc:$CAROL_SUB" -H 'Content-Type: application/json' -d '{"role": "read"}'

curl -u bob:… https://nbsandbox.example/v1/spaces                      # alice's space, with "role": "write" and "owner": "alice"
curl -u bob:… https://nbsandbox.example/v1/spaces/$SPACE/members       # who has which role
curl -u bob:… "https://nbsandbox.example/v1/spaces/$SPACE/runs?owner=bob"   # every run, or only bob's

curl -u bob:… -X DELETE https://nbsandbox.example/v1/spaces/$SPACE/members/bob     # bob leaves
curl -u alice:… -X POST https://nbsandbox.example/v1/spaces/$SPACE/transfer -H 'Content-Type: application/json' -d '{"to": "dave"}'  # dave, an owner, becomes its creator
```

gRPC has the same (`SpaceService`: `ListMembers`, `PutMember`, `DeleteMember`, `TransferSpace`), and MCP's `list_spaces` shows each space's role. Grants, changes, revocations and hand-overs are in the audit log.

## Data: each member's own connections

A space's **owners declare its names**: what notebooks call (`crm`, `events`), each with a connection type, in each environment (`dev`, `prod`). **Each member binds their own connections** to them. A run uses the bindings of whoever starts it, a session its opener's, a function call its caller's: nobody reads with someone else's credentials.

```sh
# alice (an owner) declares the names, without binding anything:
curl -u alice:… -X PUT https://nbsandbox.example/v1/spaces/$SPACE/names -H 'Content-Type: application/json' -d '{
  "default_environment": "dev", "environments": ["dev", "prod"],
  "names": {"crm": "postgresql", "events": "http"}}'

# bob binds his own connections (by name or ID), of those types:
curl -u bob:… -X PUT https://nbsandbox.example/v1/spaces/$SPACE/connections -H 'Content-Type: application/json' -d '{
  "environments": {"dev": {"crm": "bobs-crm-dev", "events": "bobs-events"}}}'
# {"default_environment": "dev", "environments": {"dev": {…}, "prod": {}}, "missing": {"prod": ["crm", "events"]}}
```

- **A run needs every declared name bound** by its caller in the environment it uses; otherwise it is refused when submitted (`400`), naming the names to bind. A function call needs only the names its version reads.
- **Members see which names others have bound,** never which connection (`GET /v1/spaces/{id}/names`, `bound`).
- **Changing a declaration** (an environment or a name removed, a name's type changed) drops the bindings it no longer fits.
- **As before shared spaces,** an owner's `PUT …/connections` with `default_environment` declares the names from their bindings and binds them in one call.

## In the web UI

- **Spaces** shows spaces shared with you, who shares them and what you can do.
- A space's **Members** section lists who has which role; owners grant, change and revoke roles there, and anyone can leave.
- The **Connections** section: owners add names, their types and environments; everyone picks their own connection for each name, and sees what their runs still need.
- **Runs** show who started them. What your role doesn't allow isn't offered.
