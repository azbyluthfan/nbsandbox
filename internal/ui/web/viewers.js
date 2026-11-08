// Copyright 2026 The nbsandbox Authors
// SPDX-License-Identifier: Apache-2.0
//
// Read-only viewers: notebooks, delimited data, plain text.

import { el } from "./dom.js";
import { highlighted, languageFor } from "./editor.js";

export const MAX_PREVIEW_BYTES = 2 * 1024 * 1024;
export const MAX_TABLE_ROWS = 200;

const TEXT_EXT = new Set([
  "txt", "md", "py", "json", "yaml", "yml", "log", "ini", "toml", "cfg", "conf", "sql", "r", "sh",
  "xml", "html", "htm", "js", "ts", "go", "rs", "java", "c", "h", "cpp", "css", "env", "gitignore",
]);

export function extension(path) {
  const base = path.split("/").pop();
  const i = base.lastIndexOf(".");
  return i > 0 ? base.slice(i + 1).toLowerCase() : "";
}

export function kindOf(path) {
  const ext = extension(path);
  if (ext === "ipynb") return "notebook";
  if (ext === "csv" || ext === "tsv") return "table";
  if (TEXT_EXT.has(ext)) return "text";
  return "unknown";
}

/** True when the bytes look like text (no NUL in the first 8 KB). */
export function looksLikeText(bytes) {
  const n = Math.min(bytes.length, 8192);
  for (let i = 0; i < n; i++) if (bytes[i] === 0) return false;
  return true;
}

export function decode(bytes) {
  return new TextDecoder("utf-8", { fatal: false }).decode(bytes);
}

function sourceText(src) {
  return Array.isArray(src) ? src.join("") : src || "";
}

// Terminal colour codes appear in tracebacks; drop them.
const ANSI = /\u001b\[[0-9;]*[A-Za-z]/g;

/** Highlighted, read-only source with line numbers. */
export function codeBlock(text, lang = "python") {
  const body = text.replace(/\n$/, "");
  const n = body.split("\n").length;
  const numbers = Array.from({ length: n }, (_, i) => String(i + 1)).join("\n");
  return el("div", { class: "source" }, el("pre", { class: "ln", "aria-hidden": "true" }, numbers), el("pre", { class: "code" }, highlighted(body, lang)));
}

export function renderOutput(out) {
  switch (out.output_type) {
    case "stream":
      return el("div", { class: "output" }, sourceText(out.text));
    case "error":
      return el(
        "div",
        { class: "output error" },
        `${out.ename}: ${out.evalue}\n`,
        (out.traceback || []).join("\n").replace(ANSI, ""),
      );
    case "execute_result":
    case "display_data": {
      const data = out.data || {};
      const png = data["image/png"];
      if (typeof png === "string" && /^[A-Za-z0-9+/=\s]+$/.test(png)) {
        return el("div", { class: "output" }, el("img", { src: `data:image/png;base64,${png.replace(/\s/g, "")}`, alt: "output image" }));
      }
      if (data["text/plain"] !== undefined) return el("div", { class: "output" }, sourceText(data["text/plain"]));
      return el("div", { class: "output muted" }, `[${Object.keys(data).join(", ") || "empty"} output]`);
    }
    default:
      return null;
  }
}

/**
 * Renders a notebook read-only: markdown and raw cells as plain text, code
 * cells with line numbers and their stored outputs.
 */
export function renderNotebook(text) {
  let nb;
  try {
    nb = JSON.parse(text);
  } catch {
    return el("p", { class: "message bad" }, "This file is not valid notebook JSON.");
  }
  const wrap = el("div", { class: "notebook" });
  const cells = Array.isArray(nb.cells) ? nb.cells : [];
  if (cells.length === 0) wrap.append(el("p", { class: "muted" }, "This notebook has no cells."));
  let codeIndex = -1;
  for (const cell of cells) {
    const src = sourceText(cell.source);
    if (cell.cell_type === "code") {
      codeIndex++;
      const tags = (cell.metadata && cell.metadata.tags) || [];
      const count = cell.execution_count == null ? " " : cell.execution_count;
      const prompt = el("div", { class: "prompt" }, `In [${count}]:`, tags.includes("parameters") ? el("div", {}, "params") : null);
      const row = el("div", { class: "cell", "data-code-index": codeIndex }, prompt, codeBlock(src));
      const outs = (cell.outputs || []).map(renderOutput).filter(Boolean);
      if (outs.length) row.append(el("div", { class: "outputs" }, outs));
      wrap.append(row);
    } else {
      const lang = cell.cell_type === "markdown" ? "markdown" : "text";
      wrap.append(el("div", { class: "cell" }, el("div", { class: "prompt" }, ""), el("div", { class: "markdown" }, el("pre", {}, highlighted(src, lang)))));
    }
  }
  return wrap;
}

/** Parses delimited text (RFC 4180 quoting) up to maxRows rows. */
export function parseDelimited(text, delimiter, maxRows) {
  const rows = [];
  let row = [];
  let field = "";
  let quoted = false;
  for (let i = 0; i < text.length && rows.length < maxRows; i++) {
    const c = text[i];
    if (quoted) {
      if (c === '"' && text[i + 1] === '"') {
        field += '"';
        i++;
      } else if (c === '"') quoted = false;
      else field += c;
    } else if (c === '"' && field === "") quoted = true;
    else if (c === delimiter) {
      row.push(field);
      field = "";
    } else if (c === "\n" || c === "\r") {
      if (c === "\r" && text[i + 1] === "\n") i++;
      row.push(field);
      rows.push(row);
      row = [];
      field = "";
    } else field += c;
  }
  if ((field !== "" || row.length) && rows.length < maxRows) {
    row.push(field);
    rows.push(row);
  }
  return rows;
}

export function renderTable(text, path) {
  const delimiter = extension(path) === "tsv" ? "\t" : ",";
  const rows = parseDelimited(text, delimiter, MAX_TABLE_ROWS + 1);
  if (rows.length === 0) return el("p", { class: "muted" }, "Empty file.");
  const [header, ...body] = rows;
  const table = el(
    "table",
    { class: "data" },
    el("thead", {}, el("tr", {}, header.map((h) => el("th", {}, h)))),
    el("tbody", {}, body.map((r) => el("tr", {}, r.map((v) => el("td", {}, v))))),
  );
  const totalLines = text.split("\n").filter((l) => l !== "").length;
  const note = totalLines > MAX_TABLE_ROWS + 1 ? `Showing the first ${MAX_TABLE_ROWS} rows of ${totalLines - 1}.` : `${body.length} rows.`;
  return el("div", {}, el("p", { class: "muted" }, note), el("div", { class: "table-wrap" }, table));
}

export function renderText(text, path = "") {
  return el("pre", { class: "text" }, highlighted(text, languageFor(path)));
}
