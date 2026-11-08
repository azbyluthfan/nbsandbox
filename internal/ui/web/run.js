// Copyright 2026 The nbsandbox Authors
// SPDX-License-Identifier: Apache-2.0
//
// The run view: status, cancel, the live log through the SSE
// stream, and the result when the run ends.

import { el, clear, toast, badge, timeAgo, formatBytes } from "./dom.js";
import { encodePath } from "./api.js";
import { codeBlock } from "./viewers.js";

const ACTIVE = new Set(["queued", "running"]);

/**
 * Shows one run in `main` and keeps it live until it ends. Returns a
 * function that stops everything (called when the user navigates away).
 */
export function showRun(main, api, space, runId, onChange) {
  let stopped = false;
  let source = null;
  let poll = 0;

  const header = el("div", { class: "run-head" });
  const meta = el("div", { class: "run-meta" });
  const message = el("div", { class: "message" });
  const follow = el("input", { type: "checkbox", checked: true, id: "follow-log" });
  const lineCount = el("span", {}, "0 lines");
  const log = el("pre", { class: "log", "aria-live": "off", "aria-label": "Run log" });
  const result = el("div", { class: "result" });
  clear(main).append(
    header,
    meta,
    message,
    el("div", { class: "log-tools" }, el("label", {}, follow, " Follow the end"), lineCount, el("span", { id: "stream-state" }, "")),
    log,
    result,
  );

  // Batch incoming lines and draw them once per frame: a notebook can print
  // thousands of lines a second.
  let pending = [];
  let lines = 0;
  let frame = 0;
  const flush = () => {
    frame = 0;
    if (pending.length === 0) return;
    log.append(document.createTextNode(pending.join("\n") + "\n"));
    pending = [];
    lineCount.textContent = `${lines} lines`;
    if (follow.checked) log.scrollTop = log.scrollHeight;
  };
  // Scrolling up pauses following; scrolling to the end resumes it.
  log.addEventListener("scroll", () => {
    follow.checked = log.scrollHeight - log.scrollTop - log.clientHeight < 40;
  });
  const streamState = () => main.querySelector("#stream-state");

  let lastHeader = "";
  function renderHeader(run) {
    // Redraw only when something shown changes: replacing the Cancel button
    // every poll could swallow a click that lands mid-redraw.
    const key = [run.status, run.queue_position, run.message, run.started_at].join("|");
    if (key === lastHeader) return;
    lastHeader = key;
    // Native append() would print null as text: only pass real nodes.
    const title = run.function ? `${run.function} v${run.version}` : run.notebook;
    const parts = [el("h1", {}, title), badge(run.status)];
    if (run.status === "queued" && run.queue_position) {
      parts.push(el("span", { class: "muted" }, `position ${run.queue_position} in the queue`));
    }
    if (ACTIVE.has(run.status)) {
      parts.push(el("button", { type: "button", class: "danger", onclick: () => cancel() }, "Cancel"));
    }
    clear(header).append(...parts);
    const params = run.parameters && Object.keys(run.parameters).length ? ` · parameters ${JSON.stringify(run.parameters)}` : "";
    meta.textContent = `run ${run.id} · started ${timeAgo(run.started_at || run.created_at)}${params}`;
    message.textContent = run.message || "";
    message.className = "message" + (run.fault && run.fault !== "caller" ? " bad" : "");
    if (run.dependency_warnings && run.dependency_warnings.length && !run.message) {
      message.textContent = "Not in the runner image: " + run.dependency_warnings.map((w) => w.module).join(", ");
    }
  }

  async function cancel() {
    try {
      await api.cancelRun(space, runId);
      toast("Cancelling…", "info");
      refresh();
    } catch (e) {
      toast(e.message);
    }
  }

  async function renderResult(run) {
    clear(result);
    const r = run.result;
    if (!r) return;
    if (r.error) {
      const cell = r.error.cell_index != null ? `cell ${r.error.cell_index}` : "a cell";
      result.append(el("p", { class: "message bad" }, `${r.error.exception_type} in ${cell}: ${r.error.exception_message}`));
    }
    // What a caller gets: the returned value, or the error object.
    try {
      const cr = await api.result(space, runId);
      if (cr.output !== undefined || cr.error) {
        const body = cr.output !== undefined ? cr.output : cr.error;
        result.append(
          el("h2", {}, cr.output !== undefined ? "Returned value" : "Result for the caller"),
          codeBlock(JSON.stringify(body, null, 2), "json"),
        );
      }
    } catch {
      /* no result (yet) */
    }
    const usage = r.usage || {};
    const parts = [`${(usage.duration_seconds || 0).toFixed(1)} s`];
    if (usage.peak_memory_bytes) parts.push(`peak memory ${formatBytes(usage.peak_memory_bytes)}`);
    if (usage.cpu_seconds) parts.push(`${usage.cpu_seconds.toFixed(1)} s CPU`);
    if (r.log_truncated) parts.push("log truncated");
    result.append(el("p", { class: "muted" }, parts.join(" · ")));

    const links = el("div", { class: "links" });
    if (r.rendered) {
      // Opens in its own tab; the response's sandbox CSP blocks the
      // notebook's scripts and gives it a unique origin.
      links.append(el("a", { href: api.runUrl(space, runId, "/output.html"), target: "_blank", rel: "noopener noreferrer" }, "View rendered notebook ↗"));
    } else {
      // Runs stopped early never reach the rendering step.
      const why = run.status === "succeeded" || run.status === "failed" ? " (HTML rendering failed)" : "";
      links.append(el("span", { class: "muted" }, "No rendered view" + why));
    }
    const nbUrl = api.runUrl(space, runId, "/output.ipynb");
    links.append(el("a", { href: nbUrl, download: "output.ipynb" }, r.partial ? "Partial notebook (.ipynb)" : "Executed notebook (.ipynb)"));
    links.append(el("a", { href: api.runUrl(space, runId, "/log"), download: "log.txt" }, "Log (.txt)"));
    result.append(links);

    try {
      const arts = await api.artifacts(space, runId);
      if (arts.length) {
        result.append(
          el("h2", {}, "Artifacts"),
          el(
            "ul",
            {},
            arts.map((a) =>
              el("li", {}, el("a", { href: api.runUrl(space, runId, "/artifacts/" + encodePath(a.path)), download: a.path.split("/").pop() }, a.path), ` (${formatBytes(a.size)})`),
            ),
          ),
        );
      }
    } catch {
      /* no artifacts */
    }

  }

  async function refresh() {
    if (stopped) return null;
    try {
      const run = await api.run(space, runId);
      if (stopped) return null;
      renderHeader(run);
      if (!ACTIVE.has(run.status)) {
        clearInterval(poll);
        await renderResult(run);
        onChange && onChange();
      }
      return run;
    } catch (e) {
      toast(e.message);
      return null;
    }
  }

  // The live log. EventSource reconnects by itself and sends Last-Event-ID,
  // so a dropped connection resumes without gaps. It must be closed on
  // "done", or it would reconnect and replay the finished log.
  function connect() {
    source = new EventSource(api.runUrl(space, runId, "/log/stream"));
    const state = (t) => {
      const s = streamState();
      if (s) s.textContent = t;
    };
    source.addEventListener("open", () => state("● live"));
    source.addEventListener("line", (ev) => {
      lines++;
      pending.push(ev.data);
      if (!frame) frame = requestAnimationFrame(flush);
    });
    source.addEventListener("done", () => {
      source.close();
      state("");
      flush();
      refresh();
    });
    source.addEventListener("dropped", () => state("reconnecting…"));
    source.addEventListener("shutdown", () => state("server restarting, reconnecting…"));
    source.addEventListener("error", () => {
      if (source.readyState === EventSource.CONNECTING) state("reconnecting…");
    });
  }

  refresh().then((run) => {
    if (!run || stopped) return;
    connect();
    if (ACTIVE.has(run.status)) poll = setInterval(refresh, 1500);
  });

  return () => {
    stopped = true;
    clearInterval(poll);
    if (frame) cancelAnimationFrame(frame);
    if (source) source.close();
  };
}
