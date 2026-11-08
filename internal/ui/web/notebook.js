// Copyright 2026 The nbsandbox Authors
// SPDX-License-Identifier: Apache-2.0
//
// Notebook editing, cell by cell: one editor per cell (Python or Markdown),
// add, delete and move cells, switch a cell between code and markdown, tag
// the parameters cell, and edit the notebook's contract (input and output
// JSON Schemas in metadata.nbsandbox.contract). Stored outputs are shown
// read-only and cleared for a cell once its code changes. Saving writes the
// notebook back as nbformat 4.

import { el, clear, toast } from "./dom.js";
import { createEditor } from "./editor.js";
import { renderOutput } from "./viewers.js";

function sourceText(src) {
  return Array.isArray(src) ? src.join("") : src || "";
}

function newCellId() {
  const b = new Uint8Array(4);
  crypto.getRandomValues(b);
  return Array.from(b, (x) => x.toString(16).padStart(2, "0")).join("");
}

export class NotebookEditor {
  constructor(parent, text, { modules, onChange } = {}) {
    this.parent = parent;
    this.modules = modules;
    this.onChange = onChange || (() => {});
    this.cells = []; // {data, editor, node, outputsNode}
    try {
      this.nb = JSON.parse(text);
    } catch {
      this.nb = null;
      parent.append(el("p", { class: "message bad" }, "This file is not valid notebook JSON; open it as text by renaming it, or replace it."));
      return;
    }
    if (!Array.isArray(this.nb.cells)) this.nb.cells = [];
    this.nb.metadata = this.nb.metadata || {};
    this.wrap = el("div", { class: "notebook editing" });
    this.contractBox = this.contractEditor();
    parent.append(this.contractBox, this.wrap, el("div", { class: "cell-add" }, this.addButtons(-1)));
    this.nb.cells.forEach((c) => this.insertCell(c, this.cells.length));
    if (this.nb.cells.length === 0) this.wrap.append(el("p", { class: "muted empty-note" }, "No cells yet: add one below."));
  }

  // ---------- the notebook as text ----------

  text() {
    if (!this.nb) return "";
    const nb = { ...this.nb, cells: this.cells.map((c) => this.cellJSON(c)) };
    const contract = this.contractValue();
    nb.metadata = { ...nb.metadata };
    if (contract === undefined) {
      // unparsable: keep what was there, the box shows the error
    } else if (contract === null) {
      if (nb.metadata.nbsandbox) {
        const { contract: _, ...rest } = nb.metadata.nbsandbox;
        nb.metadata.nbsandbox = rest;
        if (Object.keys(rest).length === 0) delete nb.metadata.nbsandbox;
      }
    } else {
      nb.metadata.nbsandbox = { ...(nb.metadata.nbsandbox || {}), contract };
    }
    return JSON.stringify(nb, null, 1) + "\n";
  }

  /** The code of every code cell, for completion across cells. */
  code() {
    return this.cells
      .filter((c) => c.editor && c.data.cell_type === "code")
      .map((c) => c.editor.text)
      .join("\n");
  }

  cellJSON(c) {
    const d = { ...c.data, source: c.editor.text };
    if (d.cell_type === "code") {
      d.outputs = d.outputs || [];
      if (d.execution_count === undefined) d.execution_count = null;
    } else {
      delete d.outputs;
      delete d.execution_count;
    }
    return d;
  }

  destroy() {
    for (const c of this.cells) c.editor.destroy();
    if (this.contractEd) this.contractEd.destroy();
  }

  // ---------- cells ----------

  insertCell(data, index) {
    data.metadata = data.metadata || {};
    if (!data.id && this.nb.nbformat_minor >= 5) data.id = newCellId();
    const empty = this.wrap.querySelector(".empty-note");
    if (empty) empty.remove();
    const c = { data };
    const isCode = () => c.data.cell_type === "code";
    const tags = () => c.data.metadata.tags || [];

    const typeSel = el("select", { "aria-label": "Cell type" },
      el("option", { value: "code" }, "Code"), el("option", { value: "markdown" }, "Markdown"), el("option", { value: "raw" }, "Raw"));
    typeSel.value = data.cell_type;
    const paramBtn = el("button", { type: "button", class: "small secondary", title: "The cell papermill puts the caller's parameters after" }, "parameters");
    const syncTags = () => {
      paramBtn.hidden = !isCode();
      paramBtn.classList.toggle("on", tags().includes("parameters"));
    };
    paramBtn.addEventListener("click", () => {
      const t = tags().filter((x) => x !== "parameters");
      if (!tags().includes("parameters")) {
        // only one parameters cell: take the tag off the others
        for (const o of this.cells) {
          if (o !== c && o.data.metadata.tags) o.data.metadata.tags = o.data.metadata.tags.filter((x) => x !== "parameters");
          o.syncTags && o.syncTags();
        }
        t.push("parameters");
      }
      c.data.metadata.tags = t;
      syncTags();
      this.onChange();
    });
    c.syncTags = syncTags;

    const prompt = el("div", { class: "prompt" });
    const showPrompt = () => {
      clear(prompt).append(isCode() ? `In [${c.data.execution_count == null ? " " : c.data.execution_count}]:` : "");
    };
    c.outputsNode = el("div", { class: "outputs" });
    const showOutputs = () => {
      clear(c.outputsNode);
      const outs = (isCode() ? c.data.outputs || [] : []).map(renderOutput).filter(Boolean);
      c.outputsNode.hidden = outs.length === 0;
      c.outputsNode.append(...outs);
    };
    const editorHost = el("div", { class: "cell-editor" });
    const mkEditor = (text) =>
      createEditor(editorHost, {
        doc: text,
        lang: isCode() ? "python" : c.data.cell_type === "markdown" ? "markdown" : "text",
        modules: this.modules,
        context: () => this.code(),
        label: "Cell source",
        hint: isCode() ? "Python code" : "Markdown",
        onChange: () => {
          if (isCode() && (c.data.outputs || []).length) {
            c.data.outputs = []; // stale once the code changed
            c.data.execution_count = null;
            showOutputs();
            showPrompt();
          }
          this.onChange();
        },
      });
    c.editor = mkEditor(sourceText(data.source));
    typeSel.addEventListener("change", () => {
      const text = c.editor.text;
      c.editor.destroy();
      c.data.cell_type = typeSel.value;
      if (!isCode()) {
        c.data.outputs = [];
        c.data.metadata.tags = tags().filter((x) => x !== "parameters");
      }
      c.editor = mkEditor(text);
      syncTags();
      showPrompt();
      showOutputs();
      this.onChange();
    });

    const tools = el(
      "div",
      { class: "cell-tools" },
      typeSel,
      paramBtn,
      el("span", { class: "spacer" }),
      el("button", { type: "button", class: "small secondary", title: "Move up", onclick: () => this.move(c, -1) }, "↑"),
      el("button", { type: "button", class: "small secondary", title: "Move down", onclick: () => this.move(c, 1) }, "↓"),
      el("button", { type: "button", class: "small secondary", title: "Delete cell", onclick: () => this.remove(c) }, "✕"),
    );
    c.node = el("div", { class: "cell" }, prompt, el("div", { class: "cell-main" }, tools, editorHost, c.outputsNode), el("div", { class: "cell-add" }, this.addButtons(c)));
    syncTags();
    showPrompt();
    showOutputs();
    const before = this.cells[index];
    this.cells.splice(index, 0, c);
    this.wrap.insertBefore(c.node, before ? before.node : null);
    return c;
  }

  // Buttons that add a cell after `after` (a cell, or -1 for the end).
  addButtons(after) {
    const add = (type) => () => {
      const index = after === -1 ? this.cells.length : this.cells.indexOf(after) + 1;
      const data = type === "code"
        ? { cell_type: "code", source: "", metadata: {}, outputs: [], execution_count: null }
        : { cell_type: "markdown", source: "", metadata: {} };
      const c = this.insertCell(data, index);
      c.editor.focus();
      this.onChange();
    };
    return [
      el("button", { type: "button", class: "small secondary", onclick: add("code") }, "+ Code"),
      el("button", { type: "button", class: "small secondary", onclick: add("markdown") }, "+ Markdown"),
    ];
  }

  move(c, delta) {
    const i = this.cells.indexOf(c);
    const j = i + delta;
    if (j < 0 || j >= this.cells.length) return;
    this.cells.splice(i, 1);
    this.cells.splice(j, 0, c);
    const next = this.cells[j + 1];
    this.wrap.insertBefore(c.node, next ? next.node : null);
    this.onChange();
  }

  remove(c) {
    if (c.editor.text.trim() && !confirm("Delete this cell?")) return;
    c.editor.destroy();
    c.node.remove();
    this.cells.splice(this.cells.indexOf(c), 1);
    this.onChange();
  }

  // ---------- contract ----------

  contractEditor() {
    const current = this.nb.metadata.nbsandbox && this.nb.metadata.nbsandbox.contract;
    const box = el("details", { class: "contract" }, el("summary", {}, current ? "Contract (input and output schemas)" : "Contract: none (add input and output schemas)"));
    this.contractError = el("p", { class: "message bad", hidden: true });
    const host = el("div", {});
    box.append(
      el("p", { class: "muted" },
        "JSON Schemas for what this notebook accepts and returns. Callers' parameters are checked against \"input\"; the value from nbsandbox.output against \"output\". Leave empty for none."),
      host,
      this.contractError,
    );
    this.contractEd = createEditor(host, {
      doc: current ? JSON.stringify(current, null, 2) : "",
      lang: "json",
      label: "Contract",
      hint: '{"input": {"type": "object", "required": ["region"]}, "output": {"type": "object"}}',
      onChange: () => {
        this.contractValue();
        this.onChange();
      },
    });
    return box;
  }

  // The contract to store: an object, null (none), or undefined when the
  // text isn't valid (the error is shown and saving keeps the old one).
  contractValue() {
    if (!this.contractEd) return null;
    const t = this.contractEd.text.trim();
    const fail = (msg) => {
      this.contractError.textContent = msg;
      this.contractError.hidden = false;
      return undefined;
    };
    this.contractError.hidden = true;
    if (!t) return null;
    let v;
    try {
      v = JSON.parse(t);
    } catch (e) {
      return fail(`The contract is not valid JSON: ${e.message}`);
    }
    if (typeof v !== "object" || v === null || Array.isArray(v)) return fail('The contract must be an object: {"input": {...}, "output": {...}}');
    const extra = Object.keys(v).filter((k) => k !== "input" && k !== "output");
    if (extra.length) return fail(`Unknown contract keys: ${extra.join(", ")} (only "input" and "output")`);
    return v;
  }

  // For the publish form.
  hasContractError() {
    return this.contractValue() === undefined;
  }
}

export function toastInvalid(nbEditor) {
  if (nbEditor && nbEditor.hasContractError && nbEditor.hasContractError()) {
    toast("Fix the contract first: it isn't valid JSON.");
    return true;
  }
  return false;
}
