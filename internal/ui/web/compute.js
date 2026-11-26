// Copyright 2026 The nbsandbox Authors
// SPDX-License-Identifier: Apache-2.0
//
// The compute view (docs/plan-compute-view.md): the workers, what each runs
// on and how many runs and sessions it holds. Every signed-in user sees the
// counts; administrators also see which runs and sessions are on each.

import { ApiError } from "./api.js";
import { el, clear, timeAgo, formatBytes } from "./dom.js";

const REFRESH = 5000;

/** Fills page with the workers, refreshed every few seconds; stop() ends that. */
export function showCompute(api, page) {
  clear(page).append(el("div", { class: "hero" }, el("h1", {}, "Compute"),
    el("p", {}, "The workers that run notebooks and interactive sessions, and how busy each is.")));
  const body = el("div", { class: "section" }, el("p", { class: "muted" }, "Loading…"));
  page.append(body);
  let admin = null; // unknown until the admin lists answer (403: not an administrator)
  let timer = 0;
  let stopped = false;
  const open = new Set(); // workers whose runs and sessions are shown

  const adminLists = async (w) => {
    if (admin === false) return null;
    const q = w.role === "all" ? "" : `?worker=${encodeURIComponent(w.id)}`;
    try {
      const [runs, sessions] = await Promise.all([
        api.request("GET", `/admin/runs${q ? q + "&" : "?"}status=running&limit=50`).then((d) => d.items),
        api.request("GET", `/admin/sessions${q}`).then((d) => d.items).catch((e) => {
          if (e instanceof ApiError && e.status === 404) return []; // sessions off
          throw e;
        }),
      ]);
      admin = true;
      return { runs, sessions };
    } catch (e) {
      if (e instanceof ApiError && e.status === 403) {
        admin = false;
        return null;
      }
      throw e;
    }
  };

  const render = async () => {
    let workers;
    try {
      workers = (await api.request("GET", "/workers")).items;
    } catch (e) {
      clear(body).append(el("p", { class: "error-text" }, e.message));
      return;
    }
    const live = workers.filter((w) => w.state !== "gone");
    const total = (k, f) => live.reduce((n, w) => n + w[k][f], 0);
    const rows = [];
    for (const w of workers) {
      rows.push(el("tr", { class: w.state === "gone" ? "gone" : "" },
        el("td", {}, el("strong", {}, w.id), el("div", { class: "muted small" }, w.role === "all" ? "single process" : "worker")),
        el("td", {}, el("span", { class: `badge worker-${w.state}` }, w.state)),
        el("td", {}, slots(w.runs)),
        el("td", {}, w.sessions.capacity ? slots(w.sessions) : el("span", { class: "muted" }, "off")),
        el("td", { class: "muted" }, [w.runtime || "?", w.platform].filter(Boolean).join(" · "),
          el("div", { class: "small" }, `each run: ${formatBytes(w.run_limits.memory_bytes)}, ${w.run_limits.cpus} CPU`)),
        el("td", { class: "muted" }, w.version || "", el("div", { class: "small" },
          w.state === "gone" ? `last seen ${timeAgo(w.seen_at)}` : w.started_at ? `up since ${timeAgo(w.started_at)}` : "")),
        el("td", {}, admin === false || w.state === "gone" ? null : el("button", { type: "button", class: "ghost small",
          onclick: () => { open.has(w.id) ? open.delete(w.id) : open.add(w.id); render(); } }, open.has(w.id) ? "Hide" : "Show")),
      ));
      if (open.has(w.id) && w.state !== "gone") {
        const cell = el("td", { colspan: 7 }, el("p", { class: "muted" }, "Loading…"));
        rows.push(el("tr", { class: "detail" }, cell));
        adminLists(w).then((l) => {
          if (!l) return clear(cell).append(el("p", { class: "muted" }, "Only administrators see whose runs and sessions these are."));
          clear(cell).append(
            l.runs.length ? el("table", { class: "list inner" }, el("tbody", {}, l.runs.map((r) => el("tr", {},
              el("td", {}, r.notebook), el("td", { class: "muted" }, r.owner), el("td", { class: "muted mono" }, r.space_id.slice(0, 8)),
              el("td", { class: "muted" }, r.started_at ? `started ${timeAgo(r.started_at)}` : "starting"))))) : el("p", { class: "muted" }, "No runs."),
            l.sessions.length ? el("table", { class: "list inner" }, el("tbody", {}, l.sessions.map((s) => el("tr", {},
              el("td", {}, s.notebook, el("span", { class: "muted" }, " (session)")), el("td", { class: "muted" }, s.owner),
              el("td", { class: "muted mono" }, s.space_id.slice(0, 8)), el("td", { class: "muted" }, `opened ${timeAgo(s.created_at)}`))))) : null);
        }).catch((e) => clear(cell).append(el("p", { class: "error-text" }, e.message)));
      }
    }
    clear(body).append(
      el("div", { class: "section-head" }, el("h2", {}, `${live.length} worker${live.length === 1 ? "" : "s"}`),
        el("span", { class: "muted" }, `runs ${total("runs", "active")} of ${total("runs", "capacity")} · sessions ${total("sessions", "active")} of ${total("sessions", "capacity")}`)),
      workers.length ? el("table", { class: "list compute" },
        el("thead", {}, el("tr", {}, ["Worker", "State", "Runs", "Sessions", "Runs on", "Version", ""].map((h) => el("th", {}, h)))),
        el("tbody", {}, rows))
        : el("p", { class: "muted" }, "No worker has reported in yet."));
  };

  const loop = async () => {
    await render();
    if (!stopped) timer = setTimeout(loop, REFRESH);
  };
  loop();
  return { stop: () => { stopped = true; clearTimeout(timer); }, dirty: () => false };
}

function slots(s) {
  const pct = s.capacity ? Math.min(100, Math.round((100 * s.active) / s.capacity)) : 0;
  const bar = el("div");
  bar.style.width = `${pct}%`; // the page's CSP refuses style attributes, not the CSSOM
  return el("div", { class: "slots" }, el("span", {}, `${s.active} of ${s.capacity}`), el("div", { class: "meter" }, bar));
}
