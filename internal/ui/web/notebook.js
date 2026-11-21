// Copyright 2026 The nbsandbox Authors
// SPDX-License-Identifier: Apache-2.0
//
// Notebook editing, cell by cell: one editor per cell (Python or Markdown),
// add, delete and move cells, switch a cell between code and markdown, tag
// the parameters cell, and edit the notebook's contract (input and output
// JSON Schemas in metadata.nbsandbox.contract). Stored outputs are shown
// read-only and cleared for a cell once its code changes. Saving writes the
// notebook back as nbformat 4.
//
// With an interactive session (session.js), code cells run in its kernel:
// the run button, Shift-Enter (run and go to the next cell) or Ctrl/⌘-Enter
// (run in place). Outputs appear as they come and are kept like Jupyter's:
// editing the code doesn't clear them, and saving stores them.

import { el, clear, toast, icon } from "./dom.js";
import { createEditor } from "./editor.js";
import { renderOutput } from "./viewers.js";
import { signatureAt } from "./complete.js";

// The most a cell's outputs take in the saved notebook (as JSON); later
// outputs are left out, with a note, as a run's output.ipynb is capped.
const MAX_CELL_OUTPUT = 1 << 20;

function sourceText(src) {
  return Array.isArray(src) ? src.join("") : src || "";
}

function newCellId() {
  const b = new Uint8Array(4);
  crypto.getRandomValues(b);
  return Array.from(b, (x) => x.toString(16).padStart(2, "0")).join("");
}

export class NotebookEditor {
  /**
   * kernel(): the notebook's KernelSession while it has one (or null);
   * startKernel(): opens one and resolves to it, for a cell run without one.
   */
  constructor(parent, text, { modules, onChange, kernel, startKernel } = {}) {
    this.parent = parent;
    this.modules = modules;
    this.onChange = onChange || (() => {});
    this.kernel = kernel || (() => null);
    this.startKernel = startKernel || null;
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
      const n = c.running ? "*" : c.data.execution_count == null ? " " : c.data.execution_count;
      clear(prompt).append(isCode() ? `In [${n}]:` : "");
      c.node && c.node.classList.toggle("running", !!c.running);
    };
    c.showPrompt = () => showPrompt();
    c.outputsNode = el("div", { class: "outputs" });
    c.sigNode = el("div", { class: "sig-help", hidden: true });
    const showOutputs = () => {
      clear(c.outputsNode);
      const outs = (isCode() ? c.data.outputs || [] : []).map(renderOutput).filter(Boolean);
      c.outputsNode.hidden = outs.length === 0;
      c.outputsNode.append(...outs);
    };
    c.showOutputs = () => showOutputs();
    const editorHost = el("div", { class: "cell-editor" });
    const mkEditor = (text) =>
      createEditor(editorHost, {
        doc: text,
        lang: isCode() ? "python" : c.data.cell_type === "markdown" ? "markdown" : "text",
        modules: this.modules,
        context: () => this.code(),
        kernel: () => this.kernel(),
        keys: [
          { key: "Shift-Enter", run: () => (this.runCell(c, true), true) },
          { key: "Mod-Enter", run: () => (this.runCell(c, false), true) },
          { key: "Ctrl-Enter", run: () => (this.runCell(c, false), true) }, // Jupyter's, on a Mac too
          { key: "Escape", run: () => ((c.sigNode.hidden = true), false) },
        ],
        onCursor: (state, changed) => this.signature(c, state, changed),
        label: "Cell source",
        hint: isCode() ? "Python code" : "Markdown",
        onChange: () => {
          // Without a session, outputs belong to an older version of the
          // code; with one, they stay until the cell runs again (Jupyter).
          if (isCode() && (c.data.outputs || []).length && !this.kernel()) {
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
      c.syncTags();
      showPrompt();
      showOutputs();
      this.onChange();
    });

    c.runBtn = el("button", { type: "button", class: "small secondary run-cell", title: "Run this cell in the session (Shift-Enter)",
      "aria-label": "Run cell", onclick: () => this.runCell(c, false) }, icon("play"));
    const tools = el(
      "div",
      { class: "cell-tools" },
      c.runBtn,
      typeSel,
      paramBtn,
      el("span", { class: "spacer" }),
      el("button", { type: "button", class: "small secondary", title: "Move up", onclick: () => this.move(c, -1) }, "↑"),
      el("button", { type: "button", class: "small secondary", title: "Move down", onclick: () => this.move(c, 1) }, "↓"),
      el("button", { type: "button", class: "small secondary", title: "Delete cell", onclick: () => this.remove(c) }, "✕"),
    );
    c.node = el("div", { class: "cell" }, prompt, el("div", { class: "cell-main" }, tools, editorHost, c.sigNode, c.outputsNode), el("div", { class: "cell-add" }, this.addButtons(c)));
    const syncTagsBase = syncTags;
    c.syncTags = () => {
      syncTagsBase();
      c.runBtn.hidden = !isCode();
    };
    c.syncTags();
    showPrompt();
    showOutputs();
    const before = this.cells[index];
    this.cells.splice(index, 0, c);
    this.wrap.insertBefore(c.node, before ? before.node : null);
    return c;
  }

  // ---------- running cells in the session ----------

  /**
   * Runs a code cell in the session's kernel (opening a session first if
   * there is none); advance: then focus the next cell, adding one at the end.
   */
  async runCell(c, advance) {
    if (advance) this.focusNext(c);
    if (c.data.cell_type !== "code") return;
    let k = this.kernel();
    if (!k || !k.ready) {
      if (!this.startKernel) return toast("Start a session to run cells.");
      try {
        k = await this.startKernel();
      } catch (e) {
        return toast(e.message);
      }
      if (!k || !k.ready) return;
    }
    const code = c.editor.text;
    c.sigNode.hidden = true;
    c.data.outputs = [];
    c.outputBytes = 0;
    c.clearNext = false;
    c.running = true;
    c.showPrompt();
    c.showOutputs();
    try {
      k.execute(code, {
        input: (n) => {
          c.data.execution_count = n;
        },
        output: (msg) => this.addOutput(c, msg),
        done: (reply) => {
          c.running = false;
          if (reply && reply.execution_count != null) c.data.execution_count = reply.execution_count;
          c.showPrompt();
          this.onChange();
        },
        error: (why) => {
          c.running = false;
          c.showPrompt();
          toast(`The cell didn't finish: ${why}.`);
        },
      });
    } catch (e) {
      c.running = false;
      c.showPrompt();
      toast(e.message);
    }
  }

  /** Runs every code cell in order, each after the previous one finished. */
  async runAll() {
    for (const c of this.cells) {
      if (c.data.cell_type !== "code") continue;
      await this.runCell(c, false);
      await new Promise((resolve) => {
        const wait = () => (c.running ? setTimeout(wait, 100) : resolve());
        wait();
      });
      if ((c.data.outputs || []).some((o) => o.output_type === "error")) break; // as Jupyter stops on an error
    }
  }

  focusNext(c) {
    const i = this.cells.indexOf(c);
    let next = this.cells[i + 1];
    if (!next) {
      next = this.insertCell({ cell_type: "code", source: "", metadata: {}, outputs: [], execution_count: null }, this.cells.length);
      this.onChange();
    }
    next.editor.focus();
    next.node.scrollIntoView({ block: "nearest" });
  }

  // A Jupyter iopub message, as an nbformat output of the cell.
  addOutput(c, msg) {
    const t = msg.header.msg_type;
    const m = msg.content || {};
    if (t === "clear_output") {
      if (m.wait) c.clearNext = true;
      else {
        c.data.outputs = [];
        c.outputBytes = 0;
      }
      c.showOutputs();
      return;
    }
    let out = null;
    if (t === "stream") out = { output_type: "stream", name: m.name, text: m.text };
    else if (t === "display_data" || t === "update_display_data") out = { output_type: "display_data", data: m.data || {}, metadata: m.metadata || {} };
    else if (t === "execute_result") out = { output_type: "execute_result", data: m.data || {}, metadata: m.metadata || {}, execution_count: m.execution_count };
    else if (t === "error") out = { output_type: "error", ename: m.ename, evalue: m.evalue, traceback: m.traceback || [] };
    if (!out) return;
    if (c.clearNext) {
      c.data.outputs = [];
      c.outputBytes = 0;
      c.clearNext = false;
    }
    const outs = c.data.outputs;
    const last = outs[outs.length - 1];
    if (out.output_type === "stream" && last && last.output_type === "stream" && last.name === out.name) {
      last.text += out.text; // one stream output, as Jupyter keeps them
    } else {
      const size = JSON.stringify(out).length;
      if ((c.outputBytes || 0) + size > MAX_CELL_OUTPUT) {
        if (!c.capped) {
          c.capped = true;
          outs.push({ output_type: "stream", name: "stderr", text: "[nbsandbox] more output not shown or saved: over 1 MiB for this cell\n" });
        }
      } else {
        c.outputBytes = (c.outputBytes || 0) + size;
        outs.push(out);
      }
    }
    c.showOutputs();
  }

  // After "(": the signature of the call, from the kernel, under the cell.
  signature(c, state, changed) {
    const k = this.kernel();
    if (!k || c.data.cell_type !== "code") return;
    const pos = state.selection.main.head;
    const before = state.doc.sliceString(Math.max(0, pos - 1), pos);
    if (before === ")") {
      c.sigNode.hidden = true;
      return;
    }
    if (!changed || (before !== "(" && before !== ",")) return;
    clearTimeout(c.sigTimer);
    c.sigTimer = setTimeout(async () => {
      const sig = await signatureAt(k, state.doc.toString(), pos);
      c.sigNode.textContent = sig;
      c.sigNode.hidden = !sig;
    }, 120);
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
