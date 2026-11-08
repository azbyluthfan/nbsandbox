// Copyright 2026 The nbsandbox Authors
// SPDX-License-Identifier: Apache-2.0
//
// A file opened in the UI: highlighted and editable when it is text or a
// notebook, with the version it was opened at.
//
// Versions: the page works on the version it loaded. If the file changes
// elsewhere (another tab, another client, a run), a notice says so and the
// page doesn't change; Reload fetches the newest. Saving sends the version
// being replaced; if the file changed meanwhile the server refuses (412) and
// the user chooses: reload, save as a new file, or overwrite.

import { ApiError } from "./api.js";
import { el, clear, toast, formatBytes, timeAgo, icon, fileIcon } from "./dom.js";
import { kindOf, looksLikeText, decode, renderTable, MAX_PREVIEW_BYTES } from "./viewers.js";
import { createEditor, languageFor } from "./editor.js";
import { NotebookEditor } from "./notebook.js";

const CHECK_EVERY_MS = 30000;

/**
 * Opens path in main. ctx: {api, space, path, file, modules, onSaved(path),
 * headActions(path) -> [elements] (next to Download),
 * extraActions(path) -> [elements] (below the header), onMoved(newPath)}.
 * Returns {dirty(), stop()}.
 */
export function openFilePage(main, ctx) {
  const { api, space, path, file } = ctx;
  let version = file ? file.version : "";
  let savedAt = file ? file.modified : ""; // shown as "saved 3m ago"; the version is in the tooltip
  let doc = null; // {text(): string, destroy()}
  let saved = ""; // text as last loaded or saved
  let stopped = false;
  let timer = 0;
  let inConflict = false; // a save was refused: the choices stay until one is made

  const status = el("span", { class: "doc-status muted" });
  const saveBtn = el("button", { type: "button", disabled: true, title: "Save (Ctrl/⌘-S)" }, icon("save"), "Save");
  const reloadBtn = el("button", { type: "button", class: "icon-button", title: "Reload from the space", "aria-label": "Reload" }, icon("refresh"));
  const download = el("a", { href: api.fileUrl(space, path), download: path.split("/").pop(), class: "icon-button", title: "Download", "aria-label": "Download" }, icon("download"));
  const banner = el("div", { class: "banner", hidden: true });
  const body = el("div", { class: "doc-body" });
  const head = el(
    "div",
    { class: "file-head" },
    el("h1", {}, fileIcon(kindOf(path)), path),
    file ? el("span", { class: "muted doc-status" }, formatBytes(file.size)) : null,
    el("span", { class: "spacer" }),
    status,
    saveBtn,
    reloadBtn,
    download,
    ...(ctx.headActions ? ctx.headActions(path) : []),
  );
  clear(main).append(head, ...(ctx.extraActions ? ctx.extraActions(path) : []), banner, body);
  if (!file) {
    body.append(el("p", { class: "message bad" }, "This file is not in the space (any more)."));
    return { dirty: () => false, stop() {} };
  }

  const dirty = () => doc !== null && doc.text() !== saved;
  const showState = () => {
    saveBtn.disabled = !dirty();
    status.textContent = dirty() ? "unsaved changes" : savedAt ? `saved ${timeAgo(savedAt)}` : "";
    status.title = version ? `version ${version}` : "";
    status.className = "doc-status " + (dirty() ? "warn" : "muted");
  };

  function setBanner(...content) {
    inConflict = false;
    clear(banner);
    banner.hidden = content.length === 0;
    banner.append(...content);
  }

  async function load() {
    setBanner();
    if (doc) {
      doc.destroy();
      doc = null;
    }
    clear(body);
    if (file.size > MAX_PREVIEW_BYTES) {
      body.append(el("p", { class: "muted" }, "Too large to open here; use Download."));
      return;
    }
    let got;
    try {
      got = await api.fileWithVersion(space, path);
    } catch (e) {
      body.append(el("p", { class: "message bad" }, e.message));
      return;
    }
    if (stopped) return;
    version = got.version;
    const kind = kindOf(path);
    if (kind === "notebook") {
      doc = new NotebookEditor(body, decode(got.bytes), { modules: ctx.modules, onChange: showState });
    } else if (kind === "table" && !ctx.editTables) {
      body.append(
        el("p", {}, el("button", { type: "button", class: "secondary", onclick: () => { ctx.editTables = true; load(); } }, "Edit as text")),
        renderTable(decode(got.bytes), path),
      );
    } else if (kind !== "unknown" || looksLikeText(got.bytes)) {
      const ed = createEditor(body, { doc: decode(got.bytes), lang: languageFor(path), modules: ctx.modules, onChange: showState, label: path });
      doc = { text: () => ed.text, destroy: () => ed.destroy() };
    } else {
      body.append(el("p", { class: "muted" }, "Binary file: no preview. Use Download."));
    }
    saved = doc ? doc.text() : "";
    showState();
  }

  async function save(asVersion) {
    if (!doc) return;
    const text = doc.text();
    saveBtn.disabled = true;
    try {
      const f = await api.saveFile(space, path, text, asVersion || version);
      version = f.version;
      savedAt = f.modified || new Date().toISOString();
      saved = text;
      setBanner();
      toast("Saved.", "info");
      if (ctx.onSaved) ctx.onSaved(path);
    } catch (e) {
      if (e instanceof ApiError && e.status === 412) conflict(e.body ? e.body.current_version : "", text);
      else toast(e.message);
    } finally {
      showState();
    }
  }

  // The file changed since this page loaded it: the user decides.
  function conflict(current, text) {
    const gone = !current;
    setBanner();
    inConflict = true;
    banner.hidden = false;
    banner.append(
      el("strong", {}, gone ? "This file was deleted elsewhere." : "This file was changed elsewhere since you opened it."),
      " Your edits are still here. ",
      el("button", { type: "button", class: "secondary", onclick: () => confirmDiscard() && load() }, gone ? "Close" : "Reload (lose my edits)"),
      el("button", { type: "button", class: "secondary", onclick: () => saveAs(text) }, "Save as a new file…"),
      gone
        ? el("button", { type: "button", onclick: () => save(null) }, "Save it again")
        : el("button", { type: "button", class: "danger", onclick: () => save(current) }, "Overwrite their changes"),
    );
  }

  async function saveAs(text) {
    const dot = path.lastIndexOf(".");
    const suggestion = dot > 0 ? `${path.slice(0, dot)} (copy)${path.slice(dot)}` : `${path} (copy)`;
    const to = prompt("Save your version as:", suggestion);
    if (!to) return;
    try {
      await api.saveFile(space, to.trim(), text, null);
      toast(`Saved as ${to.trim()}.`, "info");
      if (ctx.onMoved) ctx.onMoved(to.trim());
    } catch (e) {
      toast(e instanceof ApiError && e.status === 412 ? `${to} already exists.` : e.message);
    }
  }

  function confirmDiscard() {
    return !dirty() || confirm("Discard your unsaved changes?");
  }

  // A newer version: tell, don't touch.
  async function checkNewer() {
    if (stopped || !version || document.hidden || inConflict) return;
    const state = await api.checkVersion(space, path, version).catch(() => "same");
    if (stopped || state === "same") return;
    if (state === "gone") return setBanner(el("strong", {}, "This file was deleted elsewhere."), " What you see is the version you opened.");
    setBanner(
      el("strong", {}, "A newer version of this file exists."),
      dirty() ? " Saving will ask what to do with it. " : " ",
      el("button", { type: "button", class: "secondary", onclick: () => confirmDiscard() && load() }, "Reload"),
    );
  }

  saveBtn.addEventListener("click", () => save());
  reloadBtn.addEventListener("click", () => confirmDiscard() && load());
  const onKey = (e) => {
    if ((e.ctrlKey || e.metaKey) && e.key === "s") {
      e.preventDefault();
      if (dirty()) save();
    }
  };
  const onFocus = () => checkNewer();
  const onUnload = (e) => {
    if (dirty()) {
      e.preventDefault();
      e.returnValue = "";
    }
  };
  document.addEventListener("keydown", onKey);
  window.addEventListener("focus", onFocus);
  window.addEventListener("beforeunload", onUnload);
  timer = setInterval(checkNewer, CHECK_EVERY_MS);
  load();

  return {
    dirty,
    stop() {
      stopped = true;
      clearInterval(timer);
      document.removeEventListener("keydown", onKey);
      window.removeEventListener("focus", onFocus);
      window.removeEventListener("beforeunload", onUnload);
      if (doc) doc.destroy();
    },
  };
}
