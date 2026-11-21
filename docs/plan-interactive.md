# Plan: interactive mode

**Status (2026-11-21):** agreed, with the changes from the research below. Step 1, the spike, is done (results below); steps 2 to 7 are being built, for **v0.3.0** (the release after 0.2.x; this plan said v0.2.0 before that shipped without it).

## Today

The editor edits notebooks, and **Run** runs a whole notebook as a batch run: a fresh container, every cell in order, results at the end. There is no way to run one cell, look at a value, and run the next. Completion comes from a static index of the runner image's packages: `pd.` works, but `df.` after `df = pd.read_csv(...)` doesn't, because nothing knows what `df` is.

## Goal

Jupyter-style work in nbsandbox's own editor, with nbsandbox's isolation:
- **Run a cell, see its output,** keep the variables, run the next. Interrupt; restart.
- **Autocomplete from the live kernel** (a must): `df.` lists the DataFrame's methods *and columns*, `df["` its column names, `d["` a dict's keys, `obj.` whatever the object really has. Signature help on `(`, docs on hover, all from the real values.
- **The same isolation as a run:** no network, limits, gVisor if configured, the space's files and its package set.
- No JupyterHub, no Jupyter server: nbsandboxd runs the session and speaks for it.

## Design

### A session

A **session** is one notebook's live kernel, as in Databricks and Colab: opening a second notebook gives it a session of its own, so notebooks don't share variables by surprise. It runs in a long-lived container from the runner image, with the same mounts, limits, runtime and package set as a run, and no network. Its main process is a small **session bridge** (in the runner image), which:
- starts an IPython kernel over Unix sockets inside the container (`ipc` transport, as runs already do);
- speaks to nbsandboxd over the container's **stdin and stdout**: one **Jupyter message** (header, parent header, metadata, content, channel; JSON, without binary buffers) per line, the same messages Jupyter's own WebSocket carries.

```
browser ⇄ WebSocket ⇄ nbsandboxd ⇄ container stdio (attach) ⇄ session bridge ⇄ kernel (ipc sockets)
```

Why stdio rather than a socket file or a port:
- no network, not even to nbsandboxd;
- no socket files shared through bind mounts, which may not work across Docker Desktop's shared file system;
- the same channel on Kubernetes (`pods/attach`);
- nbsandboxd can re-attach after it restarts, and the session survives.

The bridge allows only these message types:
- `execute_request`;
- `complete_request`, `inspect_request` (docs and signatures), `is_complete_request`;
- `interrupt_request`, and restart (nbsandboxd's own, as `shutdown_request` with `restart`);
- `kernel_info_request`;
- and their replies, with the `iopub` messages (`status`, `stream`, `display_data`, `update_display_data`, `execute_result`, `error`, `execute_input`, `clear_output`).

It refuses everything else: input prompts (`input()` raises), comms and widgets, history, shutdown from the browser. Outputs are sent as Jupyter MIME bundles, capped in size.

### Lifecycle and limits

| | Default | Setting |
|---|---|---|
| Ends after being idle (no cell running **and** no browser connected, Colab's rule) | 30 min | `sessions.idle` |
| Ends in any case after | 8 h | `sessions.max` |
| Open sessions per user | 2 | `sessions.per_user` |
| Open sessions per worker | 4 | `sessions.per_worker` (beside `runs.concurrency`) |

- **Ending:**
  - when idle or past the maximum;
  - **End session** in the UI (`DELETE`);
  - when the space is deleted;
  - when an administrator ends it (`/v1/admin/sessions`);
  - when the workers are drained: no new sessions start, and open ones get a warning and end at the maximum, or when the administrator ends them.
- **Restart** keeps the container and starts a new kernel. **Interrupt** stops the current cell.
- **nbsandboxd restarts:** open sessions keep running and are re-attached. A session whose worker is gone ends as `interrupted`.

### Files: read-write or read-only

- **Read-write session** (the default from the editor):
  - holds the space's write lock, like a writing run: no writing run starts in the space while it is open, and a second notebook opened meanwhile gets a read-only session;
  - `/space` is the space's working copy, as for a run;
  - files the kernel writes are written back like a run's: on **Save files**, every few minutes, and at the end;
  - the editor's own saves (the notebook file) go through the API as now.
- **Read-only session:** `/space` read-only, no lock. It can run beside runs and other sessions; it is for exploring.

### Autocomplete (the must-have)

- **With a session:**
  - the editor asks the kernel (`complete_request`) as you type, debounced (about 100 ms), with a deadline (300 ms; a late answer is dropped, as in JupyterLab), so typing never waits. IPython's completer with or without Jedi: the spike measures both;
  - results show the kind (function, column, key, module) and, where IPython gives it, the type;
  - `(` asks for the signature (`inspect_request`); hover, and the highlighted completion item, ask for the docstring (lazily, one request at a time).
  - **It works from the real objects:** `df.` includes columns, `df["` lists them, `d["` lists keys, a variable from a cell that ran completes fully, and so do packages from the space's package set.
- **Without a session, or while the kernel is busy running a cell:** today's static index (modules, aliases, names across cells), merged with what the kernel knows, so completion never goes blank.
- **Names from cells that haven't run** come from the static analysis; names from cells that ran come from the kernel.

### API

| | |
|---|---|
| `POST /v1/spaces/{id}/sessions` `{"notebook": "...", "mode": "read-write" \| "read-only"}` | start a notebook's session (or return its open one); 201 with the session (`starting`, then `ready`) |
| `GET /v1/spaces/{id}/sessions` | the space's open sessions |
| `DELETE /v1/spaces/{id}/sessions/{sid}` | end |
| `GET /v1/spaces/{id}/sessions/{sid}/channel` | the WebSocket: Jupyter messages (JSON) both ways, filtered to the allowed types |

- **Scope:** `nbsandbox.runs`, since a session runs code. Owner only, like everything in a space.
- **Web UI:** through `/ui/api/...` with the session cookie (a WebSocket can't send a bearer token, which was one reason for the UI login). **API clients:** a bearer token on the WebSocket request.
- **gRPC:** the same, as a bidirectional stream.
- **Audit:** session start and end, interrupts and restarts. The code a cell runs is never logged.

### Cluster mode

The browser's WebSocket reaches an API node, but the session runs on a worker:
- workers serve sessions on an internal port, authenticated with a cluster secret;
- API nodes proxy the WebSocket to the worker holding the session (its address is in the `workers` table);
- with `role: all` (one process), there's no proxy.

### Outputs in the editor

- **Text, errors and images** are shown inline. HTML (pandas tables, plots) is shown in a sandboxed iframe (no scripts with the UI's origin, no network), like a run's `output.html`.
- **Saving** a notebook saves its outputs, as Jupyter does, capped per cell. A batch run of the notebook re-runs it from the start as now.

### Security

| Risk | Answer |
|---|---|
| A session reaches the network | Network `none`, as a run; nbsandboxd talks to it over stdio only |
| A notebook's output runs script in the UI | HTML outputs only in sandboxed iframes; text inserted as text |
| One user exhausts a worker | Per-user and per-worker limits, idle and maximum timeouts, the run's resource limits |
| Someone else's session | Owner only; the session ID alone gives nothing; the WebSocket needs the owner's credentials and the UI's origin |
| A long session keeps old credentials | It has none; the UI session's own expiry still applies to the browser |

## Order of work, and what's in v0.3.0

| | Step |
|---|---|
| 1 | Spike: the session bridge, stdio attach through Docker and the socket proxy, latency of `complete` |
| 2 | Sessions on Docker: lifecycle, limits, read-write and read-only, write-back, re-attach, audit, admin |
| 3 | The WebSocket API (REST, UI route), and the protocol tests |
| 4 | The editor: run cell, outputs, interrupt and restart, a kernel status indicator, **kernel autocomplete, signature and hover** |
| 5 | Cluster mode (proxying to workers) |
| 6 | Kubernetes (`pods/attach`) |
| 7 | Docs, isolation report checks (no network from a session, limits, re-attach) |

For v0.3.0: **all seven steps.**

## Spike results (step 1, 2026-11-21)

Measured on Docker Desktop (Apple silicon), the runner image, a container with a run's isolation (`--network none`, read-only root, no capabilities, 1 CPU, 2 GiB):

| | Without Jedi | With Jedi |
|---|---|---|
| `df.` (a DataFrame's methods and columns) | **5 ms** median, 11 ms max | 36 ms median, 323 ms max |
| `df["`, `d["` (columns, keys) | 2 ms | 2 ms |
| `pd.read_`, `np.lin` | 2 ms | 6–9 ms |
| `inspect` of `pd.read_csv(` (docs and signature) | 34 ms | 30 ms |

- **Jedi off.** It is slower and adds nothing for the cases that matter: columns and keys come from IPython's own completer either way.
- **A busy kernel doesn't answer** a completion until its cell ends, so the editor needs the static index as a fallback, as planned.
- **Kernel ready** about 1.5 s after the container starts.
- **Interrupt** answers in about 10 ms and stops the cell with `KeyboardInterrupt`.
- **Through the socket proxy:** attaching to a container's stdio (a hijacked HTTP connection) works through wollomatic/socket-proxy with one more allowed call, `POST /containers/{id}/attach`. Detaching and attaching again works, and the container keeps running.
- **Refused message types** (`history_request` and others) get no reply from the kernel; the bridge answers with a refusal instead.

## What others do (research, 2026-11-15)

| | How it works | What we take |
|---|---|---|
| **Databricks** | A REPL per notebook on the attached cluster. Python autocomplete needs the notebook attached and the defining cells run; Jedi static analysis plus runtime docstrings (Shift+Tab). SQL keywords complete without compute. A Variable Explorer (name, type, shape, DataFrame columns), updated as cells run. | A kernel **per notebook**; completion that degrades to static without a kernel (as SQL does); a small variables panel, later |
| **Google Colab / Colab Enterprise** | A runtime per notebook. Idle shutdown after 180 min, and only when there's **no kernel activity and no notebook connected**. Outputs rendered in **sandboxed iframes on a separate origin**. | Colab's idle rule; outputs always in sandboxed iframes |
| **SageMaker Studio** | JupyterLab spaces (storage) separate from the compute app; idle means no active kernel and no terminal; per-app idle settings. | Confirms our split (space files in storage, sessions as compute); idle tied to kernel activity |
| **JupyterLab 4** | The completer merges providers (kernel, editor context, extensions) with a per-provider timeout; a late provider's results are dropped. Kernel docs are lazy-loaded with `inspect` for the selected item only. HTML outputs are sanitized, not iframed. | The merge-with-deadline design, lazy docs; not sanitizing in place (Colab's iframes are safer) |
| **jupyterlab-lsp** | A Python language server beside the kernel; kernel completions merged when they arrive within about 600 ms, otherwise only the server's. | The deadline. Not a language server: another process and its upkeep, and our static index already covers no-kernel completion |
| **Jupyter Enterprise Gateway** | Kernels as a service: a gateway launches remote kernels (Kubernetes pods) and proxies the kernel WebSocket protocol. | Its protocol, not the component: it needs network between gateway and kernels (ZeroMQ ports), which our sessions must not have, and it is a Python server to run and secure |
| **Jupyter server's WebSocket protocol** | Jupyter messages (header, parent header, metadata, content, channel) over one WebSocket. | **Our channel speaks Jupyter messages**, filtered to an allow-list, instead of a protocol of our own |

### Changes this research suggests (low risk, less debt)

1. **Jupyter's own message format on the channel.** The bridge forwards real Jupyter messages (JSON; no binary buffers) for the allowed message types, instead of a home-made protocol. There's nothing of our own to version or document; Jupyter's documentation applies; later clients (VS Code, `jupyter_client`) are closer to working.
2. **A session per notebook,** as in Databricks and Colab: opening a second notebook gives it its own kernel, so notebooks don't share variables by surprise. Only one read-write session per space at a time (the write lock); others are read-only.
3. **Colab's idle rule:** the idle clock runs only while no cell is executing **and** no browser is connected. A long cell is never cut off, and a closed tab doesn't keep a container alive. The maximum length (8 h) still applies.
4. **Completion as JupyterLab does it:**
   - static and kernel providers, merged and de-duplicated, kernel results ranked first;
   - a per-provider deadline (start at 300 ms; jupyterlab-lsp uses 600 ms);
   - docs fetched with `inspect` only for the highlighted item.

   The spike measures IPython's completer with and without Jedi, since Jedi is the slow part and plain attribute completion already includes DataFrame columns and dict keys.
5. **Outputs in sandboxed iframes** as Colab does: `sandbox` without `allow-same-origin`, and a CSP inside that blocks the network, so even script outputs (plots) can't reach the UI's cookies or send data out. Text and images are inserted directly.
6. **A variables panel** (name, type, shape, DataFrame columns) as in Databricks and Deepnote: one silent request to the kernel after each cell. Small and low-risk, but optional.

### Not adopting

- **Jupyter Enterprise Gateway, Jupyter Server or JupyterHub as components:** they need network between server and kernels, plus a Python service to run, patch and secure.
- **A language server** (pyright, jedi-language-server): another process per session for what the static index and the kernel already give.
- **Inline (ghost text) and AI completion:** later, if at all.
- **Widgets and `input()`:** comms and stdin stay refused.

## Decisions for review

1. **Stdio channel** (attach) rather than socket files or ports.
2. **Read-write sessions write files back** like a run (on save, every few minutes, at the end), not continuously.
3. **Limits:** 30 min idle, 8 h maximum, 2 per user, 4 per worker.
4. **Outputs are saved** with the notebook (capped), as in Jupyter.
5. **v0.3.0 scope:** Docker, single process and cluster, and Kubernetes.
6. **Widgets (ipywidgets) and `input()`:** not supported (refused), at least at first.
7. **From the research:** Jupyter messages on the channel; a session per notebook; Colab's idle rule; JupyterLab-style completion merging; sandboxed iframes for outputs; a variables panel if time allows.
