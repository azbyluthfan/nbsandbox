# nbsandbox API

This guide shows how to use the HTTP API, task by task: which endpoints to call, in what order, and what to do with each answer. The full reference, with every field and status code, is the OpenAPI 3.1 spec: [api/openapi.yaml](../api/openapi.yaml). A test keeps that file in step with the routes nbsandboxd serves, and every response in the tests is checked against it.

- [Basics](#basics): authentication, IDs, errors, lists
- [1. Set up a space and its files](#1-set-up-a-space-and-its-files)
- [2. Run a notebook and collect its outputs](#2-run-a-notebook-and-collect-its-outputs)
- [3. Edit a file without overwriting someone else's changes](#3-edit-a-file-without-overwriting-someone-elses-changes)
- [4. Organise files](#4-organise-files)
- [5. Publish a notebook and call it from another service](#5-publish-a-notebook-and-call-it-from-another-service)
- [6. Follow progress: which stream to use](#6-follow-progress-which-stream-to-use)
- [7. Errors and retries](#7-errors-and-retries)
- [Endpoint overview](#endpoint-overview), [viewing the spec](#viewing-the-spec), [compatibility policy](#compatibility-policy)

## Basics

The examples use curl through a small shell helper:

```bash
export NBSANDBOX_URL=https://nbsandbox.example.com NBSANDBOX_USER=alice NBSANDBOX_PASSWORD=…
nb() { curl -sS -u "$NBSANDBOX_USER:$NBSANDBOX_PASSWORD" "$@"; }
```

[scripts/nbsandbox.sh](../scripts/nbsandbox.sh) wraps the same calls as commands (`nbsandbox.sh files upload …`); the [user guide](user-guide.md) uses it.

- **Authentication:** every `/v1` endpoint needs a bearer access token from the configured identity provider, or HTTP basic auth (users from `users.yaml`, for development). `/healthz` and `/readyz` need none. Missing, wrong or expired credentials get `401`; a token without the endpoint's scope gets `403`. Spaces belong to the caller's ID. See [auth.md](auth.md).
- **Ownership:** you see only your own spaces. Another user's space answers `404`, exactly like one that doesn't exist, whatever the request contains.
- **IDs:** spaces and runs are lowercase UUIDs; anything else in their place is `400`. File paths are relative to the space and may contain `/`; `..`, absolute paths and names starting with `.nbsandbox` are refused.
- **Request bodies:** JSON with `Content-Type: application/json`, except uploads (`multipart/form-data`) and file saves (the raw file). Unknown fields are refused with `400`, so typos don't pass silently.
- **Errors:** always `{"error": {"code": "…", "message": "…"}}`. Act on `code` and the status; `message` is for people. Some errors add fields: `problems` (where parameters broke the contract), `current_version` (on file saves), `status` (asking for the result of a run that hasn't finished). See [7. Errors and retries](#7-errors-and-retries).
- **Lists:** paged with `limit` (default 50, at most 200) and `cursor`. Pass the response's `next_cursor` to get the next page; it is empty on the last one.
- **Times** are RFC 3339 in UTC.

## 1. Set up a space and its files

A **space** holds a notebook and the files it reads. Runs see the space's files as their working directory.

**Create the space:**

```bash
nb -H 'Content-Type: application/json' -d '{"name": "forecast"}' "$NBSANDBOX_URL/v1/spaces"
```
```json
{"id": "6f1c…", "name": "forecast", "created_at": "2026-11-07T08:00:00Z", "updated_at": "2026-11-07T08:00:00Z"}
```

Keep the `id`: every other call uses it (`SPACE=6f1c…` below).

**Upload files:** one request takes any number of parts named `file`. It is all or nothing: if one part is refused, nothing is stored. `?folder=` puts them in a folder (created if missing).

```bash
nb -F file=@forecast.ipynb "$NBSANDBOX_URL/v1/spaces/$SPACE/files"
nb -F file=@q1.csv -F file=@q2.csv "$NBSANDBOX_URL/v1/spaces/$SPACE/files?folder=data"
```
```json
{"files": [{"path": "data/q1.csv", "type": "file", "size": 5120, "modified": "…", "version": "9f86d081…"},
           {"path": "data/q2.csv", "type": "file", "size": 4980, "modified": "…", "version": "2c26b46b…"}]}
```

- **`201`:** stored. An upload replaces files with the same name.
- **`400`:** a bad name (a filename can't contain `/` or `..`; use `?folder=`), or a notebook that isn't nbformat 4.
- **`413`:** a file is over `uploads.max_file` (50 MB by default) or the space would go over its quota. Nothing was stored.

**Check what's there:** `GET /v1/spaces/$SPACE/files` lists files and folders, sorted by path, each file with its `version`. `GET /v1/spaces/$SPACE` gives the space with its usage against the quota.

**Check the runtime first (optional):** `GET /v1/runtime` lists the runner image's Python version, packages and module names, and the limits every run gets. A notebook that imports something not on the list fails with `missing_package`.

## 2. Run a notebook and collect its outputs

```
POST …/runs ──► queued ──► running ──► succeeded | failed | oom | kernel_died | timeout | cancelled | interrupted | platform_error
                  │            │                  │
                  │            │                  └─► GET …/result, …/output.html, …/artifacts
                  │            └─► GET …/log/stream (live output)
                  └─► GET …/runs/{run_id}/events (status changes)
```

**Start it:**

```bash
nb -H 'Content-Type: application/json' \
   -d '{"notebook": "forecast.ipynb", "parameters": {"region": "bandung", "cfg": {"months": 4}}}' \
   "$NBSANDBOX_URL/v1/spaces/$SPACE/runs"
```
```json
{"id": "a93e…", "space_id": "6f1c…", "notebook": "forecast.ipynb", "notebook_sha256": "…",
 "parameters": {"cfg": {"months": 4}, "region": "bandung"}, "status": "queued", "queue_position": 1,
 "created_at": "…"}
```

- **Python only:** a notebook whose kernel is another language (R, Julia, …) is refused with `400 bad_request`, before anything runs. Publishing one is refused the same way.
- **`parameters`** is a JSON object. Each top-level key becomes a Python variable after the cell tagged `parameters`, so keys must be valid Python names. Values can be any JSON (nested objects and arrays, `null`), up to 32 levels deep and 1 MiB in total by default.
- **The notebook is snapshotted** when you submit: editing it afterwards doesn't change this run. `notebook_sha256` says which version ran.
- **`"space": "read-only"`** mounts the space's files read-only. Such runs can't change the space, so they may run alongside others.
- **One writing run per space:** while a run that can write the space is queued or running, starting another one is `409`. Wait for it, or cancel it.
- **`Idempotency-Key` header:** send one when you may retry the request (for example after a timeout). The same key and body within 24 hours return the original run with `200` instead of starting another; the same key with a different body is `422`.
- **`dependency_warnings`** on the run lists imports the runner image doesn't have. With the server's `dependency_check: reject`, such a run is refused with `422` instead.
- **`503`** means Docker is unavailable; retry after the `Retry-After` seconds.

**Wait for it to finish.** Pick one:

| How | When |
|---|---|
| `GET …/runs/{run_id}/result?wait=30s` | Simplest. Holds the request until the run finishes (at most 60 s): `200` with the result, or `202` with the run if it's still going; then ask again. |
| `GET …/runs/{run_id}/result` with `Accept: text/event-stream` (or `?stream=1`) | The result as a stream: a streamed output's items as they are made, then the result. See [6](#6-follow-progress-which-stream-to-use). |
| `GET …/runs/{run_id}/events` | One `status` event per change, the last one carrying the result. |
| `GET …/runs/{run_id}` | Polling. `status` and `queue_position` now; `result` once finished. |

**Read the result:** `GET …/runs/{run_id}/result` gives a short answer meant for programs: the `output` the notebook returned with `nbsandbox.output(...)` if it succeeded, otherwise an `error` with the reasons. A notebook that streams its output (`nbsandbox.emit`) returns the list of its items as `output`, with `items` (how many); if it failed after sending some, the result has both the `error` and the items so far, with `"partial": true`.

```json
{"run_id": "a93e…", "status": "failed",
 "error": {"kind": "execution_error", "fault": "notebook", "retryable": false,
           "message": "ValueError in cell 3: months must be positive",
           "cell_index": 3, "exception_type": "ValueError", "exception_message": "months must be positive"}}
```

Without `?wait=`, asking before the run has finished is `409` with `"status": "queued"` or `"running"`.

`GET …/runs/{run_id}` has the full record instead: timings, resource use, limits and the runner image, in `result`.

**Collect the outputs:**

| Endpoint | What |
|---|---|
| `GET …/runs/{run_id}/output.html` | The executed notebook as HTML, for people (served sandboxed: no scripts). |
| `GET …/runs/{run_id}/output.ipynb` | The executed notebook. Partial if the run didn't succeed. |
| `GET …/runs/{run_id}/log` | The console output (stdout, stderr, errors). |
| `GET …/runs/{run_id}/artifacts` | Files the notebook wrote to `NBSANDBOX_OUTPUT_DIR`; download one with `…/artifacts/{path}`. |
| `POST …/runs/{run_id}/promote` | Copy an artifact into the space's files: `{"artifact": "chart.png", "to": "reports/chart.png"}`. `409` if `to` exists. |

A run's outputs live outside the space's files, so runs can't read each other's outputs. Promote an artifact when the next run should use it.

**Cancel:** `POST …/runs/{run_id}/cancel`, optionally with `{"reason": "superseded"}`. `202`: a queued run is cancelled at once; a running one stops shortly after and ends as `cancelled`, with `cancelled_by` and `cancel_reason` in the result. A run that has already finished is `409`.

**Clean up:** `DELETE …/runs/{run_id}` removes a finished run's outputs (`409` while it's active). `GET …/runs` lists runs, newest first, filtered with `status`, `created_after` and `created_before`.

## 3. Edit a file without overwriting someone else's changes

Every file has a **version** that changes whenever its contents do (the SHA-256 of its bytes; treat it as an opaque string). Send the version you edited when saving; if the file changed in the meantime, the server refuses rather than silently overwriting.

```
GET file ──► ETag: "v1" ──► edit ──► PUT with If-Match: "v1"
                                        ├─ 200: saved; the response has the new version
                                        └─ 412: changed meanwhile; current_version says to what
```

**1. Get the file and its version:**

```bash
nb -D - -o model.py "$NBSANDBOX_URL/v1/spaces/$SPACE/files/src/model.py"   # the ETag header is the version
```

The version is also in the file listing.

**2. Save it back:**

```bash
nb -X PUT --data-binary @model.py -H 'If-Match: "2fk-1b6x…"' "$NBSANDBOX_URL/v1/spaces/$SPACE/files/src/model.py"
```

| Answer | Meaning | What to do |
|---|---|---|
| `200` | Saved. The body is the file with its new `version`. | Use the new version for the next save. |
| `412 version_mismatch` | Someone changed the file since your version. `current_version` names the version now there (`""` if the file was deleted). | Reload and redo your edit, save under another name, or overwrite deliberately by sending `If-Match: "<current_version>"`. |
| `428 version_required` | The file exists and you sent no `If-Match`. | Get the version first. |

**Create a new file:** `PUT` with `If-None-Match: *` creates it only if it doesn't exist (`201`); if it does, `412`. A `PUT` with neither header creates the file when it doesn't exist.

Saves have the same limits as uploads: size, quota, and nbformat 4 for notebooks. A run works on a copy of the space taken when it starts, so save before you start the run.

**Runs that change files.** A run that can write the space (the default) works on its own copy. When it ends, whatever the outcome, its changes are written back, each checked against the version the run started from:
- a file nobody else touched gets the run's version;
- a file you saved while the run worked keeps **your** version, and the run's version is kept beside it as `name.conflict-<first 8 characters of the run id>.ext`;
- a file the run deleted but you changed meanwhile is not deleted;
- if the changes would take the space over its quota, none of them are stored.

The run's log ends with a line for each of these cases, so nothing is lost silently. Changes appear in the space when the run finishes, not while it runs; links and special files the run creates are not stored.

## 4. Organise files

| Task | Request | Notes |
|---|---|---|
| New folder | `POST …/folders` `{"path": "data/2026"}` | Creates missing parents. `409` if something is there. |
| Rename or move a file or folder | `PATCH …/files/{path}` `{"path": "new/path"}` | `409` if the new path exists. Add `If-Match` to move a file only if it is still that version. |
| Delete a file | `DELETE …/files/{path}` | `204`. |
| Delete a folder | `DELETE …/files/{path}` | An empty folder only; add `?recursive=true` for a folder and everything in it. `409` if not empty without it. |
| Download | `GET …/files/{path}` | Always sent as an attachment. `If-None-Match` with the version gives `304` when it hasn't changed. |

## 5. Publish a notebook and call it from another service

A notebook can serve as a **function**: another service sends JSON parameters and gets JSON back. Publishing freezes the notebook and the files it needs into an immutable version, so editing the space doesn't affect callers.

```
POST …/functions/{name}/versions ──► version 1, 2, …
POST …/functions/{name}/invoke?wait=30s ──► 200 {output} | 200 {error} | 202 run (keep waiting) | 400 invalid_parameters
```

```bash
# Publish (the first publish creates the function):
nb -H 'Content-Type: application/json' -d '{"notebook": "forecast.ipynb", "files": ["models/model.pkl", "data"]}' \
   "$NBSANDBOX_URL/v1/spaces/$SPACE/functions/forecast/versions"

# Call the latest version and wait up to 30 s:
nb -H 'Content-Type: application/json' -d '{"parameters": {"region": "bandung"}}' \
   "$NBSANDBOX_URL/v1/spaces/$SPACE/functions/forecast/invoke?wait=30s"
```

- **Pin a version** with `…/functions/forecast/versions/2/invoke`.
- **The answer is the same result** as `GET …/runs/{run_id}/result` in [2](#2-run-a-notebook-and-collect-its-outputs). A call is an ordinary run, so its log, outputs and events work the same way.
- **A contract** (JSON Schemas in the notebook's metadata) checks parameters before anything runs (`400 invalid_parameters` with `problems`) and checks the output afterwards (`failed`, `output_invalid`). `GET …/functions/{name}/contract` returns it.
- **Calls run side by side**, see the bundle read-only, and don't block the space's other runs.

The notebook side (`nbsandbox.output`, `nbsandbox.parameters`), contracts, error kinds and client examples are in [functions.md](functions.md).

## 6. Follow progress: which stream to use

All streams are Server-Sent Events (`text/event-stream`). They send a heartbeat comment every 15 s, and you resume them with the `Last-Event-ID` header (or `?after=N`) without losing or repeating anything. Browsers' `EventSource` does this by itself; with other clients, remember the last `id:`.

| Stream | Events | Ends | Use it for |
|---|---|---|---|
| `GET …/runs/{run_id}/result` as a stream (`Accept: text/event-stream` or `?stream=1`) | `item` per item of a streamed output as soon as it's made (its `id` is the item's number), `progress` with `?include=progress`, then `result` | after `result` | using a result as it's made, e.g. drawing a chart as data arrives |
| `GET …/runs/{run_id}/events` | `status` per change: `queued` (with `queue_position`), `running`, then the final status with `result`; `progress` and `cell` with `?include=progress,cell` | after the final status | waiting for one run or call |
| `GET /v1/spaces/{id}/events` | the same events for every run in the space; `?function=forecast` for one function's calls | when you disconnect | a service making many calls: one connection instead of polling each |
| `GET …/runs/{run_id}/log/stream` | `line` per log line (its `id` is the line number), then `done` with the final status | after `done` | showing the notebook's console output live |

```
id: 42
event: status
data: {"run_id":"a93e…","status":"running"}

id: 45
event: status
data: {"run_id":"a93e…","status":"succeeded","result":{"run_id":"a93e…","status":"succeeded","output":{…}}}
```

**A streamed output** (the notebook calls `nbsandbox.emit(item)`):

```
id: 1
event: item
data: {"run_id":"a93e…","kind":"item","index":1,"total":3,"item":{"region":"jakarta","total":1200}}

event: result
data: {"run_id":"a93e…","status":"succeeded","items":3,"output":[{"region":"jakarta",…},…]}
```

The items are exactly the list in the result's `output` (item *i* is `output[i-1]`), so a client can follow the stream, reconnect with `Last-Event-ID`, or read the JSON result afterwards, and deduplicate by run ID and index. A run that fails after sending items still has them: the result carries the `error` and `output` = the items so far, `"partial": true`. A notebook that returns one value sends just the `result` event. See [functions.md](functions.md#5-streamed-output).

**What a run reports while it works,** with `?include=` (data: `RunEvent` in the spec):
- **`progress`:** `nbsandbox.progress(fraction, message)`: `{"kind":"progress","fraction":0.4,"message":"3 of 8 regions"}`.
- **`cell`:** a code cell started or finished: `{"kind":"cell","cell_index":3,"state":"finished","outcome":"ok","duration_seconds":1.2}`, numbered as in the uploaded notebook.

They come from the notebook, so treat them as information, not as facts about the run.

**Other events:**
- **`shutdown`:** nbsandboxd is restarting. Reconnect in a few seconds with `Last-Event-ID`.
- **`pruned`** (status streams): you resumed from before the oldest kept event (kept 24 h, `runs.event_retention`). Re-read the runs you were waiting for with `GET …/runs/{run_id}`.
- **`dropped`** (log stream): you fell too far behind. nbsandboxd no longer sends it (a slow reader just reads at its own pace), but a client should still handle it: reconnect with `Last-Event-ID`.

Without SSE, poll with `GET …/runs/{run_id}/result?wait=30s`: one request per 30 s, answered as soon as the run finishes.

## 7. Errors and retries

| Status | `code` | What happened | What to do |
|---|---|---|---|
| `400` | `bad_request` | Malformed request: bad JSON, unknown field, bad path or ID; or a notebook that isn't Python. | Fix the request, or switch the notebook's kernel to Python 3. |
| `400` | `invalid_parameters` | Parameters aren't a valid object or break the notebook's input contract; `problems` lists JSON pointers. | Fix the parameters. |
| `400` | `invalid_contract` | The notebook's contract isn't a valid JSON Schema. | Fix the notebook's metadata. |
| `401` | `unauthorized` | Missing or wrong credentials, or an invalid or expired token. | Check the credentials; get a fresh token. |
| `403` | `forbidden` | The token lacks the endpoint's scope (named in `WWW-Authenticate`). | Request the scope at the identity provider. |
| `404` | `not_found` | It doesn't exist, or it belongs to another user. | Check the IDs. |
| `409` | `conflict` | A writing run is already active in the space; the run is (not) finished yet; the target path exists; the folder isn't empty. | Wait, cancel, or pick another path; the message says which. |
| `412` / `428` | `version_mismatch` / `version_required` | A file save with a stale or missing version. | See [3](#3-edit-a-file-without-overwriting-someone-elses-changes). |
| `413` | `payload_too_large` | A file, the request, or the space's quota is too large. Nothing was stored. | Make it smaller or free space. |
| `422` | `unprocessable` | `Idempotency-Key` reused with another body, or missing packages with `dependency_check: reject`. | Use a new key, or fix the imports. |
| `503` | `docker_unavailable` | Docker isn't reachable. | Retry after `Retry-After`. |
| `500` | `internal` | A bug. | Report it with the time and request. |

**A run that fails is not an HTTP error.** Submitting succeeded (`201`); the run's own outcome is in its `status` and its result's `error`. Retry a run only when `error.retryable` is true (`interrupted`, `platform_error`); any other failure will fail the same way with the same input. Send the same `Idempotency-Key` when retrying a request whose answer you didn't get, so you don't start the run twice.

## Endpoint overview

| Area | Endpoints |
|---|---|
| Operations (no auth) | `GET /healthz`, `GET /readyz`, `GET /metrics` (internal; not forwarded by Caddy) |
| Runtime | `GET /v1/runtime`: runner image, Python, packages, limits |
| Spaces | `POST/GET /v1/spaces`, `GET/PATCH/DELETE /v1/spaces/{id}` |
| Files | `POST/GET /v1/spaces/{id}/files` (upload with `?folder=`, list), `GET/PUT/PATCH/DELETE /v1/spaces/{id}/files/{path}` (download, save with `If-Match`, move, delete), `POST …/folders` |
| Runs | `POST/GET …/runs`, `GET/DELETE …/runs/{run_id}`, `POST …/cancel`, `GET …/result` (`?wait=`) |
| Run outputs | `…/output.ipynb`, `…/output.html`, `…/artifacts`, `…/artifacts/{path}`, `POST …/promote`, `…/log`, `…/log/stream` (SSE) |
| Status streams (SSE) | `GET …/runs/{run_id}/events`, `GET /v1/spaces/{id}/events` |
| Functions | `GET …/functions`, `GET/DELETE …/functions/{name}`, `POST …/functions/{name}/versions` (publish), `GET …/versions/{version}`, `GET …/contract`, `POST …/invoke`, `POST …/versions/{version}/invoke` |

## gRPC

The same API is served over gRPC, gRPC-Web and the Connect protocol, on the same port as REST: [api/proto/nbsandbox/v1/nbsandbox.proto](../api/proto/nbsandbox/v1/nbsandbox.proto). Three services, one RPC per REST operation, with the same names as the scopes table uses:

| Service | RPCs |
|---|---|
| `SpaceService` | `CreateSpace`, `ListSpaces`, `GetSpace`, `UpdateSpace`, `DeleteSpace`, `UploadFiles` (client stream), `ListFiles`, `GetFile` (server stream), `PutFile` (client stream), `MoveFile`, `DeleteFile`, `CreateFolder` |
| `FunctionService` | `ListFunctions`, `GetFunction`, `DeleteFunction`, `PublishFunction`, `GetFunctionVersion`, `GetFunctionContract`, `InvokeFunction` (with `wait_seconds`), `InvokeFunctionStream` (server stream) |
| `RunService` | `GetRuntime`, `CreateRun`, `ListRuns`, `GetRun`, `CancelRun`, `GetRunResult` (with `wait_seconds`), `FollowRunResult` (server stream: items, then the result), `DeleteRun`, `GetRunNotebook`, `GetRunHTML`, `GetRunLog`, `GetArtifact` (server streams), `ListArtifacts`, `PromoteArtifact`, `FollowRunLog`, `FollowRunEvents`, `FollowSpaceEvents` (server streams) |

- **Same data.** Every message has the fields of its REST JSON twin, with the same names. Parameters and outputs are `google.protobuf.Value`, times are `Timestamp`.
- **Same auth.** Send the same `authorization` metadata as the REST header (`Bearer <token>` or `Basic …`). Each RPC needs the same scope as its REST twin.
- **Errors map by meaning:**

  | gRPC code | REST status |
  |---|---|
  | `UNAUTHENTICATED` | 401 |
  | `PERMISSION_DENIED` | 403 |
  | `INVALID_ARGUMENT` | 400 |
  | `NOT_FOUND` | 404 |
  | `ALREADY_EXISTS` | 409, the path exists |
  | `FAILED_PRECONDITION` | other 409s, 412, 428, 422 |
  | `RESOURCE_EXHAUSTED` | 413 |
  | `UNAVAILABLE` | 503, or the server is shutting down |
- **Waiting.** `GetRunResult` and `InvokeFunction` answer `RunOrResult`: the `result` once the run has finished, otherwise the `run` as it is now. `wait_seconds` (at most 60) waits for the result first.
- **Streams resume** like their SSE twins: `after` takes the last line number or event `seq` you received.
- **Plain gRPC needs HTTP/2.** nbsandboxd accepts it without TLS (h2c), for a proxy in front that terminates TLS. Caddy forwards it with `transport http { versions h2c }`.

Go clients can import the generated package `github.com/azbyluthfan/nbsandbox/gen/nbsandbox/v1/nbsandboxv1connect`; [examples/go/grpcclient](../examples/go/grpcclient/main.go) runs a notebook with it. For other languages, generate from the `.proto` file. `make proto` regenerates the Go code.

## Viewing the spec

Any OpenAPI viewer works. Without installing anything:

```bash
docker run --rm -p 127.0.0.1:8088:8080 -e SWAGGER_JSON=/spec/openapi.yaml -v "$PWD/api:/spec:ro" swaggerapi/swagger-ui
```

Then open http://127.0.0.1:8088.

## Compatibility policy

**Before v1.0:**
- `/v1` can still change in a minor release (0.x).
- Every change is listed in [CHANGELOG.md](../CHANGELOG.md), with breaking ones marked.

**From v1.0:**
- **Changes to `/v1` are additive only.** New endpoints, new optional request fields, new response fields, new enum values in fields that the spec marks as open (`error_kind`).
- **Clients must ignore** response fields they don't know.
- **Never in `/v1`:** removing or renaming a field or endpoint, changing a field's type or meaning, making an optional field required, or adding a `status` value. Such changes need `/v2`.
- **Deprecation:** `/v1` would then keep working for at least 12 months after `/v2` ships. Deprecated endpoints answer with a `Deprecation` header and are marked in the spec.

**Not part of the API, and free to change:**
- `/metrics` names and labels follow the changelog but aren't covered by this policy.
- The web UI's `/ui/api/...` routes.
- The status file between the runner image and nbsandboxd, which has its own version ([releasing.md](releasing.md)).
