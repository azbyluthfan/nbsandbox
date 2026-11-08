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

The notebooks:

- [notebooks/sales_report.ipynb](notebooks/sales_report.ipynb) takes `region` and `months`, reads `data/sales.csv`, returns the totals as one value (`nbsandbox.output`), and writes `monthly.csv` as an artifact. Its input contract only allows the three regions in the data, so a wrong value is refused with `400 invalid_parameters` before anything runs.
- [notebooks/sales_by_month.ipynb](notebooks/sales_by_month.ipynb) takes the same input but streams its output: one item per month (`nbsandbox.emit`), half a second apart, as a chart would draw them. Its contract declares `{"mode": "stream", "item": …}`; the result's `output` is the list of the items.

## Running them

Against the development stack (`make up`), from the repository root:

```bash
export NBSANDBOX_URL=http://127.0.0.1:8080 NBSANDBOX_USER=alice NBSANDBOX_PASSWORD=alice-dev-password
```

```bash
python examples/python/run_notebook.py --region bandung --months 4
```

```bash
go run ./examples/go/runnotebook -region surabaya
```

```bash
go run ./examples/go/runnotebook -stream
```

```bash
go run ./examples/go/grpcclient -stream
```

With tokens instead (see [docs/auth.md](../docs/auth.md)):

- `NBSANDBOX_TOKEN=<token issued for nbsandbox>`: used as is.
- Python only: `USER_TOKEN=<a signed-in user's token for your app>` plus `AZURE_TENANT_ID`, `APP_CLIENT_ID`, `APP_CLIENT_SECRET` and `NBSANDBOX_SCOPES` (e.g. `api://<nbsandbox client id>/nbsandbox.read api://<nbsandbox client id>/nbsandbox.runs`): the example exchanges the user's token for an nbsandbox token and caches it until shortly before it expires. nbsandbox then sees the user, so the space belongs to them.

The token needs the scopes for what the example does: `nbsandbox.spaces.write`, `nbsandbox.files.write`, `nbsandbox.runs` and `nbsandbox.read`, plus `nbsandbox.functions.publish` and `nbsandbox.functions.invoke` for the function part. A backend that only calls existing functions needs just `nbsandbox.functions.invoke nbsandbox.read`.

Each run creates a new space named `example <date time>`; delete it in the web UI or with `DELETE /v1/spaces/{id}` when done.
