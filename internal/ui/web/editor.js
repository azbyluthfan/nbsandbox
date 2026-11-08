// Copyright 2026 The nbsandbox Authors
// SPDX-License-Identifier: Apache-2.0
//
// Code editing and highlighting, on the vendored CodeMirror bundle.
//
// Highlighting uses CodeMirror's class highlighter in both the editor and
// the read-only views, so the same CSS (app.css, .tok-*) colours both, in
// light and dark mode. Text is always inserted as text nodes.

import {
  EditorState, Compartment, EditorView, keymap, lineNumbers, highlightActiveLine, highlightActiveLineGutter,
  drawSelection, highlightSpecialChars, placeholder, history, historyKeymap, defaultKeymap, indentWithTab,
  searchKeymap, highlightSelectionMatches, closeBrackets, closeBracketsKeymap, syntaxHighlighting,
  bracketMatching, indentOnInput, syntaxTree, linter, lintGutter, python, pythonLanguage, markdown,
  markdownLanguage, json, jsonLanguage, highlightCode, classHighlighter, autocompletion, completionKeymap,
  hoverTooltip,
} from "./vendor/codemirror.js";
import { el } from "./dom.js";
import { completionSource, hoverSource } from "./complete.js";

/** The language for a file name or a notebook cell type. */
export function languageFor(nameOrCellType) {
  const n = nameOrCellType.toLowerCase();
  if (n === "code" || n.endsWith(".py")) return "python";
  if (n === "markdown" || n.endsWith(".md")) return "markdown";
  if (n.endsWith(".json") || n.endsWith(".ipynb")) return "json";
  return "text";
}

// The page's CSP nonce: CodeMirror tags its <style> element with it.
const nonceMeta = document.querySelector('meta[name="csp-nonce"]');
const cspNonce = nonceMeta ? nonceMeta.getAttribute("content") : "";

// Tooltips follow the page's colours (light and dark). A CodeMirror theme
// outranks CodeMirror's own light tooltip styles; a stylesheet doesn't.
const uiTheme = EditorView.theme({
  ".cm-tooltip": { background: "var(--surface)", color: "var(--text)", border: "1px solid var(--border)", borderRadius: "var(--radius-sm)" },
  ".cm-tooltip-autocomplete > ul": { fontFamily: "var(--mono)", fontSize: "13px", maxHeight: "16em" },
  ".cm-tooltip-autocomplete > ul > li": { padding: "2px 8px" },
  ".cm-tooltip-autocomplete > ul > li[aria-selected]": { background: "var(--accent-soft)", color: "var(--text)" },
  ".cm-completionLabel": { color: "var(--text)" },
  ".cm-completionDetail": { color: "var(--muted)", fontStyle: "normal", marginLeft: ".75em" },
  ".cm-completionMatchedText": { textDecoration: "none", fontWeight: "600", color: "var(--accent)" },
  ".cm-tooltip.cm-completionInfo": { padding: "8px 10px", maxWidth: "min(36em, 90vw)" },
  ".cm-hover-doc": { padding: "8px 10px", maxWidth: "min(36em, 90vw)" },
});

const parsers = { python: pythonLanguage.parser, markdown: markdownLanguage.parser, json: jsonLanguage.parser };

/**
 * Highlights code into a <code> element, read-only and cheap (no editor):
 * spans with .tok-* classes, as text nodes only.
 */
export function highlighted(text, lang) {
  const code = el("code", { class: "hl" });
  const parser = parsers[lang];
  if (!parser) {
    code.append(text);
    return code;
  }
  highlightCode(
    text,
    parser.parse(text),
    classHighlighter,
    (piece, classes) => code.append(classes ? el("span", { class: classes }, piece) : piece),
    () => code.append("\n"),
  );
  return code;
}

// ---------- syntax check ----------

// IPython magics (%...) and shell lines (!...) are fine in a notebook but
// not Python; they are not flagged.
const MAGIC = /^\s*[%!]/;

/** Syntax errors from the editor's own Python parser, as diagnostics. */
function pythonSyntax(view) {
  const doc = view.state.doc;
  const out = [];
  const seenLines = new Set();
  syntaxTree(view.state).iterate({
    enter(node) {
      if (!node.type.isError) return;
      const line = doc.lineAt(node.from);
      if (MAGIC.test(line.text) || seenLines.has(line.number)) return;
      seenLines.add(line.number);
      const to = Math.min(Math.max(node.to, node.from + 1), line.to || node.from + 1);
      out.push({ from: node.from, to: Math.max(to, node.from), severity: "error", message: "Syntax error" });
    },
  });
  return out;
}

const IMPORT = /^\s*(?:from\s+([A-Za-z_][\w]*)[\w.]*\s+import\b|import\s+(.+))/;

/** Imports of top-level modules the runner image doesn't provide. */
function missingImports(view, provided) {
  if (!provided || provided.size === 0) return [];
  const out = [];
  const doc = view.state.doc;
  for (let i = 1; i <= doc.lines; i++) {
    const line = doc.line(i);
    const m = IMPORT.exec(line.text);
    if (!m) continue;
    const names = m[1] ? [m[1]] : m[2].split(",").map((s) => s.trim().split(/\s+/)[0].split(".")[0]);
    for (const name of names) {
      if (!name || name.startsWith("_") || provided.has(name)) continue;
      const at = line.from + line.text.indexOf(name);
      out.push({
        from: at, to: at + name.length, severity: "warning",
        message: `The runner image has no module "${name}": the run will fail with ModuleNotFoundError (unless it's a file in the space).`,
      });
    }
  }
  return out;
}

/**
 * Creates an editor.
 *   lang: python | markdown | json | text
 *   onChange(text): after every change
 *   modules: Set of importable module names, for the missing-package check
 *   context(): the code around this editor (a notebook's other cells), so
 *     completion knows what they import and define
 */
export function createEditor(parent, { doc = "", lang = "text", readOnly = false, onChange, modules, label, hint, context } = {}) {
  const langExt = { python: python(), markdown: markdown(), json: json() }[lang] || [];
  const checks = [];
  if (lang === "python" && !readOnly) {
    checks.push(linter((view) => [...pythonSyntax(view), ...missingImports(view, modules)], { delay: 400 }), lintGutter());
    checks.push(autocompletion(), pythonLanguage.data.of({ autocomplete: completionSource(context) }));
  }
  if (lang === "python") checks.push(hoverTooltip(hoverSource(context), { hoverTime: 400 }));
  const editable = new Compartment();
  const view = new EditorView({
    parent,
    state: EditorState.create({
      doc,
      extensions: [
        lineNumbers(), highlightActiveLineGutter(), highlightSpecialChars(), history(), drawSelection(),
        indentOnInput(), bracketMatching(), closeBrackets(), highlightActiveLine(), highlightSelectionMatches(),
        cspNonce ? EditorView.cspNonce.of(cspNonce) : [],
        syntaxHighlighting(classHighlighter), uiTheme,
        keymap.of([...closeBracketsKeymap, ...completionKeymap, ...defaultKeymap, ...searchKeymap, ...historyKeymap, indentWithTab]),
        langExt, checks,
        hint ? placeholder(hint) : [],
        editable.of([EditorView.editable.of(!readOnly), EditorState.readOnly.of(readOnly)]),
        EditorView.contentAttributes.of(label ? { "aria-label": label } : {}),
        EditorView.updateListener.of((u) => {
          if (u.docChanged && onChange) onChange(u.state.doc.toString());
        }),
      ],
    }),
  });
  return {
    view,
    get text() {
      return view.state.doc.toString();
    },
    set text(t) {
      view.dispatch({ changes: { from: 0, to: view.state.doc.length, insert: t } });
    },
    setReadOnly(ro) {
      view.dispatch({ effects: editable.reconfigure([EditorView.editable.of(!ro), EditorState.readOnly.of(ro)]) });
    },
    focus() {
      view.focus();
    },
    destroy() {
      view.destroy();
    },
  };
}
