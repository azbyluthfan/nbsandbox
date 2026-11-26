// Copyright 2026 The nbsandbox Authors
// SPDX-License-Identifier: Apache-2.0
//
// A run's usage over its life (docs/plan-compute-view.md): memory against
// its limit, CPU against its limit, and disk written. Plain SVG; colours
// come from app.css (the page's CSP refuses style attributes).

import { el, clear, formatBytes } from "./dom.js";

const NS = "http://www.w3.org/2000/svg";
const W = 320;
const H = 90;

function svg(tag, attrs = {}, ...children) {
  const e = document.createElementNS(NS, tag);
  for (const [k, v] of Object.entries(attrs)) e.setAttribute(k, String(v));
  for (const c of children) if (c) e.append(c);
  return e;
}

/**
 * One chart: points [[ms, value]], drawn against max (the limit, when there
 * is one, as a dashed line), labelled with the latest value.
 */
function chart(title, points, { limit = 0, format, unit = "" }) {
  const last = points.length ? points[points.length - 1][1] : 0;
  const peak = points.reduce((m, p) => Math.max(m, p[1]), 0);
  const top = Math.max(limit, peak) * 1.05 || 1;
  const t0 = points.length ? points[0][0] : 0;
  const span = points.length > 1 ? points[points.length - 1][0] - t0 : 1;
  const x = (t) => ((t - t0) / span) * W;
  const y = (v) => H - (v / top) * H;
  const line = points.map((p, i) => `${i ? "L" : "M"}${x(p[0]).toFixed(1)},${y(p[1]).toFixed(1)}`).join(" ");
  const area = points.length > 1 ? `${line} L${x(points[points.length - 1][0]).toFixed(1)},${H} L0,${H} Z` : "";
  const box = svg("svg", { viewBox: `0 0 ${W} ${H}`, preserveAspectRatio: "none", class: "chart-plot", role: "img",
    "aria-label": `${title}: ${format(last)}${unit}` },
  area ? svg("path", { d: area, class: "chart-area" }) : null,
  limit ? svg("line", { x1: 0, x2: W, y1: y(limit), y2: y(limit), class: "chart-limit" }) : null,
  points.length > 1 ? svg("path", { d: line, class: "chart-line" }) : null);
  return el("div", { class: "chart" },
    el("div", { class: "chart-head" }, el("span", {}, title), el("strong", {}, `${format(last)}${unit}`)),
    box,
    el("div", { class: "chart-foot muted" }, `peak ${format(peak)}${unit}`, limit ? ` · limit ${format(limit)}${unit}` : ""));
}

/** Fills box with the run's charts from GET …/usage. */
export function renderUsage(box, usage, active) {
  const ss = usage.samples || [];
  clear(box);
  if (ss.length < 1) {
    box.append(el("p", { class: "muted" }, active ? "Usage appears a few seconds after the run starts." :
      "No usage was sampled for this run (it was short, or this cluster can't sample)."));
    return;
  }
  const at = (s) => Date.parse(s.at);
  const charts = [
    chart("Memory", ss.map((s) => [at(s), s.memory_bytes]), { limit: usage.limits.memory_bytes, format: formatBytes }),
    chart("CPU", ss.map((s) => [at(s), s.cpus]), { limit: usage.limits.cpus, format: (v) => v.toFixed(2), unit: " cores" }),
  ];
  if (ss.some((s) => s.written_bytes != null)) {
    charts.push(chart("Written to disk", ss.filter((s) => s.written_bytes != null).map((s) => [at(s), s.written_bytes]), { format: formatBytes }));
  }
  box.append(el("div", { class: "charts" }, charts));
}
