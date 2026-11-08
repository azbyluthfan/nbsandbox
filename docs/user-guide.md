# Using nbsandbox

For people who run notebooks on an nbsandbox server. The examples use `curl` and the small client `scripts/nbsandbox.sh`. The full API is in [api.md](api.md).

```bash
export NBSANDBOX_URL=https://nbsandbox.example.com NBSANDBOX_USER=alice NBSANDBOX_PASSWORD=...
```

Every request uses HTTP basic auth. Another user's spaces never show up: they answer `404`, as if they didn't exist.

## Spaces

A **space** is a folder for one project: notebooks, data files and run outputs. Code running in one space can't see any other space.

```bash
scripts/nbsandbox.sh spaces create sales-forecast       # → {"id":"…","name":"sales-forecast",…}
scripts/nbsandbox.sh spaces list
```

A space can hold up to 500 MB by default (`uploads.max_space`), **including run outputs**. Deleting a space deletes everything in it.

## Files

```bash
scripts/nbsandbox.sh files upload "$ID" forecast.ipynb data.csv
curl -u "$NBSANDBOX_USER:$NBSANDBOX_PASSWORD" -F file=@q1.csv "$NBSANDBOX_URL/v1/spaces/$ID/files?folder=data"
scripts/nbsandbox.sh files list "$ID"
```

- **All or nothing:** an upload either stores every file or none.
- **Replacing:** uploading a file with an existing name replaces it.
- **Folders:** use `?folder=`. A filename itself may not contain `/` or `..`.
- **Limits:** 50 MB per file by default (`uploads.max_file`). Notebooks must be nbformat 4.
- **Python only:** runs use the Python 3 kernel. You can store any notebook, but running or publishing one whose kernel is another language (its `kernelspec` or `language_info` says R, Julia, …) is refused with `400`. A notebook that names no kernel is treated as Python.
- **Reserved names:** names starting with `.nbsandbox` are reserved.

Notebooks read files with paths relative to the space, e.g. `pd.read_csv("data/q1.csv")`. The working directory is the space.

### Editing and managing files

```bash
scripts/nbsandbox.sh files list "$ID"                          # files and folders, each file with its version
scripts/nbsandbox.sh files put "$ID" src/model.py model.py new  # create (fails if it exists)
scripts/nbsandbox.sh files put "$ID" src/model.py model.py 5b-mx2  # replace the version you have
scripts/nbsandbox.sh files move "$ID" src/model.py src/forecast.py
scripts/nbsandbox.sh files new-folder "$ID" data/2026
scripts/nbsandbox.sh files delete-folder "$ID" data/2026       # a folder and everything in it
```

- **Versions:** every file has a **version** that changes whenever the file does. You get it in the listing, and as the `ETag` header when you download the file.
- **Saving:** `PUT .../files/{path}` saves a file. Replacing one needs `If-Match` with the version you are replacing. If someone saved it since, you get `412` with the current version: reload, then save again (or save as a new file). Without `If-Match` you get `428`. `If-None-Match: *` creates a file only if it doesn't exist.
- **Rename or move:** `PATCH .../files/{path}` with `{"path": "new/place"}` renames or moves a file or folder. The target must not exist (`409`).
- **Folders:** `POST .../folders` with `{"path": "..."}` creates an empty one. `DELETE .../files/{path}` deletes a file or an empty folder; add `?recursive=true` for a folder with files in it.

## Runs

```bash
RUN=$(scripts/nbsandbox.sh runs start "$ID" forecast.ipynb '{"region":"EU","months":6}' | sed 's/.*"id":"\([^"]*\)".*/\1/')
scripts/nbsandbox.sh runs follow "$ID" "$RUN"
```

**Parameters** are injected by [papermill](https://papermill.readthedocs.io/): tag a cell `parameters` in your notebook with the defaults. The values you send are added in a new cell after it. Names must be Python identifiers; values can be any JSON (strings, numbers, booleans, `null`, nested objects and arrays), at most 32 levels deep and 1 MiB in total by default (`runs.max_parameters`). `nbsandbox.parameters()` returns them exactly as sent.

**Snapshot:** a run executes the notebook **as it was when you started it**. Editing the file afterwards doesn't change a queued or running run.

**Returning a value:** call `nbsandbox.output(value)` in the notebook. When the run succeeds, `GET .../runs/{id}/result` returns it. For publishing a notebook as a function other services call, see [functions.md](functions.md).

**Returning items as they're ready:** instead of one value, a notebook can stream its output with `nbsandbox.emit(item)`, one call per item. Callers following the result get each item at once (see [api.md §6](api.md#6-follow-progress-which-stream-to-use)), and the result's `output` is the list of items. A notebook either returns one value or streams items, never both (calling the other one raises in that cell). If it fails part-way, the items it sent are kept in the result, marked partial. Items convert like `nbsandbox.output`, at most 64 KiB each and 1,000 per run.

**Progress:** `nbsandbox.progress(0.4, "3 of 8 regions")` tells callers watching (`?include=progress`) how far along it is. It never changes the result.

```python
import nbsandbox

for i, region in enumerate(regions):
    nbsandbox.emit({"region": region, "total": summarise(region)}, total=len(regions))
    nbsandbox.progress((i + 1) / len(regions), f"{i + 1} of {len(regions)} regions")
```

**Queue:** runs wait in a first-in, first-out queue. The server runs a fixed number at a time (`runs.concurrency`, 2 by default). A space has **one active run that can change its files at a time**: starting another while one is queued or running gives `409`. Runs started with `"space": "read-only"`, and function calls, see the files read-only and can run alongside. `queue_position` says where a queued run is (1 = next).

**Retrying safely:** send an `Idempotency-Key` header. Repeating the same request with the same key within 24 hours returns the original run instead of starting a second one.

**Cancel:** `scripts/nbsandbox.sh runs cancel "$ID" "$RUN"`. The kernel gets 5 seconds to stop, then it's killed. An optional body `{"reason": "..."}` is recorded with the result, along with who cancelled.

**Waiting for the end:** `GET .../runs/{id}/result?wait=30s` holds the request until the run finishes (at most 60 s). `GET .../runs/{id}/events` streams its status changes (Server-Sent Events), and `GET .../spaces/{id}/events` streams those of every run in the space. See [functions.md](functions.md#5-follow-status-server-sent-events).

### What a run gets

Each run starts in a **fresh container** from the server's runner image:
- **No network.** Packages can't be installed at run time.
- **Only its own space's files**, at `/space`, its working directory: a copy taken when the run starts (read-only for function calls, or when the run asks for it). A run that can write the space has its changes written back when it ends; a file you edited meanwhile keeps your edit, and the run's version is kept beside it (`name.conflict-<run>.ext`). See [api.md](api.md#3-edit-a-file-without-overwriting-someone-elses-changes). Its outputs go to `/run`, its own folder: other runs and published functions don't exist inside. The rest of the file system is read-only, except `/tmp`.
- **Limits** (defaults; see `GET /v1/runtime` for this server's):

| Resource | Limit | When exceeded |
|---|---|---|
| Memory | 2 GiB, no swap | status `oom` |
| `/tmp` | 256 MiB, **counts toward memory** | write error in your code; possibly `oom` |
| CPU | 1 core | slower, never stopped |
| Processes | 256 | `failed` with `error_kind: resource_pids` |
| Time | 10 minutes | status `timeout` |
| Disk | the space's quota (500 MB), checked every few seconds | `failed` with `error_kind: resource_disk`; its changes to the space are not stored |

### Packages

The runner image has a fixed set of packages. `GET /v1/runtime` lists them with their versions, and the Python version:

```bash
curl -s -u "$NBSANDBOX_USER:$NBSANDBOX_PASSWORD" "$NBSANDBOX_URL/v1/runtime"
```

When you start a run, nbsandbox reads the notebook's imports and the space's `requirements.txt`, if there is one, and reports packages the image doesn't have in `dependency_warnings`. The run still starts, and probably fails with `ModuleNotFoundError` (`error_kind: missing_package`). An operator can set `runs.dependency_check: reject` to refuse such runs with `422` instead. To get a package added, ask the operator ([operations.md](operations.md#runner-image)).

## Results

| Status | Meaning | Whose fault |
|---|---|---|
| `queued`, `running` | not finished | |
| `succeeded` | every cell ran | |
| `failed` | a cell raised an exception (see `result.error`), a resource limit was hit, or the returned value was invalid (`output_invalid`, `output_too_large`) | notebook |
| `oom` | memory limit reached | notebook |
| `kernel_died` | the kernel crashed | notebook |
| `timeout` | time limit reached | notebook |
| `cancelled` | you cancelled it | caller |
| `interrupted` | nbsandboxd or Docker stopped mid-run | platform: try again |
| `platform_error` | the run couldn't be started or finished (e.g. a missing runner image) | platform: ask the operator |

```bash
scripts/nbsandbox.sh runs get "$ID" "$RUN"        # status, result, usage, error
```

`result.error.cell_index` is **0-based and counts the cells of your notebook** as you uploaded it (the parameters cell papermill adds isn't counted). `result.usage` has duration, peak memory and CPU time.

Outputs, for any run that got as far as running:
- **`output.ipynb`:** the executed notebook. For a failed, `oom` or `timeout` run it holds the cells that completed (`result.partial: true`). Stored cell output is capped at 5 MiB in total; beyond that a note replaces further output. The full console output is in the log.
- **`output.html`:** the rendered notebook (`result.rendered`). It opens in the browser under a sandbox policy: no scripts run and it can't reach anything else.
- **Artifacts:** files the notebook wrote into the folder named by the `NBSANDBOX_OUTPUT_DIR` environment variable (the run's own `artifacts/` folder). They're listed under `/artifacts` and kept with the run. Files written anywhere else in the space (when it is writable) become ordinary space files. `POST .../runs/{id}/promote` with `{"artifact": "model.pkl", "to": "models/model.pkl"}` copies an artifact into the space's files.
- **Result:** `GET .../runs/{id}/result` gives the value returned with `nbsandbox.output` for a succeeded run, or an error object with the reasons (kind, fault, `retryable`, failing cell, who cancelled) for any other status.
- **Log:** `GET …/log`, the console output (stdout and stderr), capped at 10 MiB (`result.log_truncated`).

Runs are deleted automatically after 30 days by default (`runs.retention`), or by you with `DELETE …/runs/{run_id}`.

## Live logs

`GET …/log/stream` is a Server-Sent Events stream:
- **Event `line`:** one per line of console output. Its `id` is the line number.
- **Order:** lines already written come first, then live lines.
- **Event `done`:** carries the final status, and the stream ends.

```bash
curl -N -u "$NBSANDBOX_USER:$NBSANDBOX_PASSWORD" "$NBSANDBOX_URL/v1/spaces/$ID/runs/$RUN/log/stream"
```

- **Reconnect:** send `Last-Event-ID: <last id you got>` (browsers do this on their own) and the stream continues with no gap and no duplicates.
- **Slow viewers:** a viewer reads at its own pace and is not disconnected; it gets every line once it reads again. (Clients should still handle the older `dropped` event by reconnecting.)
- **Event `shutdown`:** the server is restarting. Reconnect later.
- **Effect on the run:** none. A slow or disconnected viewer never slows it down.

A notebook's output appears when Python writes it. Use `print(..., flush=True)` for progress lines that should show up immediately.

## Local web UI (development only)

What it can do:
- **Browse and manage files:** a folder tree with upload (button, or drag and drop onto a folder), new file, new folder, rename or move, and delete (a folder asks first and says how many files it holds).
- **Edit:** notebooks cell by cell (add, move, delete, code or markdown, the `parameters` tag, the contract) and text files, with syntax highlighting. Python syntax errors are underlined as you type, and imports the runner image doesn't have get a warning. Typing `pd.` (after `import pandas as pd` in any cell) lists what the runner image's pandas offers, with signatures and docs; `Ctrl+Space` opens the list anywhere, and hovering a library name shows its docs. Completion knows modules and the names your cells define, not the types of your variables. You edit the version you opened: if someone saves the file meanwhile you see a notice, and saving asks whether to reload, save a copy, or overwrite.
- **Run and publish:** run a notebook (optionally read-only) and follow the live log; publish it as a function and call it from a form built from its input schema.


When enabled by whoever runs nbsandbox on their own machine, a small web UI at `http://127.0.0.1:8090` lets you pick a user and a space, browse files, read notebooks, run one, and follow the live log. It has **no login**, so it is never part of a production deployment.
