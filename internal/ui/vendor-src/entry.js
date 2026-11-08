// Copyright 2026 The nbsandbox Authors
// SPDX-License-Identifier: Apache-2.0
//
// The editor bundle for the local web UI: exactly the CodeMirror parts the UI
// uses, bundled into web/vendor/codemirror.js by `make ui-vendor`.

export { EditorState, Compartment } from "@codemirror/state";
export {
  EditorView, keymap, lineNumbers, highlightActiveLine, highlightActiveLineGutter, drawSelection,
  highlightSpecialChars, placeholder, hoverTooltip,
} from "@codemirror/view";
export { history, historyKeymap, defaultKeymap, indentWithTab } from "@codemirror/commands";
export { searchKeymap, highlightSelectionMatches } from "@codemirror/search";
export {
  closeBrackets, closeBracketsKeymap, autocompletion, completionKeymap, startCompletion,
} from "@codemirror/autocomplete";
export {
  syntaxHighlighting, defaultHighlightStyle, bracketMatching, indentOnInput, foldGutter, foldKeymap, syntaxTree,
} from "@codemirror/language";
export { linter, lintGutter } from "@codemirror/lint";
export { python, pythonLanguage } from "@codemirror/lang-python";
export { markdown, markdownLanguage } from "@codemirror/lang-markdown";
export { json, jsonLanguage } from "@codemirror/lang-json";
export { oneDark } from "@codemirror/theme-one-dark";
export { highlightCode, classHighlighter } from "@lezer/highlight";
