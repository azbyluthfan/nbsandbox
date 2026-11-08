// Copyright 2026 The nbsandbox Authors
// SPDX-License-Identifier: Apache-2.0
//
// Library-aware completion and hover docs for Python, from the runner
// image's completion index (GET /v1/runtime/completions): `pd.` lists
// pandas' public names with signatures and docs, with no kernel running.
// Names come from the index and from the code itself (imports and
// definitions across the notebook's cells); the types of variables are not
// followed (`df.` after `df = pd.read_csv(...)` is not completed).

import { el } from "./dom.js";

let index = null; // Map module -> Map name -> {kind, sig, doc}
let loading = null;

/** Loads the index once (ref: the runtime's completions, or null). */
export function loadCompletions(api, ref) {
  if (!ref || loading) return loading;
  loading = fetch(api.url("/runtime/completions"), { headers: { Accept: "application/json" } })
    .then((r) => (r.ok ? r.json() : null))
    .then((data) => {
      if (!data || typeof data.modules !== "object") return;
      const m = new Map();
      for (const [mod, entries] of Object.entries(data.modules)) {
        const names = new Map();
        for (const [name, kind, sig, doc] of entries) names.set(name, { kind, sig, doc });
        m.set(mod, names);
      }
      index = m;
    })
    .catch(() => {
      loading = null; // try again next time
    });
  return loading;
}

const KIND = { m: "namespace", c: "class", f: "function", v: "variable" };

// ---------- what the code imports and defines ----------

const IMPORT_AS = /^[ \t]*import[ \t]+([\w.]+(?:[ \t]+as[ \t]+\w+)?(?:[ \t]*,[ \t]*[\w.]+(?:[ \t]+as[ \t]+\w+)?)*)/gm;
const FROM_IMPORT = /^[ \t]*from[ \t]+([\w.]+)[ \t]+import[ \t]+\(?([\w \t,\n]+?)\)?[ \t]*$/gm;
const DEFINED = /^[ \t]*(?:(?:async[ \t]+)?def[ \t]+(\w+)|class[ \t]+(\w+)|(\w+)[ \t]*(?::[^=\n]*)?=(?!=))/gm;

/**
 * Names the code binds: aliases of modules (pd -> pandas), names imported
 * from modules (read_csv -> [pandas, read_csv]), and its own definitions.
 */
export function scope(text) {
  const modules = new Map(); // name -> module path
  const imported = new Map(); // name -> [module, name]
  const defined = new Map(); // name -> "def" | "class" | "var"
  for (const m of text.matchAll(IMPORT_AS)) {
    for (const part of m[1].split(",")) {
      const [path, alias] = part.trim().split(/[ \t]+as[ \t]+/);
      if (!path) continue;
      if (alias) modules.set(alias, path);
      else modules.set(path.split(".")[0], path.split(".")[0]);
    }
  }
  for (const m of text.matchAll(FROM_IMPORT)) {
    const from = m[1];
    for (const part of m[2].split(",")) {
      const [name, alias] = part.trim().split(/[ \t]+as[ \t]+/);
      if (!name || name === "*") continue;
      const sub = `${from}.${name}`;
      if (index && index.has(sub)) modules.set(alias || name, sub);
      else imported.set(alias || name, [from, name]);
    }
  }
  for (const m of text.matchAll(DEFINED)) {
    const name = m[1] || m[2] || m[3];
    if (name && !defined.has(name)) defined.set(name, m[1] ? "def" : m[2] ? "class" : "var");
  }
  return { modules, imported, defined };
}

/** The module a dotted expression names (pd.api -> pandas.api), or null. */
function moduleOf(dotted, sc) {
  const parts = dotted.split(".");
  let mod = sc.modules.get(parts[0]);
  if (!mod) return null;
  for (const p of parts.slice(1)) {
    const next = `${mod}.${p}`;
    if (index && index.has(next)) mod = next;
    else if (index && index.get(mod)?.get(p)?.kind === "m" && index.has(next)) mod = next;
    else return null;
  }
  return mod;
}

/** The index entry a dotted name stands for, or null. */
export function lookup(dotted, sc) {
  if (!index) return null;
  const i = dotted.lastIndexOf(".");
  if (i < 0) {
    const mod = sc.modules.get(dotted);
    if (mod && index.has(mod)) return { module: mod, name: dotted, kind: "m", sig: "", doc: "" };
    const imp = sc.imported.get(dotted);
    const e = imp && index.get(imp[0])?.get(imp[1]);
    return e ? { module: imp[0], name: imp[1], ...e } : null;
  }
  const mod = moduleOf(dotted.slice(0, i), sc);
  const name = dotted.slice(i + 1);
  if (!mod) return null;
  if (index.has(`${mod}.${name}`)) return { module: `${mod}.${name}`, name, kind: "m", sig: "", doc: "" };
  const e = index.get(mod)?.get(name);
  return e ? { module: mod, name, ...e } : null;
}

function option(name, e) {
  const o = { label: name, type: KIND[e.kind] || "variable" };
  if (e.sig) o.detail = e.sig.length > 40 ? e.sig.slice(0, 37) + "..." : e.sig;
  if (e.doc || e.sig) o.info = () => docNode(name, e);
  return o;
}

function docNode(name, e) {
  const node = el("div", { class: "cm-doc" });
  if (e.sig) node.append(el("code", { class: "cm-doc-sig" }, name + e.sig));
  if (e.doc) node.append(el("p", {}, e.doc));
  return node;
}

/**
 * The completion source. context() returns the code around this editor
 * (the notebook's other cells), so an import in cell 1 counts in cell 3.
 */
export function completionSource(context) {
  return (cx) => {
    const line = cx.state.doc.lineAt(cx.pos);
    const before = line.text.slice(0, cx.pos - line.from);

    // import pan|  /  from pan|  /  import matplotlib.py|
    let m = /^\s*(?:import|from)\s+([\w.]*)$/.exec(before);
    if (m && index) {
      const typed = m[1];
      const dot = typed.lastIndexOf(".");
      const parent = dot < 0 ? "" : typed.slice(0, dot + 1);
      const options = [];
      for (const mod of index.keys()) {
        if (!mod.startsWith(parent)) continue;
        const rest = mod.slice(parent.length);
        if (rest && !rest.includes(".")) options.push({ label: rest, type: "namespace" });
      }
      return { from: cx.pos - (typed.length - parent.length), options, validFor: /^\w*$/ };
    }

    const sc = scope(`${context ? context() : ""}\n${cx.state.doc.toString()}`);

    // from pandas import rea|
    m = /^\s*from\s+([\w.]+)\s+import\s+(?:.*,\s*)?(\w*)$/.exec(before);
    if (m && index && index.has(m[1])) {
      const options = [...index.get(m[1])].map(([n, e]) => option(n, e));
      return { from: cx.pos - m[2].length, options, validFor: /^\w*$/ };
    }

    // pd.rea|  /  plt.|  /  np.linalg.n|
    m = /([A-Za-z_]\w*(?:\.[A-Za-z_]\w*)*)\.(\w*)$/.exec(before);
    if (m) {
      const mod = moduleOf(m[1], sc);
      if (!mod || !index) return null;
      const options = [...index.get(mod)].map(([n, e]) => option(n, e));
      for (const sub of index.keys()) {
        if (sub.startsWith(mod + ".") && !sub.slice(mod.length + 1).includes(".") && !index.get(mod).has(sub.slice(mod.length + 1))) {
          options.push({ label: sub.slice(mod.length + 1), type: "namespace" });
        }
      }
      return { from: cx.pos - m[2].length, options, validFor: /^\w*$/ };
    }

    // A bare name: modules imported anywhere in the notebook, names imported
    // from them, and its definitions (the Python mode adds keywords, builtins
    // and this cell's own locals).
    const word = cx.matchBefore(/[A-Za-z_]\w*/);
    if (!word || (word.from === word.to && !cx.explicit)) return null;
    const options = [];
    for (const [name, mod] of sc.modules) options.push({ label: name, type: "namespace", detail: mod === name ? "" : mod });
    for (const [name, [mod, orig]] of sc.imported) {
      const e = index?.get(mod)?.get(orig);
      options.push(e ? option(name, e) : { label: name, type: "variable", detail: mod });
    }
    for (const [name, what] of sc.defined) {
      options.push({ label: name, type: what === "def" ? "function" : what === "class" ? "class" : "variable" });
    }
    return { from: word.from, options, validFor: /^\w*$/ };
  };
}

/** Hover docs for library names: pd.read_csv shows its signature and docs. */
export function hoverSource(context) {
  return (view, pos) => {
    if (!index) return null;
    const line = view.state.doc.lineAt(pos);
    const at = pos - line.from;
    const text = line.text;
    let start = at;
    let end = at;
    while (start > 0 && /[\w.]/.test(text[start - 1])) start--;
    while (end < text.length && /\w/.test(text[end])) end++;
    if (start === end) return null;
    const dotted = text.slice(start, end).replace(/^\.+/, "");
    const sc = scope(`${context ? context() : ""}\n${view.state.doc.toString()}`);
    const e = lookup(dotted, sc);
    if (!e || (!e.sig && !e.doc && e.kind !== "m")) return null;
    return {
      pos: line.from + start,
      end: line.from + end,
      above: true,
      create: () => {
        const dom = docNode(e.kind === "m" ? e.module : e.name, e.kind === "m" ? { ...e, doc: e.doc || `module ${e.module}` } : e);
        dom.classList.add("cm-hover-doc");
        return { dom };
      },
    };
  };
}
