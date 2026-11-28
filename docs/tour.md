# A Tour of nbsandbox

A walk through what nbsandbox does, screen by screen: first the web editor, then the same things through the API, the way another service uses it. Every screenshot and every output below is real, taken from the development stack (`make up`) with the example notebooks in [examples/notebooks](../examples/notebooks). `make tour` retakes the screenshots (`TOUR_ONLY=sessions` only those of the live session, steps 13 to 15).

The example is a small sales analysis: [sales_report.ipynb](../examples/notebooks/sales_report.ipynb) reads `data/sales.csv`, summarises one region's last few months, returns the totals as JSON and writes a CSV file; [sales_by_month.ipynb](../examples/notebooks/sales_by_month.ipynb) sends the same numbers month by month as it computes them.

## Part 1: the web editor

The web UI ships with nbsandboxd. With `make up` it runs on http://127.0.0.1:8090 without sign-in: it acts as whichever user you pick. A deployment serves it with sign-in instead ([auth.md](auth.md#signing-in-to-the-web-ui)), and the user picker becomes the signed-in user's name and **Sign out**. Everything it does goes through the same API as any client.

### 1. Spaces

![The list of spaces](images/tour/01-spaces.png)

A **space** holds notebooks, data files and runs. It belongs to one user (here `alice`, picked at the top right); another user can't see it, list it or guess its contents.

### 2. A space

![The sales analysis space: its notebooks, recent runs and files](images/tour/02-space.png)

The space overview: its notebooks with their last run, and its files. The tree on the left is the same. Files are uploaded here, or through the API; their contents are stored in object storage, once each, however many files share them.

### 3. The notebook editor

![A notebook open in the editor](images/tour/03-editor.png)

Notebooks are edited cell by cell, with syntax highlighting. The **Run** bar at the top takes the run's parameters as JSON: they replace the values in the cell tagged `parameters`. The collapsed **Contract** panel holds the notebook's input and output schemas, which are checked before a run starts and after it ends.

### 4. Completion from the runner image

![Typing pd.read_ lists pandas' read functions with signatures and docs](images/tour/04-completion.png)

Completion knows the packages installed in the runner image, the exact versions notebooks run with: typing `pd.read_` lists pandas' readers, each with its signature and the first line of its documentation. The index is built into the image when it is built, so it always matches.

### 5. Documentation on hover

![Hovering read_csv shows its signature and summary](images/tour/05-hover.png)

Hovering a name shows its signature and summary.

### 6. A run, followed live

![The run page while the notebook works: the log as it is written](images/tour/06-run-live.png)

Pressing **Run** (here with `{"region": "bandung", "months": 4}`) starts a **run**: a fresh container with no network, its own copy of the space's files, and fixed limits on memory, CPU, processes and time. The log arrives line by line as the notebook writes it.

### 7. The result

![The finished run: log, returned value, usage and downloads](images/tour/07-run-result.png)

When it ends: the status, the value the notebook returned with `nbsandbox.output(...)`, how long it took and how much memory and CPU it used, the executed notebook, a rendered HTML copy, the log, and the **artifacts**, the files it wrote to its output folder (here `monthly.csv`). A failure says which cell failed and why, and whether it was the notebook's fault or the platform's.

### 8. The rendered notebook

![The executed notebook rendered as HTML, with the parameters cell papermill added](images/tour/08-rendered.png)

The executed notebook as HTML: the run's parameters appear in their own cell after the defaults. It is served in a sandbox where no script can run, whatever the notebook printed.

### 9. Publishing as a function

![The publish dialog: function name and the files to include](images/tour/09-publish.png)

A notebook becomes a **function** that other services call with JSON. Publishing copies the saved notebook and the files you pick into an immutable **version**: editing the notebook or its data afterwards changes nothing for callers until you publish again.

### 10. The function

![The function page: a form built from the contract, a ready-to-use curl command and the contract](images/tour/10-function.png)

The function's page builds a form from the input contract (`region` is one of three values; `months` is 1 to 12), and shows the `curl` command a service would use.

### 11. Calling it

![A call from the form: succeeded, with the returned JSON](images/tour/11-function-call.png)

Each call runs in its own fresh container, read-only, from the published version. Calls to the same function run side by side.

### 12. Runs and calls

![The runs panel: the run and the call, each with status and duration](images/tour/12-runs.png)

The **Runs** panel lists the space's runs and function calls, newest first, live.

### 13. A live session

![Cells run in a session: the DataFrame shown as a table under the printed text](images/tour/13-session.png)

A run starts from scratch every time. While writing a notebook, **Start session** gives it a live kernel instead ([sessions.md](sessions.md)): cells run one at a time (the play button or Shift-Enter), and the variables stay between them, so changing one cell doesn't run everything again. A session is held to the same isolation as a run: no network, the same limits, only its own space's files. It opens in about two seconds; after that a small cell answers in milliseconds ([perf.md](perf.md#interactive-sessions)). Read-write sessions save the files the kernel writes back to the space, every few minutes and at the end; read-only sessions can't change them.

### 14. Completion from the live objects

![Typing mine[" lists the DataFrame's columns, with what the kernel knows about it](images/tour/14-session-completion.png)

With a session, completion asks the kernel itself, so it knows the real objects: `mine["` lists the DataFrame's columns, which no index built ahead of time could know.

### 15. The variables

![The variables panel: each name with its type, shape and value; a DataFrame's columns](images/tour/15-variables.png)

**Variables** lists what the kernel holds, refreshed after every cell. **End session** stops the kernel; one with no cell running and nobody connected ends by itself after 30 minutes (`sessions.idle`).

### 16. Your connections

![The Connections page: a PostgreSQL connection and an HTTP API, where each points, their secrets never shown](images/tour/16-connections.png)

Where the operator turns them on, **Connections** holds your databases, buckets and APIs ([connections.md](connections.md)). You give each one's address and credential once; the secret goes to nbsandbox's secret provider and is never shown again.

### 17. A space's names for them

![A space's Connections table: the names crm and events, the environments dev and prod, the connection each means](images/tour/17-space-connections.png)

A space gives each connection a short name, per environment, so the same notebook reads development or production data: `nbsandbox.sql("SELECT … FROM crm.orders")`, or calls an API: `nbsandbox.http("events").get("orders")`. A run never sees the credential: a fetcher beside it reads the source, or adds the credential to the notebook's requests.

### 18. What a run uses

![A run's page: memory against its limit, CPU, and disk written, charted live](images/tour/18-run-usage.png)

A run's page names the worker it runs on and charts its memory (against its limit), CPU and disk writes, every few seconds while it runs and kept afterwards. This is `compute_demo.ipynb` from the examples, which uses each in turn.

### 19. The workers

![The Compute page: each worker, its runs and sessions of its capacity, and what they use now](images/tour/19-compute.png)

**Compute** lists the workers that run notebooks and sessions: their state, how many runs and sessions each holds of what it can take, the memory and CPU those use against what they reserve, and what they run on ([scaling.md](scaling.md)). Every signed-in user sees the counts; administrators also see whose runs and sessions are on each.

### 20. Sharing a space

![A space's Members section: alice, its creator, and bob with write; a field to grant someone a role](images/tour/20-members.png)

A space's **Members** lists who works in it. Its owners grant others `read`, `write` or `owner`, by caller ID; everyone sees every run in the space, and each member reads data with their own connections under the space's names ([sharing.md](sharing.md)).

## Part 2: the API

The same things from a service, with `curl`. The API is REST with an [OpenAPI spec](../api/openapi.yaml), and also gRPC ([api.md](api.md)). In development it takes basic auth; in production, bearer tokens from your identity provider ([auth.md](auth.md)).

```bash
export U=http://127.0.0.1:8080 A=alice:alice-dev-password
```

### What runs notebooks

```bash
curl -s -u $A $U/v1/runtime
```

```json
{
  "runtime": "runc",
  "image": {
    "ref": "nbsandbox-runner:dev",
    "id": "sha256:3c7a6e817519841da72fa990f8d9c5cef6abba06adc635e41e89a8b67196a5d2"
  },
  "python_version": "3.13.18",
  "limits": {
    "memory_bytes": 2147483648,
    "cpus": 1,
    "pids": 256,
    "tmp_bytes": 268435456,
    "timeout_seconds": 600,
    "log_bytes": 10485760,
    "max_file_bytes": 52428800,
    "max_space_bytes": 524288000,
    "concurrency": 2
  },
  "packages": { "asttokens": "3.0.2", "attrs": "26.1.0", "beautifulsoup4": "4.15.0", "…": "…" }
}
```

The image, its Python, its packages and the limits every run gets. Requests can't change the limits.

### Start a run

```bash
curl -s -u $A -H 'Content-Type: application/json' -H 'Idempotency-Key: tour-1' \
  -d '{"notebook":"sales_report.ipynb","parameters":{"region":"jakarta","months":3}}' \
  $U/v1/spaces/$S/runs
```

```json
{"id":"e768c889-c007-4fb9-9fcc-fe97f08ade16","space_id":"f87d8e2a-6c51-48be-bcf8-1abd7a157da6","notebook":"sales_report.ipynb","notebook_sha256":"7f4525dd0ddc1bb22b766ab97aa48c8db0d0c371a654fc3de10338570c8b3f83","read_only_space":false,"parameters":{"months":3,"region":"jakarta"},"status":"queued","retryable":false,"queue_position":1,"created_at":"2026-11-08T05:46:46.494Z"}
```

The `Idempotency-Key` makes a retry safe: the same key returns the same run instead of starting a second one.

### Follow its log

```bash
curl -s -N -u $A $U/v1/spaces/$S/runs/$RUN/log/stream
```

```text
: nbsandbox log stream

id: 1
event: line
data: Input Notebook:  /run/input.ipynb

id: 2
event: line
data: Output Notebook: /run/output.ipynb

id: 3
event: line
data: Working directory: /space
…
```

Server-Sent Events: each line has an id, so a client that reconnects with `Last-Event-ID` continues without gaps or duplicates.

### Get the result

```bash
curl -s -u $A "$U/v1/spaces/$S/runs/$RUN/result?wait=30s"
```

```json
{
  "run_id": "e768c889-c007-4fb9-9fcc-fe97f08ade16",
  "status": "succeeded",
  "output": {
    "region": "jakarta",
    "months": 3,
    "total": 489.0,
    "by_month": [
      { "month": "2026-04", "amount": 160.0 },
      { "month": "2026-05", "amount": 158.0 },
      { "month": "2026-06", "amount": 171.0 }
    ]
  }
}
```

`?wait=30s` answers as soon as the run finishes, so a client doesn't need to poll. The artifact:

```bash
curl -s -u $A $U/v1/spaces/$S/runs/$RUN/artifacts/monthly.csv
```

```text
region,month,amount
jakarta,2026-04,160.0
jakarta,2026-05,158.0
jakarta,2026-06,171.0
```

What the run used, from `GET …/runs/$RUN`:

```json
{
  "status": "succeeded",
  "usage": { "duration_seconds": 4.74, "peak_memory_bytes": 123797504, "cpu_seconds": 4.31 },
  "runtime": "runc",
  "limits": { "memory_bytes": 2147483648, "cpus": 1, "pids": 256, "timeout_seconds": 600 }
}
```

### Call a function

```bash
curl -s -u $A -H 'Content-Type: application/json' \
  -d '{"parameters":{"region":"jakarta","months":2}}' \
  "$U/v1/spaces/$S/functions/sales_report/invoke?wait=30s"
```

```json
{
  "run_id": "6e236877-9406-4f04-9633-a2a02f2e03ff",
  "status": "succeeded",
  "function": "sales_report",
  "version": 1,
  "output": {
    "region": "jakarta",
    "months": 2,
    "total": 329.0,
    "by_month": [
      { "month": "2026-05", "amount": 158.0 },
      { "month": "2026-06", "amount": 171.0 }
    ]
  }
}
```

Parameters that break the contract are refused before anything runs:

```bash
curl -s -u $A -H 'Content-Type: application/json' \
  -d '{"parameters":{"region":"medan"}}' \
  "$U/v1/spaces/$S/functions/sales_report/invoke?wait=30s"
```

```json
HTTP 400
{"error":{"code":"invalid_parameters","message":"parameters don't match the notebook's input contract: /region: value must be one of 'jakarta', 'bandung', 'surabaya'","problems":[{"pointer":"/region","message":"value must be one of 'jakarta', 'bandung', 'surabaya'"}]}}
```

### A function that streams its output

`sales_by_month` declares a streamed output and sends one item per month with `nbsandbox.emit(...)`. Asking for `text/event-stream` gets the items as they are made (the times on the left were added by the client):

```bash
curl -s -N -u $A -H 'Content-Type: application/json' -H 'Accept: text/event-stream' \
  -d '{"parameters":{"region":"bandung","months":4}}' \
  $U/v1/spaces/$S/functions/sales_by_month/invoke
```

```text
  0.0s  : nbsandbox result stream
  3.2s  event: item
  3.2s  data: {"index":1,"total":4,"item":{"month":"2026-03","amount":90.0,"running_total":90.0}, …}
  3.7s  event: item
  3.7s  data: {"index":2,"total":4,"item":{"month":"2026-04","amount":95.0,"running_total":185.0}, …}
  4.2s  event: item
  4.2s  data: {"index":3,"total":4,"item":{"month":"2026-05","amount":99.0,"running_total":284.0}, …}
  4.7s  event: item
  4.7s  data: {"index":4,"total":4,"item":{"month":"2026-06","amount":104.0,"running_total":388.0}, …}
  6.8s  event: result
  6.8s  data: {"status":"succeeded","output":[…the four items…],"items":4, …}
```

The first item arrives about 3 seconds after the call (the container starts and the kernel loads pandas), then one every half second, as the notebook produces them.

### Another user sees nothing

```bash
curl -s -u bob:bob-dev-password $U/v1/spaces/$S/runs/$RUN/log
```

```json
HTTP 404
{"error":{"code":"not_found","message":"not found"}}
```

Another user's space doesn't exist as far as bob is concerned: `404`, never `403`, so its existence isn't revealed either.

## Next

- **Try it yourself:** the [README's quickstart](../README.md#quickstart-local-docker-desktop-or-linux), then [examples/](../examples/) for complete clients in Python and Go.
- **How isolation is checked:** the [isolation reports](isolation-report.md) list every check and what it observed.
- **Deploying:** [deployment.md](deployment.md) (one server), [kubernetes.md](kubernetes.md), [gvisor.md](gvisor.md).
