# Examples: another app calling nbsandbox

Small, complete clients showing what an application does to run notebooks in nbsandbox. Each one:

1. authenticates (a bearer token from your identity provider, or basic auth in development);
2. creates a space and uploads a notebook and the data it reads;
3. starts a run with parameters, with an `Idempotency-Key` so a retry can't start it twice;
4. waits for the run and prints what the notebook returned (`nbsandbox.output`).

| Example | Waits with | Also shows |
|---|---|---|
| [python/run_notebook.py](python/run_notebook.py) (`requests`) | long-polling: `GET …/result?wait=50s` | exchanging a user's token for an nbsandbox token (Entra ID On-Behalf-Of), downloading an artifact, publishing the notebook as a function and calling it, calling a function that streams its output |
| [go/runnotebook](go/runnotebook/main.go) (standard library) | the result stream (`GET …/result` as Server-Sent Events) | reading SSE without a library; items of a streamed output as they arrive (`-stream`), resuming with `Last-Event-ID` |
| [go/grpcclient](go/grpcclient/main.go) (gRPC, the generated client) | `FollowRunResult` | uploading in chunks over a client stream; `-stream` as above |

One more works with a notebook's live kernel instead of runs, as the web editor does ([docs/sessions.md](../docs/sessions.md)): [python/interactive_session.py](python/interactive_session.py) (`requests`, `websocket-client`) opens the notebook's session, connects to its WebSocket (Jupyter messages as JSON), runs cells whose variables stay between them, asks the kernel for a DataFrame's columns and a function's docs, stops a long cell, saves a file the kernel wrote to the space, and ends the session.

The notebooks:

- [notebooks/sales_report.ipynb](notebooks/sales_report.ipynb) takes `region` and `months`, reads `data/sales.csv`, returns the totals as one value (`nbsandbox.output`), and writes `monthly.csv` as an artifact. Its input contract only allows the three regions in the data, so a wrong value is refused with `400 invalid_parameters` before anything runs.
- [notebooks/sales_by_month.ipynb](notebooks/sales_by_month.ipynb) takes the same input but streams its output: one item per month (`nbsandbox.emit`), half a second apart, as a chart would draw them. Its contract declares `{"mode": "stream", "item": …}`; the result's `output` is the list of the items.
- [notebooks/sales_dashboard.ipynb](notebooks/sales_dashboard.ipynb) draws charts with matplotlib from 26 weeks of a made-up chain of stores in Java (`data/orders.csv`, `data/stores.csv`): weekly revenue per province with 4-week averages, revenue by month and category (stacked), each store's revenue and growth, and each category's weekly units. Every chart is shown in the notebook (and `output.html`) and kept as a PNG artifact; the totals come back as JSON. Inputs: `provinces` (a subset of the six) and `weeks`.
- [notebooks/sales_map.ipynb](notebooks/sales_map.ipynb) draws a map of the same stores with matplotlib alone (runs have no network, so no map tiles): Java's provinces shaded by `metric` (`revenue`, `units` or `growth`), each store a bubble sized by revenue and coloured by growth, with labels, a scale bar and a north arrow. It keeps `map.png` and `provinces.geojson` (the provinces with their figures, for QGIS or any GIS tool) as artifacts.

- [notebooks/compute_demo.ipynb](notebooks/compute_demo.ipynb) runs for about two minutes in three phases (memory growing to about 480 MiB, a busy CPU, 160 MiB written to the space and removed), so a run's usage charts and the web UI's **Compute** page have something to show. Start it in several spaces at once to see runs spread over the workers, and queue when they're all busy.

The data: `sales.csv` is the small table the first two use; `orders.csv` and `stores.csv` are synthetic, written by [data/make_orders.py](notebooks/data/make_orders.py) (deterministic: run it again for the same files; the store locations are the real cities'); `java_provinces.geojson` is Java's six provinces from [Natural Earth](https://www.naturalearthdata.com/) (public domain), simplified to 13 KB.

## Running them

Against the development stack (`make up`), from the repository root:

```bash
export NBSANDBOX_URL=http://127.0.0.1:8080 NBSANDBOX_USER=alice NBSANDBOX_PASSWORD=alice-dev-password
```

```bash
python examples/python/run_notebook.py --region bandung --months 4
```

```bash
python examples/python/interactive_session.py
```

```bash
go run ./examples/go/runnotebook -region surabaya
```

```bash
go run ./examples/go/runnotebook -stream
```

The chart and map notebooks need no client: `make seed` puts them in alice's **Sales analysis** space with their data and runs them once, so the web UI shows their charts and the map. Or run one by hand:

```bash
scripts/nbsandbox.sh runs start "$ID" sales_map.ipynb '{"metric": "growth"}'
```

```bash
go run ./examples/go/grpcclient -stream
```

With tokens instead (see [docs/auth.md](../docs/auth.md)):

- `NBSANDBOX_TOKEN=<token issued for nbsandbox>`: used as is.
- Python only: `USER_TOKEN=<a signed-in user's token for your app>` plus `AZURE_TENANT_ID`, `APP_CLIENT_ID`, `APP_CLIENT_SECRET` and `NBSANDBOX_SCOPES` (e.g. `api://<nbsandbox client id>/nbsandbox.read api://<nbsandbox client id>/nbsandbox.runs`): the example exchanges the user's token for an nbsandbox token and caches it until shortly before it expires. nbsandbox then sees the user, so the space belongs to them.

The token needs the scopes for what the example does: `nbsandbox.spaces.write`, `nbsandbox.files.write`, `nbsandbox.runs` and `nbsandbox.read`, plus `nbsandbox.functions.publish` and `nbsandbox.functions.invoke` for the function part. A backend that only calls existing functions needs just `nbsandbox.functions.invoke nbsandbox.read`.

Each run creates a new space named `example <date time>`; delete it in the web UI or with `DELETE /v1/spaces/{id}` when done.

## From an AI agent

The same API is available to AI agents (Claude Code, Claude Desktop, others) as MCP tools: `nbsandbox-mcp` lets an agent write a notebook into a space, run it and read the result, as its user, or open a notebook's interactive session and run cells in it one by one, its variables kept between calls (`open_session`, `run_cell`, `end_session`). With the development stack and the same variables as above:

```bash
claude mcp add nbsandbox -e NBSANDBOX_URL=http://127.0.0.1:8080 -e NBSANDBOX_USER=alice -e NBSANDBOX_PASSWORD=alice-dev-password -- go run ./cmd/nbsandbox-mcp
```

[docs/mcp.md](../docs/mcp.md) has the tools, other clients, and running it as a shared server.

