# Web UI Check Record

A manual, scripted pass through the local web UI in a real browser. Repeat it after changes to `internal/ui/web/` and add a dated section.

## 2026-11-07 — editor and file management, `make ui-preview` (no Docker), http://127.0.0.1:18090

Done while the 24 h soak held the dev stack, so with a native nbsandboxd whose Docker address doesn't exist: everything except running. Running, calling functions and the live views are rechecked on the dev stack after the soak. Setup: alice's "UI check" space with `forecast_fn.ipynb` (with a contract), `src/model.py` (a missing colon, `import torch`, a `%timeit` line) and `data/sales.csv`.

| # | Step | Expected | Result |
|---|---|---|---|
| 1 | Open `src/model.py` | Highlighted Python in the editor, line numbers; the missing colon underlined as a syntax error; the `%timeit` line not flagged | ✅ (after the CSP fix below) |
| 2 | Fix the colon, Ctrl/⌘-S | "unsaved changes", then saved; the error marker clears; the version changes | ✅ |
| 3 | Another client saves the file; then edit and save here | "A newer version exists" on focus; saving shows "changed elsewhere" with Reload / Save as / Overwrite; the editor keeps my edits | ✅ |
| 4 | Overwrite | the server has my version | ✅ |
| 5 | Open `forecast_fn.ipynb` | Cells as editors, `parameters` tag lit, contract section, Run (with read-only) and Publish | ✅ |
| 6 | Add a code cell with `def broken(:`, move it down, save | Syntax error flagged in that cell; saved notebook passes `nbformat.validate`; cell order, tags and contract kept | ✅ |
| 7 | Publish as function, with `src/` | Dialog with name and files; v1 published; function page with a form from the input schema and the contract | ✅ |
| 8 | Call with `{"cfg": {"months": 99}}` | Form refuses a missing required `region`; as JSON, the server's `400` shown with the JSON pointer and a readable message | ✅ (after the message fix below) |
| 9 | New folder, rename/move it, delete it | `data/2026` → `archive/2026` (parent created) → deleted; server agrees | ✅ |
| 10 | Upload two files into a folder | Both stored there; tree and folder page updated | ✅ (after the redraw fix below) |
| 11 | Edit, then click another file and choose to stay | Asked once; still on the file; edits intact | ✅ |
| 12 | Run the notebook without `region` | Refused by the input contract with a readable message | ✅ |

Found and fixed during this check:
- **The editor's own styles were blocked by the page's CSP** (`style-src 'self'`): CodeMirror adds them as a `<style>` element. The page is now served with a fresh nonce on every load, in its CSP and in `<meta name="csp-nonce">`, and the editor tags its styles with it. No `'unsafe-inline'`.
- **An empty banner showed:** `display: flex` overrode `hidden`; `[hidden]` now always hides.
- **The conflict choices were replaced by the weaker "newer version" notice** from the 30 s check; a conflict now stays until one choice is made.
- **Contract messages were unreadable** (`&{99/1 24/1}`); they now come from the schema library's English messages ("maximum: got 99, want 24").
- **"null" text on folder pages**, and **the folder page didn't redraw after an upload**: both fixed.

## 2026-11-06 — Docker Desktop (macOS), dev stack (`make up`), http://127.0.0.1:8090

Setup: alice has a "Sales forecast" space with `smoke`, `stream`, `fail_cell3`, `long` and `f1_params_csv` notebooks, `data/sales.csv` (including a quoted field with a comma) and `README.md`; bob has an empty "Bob private" space.

| # | Step | Expected | Result |
|---|---|---|---|
| 1 | Open the UI | User picker (alice, bob), space picker, "local UI · no login" note | ✅ |
| 2 | Pick alice → "Sales forecast" | Folder tree: `data/sales.csv`, README, five notebooks with sizes; URL `#/alice/<space>` | ✅ |
| 3 | Open `data/sales.csv` | Table with header and 4 rows; `"surabaya, east"` stays one cell | ✅ |
| 4 | Open `f1_params_csv.ipynb` | Read-only cells with line numbers, `params` marker on the parameters cell, Run button and parameters box | ✅ |
| 5 | Run with `{"region": "jakarta", "months": 6}` | Run view opens; the log appears live; ends `succeeded`; usage line; `summary.csv` artifact; links to the executed notebook and log | ✅ |
| 6 | Open the rendered notebook | Opens in its own tab, styled, parameters injected; the notebook's scripts blocked (console: "Blocked script execution … sandboxed") | ✅ |
| 7 | Run `stream.ipynb` | Lines arrive one a second (`line 0` … `line 9`), "● live" shown, then `succeeded` | ✅ |
| 8 | Run `fail_cell3.ipynb` | `failed`; "ValueError in cell 3: boom in cell 3"; partial notebook link | ✅ |
| 9 | Run `long.ipynb`, Cancel after a few ticks | `cancelled` within about 1 s; log ends with `[nbsandbox] run finished: cancelled`; partial notebook | ✅ |
| 10 | Switch user to bob | Only "Bob private" listed; a pasted link to alice's space shows nothing | ✅ |
| 11 | Open a notebook containing `<img onerror=…>`, `<script>` and an HTML output | Shown as plain text; no image or script element created; page title unchanged | ✅ |

Found and fixed during this check:
- **Stray "null" text** next to the status badge. Native `append()` prints `null`; the header now passes only real nodes.
- **Cancel button rebuilt on every status poll.** A click landing mid-redraw could be lost, so the header now redraws only when its content changes. This was found because the automation's element reference went stale; the second check clicked the same button after two polls and it worked.
- **Rendered notebook frame was blank in the browser pane.** The page inside loaded with its scripts blocked as intended, but the fully sandboxed frame stayed blank in the pane. It was replaced by a link that opens the rendered notebook in its own tab under the same sandbox CSP; the page's CSP is now `frame-src 'none'`.
- **"HTML rendering failed" shown for a cancelled run.** It now appears only for runs that finished on their own.

Known cosmetic issue at the time: the kernel's "running over TCP without encryption" warning appeared in every log. Fixed since (the kernel now uses IPC sockets).

## 2026-11-07: redesign, checked in `make ui-preview`

Checked in the built-in browser against the preview (its own data folder, no Docker), at 1440 px, the pane's own size, and 375 px, in light and dark:

- **Shell:** top bar (space switcher with "All spaces" and "+ New space…", Runs and Functions buttons with an active-run count, a "Runs paused" pill when Docker is unreachable, the user picker), a file explorer that can be hidden and remembers folded folders, and a drawer for runs and functions. At 860 px and below the explorer and drawer become overlays and close when the window narrows; at 375 px nothing is wider than the screen.
- **Spaces list** (`#/<user>`): cards per space and a New space card.
- **Space overview:** stats, notebook cards (last run status, quick Run with default parameters), recent runs, functions, and the top-level files table with rename, download and delete on each row. Dropping files anywhere on the page uploads to the folder in view.
- **Folder page:** breadcrumbs, the same table, actions in a ⋯ menu.
- **Notebook page:** sticky header with saved time (the version in the tooltip), Save, Reload, Download and a ⋯ menu (publish, rename, delete); the run bar with parameters, read-only and a link to the last run.
- **Function page:** call form with required fields first, dropdowns for string enums and hints from the schema; recent calls; a copyable curl example; contract and versions.
- **Not checked here:** the run page and live log, and a call's result, since the preview can't start runs. They are restyled through the shared styles only and are checked with the dev stack after the soak.

## 2026-11-07: redesign on the dev stack (runs, live log, calls)

The parts the preview couldn't check, on a fresh dev stack with the example from `examples/` (uploaded and run first by `go run ./examples/go/runnotebook`, which passed end to end):

- **Overview with real runs:** the notebook card shows the last run's status; recent runs show status and duration; no "Runs paused" badge while Docker is up; the Runs button counts active runs.
- **Run with parameters from the notebook page:** opens the run page; the live log streams line by line ("● live", line count, follow the end); the status changes from running to succeeded; the result shows the returned value, duration, peak memory and CPU, the rendered and executed notebook and the log, and the `monthly.csv` artifact.
- **Calling a function from its page:** the form shows the required `region` first as a dropdown (from the contract's enum) and `months` with its range; the call's status updates live and the returned value appears in place; the curl example uses a valid region.

Fixed during the check:
- **A quick Run on a notebook card** sends no parameters; when the notebook's contract requires some, the server refuses (400). The card now opens the notebook and says which parameter is missing, instead of only showing the error.
- **The overview and folder pages** reused the data loaded when the space was opened, so a function published (or deleted) elsewhere didn't show up. They now fetch the space's files, runs and functions each time they open.
