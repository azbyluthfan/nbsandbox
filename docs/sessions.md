# Interactive sessions

A **session** is one notebook's live kernel. Run a cell, see its output, keep the variables, run the next cell, as in Jupyter, with nbsandbox's isolation. Completion and docs come from the real objects: `df.` lists a DataFrame's columns, `df["` its column names, `d["` a dict's keys.

A run executes a whole notebook once, in a fresh container, and returns its result to a caller. A session is for working on a notebook: the kernel stays until you end the session, or until nobody has used it for a while.

## For users

### In the web editor

Open a notebook. The bar above it says **No session**:

1. **Start session** (or just run a cell): a kernel starts in about two seconds. The bar shows its state: **Idle**, **Running**, **Restarting**.
2. **Run cells:**
   - each code cell's ▶ button;
   - **Shift-Enter** runs the cell and moves to the next one, adding a new cell at the end;
   - **Ctrl-Enter** (or **⌘-Enter**) runs the cell and stays on it;
   - **Run all** runs every code cell in order and stops at the first error.
3. **Outputs** appear as the cell produces them:
   - text, errors and tracebacks;
   - images (matplotlib's charts) inline;
   - HTML, such as pandas tables, in a sandboxed frame.

   Editing a cell keeps its output until it runs again, as in Jupyter. **Save** stores the outputs in the notebook, at most 1 MiB per cell.
4. **Completion** as you type comes from the kernel when it answers within 300 ms, merged with the editor's own index of the runner image's packages. While a cell runs, the kernel can't answer, and the editor's index still does. The highlighted suggestion shows its docs; hovering over a name shows them too. After `(`, a line under the cell shows the call's signature.
5. **Variables** (in the bar) lists the kernel's variables: name, type, shape or length, a DataFrame's columns, and the start of each value. It refreshes after every cell, with a silent request that doesn't count as a cell run.
6. **Interrupt** stops the running cell (`KeyboardInterrupt`). **Restart** starts a new kernel: the variables are gone, the files stay.
7. **End session** stops the kernel. You don't have to: leaving the page keeps the session (reopen the notebook to rejoin it), and an unused session ends by itself ([limits](#limits)).

### Files: read-write and read-only

The kernel sees the space's files at `/space` (its working directory), as a run does.

- **Read-write session (the default):**
  - What the kernel writes in `/space` is written to the space: when you **Save files**, every 5 minutes, and when the session ends.
  - While it is open, the session holds the space's write lock, as a writing run does: no writing run starts in the space (`409`). Read-only runs (function calls, runs marked read-only) still can.
  - A file changed in the space meanwhile (saved in the editor, uploaded) keeps that change. The session's version is kept beside it, as for a run.
- **Read-only session:** `/space` can't be written. You get one when you ask for it (`"mode": "read-only"`), or when something else writes the space: a writing run, or another notebook's read-write session. The bar shows the mode.

The editor saves the notebook file itself through the API, as before. A session doesn't need it saved: it runs the cells you send it.

### Notebooks written for runs

Notebooks that use `nbsandbox.parameters()`, `nbsandbox.output()`, `emit()` and `progress()` work in a session too:
- `parameters()` returns `{}`; the cell tagged `parameters` holds the defaults.
- `output()`, `emit()` and `progress()` also print what a run would return, under the cell.
- `NBSANDBOX_OUTPUT_DIR` is a scratch folder in `/tmp`: nothing in it is kept.

### What a session can't do

- **No network:** like a run, a session has no network at all.
- **No `input()`:** the kernel can't ask you for input; `input()` raises `StdinNotImplementedError`.
- **No widgets** (ipywidgets) and no interactive JavaScript outputs: their outputs show as text.
- **Packages:** the space's [package set](packages.md) is there, built from `requirements.txt` when the session starts. After changing `requirements.txt`, end the session and start a new one.

### Through the API

```bash
# Open (or get) the notebook's session.
curl -su alice:… -X POST "$URL/v1/spaces/$SPACE/sessions" -H 'Content-Type: application/json' -d '{"notebook": "analysis.ipynb"}'
```

Then open the WebSocket `GET /v1/spaces/{id}/sessions/{session_id}/channel` and exchange Jupyter messages, one JSON object per text message. [api.md](api.md#7-work-interactively-a-session) describes the protocol; the OpenAPI spec lists every endpoint. gRPC has the same as `SessionService` (the channel is a bidirectional stream). Opening, using and ending a session needs the `nbsandbox.runs` scope: a session runs code, as a run does.

AI agents get sessions as MCP tools (`open_session`, `run_cell`, `complete_code`, `end_session`): [mcp.md](mcp.md#tools).

### Reading connections

A session reads its space's connections like a run ([connections.md](connections.md#5-in-an-interactive-session)): `nbsandbox.sql("SELECT … FROM crm.orders")` in a cell. Open it with `"environment"` to choose dev or prod (the space's default otherwise); its fetcher lives as long as the session.

## Limits

| | Default | Setting |
|---|---|---|
| A session ends after it has been **idle**: no cell running **and** nobody connected, for this long (Colab's rule) | 30 min | `sessions.idle` |
| A session ends in any case, this long after it started | 8 h | `sessions.max` |
| Open sessions per user | 2 | `sessions.per_user` |
| Sessions a worker holds, beside `runs.concurrency` | 4 | `sessions.per_worker` |
| One output larger than this is replaced by a notice | 2 MiB | `sessions.max_message` |

A session has a run's resource limits (`limits:` in `settings.yaml`): memory, CPUs, processes, `/tmp`. A cell that uses more memory than the limit kills the kernel. Where only the kernel is killed, the session starts a new one (its variables are gone) and goes on: Docker with runc, and Kubernetes with gVisor (which enforces the limit inside its sandbox). Where the whole container is killed, the session ends, saying why, its files written back, and you start a new one: Kubernetes with the default runtime (the kubelet's `memory.oom.group`), and Docker with gVisor (runsc), where the host kills the sandbox that holds the notebook's memory. A long cell is never cut off by the idle rule; `sessions.max` still applies. The run time limit (`limits.timeout`) applies to runs, not to cells.

## For operators

- **On by default** (`sessions.enabled`). A session runs in a container (or a Pod) from the runner image, with exactly a run's isolation: no network, a non-root user without capabilities, a read-only system, its own `/tmp`, the run limits, the configured runtime (gVisor or a sandbox RuntimeClass). Its main process is the **session bridge**, which nbsandboxd talks to over the container's standard input and output: Jupyter messages, filtered both ways to the ones above. Nothing listens in the container.
- **Docker:** the socket proxy must allow `POST /containers/{id}/attach` (the Compose files in `deploy/` do, from 0.3.0). Session containers are labelled `nbsandbox.session`; they log nothing (their output is the protocol).
- **Kubernetes:** session Pods are in the runs namespace, under the same deny-all NetworkPolicy, labelled `nbsandbox.session`. The workers' Role needs `pods/attach` (the Helm chart has it). A read-write session's files are copied out of the Pod before each write-back.
- **Several API and worker processes** ([scaling.md](scaling.md)): a worker takes a session and holds it; API nodes relay a client's WebSocket to it. Set `sessions.secret`, the same long random value on every process; without it, sessions stay off on such processes. With Helm, create a Secret with the key `secret` and set `sessions.secretName`.
- **Restarts:** sessions outlive nbsandboxd. A restarted worker re-attaches to its sessions' containers (their kernels kept running) and clients reconnect by themselves. When a worker is gone for good (a Kubernetes rolling upgrade replaces worker Pods with new ones; a Pod crashes or its node drains), another worker that can reach its sessions takes them over within seconds: on Kubernetes any worker, on Docker a worker on the same host. Only a session no worker can take over ends as interrupted after two minutes, and one whose container is gone ends at once; either way its files are written back.
- **Drain** ([operations.md](operations.md#upgrade)): while the workers are drained, no session starts (`503`). Open sessions go on until they end or an administrator ends them. An upgrade doesn't end them: the new nbsandboxd re-attaches. A runner image upgrade applies to sessions started afterwards.
- **Administration:** `GET /v1/admin/sessions` lists every open session (owner, worker, notebook, mode); `POST /v1/admin/sessions/{session_id}/end` ends one. Deleting a space ends its sessions.
- **Audit log:** opening, ending, interrupting, restarting and saving a session's files are recorded, never the code a cell runs.
- **Metrics:** sessions are counted with runs in the containers on a worker; size workers for `runs.concurrency` plus `sessions.per_worker` containers at the run limits.
