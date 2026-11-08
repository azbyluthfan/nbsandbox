// Copyright 2026 The nbsandbox Authors
// SPDX-License-Identifier: Apache-2.0
//
// Small DOM helpers. Content from notebooks and files is only ever set as
// text (textContent), never as HTML, so nothing in a notebook can run in the
// UI.

/**
 * el("div", {class: "x", onclick: fn}, child, "text", ...) builds an element.
 * Strings become text nodes.
 */
export function el(tag, attrs = {}, ...children) {
  const e = document.createElement(tag);
  for (const [k, v] of Object.entries(attrs || {})) {
    if (v === undefined || v === null || v === false) continue;
    if (k.startsWith("on") && typeof v === "function") e.addEventListener(k.slice(2), v);
    else if (k === "class") e.className = v;
    else e.setAttribute(k, v === true ? "" : String(v));
  }
  for (const c of children.flat()) {
    if (c === undefined || c === null || c === false) continue;
    e.append(c instanceof Node ? c : document.createTextNode(String(c)));
  }
  return e;
}

export function clear(node) {
  while (node.firstChild) node.removeChild(node.firstChild);
  return node;
}

let toastTimer = 0;

/** Shows a short message in the corner (errors by default). */
export function toast(message, kind = "error") {
  const t = document.getElementById("toast");
  t.textContent = message;
  t.className = "toast" + (kind === "info" ? " info" : "");
  t.hidden = false;
  clearTimeout(toastTimer);
  toastTimer = setTimeout(() => (t.hidden = true), 6000);
}

export function formatBytes(n) {
  if (n < 1024) return `${n} B`;
  if (n < 1024 * 1024) return `${(n / 1024).toFixed(1)} KB`;
  return `${(n / 1024 / 1024).toFixed(1)} MB`;
}

export function timeAgo(iso) {
  if (!iso) return "";
  const s = Math.max(0, (Date.now() - new Date(iso).getTime()) / 1000);
  if (s < 60) return `${Math.floor(s)}s ago`;
  if (s < 3600) return `${Math.floor(s / 60)}m ago`;
  if (s < 86400) return `${Math.floor(s / 3600)}h ago`;
  return new Date(iso).toLocaleDateString();
}

export function badge(status) {
  return el("span", { class: `badge ${status}` }, status.replace("_", " "));
}

/** An icon from icons/<name>.svg, drawn in the text colour. Decorative. */
export function icon(name, extra = "") {
  return el("span", { class: `icon i-${name}${extra ? " " + extra : ""}`, "aria-hidden": "true" });
}

/** The icon for a file, by kind. */
export function fileIcon(kind) {
  return icon(kind === "notebook" ? "notebook" : kind === "table" ? "table" : kind === "text" ? "code" : "file");
}

/**
 * A ⋯ button with a menu of items [{label, icon, onclick, danger}] (null
 * items are skipped; "-" is a divider). The menu closes on a click outside
 * or Escape.
 */
export function menuButton(items, label = "More actions") {
  const menu = el("div", { class: "menu", role: "menu", hidden: true });
  const btn = el("button", { type: "button", class: "icon-button", "aria-haspopup": "menu", "aria-expanded": "false", title: label, "aria-label": label }, icon("more"));
  const close = () => {
    menu.hidden = true;
    btn.setAttribute("aria-expanded", "false");
    document.removeEventListener("click", outside, true);
    document.removeEventListener("keydown", onKey);
  };
  const outside = (e) => {
    if (!wrap.contains(e.target)) close();
  };
  const onKey = (e) => {
    if (e.key === "Escape") close();
  };
  for (const it of items) {
    if (!it) continue;
    if (it === "-") {
      menu.append(el("hr"));
      continue;
    }
    menu.append(el("button", {
      type: "button", role: "menuitem", class: it.danger ? "danger" : "",
      onclick: (e) => {
        e.stopPropagation();
        close();
        it.onclick();
      },
    }, it.icon ? icon(it.icon) : null, it.label));
  }
  btn.addEventListener("click", (e) => {
    e.stopPropagation();
    if (!menu.hidden) return close();
    menu.hidden = false;
    btn.setAttribute("aria-expanded", "true");
    document.addEventListener("click", outside, true);
    document.addEventListener("keydown", onKey);
  });
  const wrap = el("span", { class: "menu-wrap" }, btn, menu);
  return wrap;
}
