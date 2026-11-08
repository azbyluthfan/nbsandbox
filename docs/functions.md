# Calling a notebook from your service

Build a model in a notebook, publish it, and let another app call it with JSON and get JSON back. Each call runs in its own fresh, isolated container, with the same limits, logs and statuses as any run. This guide covers the notebook side, publishing, calling, following status, and handling errors.

**Expect seconds, not milliseconds.** Every call starts a fresh container and Python kernel: today about 3–5 s before the first cell runs. That suits batch-style calls. Faster starts are on the [roadmap](../ROADMAP.md).

## 1. The notebook

```python
# cell tagged "parameters": defaults, replaced by the caller's values
region = "jakarta"
cfg = {"months": 6, "include": ["sales"]}

# ... load the model and compute ...
import pickle
import nbsandbox, pandas as pd
with open("models/model.pkl", "rb") as f:     # files are read relative to the space
    model = pickle.load(f)
forecast = model.predict(cfg["months"])
df = pd.DataFrame({"month": range(1, cfg["months"] + 1), "forecast": forecast})

nbsandbox.output({"region": region, "forecast": forecast, "table": df})
```

- **Language:** Python 3 only. Publishing a notebook written for another kernel (R, Julia, …) is refused with `400`.
- **Input:** the caller's `parameters` (a JSON object). Each top-level key becomes a Python variable (so keys must be valid Python names), placed after the cell tagged `parameters`. Values can be any JSON: nested objects and arrays, `null` (→ `None`), at most 32 levels deep and 1 MiB in total by default. `nbsandbox.parameters()` returns the exact JSON when you need it, for example big integers or keys you didn't declare.
- **Output:** `nbsandbox.output(value)` takes any JSON-serialisable value and converts the usual types:

  | You pass | The caller gets |
  |---|---|
  | numpy numbers, booleans, arrays | numbers, booleans, (nested) lists |
  | pandas DataFrame | a list of records: `[{"column": value, ...}, ...]` |
  | pandas Series, Index | a list |
  | datetime, date, time, Timestamp | ISO 8601 strings |
  | Decimal | a number |
  | tuple, set | a list |
  | NaN, Infinity | `null` |

  Anything else raises `TypeError` in that cell, so the call fails with a clear traceback. Calling `nbsandbox.output` again replaces the earlier value. The limit is 1 MiB by default (`runs.max_output`); write larger results to a file in `NBSANDBOX_OUTPUT_DIR` (an artifact) instead.
- **Or a stream of items:** instead of one value, a notebook can send its output as items, as each is ready, with `nbsandbox.emit(item)` (same conversions; optionally `total=` how many to expect). The caller gets each item at once, for example to draw a chart as data arrives, and the result's `output` is the list of items. A notebook does one or the other, never both; see the contract below and [5. Streamed output](#5-streamed-output).
- **Progress:** `nbsandbox.progress(fraction, message)` tells callers watching how far along it is. It never changes the result.
- **Files:** a call sees the published bundle (step 3) read-only, as its working directory. It can write only to `NBSANDBOX_OUTPUT_DIR` (artifacts), `/tmp`, and through `nbsandbox.output`.

## 2. The contract

Every function has a contract: what it accepts, and how it returns its result. Declare it as JSON Schemas (2020-12) in the notebook's metadata (`metadata.nbsandbox.contract`), or in the publish request (`"contract": {...}`, which replaces the notebook's without changing the file):

```json
"metadata": {"nbsandbox": {"contract": {
  "input":  {"type": "object", "required": ["region"], "properties": {
               "region": {"type": "string"},
               "cfg": {"type": "object", "properties": {"months": {"type": "integer", "minimum": 1}}}}},
  "output": {"type": "object", "required": ["forecast"]}
}}}
```

`output` is how the result comes back: a JSON Schema means **one value** (`nbsandbox.output`). For a **stream of items** (`nbsandbox.emit`), declare the mode and each item's schema:

```json
"output": {"mode": "stream", "item": {"type": "object", "required": ["region", "total"]}}
```

- **Without a contract:** any JSON object in, and the mode comes from the notebook's calls: one value if it calls `nbsandbox.output` (or nothing), a stream if it calls `nbsandbox.emit`.
- **Checked when publishing:** nbsandbox scans the notebook's cells for the two calls. A notebook that calls both, or contradicts its declared mode, is refused with `400`, naming the cells. `functions.require_schemas: true` also refuses a function without input and output schemas.
- **Checked while running** (what a scan can't see, such as calls through another variable): calling the other mode's function fails the call as `output_invalid`.
- **Bad parameters** are rejected before anything runs: `400 invalid_parameters`, with a `problems` list of JSON pointers and messages.
- **An output that doesn't match** (or no output when one is declared), or an item that doesn't match `item`, ends the call as `failed` with `error.kind: output_invalid`.
- **Callers can read the contract:** `GET .../functions/{name}/contract` gives `input`, `mode` (`single` or `stream`), `output` or `item`, and `mode_inferred` (true when the mode came from the notebook's calls), for example to generate types.
- **References:** schemas can only refer to themselves (`"$ref": "#/$defs/..."`), never to files or URLs.

## 3. Publish

```bash
curl -u alice:… -H 'Content-Type: application/json' \
  -d '{"notebook": "forecast.ipynb", "files": ["models/model.pkl", "data/lookup.csv"]}' \
  https://nbsandbox.example.com/v1/spaces/$SPACE/functions/forecast/versions
```

- **What a version is:** publishing copies the notebook, its contract and the listed files and folders (at their paths in the space) into an **immutable version**: 1, then 2, and so on.
- **What callers see:** saving the notebook, retraining the model or editing data in the space changes nothing for callers until you publish again.
- **Quota:** versions count toward the space's quota. `DELETE .../functions/{name}` removes a function and all its versions; its past calls and their results stay.
- **Names:** function names are lowercase letters, digits, `-` and `_`.

**Credentials:** a calling service sends a bearer token from your identity provider. Calling on behalf of a signed-in user, it exchanges the user's token for one issued to nbsandbox (On-Behalf-Of or token exchange) and needs the scopes `nbsandbox.functions.invoke nbsandbox.read`; the function's space must belong to that user. See [auth.md](auth.md). In development, basic auth with a `users.yaml` user works the same way.

## 4. Call

```bash
# Wait up to 30 s for the result (max 60 s):
curl -u svc:… -H 'Content-Type: application/json' -d '{"parameters": {"region": "bandung", "cfg": {"months": 4}}}' \
  "https://nbsandbox.example.com/v1/spaces/$SPACE/functions/forecast/invoke?wait=30s"
```

- `.../functions/forecast/invoke` calls the **latest** version; `.../functions/forecast/versions/2/invoke` pins one.
- **Without `wait`:** `201` with the run. Then follow it (step 6) or fetch `GET .../runs/{id}/result`.
- **With `wait`:** `200` with the result if it finished in time, otherwise `202` with the run as it is. Call `GET .../runs/{id}/result?wait=30s` to keep waiting.
- **Retries:** send an `Idempotency-Key` header to retry safely. The same key and body within 24 hours return the original call.
- **Concurrency:** calls of one space run side by side, within the server's overall limit (`runs.concurrency`).

### The result

Every finished call has the same shape: the `output` if it succeeded, an `error` with the reasons otherwise. Never both.

```json
{"run_id": "…", "status": "succeeded", "function": "forecast", "version": 2,
 "output": {"region": "bandung", "forecast": [1.5, 3.0, 4.5, 6.0]}}
```
```json
{"run_id": "…", "status": "failed", "function": "forecast", "version": 2,
 "error": {"kind": "execution_error", "fault": "notebook", "retryable": false,
           "message": "ValueError in cell 1: months must be positive",
           "cell_index": 1, "exception_type": "ValueError", "exception_message": "months must be positive"}}
```

| `status` | What it means | `retryable` |
|---|---|---|
| `succeeded` | `output` holds the value | — |
| `failed` | the notebook raised (`execution_error`), hit a limit (`resource_pids`, `resource_tmp`, `resource_disk`), returned a bad value (`output_invalid`, `output_too_large`), or imports a missing package (`missing_package`) | no |
| `oom` | out of memory; `error` has the peak and the limit | no |
| `kernel_died` | the kernel crashed | no |
| `timeout` | over the time limit | no |
| `cancelled` | cancelled; `error.cancelled_by` and `cancel_reason` say who and why | no |
| `interrupted` | nbsandboxd or Docker stopped mid-call | **yes** |
| `platform_error` | couldn't start or finish for platform reasons | **yes** |

`cell_index` is 0-based and counts the notebook's own cells. Retry only when `retryable` is true; the others will fail the same way with the same input.

**Cancel:** `POST .../runs/{id}/cancel`, optionally with `{"reason": "superseded"}`. The reason is returned in the result.

## 5. Streamed output

A function whose contract mode is `stream` returns items as they are made. Ask for the result as a stream: `Accept: text/event-stream` (or `?stream=1`) on `POST …/invoke` or `GET …/runs/{id}/result`.

```
id: 1
event: item
data: {"run_id":"…","kind":"item","index":1,"total":3,"item":{"region":"jakarta","total":1200}}

id: 2
event: item
data: {"run_id":"…","kind":"item","index":2,"total":3,"item":{"region":"bandung","total":800}}

event: result
data: {"run_id":"…","status":"succeeded","items":3,"output":[{"region":"jakarta",…},{"region":"bandung",…},{…}]}
```

- **One result, two forms:** the result's `output` is exactly the list of items the stream sent; item `index` *i* is `output[i-1]`. Read the stream while the call works, or the JSON result afterwards: same data.
- **Identity:** an item is identified by the run ID and its index. Resume a dropped stream with `Last-Event-ID` (the last index you have); a finished call's stream replays its items and ends.
- **If the call fails after sending items,** the result says why (`error`, with `retryable`) and `output` holds the items so far, with `"partial": true`. Whatever you received can always be fetched again; call again if you need the rest (a new run, with a new ID).
- **Limits:** an item is at most 64 KiB, at most 1,000 items, all of them together within `runs.max_output` (1 MiB by default). `nbsandbox.emit` paces itself to 10 a second. An item over a limit fails the call (`output_too_large`); items are never silently dropped. Large results belong in artifacts.
- **Progress:** add `?include=progress` to get `progress` events on the same stream.
- **gRPC:** `RunService.FollowRunResult` and `FunctionService.InvokeFunctionStream`.

## 6. Follow status (Server-Sent Events)

**One call:** `GET .../runs/{id}/events`. It sends one `status` event per change, then ends after the final one, which carries the result:

```
id: 41
event: status
data: {"run_id":"…","status":"queued","function":"forecast","version":2,"queue_position":1}

id: 42
event: status
data: {"run_id":"…","status":"running","function":"forecast","version":2}

id: 45
event: status
data: {"run_id":"…","status":"succeeded","function":"forecast","version":2,"result":{…}}
```

**Every call in a space:** `GET /v1/spaces/{id}/events?function=forecast` streams status changes for all calls (or one function's) on a single connection, until you disconnect. A service making many calls keeps one stream open instead of polling.

**Reconnecting:**
- Send `Last-Event-ID` with the last `id` you got (browsers' `EventSource` does this by itself). The stream continues with nothing lost.
- Events are kept 24 hours (`runs.event_retention`). If you resume from before that, you get a `pruned` event: re-read the calls you were waiting for.
- A `shutdown` event means the server is restarting: reconnect.
- Heartbeat comments every 15 s keep proxies from closing idle streams.

## 7. Examples

**Python:**

```python
import requests

S = requests.Session()
S.auth = ("svc", "…")
BASE = "https://nbsandbox.example.com/v1/spaces/" + SPACE

r = S.post(f"{BASE}/functions/forecast/invoke", params={"wait": "50s"},
           json={"parameters": {"region": "bandung", "cfg": {"months": 4}}},
           headers={"Idempotency-Key": request_id}, timeout=60)
if r.status_code == 400:
    raise ValueError(r.json()["error"])          # problems: where the input broke the contract
r.raise_for_status()
res = r.json()
while r.status_code == 202:                      # not finished yet: keep waiting
    r = S.get(f"{BASE}/runs/{res['id']}/result", params={"wait": "50s"}, timeout=60)
    res = r.json()
if res["status"] == "succeeded":
    print(res["output"])
elif res["error"]["retryable"]:
    ...                                           # try again later
else:
    raise RuntimeError(res["error"]["message"])
```

**Go:**

```go
body := strings.NewReader(`{"parameters": {"region": "bandung"}}`)
req, _ := http.NewRequest("POST", base+"/functions/forecast/invoke?wait=50s", body)
req.SetBasicAuth("svc", password)
req.Header.Set("Content-Type", "application/json")
resp, err := (&http.Client{Timeout: 60 * time.Second}).Do(req)
// 200: decode {"status", "output" | "error"}; 202: poll GET .../runs/{id}/result?wait=50s
```

**Shell** (`scripts/nbsandbox.sh`):

```bash
scripts/nbsandbox.sh functions publish "$SPACE" forecast forecast.ipynb models/model.pkl
scripts/nbsandbox.sh functions invoke "$SPACE" forecast '{"region": "bandung"}' 30
```

Complete, runnable clients are in [examples/](../examples/). The full API is in [api.md](api.md) and [api/openapi.yaml](../api/openapi.yaml).
