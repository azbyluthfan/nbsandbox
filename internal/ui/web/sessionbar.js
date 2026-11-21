// Copyright 2026 The nbsandbox Authors
// SPDX-License-Identifier: Apache-2.0
//
// The bar above a notebook that runs it interactively: start (or join) its
// session, the kernel's state, and run all, interrupt, restart, save files
// and end. The session outlives the page: leaving disconnects, and the
// session ends by itself once idle (no cell running, nobody connected).

import { el, clear, toast, icon } from "./dom.js";
import { KernelSession } from "./session.js";

const KERNEL_TEXT = {
  idle: "Idle",
  busy: "Running",
  starting: "Starting",
  restarting: "Restarting",
  dead: "Kernel died: starting again",
};

/**
 * ctx: {api, space, path, notebook(): the NotebookEditor (for Run all)}.
 * Returns {node, kernel(), start(), close()}.
 */
export function sessionBar(ctx) {
  const dot = el("span", { class: "kernel-dot", "aria-hidden": "true" });
  const label = el("span", { class: "kernel-label" }, "No session");
  const mode = el("span", { class: "badge", hidden: true });
  const startBtn = el("button", { type: "button", title: "Start this notebook's live kernel: run cells one by one, keep the variables, complete from real objects" },
    icon("play"), "Start session");
  const runAll = el("button", { type: "button", class: "secondary small", hidden: true, title: "Run every cell in order (stops at an error)" }, "Run all");
  const interrupt = el("button", { type: "button", class: "secondary small", hidden: true, title: "Stop the running cell" }, icon("stop"), "Interrupt");
  const restart = el("button", { type: "button", class: "secondary small", hidden: true, title: "Start the kernel again: its variables are gone, the files stay" }, "Restart");
  const save = el("button", { type: "button", class: "secondary small", hidden: true,
    title: "Write the files the kernel changed to the space now (also every few minutes, and at the end)" }, icon("save"), "Save files");
  const end = el("button", { type: "button", class: "ghost small", hidden: true, title: "End the session: its kernel stops" }, "End session");
  const why = el("span", { class: "muted kernel-why" });
  const varsBtn = el("button", { type: "button", class: "ghost small", hidden: true, "aria-expanded": "false",
    title: "The kernel's variables: type, shape, a DataFrame's columns" }, "Variables");
  const bar = el("div", { class: "session-bar", role: "group", "aria-label": "Interactive session" },
    el("span", { class: "kernel-state", role: "status", "aria-live": "polite" }, dot, label), mode, why,
    el("span", { class: "spacer" }), startBtn, runAll, interrupt, restart, save, varsBtn, end);
  const varsPanel = el("div", { class: "vars-panel", hidden: true });
  const node = el("div", { class: "session-area" }, bar, varsPanel);

  let starting = null;
  const k = new KernelSession(ctx.api, ctx.space, ctx.path, {
    onState: show,
    onNotice: (text) => toast(text, "info"),
  });

  function show(st) {
    const live = st.status === "ready";
    bar.dataset.state = live ? st.kernel || "idle" : st.status;
    varsBtn.hidden = !live;
    if (!live) varsPanel.hidden = true, varsBtn.setAttribute("aria-expanded", "false");
    let text;
    if (st.status === "none") text = "No session";
    else if (st.status === "starting") text = "Starting a session…";
    else if (st.status === "connecting") text = "Connecting…";
    else if (st.status === "ended") text = "Session ended";
    else text = KERNEL_TEXT[st.kernel] || "Idle";
    label.textContent = text;
    why.textContent = st.status === "ended" && st.reason ? `(${st.reason})` : "";
    mode.hidden = !live;
    mode.textContent = st.mode === "read-only" ? "read-only" : "read-write";
    mode.className = `badge ${st.mode === "read-only" ? "queued" : "succeeded"}`;
    mode.title = st.mode === "read-only"
      ? "The kernel sees the space's files but can't change them (something else writes the space)"
      : "Files the kernel writes go to the space (Save files, every few minutes, and at the end)";
    startBtn.hidden = live || st.status === "starting" || st.status === "connecting";
    startBtn.lastChild.textContent = st.status === "ended" ? "Start a new session" : "Start session";
    for (const b of [runAll, interrupt, restart, end]) b.hidden = !live;
    save.hidden = !live || st.mode !== "read-write";
    interrupt.disabled = st.kernel !== "busy";
  }

  async function start() {
    if (k.ready) return k;
    if (!starting) {
      starting = (async () => {
        try {
          await k.start();
        } catch (e) {
          k.setState({ status: "none" });
          throw e;
        }
        // Ready once the channel says so.
        await new Promise((resolve, reject) => {
          const t0 = Date.now();
          const wait = () => {
            if (k.ready) return resolve();
            if (k.status === "ended" || Date.now() - t0 > 180000) return reject(new Error(k.reason || "the session didn't start"));
            setTimeout(wait, 100);
          };
          wait();
        });
        return k;
      })().finally(() => {
        starting = null;
      });
    }
    return starting;
  }

  const act = (fn, done) => async () => {
    try {
      const r = await fn();
      if (done) done(r);
    } catch (e) {
      toast(e.message);
    }
  };
  startBtn.addEventListener("click", act(start));
  runAll.addEventListener("click", () => ctx.notebook() && ctx.notebook().runAll());
  interrupt.addEventListener("click", act(() => k.interrupt()));
  restart.addEventListener("click", act(() => confirm("Restart the kernel? Its variables are gone; the files stay.") && k.restart()));
  save.addEventListener("click", act(() => k.syncFiles(), (r) => {
    const n = (r && r.conflicts) ? r.conflicts.length : 0;
    toast(n ? `Saved; ${n} file(s) had changed meanwhile: the session's versions were kept beside them.` : "The session's files are saved to the space.", "info");
  }));
  end.addEventListener("click", act(async () => {
    if (!confirm("End the session? Its kernel stops; changed files are saved to the space first.")) return;
    await k.end();
  }));

  // The variables panel: refreshed after every cell while it's open.
  async function refreshVariables() {
    if (varsPanel.hidden) return;
    const vars = await k.variables();
    if (vars === null) return; // busy or not ready: keep what's shown
    clear(varsPanel);
    if (vars.length === 0) {
      varsPanel.append(el("p", { class: "muted" }, "No variables yet: run a cell."));
      return;
    }
    const rows = vars.map((v) => el("tr", {},
      el("td", { class: "var-name" }, v.name),
      el("td", { class: "muted" }, v.type),
      el("td", { class: "muted" }, v.shape || ""),
      el("td", { class: "var-value", title: v.columns.length ? v.columns.join(", ") : v.value },
        v.columns.length ? `columns: ${v.columns.join(", ")}` : v.value)));
    varsPanel.append(el("table", { class: "vars" },
      el("thead", {}, el("tr", {}, el("th", {}, "Name"), el("th", {}, "Type"), el("th", {}, "Shape"), el("th", {}, "Value"))),
      el("tbody", {}, rows)));
  }
  k.onExecuted = refreshVariables;
  varsBtn.addEventListener("click", () => {
    varsPanel.hidden = !varsPanel.hidden;
    varsBtn.setAttribute("aria-expanded", String(!varsPanel.hidden));
    varsBtn.classList.toggle("on", !varsPanel.hidden);
    refreshVariables();
  });

  show({ status: "none" });
  // Join the notebook's session if it has one (opened in another tab, or
  // before a reload).
  k.find().then((s) => s && s.status !== "ended" && k.attach(s)).catch(() => {});

  return {
    node,
    kernel: () => (k.status === "ready" ? k : null),
    start,
    close: () => k.close(),
    clear: () => clear(node),
  };
}
