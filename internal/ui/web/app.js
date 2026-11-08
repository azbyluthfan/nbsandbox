// Copyright 2026 The nbsandbox Authors
// SPDX-License-Identifier: Apache-2.0
//
// The local web UI: pick a user and a space, browse and manage files, open
// and edit notebooks and text files, run them and follow the live log,
// publish notebooks as functions and call them.
//
// Layout: a top bar (space, runs, functions, user), a file explorer on the
// left that can be hidden, the page in the middle, and a drawer on the right
// for the space's runs and functions.
//
// Everything visible is addressable, so a reload or a copied link reopens
// the same view:
//   #/<user>                     the user's spaces
//   #/<user>/<space>             the space's overview
//   #/<user>/<space>/f/<path>    a file
//   #/<user>/<space>/d/<path>    a folder
//   #/<user>/<space>/r/<run>     a run
//   #/<user>/<space>/fn/<name>   a function

import { Api, listUsers, ApiError } from "./api.js";
import { el, clear, toast, formatBytes, timeAgo, badge, icon, fileIcon, menuButton } from "./dom.js";
import { kindOf } from "./viewers.js";
import { showRun } from "./run.js";
import { openFilePage } from "./filepage.js";
import { showFunction, publishDialog } from "./functions.js";
import { loadCompletions } from "./complete.js";

const $ = (id) => document.getElementById(id);
const NEW_SPACE = "__new__";
const ACTIVE = new Set(["queued", "running"]);
const narrow = () => window.matchMedia("(max-width: 860px)").matches;

const state = {
  users: [], user: "", api: null, spaces: [], space: "", files: [], runs: [], functions: [],
  modules: new Set(), page: null, hash: "", ignoreHash: false,
  collapsed: new Set(), // folders folded in the explorer, per space (remembered in this browser)
  drawerTab: "runs",
};

/** "1 file", "3 files". */
function count(n, word) {
  return `${n} ${word}${n === 1 ? "" : "s"}`;
}

// Per-browser conveniences only; the page works without them.
function remember(key, value) {
  try {
    localStorage.setItem(key, JSON.stringify(value));
  } catch {
    /* private window or blocked storage */
  }
}
function recall(key, fallback) {
  try {
    const v = localStorage.getItem(key);
    return v === null ? fallback : JSON.parse(v);
  } catch {
    return fallback;
  }
}

// ---------- routing ----------

function parseHash() {
  const parts = location.hash.replace(/^#\/?/, "").split("/").map(decodeURIComponent);
  const [user = "", space = "", kind = "", ...rest] = parts;
  return { user, space, kind, target: rest.join("/") };
}

function go(user, space = "", kind = "", target = "") {
  const segs = [user, space, kind].filter(Boolean).map(encodeURIComponent);
  if (target) segs.push(target.split("/").map(encodeURIComponent).join("/"));
  const h = "#/" + segs.join("/");
  if (narrow()) setExplorer(false, false);
  if (location.hash === h) route();
  else location.hash = h;
}

async function route() {
  if (state.ignoreHash) {
    state.ignoreHash = false;
    return;
  }
  // Leaving a page with unsaved edits needs a yes.
  if (state.page && state.page.dirty && state.page.dirty() && location.hash !== state.hash) {
    if (!confirm("You have unsaved changes. Leave this page and lose them?")) {
      state.ignoreHash = true;
      location.hash = state.hash;
      return;
    }
  }
  if (state.page) {
    state.page.stop();
    state.page = null;
  }
  state.hash = location.hash;
  const h = parseHash();
  const user = state.users.includes(h.user) ? h.user : state.users[0];
  if (!user) return;
  if (user !== state.user) await selectUser(user);
  if (h.space && !state.spaces.some((s) => s.id === h.space)) {
    // A space created elsewhere since the list was loaded: look again.
    try {
      state.spaces = await state.api.spaces();
      renderSpaceSelect();
    } catch {
      /* keep the list we have */
    }
  }
  const space = state.spaces.some((s) => s.id === h.space) ? h.space : "";
  if (space !== state.space) {
    if (space) newPage().append(el("p", { class: "muted" }, "Loading…"));
    await selectSpace(space);
  } else if (space && (h.kind === "" || h.kind === "d")) {
    // Pages that list what's in the space show it as it is now (other
    // clients may have changed it since it was loaded).
    await reload();
  } else renderChrome();

  if (!space) return showSpaces();
  if (h.kind === "f" && h.target) return openFile(h.target);
  if (h.kind === "d" && h.target) return openFolder(h.target);
  if (h.kind === "r" && h.target) return openRun(h.target);
  if (h.kind === "fn" && h.target) return openFunction(h.target);
  return showOverview();
}

/** A fresh page in the main area, scrolled to the top. */
function newPage(extra = "") {
  const page = el("div", { class: "page" + (extra ? " " + extra : "") });
  clear($("main")).append(page);
  $("main").scrollTop = 0;
  return page;
}

// ---------- user and space ----------

async function selectUser(user) {
  state.user = user;
  state.api = new Api(user);
  $("user-select").value = user;
  $("user-avatar").textContent = user.slice(0, 1);
  try {
    state.spaces = await state.api.spaces();
  } catch (e) {
    state.spaces = [];
    toast(e.message);
  }
  state.space = "";
  renderSpaceSelect();
  checkRuntime();
}

function renderSpaceSelect() {
  const sel = clear($("space-select"));
  sel.append(el("option", { value: "" }, "All spaces"));
  for (const s of state.spaces) sel.append(el("option", { value: s.id }, s.name || s.id.slice(0, 8)));
  sel.append(el("option", { value: NEW_SPACE }, "+ New space…"));
  sel.value = state.space;
}

// Modules the runner image provides (for the editor's import check), its
// completion index, and whether runs can start at all. Checked again every
// 30 s while runs are paused or the index isn't there yet (a worker reads it
// out of a new image shortly after starting).
let runtimeTimer = 0;
function checkRuntime() {
  clearTimeout(runtimeTimer);
  const pill = $("runtime-status");
  const again = () => (runtimeTimer = setTimeout(checkRuntime, 30000));
  state.api.runtime().then(
    (rt) => {
      // Update in place: open editors hold this set.
      state.modules.clear();
      for (const m of rt.modules || []) state.modules.add(m);
      pill.hidden = true;
      if (rt.completions) loadCompletions(state.api, rt.completions);
      else again();
    },
    (e) => {
      pill.hidden = !(e instanceof ApiError && e.status === 503);
      pill.textContent = "Runs paused";
      pill.title = "nbsandboxd can't reach Docker: runs and calls can't start until it's back.";
      again();
    },
  );
}

async function selectSpace(space) {
  state.space = space;
  state.collapsed = new Set(recall(collapsedKey(space), []));
  $("space-select").value = space;
  setExplorer(narrow() ? false : recall("nbsandbox.explorer", true), false);
  if (!space) closeDrawer();
  await reload();
}

async function reload() {
  const [files, runs, fns] = await Promise.all([
    state.space ? state.api.files(state.space).catch((e) => (toast(e.message), [])) : [],
    state.space ? state.api.runs(state.space).catch((e) => (toast(e.message), [])) : [],
    state.space ? state.api.functions(state.space).catch(() => []) : [],
  ]);
  state.files = files;
  state.runs = runs;
  state.functions = fns;
  renderChrome();
}

async function createSpace() {
  const name = prompt("Name for the new space:", "");
  if (name === null || !name.trim()) return;
  try {
    const sp = await state.api.createSpace(name.trim());
    await selectUser(state.user);
    go(state.user, sp.id);
  } catch (e) {
    toast(e.message);
  }
}

// ---------- chrome: explorer, drawer, top bar ----------

function renderChrome() {
  renderTree();
  renderRuns();
  renderFunctions();
  const active = state.runs.filter((r) => ACTIVE.has(r.status)).length;
  const c = $("runs-count");
  c.hidden = active === 0;
  c.textContent = String(active);
  c.title = `${count(active, "run")} queued or running`;
  for (const id of ["runs-toggle", "fns-toggle", "explorer-toggle"]) $(id).hidden = !state.space;
}

function setExplorer(open, save = true) {
  document.body.classList.toggle("explorer-closed", !open || !state.space);
  $("explorer-toggle").setAttribute("aria-expanded", String(open));
  if (save) remember("nbsandbox.explorer", open);
}

// Opens the drawer on a tab; the top-bar button of the open tab closes it.
function openDrawer(tab, toggle = true) {
  const drawer = $("drawer");
  if (toggle && !drawer.hidden && state.drawerTab === tab) return closeDrawer();
  state.drawerTab = tab;
  drawer.hidden = false;
  $("tab-runs").setAttribute("aria-selected", String(tab === "runs"));
  $("tab-fns").setAttribute("aria-selected", String(tab === "fns"));
  $("run-list").hidden = tab !== "runs";
  $("fn-list").hidden = tab !== "fns";
  $("runs-toggle").setAttribute("aria-pressed", String(tab === "runs"));
  $("fns-toggle").setAttribute("aria-pressed", String(tab === "fns"));
}

function closeDrawer() {
  $("drawer").hidden = true;
  $("runs-toggle").setAttribute("aria-pressed", "false");
  $("fns-toggle").setAttribute("aria-pressed", "false");
}

function selected() {
  const h = parseHash();
  return { kind: h.kind, target: h.target };
}

function buildTree() {
  const root = { name: "", path: "", dirs: new Map(), files: [] };
  const dirNode = (path) => {
    let node = root;
    if (!path) return node;
    let acc = "";
    for (const d of path.split("/")) {
      acc = acc ? `${acc}/${d}` : d;
      if (!node.dirs.has(d)) node.dirs.set(d, { name: d, path: acc, dirs: new Map(), files: [] });
      node = node.dirs.get(d);
    }
    return node;
  };
  for (const f of state.files) {
    if (f.type === "folder") dirNode(f.path);
    else dirNode(f.path.split("/").slice(0, -1).join("/")).files.push(f);
  }
  return root;
}

function collapsedKey(space) {
  return `nbsandbox.collapsed.${state.user}.${space}`;
}

function toggleFolder(path) {
  if (state.collapsed.has(path)) state.collapsed.delete(path);
  else state.collapsed.add(path);
  remember(collapsedKey(state.space), [...state.collapsed]);
  renderTree();
}

function renderTree() {
  const tools = clear($("explorer-tools"));
  const box = clear($("file-tree"));
  if (!state.space) return;
  tools.append(
    el("button", { type: "button", class: "icon-button", title: "New file", "aria-label": "New file", onclick: () => newFile(currentFolder()) }, icon("plus")),
    el("button", { type: "button", class: "icon-button", title: "New folder", "aria-label": "New folder", onclick: () => newFolder(currentFolder()) }, icon("folder")),
    uploadButton(() => currentFolder(), true),
  );
  const sel = selected();
  if (state.files.length === 0) box.append(el("p", { class: "muted" }, "No files yet. Drop files here."));
  const render = (node) => {
    const ul = el("ul");
    for (const [, child] of [...node.dirs].sort((a, b) => a[0].localeCompare(b[0]))) {
      const folded = state.collapsed.has(child.path);
      const row = el(
        "div",
        { class: "tree-row" + (sel.kind === "d" && sel.target === child.path ? " selected" : "") },
        el("button", {
          type: "button", class: "tree-toggle", "aria-expanded": String(!folded),
          "aria-label": `${folded ? "Show" : "Hide"} what's in ${child.path}`,
          onclick: () => toggleFolder(child.path),
        }, icon(folded ? "chevron-right" : "chevron-down")),
        el("button", {
          type: "button", class: "tree-folder", title: child.path,
          onclick: () => go(state.user, state.space, "d", child.path),
        }, icon("folder"), el("span", {}, child.name)),
      );
      dropTarget(row, child.path);
      ul.append(el("li", {}, row, folded ? null : render(child)));
    }
    for (const f of node.files.sort((a, b) => a.path.localeCompare(b.path))) {
      ul.append(el("li", {}, el("button", {
        type: "button",
        class: "tree-item" + (sel.kind === "f" && f.path === sel.target ? " selected" : ""),
        title: f.path,
        onclick: () => go(state.user, state.space, "f", f.path),
      }, fileIcon(kindOf(f.path)), el("span", { class: "name" }, f.path.split("/").pop()), el("span", { class: "size" }, formatBytes(f.size)))));
    }
    return ul;
  };
  box.append(render(buildTree()));
}

// The folder new files go to: the open folder, or the open file's folder.
function currentFolder() {
  const sel = selected();
  if (sel.kind === "d") return sel.target;
  if (sel.kind === "f") return sel.target.split("/").slice(0, -1).join("/");
  return "";
}

function runTitle(r) {
  return r.function ? `${r.function} v${r.version}` : r.notebook;
}

function duration(r) {
  if (!r.started_at) return "";
  const end = r.finished_at ? new Date(r.finished_at) : new Date();
  const s = Math.max(0, (end - new Date(r.started_at)) / 1000);
  return s < 60 ? `${s.toFixed(s < 10 ? 1 : 0)} s` : `${Math.floor(s / 60)} m ${Math.round(s % 60)} s`;
}

function renderRuns() {
  const list = clear($("run-list"));
  if (!state.space) return;
  if (state.runs.length === 0) return list.append(el("li", { class: "empty-note" }, "No runs yet. Open a notebook and press Run."));
  for (const r of state.runs) {
    const detail = r.status === "queued" && r.queue_position ? `position ${r.queue_position} in the queue` : duration(r);
    list.append(el("li", {}, el("button", { type: "button", onclick: () => go(state.user, state.space, "r", r.id), title: r.id },
      badge(r.status), el("span", { class: "nb" }, runTitle(r)), el("span", { class: "when" }, timeAgo(r.created_at)),
      el("span", { class: "sub" }, [r.id.slice(0, 8), detail, r.read_only_space ? "read-only" : ""].filter(Boolean).join(" · ")))));
  }
}

function renderFunctions() {
  const list = clear($("fn-list"));
  if (!state.space) return;
  if (state.functions.length === 0) {
    return list.append(el("li", { class: "empty-note" }, "None yet. Open a notebook and choose Publish as function."));
  }
  for (const f of state.functions) {
    list.append(el("li", {}, el("button", { type: "button", onclick: () => go(state.user, state.space, "fn", f.name) },
      icon("function"), el("span", { class: "nb" }, f.name), el("span", { class: "when" }, `v${f.latest_version}`))));
  }
}

// ---------- file management ----------

function joinPath(dir, name) {
  return dir ? `${dir}/${name}` : name;
}

function emptyNotebook() {
  return JSON.stringify({
    nbformat: 4, nbformat_minor: 5,
    metadata: { kernelspec: { name: "python3", display_name: "Python 3", language: "python" } },
    cells: [{ cell_type: "code", id: "c0", metadata: { tags: ["parameters"] }, source: "# parameters\n", outputs: [], execution_count: null }],
  }, null, 1) + "\n";
}

async function createFile(path) {
  const body = path.toLowerCase().endsWith(".ipynb") ? emptyNotebook() : "";
  try {
    await state.api.saveFile(state.space, path, body, null);
    await reload();
    go(state.user, state.space, "f", path);
  } catch (e) {
    toast(e instanceof ApiError && e.status === 412 ? `${path} already exists.` : e.message);
  }
}

async function newFile(dir) {
  const name = prompt(`New file${dir ? ` in ${dir}/` : ""} (e.g. model.py, notes.md, analysis.ipynb):`, "");
  if (!name || !name.trim()) return;
  await createFile(joinPath(dir, name.trim()));
}

async function newNotebook(dir) {
  let name = prompt(`New notebook${dir ? ` in ${dir}/` : ""}:`, "untitled.ipynb");
  if (!name || !name.trim()) return;
  name = name.trim();
  if (!name.toLowerCase().endsWith(".ipynb")) name += ".ipynb";
  await createFile(joinPath(dir, name));
}

async function newFolder(dir) {
  const name = prompt(`New folder${dir ? ` in ${dir}/` : ""}:`, "");
  if (!name || !name.trim()) return;
  const path = joinPath(dir, name.trim());
  try {
    await state.api.createFolder(state.space, path);
    await reload();
    go(state.user, state.space, "d", path);
  } catch (e) {
    toast(e.message);
  }
}

async function upload(dir, files) {
  if (!files.length) return;
  const total = files.reduce((n, f) => n + f.size, 0);
  toast(`Uploading ${count(files.length, "file")} (${formatBytes(total)})…`, "info");
  try {
    await state.api.upload(state.space, dir, files);
    toast(`Uploaded ${count(files.length, "file")}${dir ? ` to ${dir}/` : ""}.`, "info");
    await reload();
    // The overview and folder pages show what's in the space: redraw them.
    const k = parseHash().kind;
    if (k === "d" || k === "") route();
  } catch (e) {
    toast(e.message);
  }
}

function uploadButton(dirFn, compact = false) {
  const input = el("input", { type: "file", multiple: true, hidden: true });
  input.addEventListener("change", () => {
    upload(dirFn(), [...input.files]);
    input.value = "";
  });
  const btn = compact
    ? el("button", { type: "button", class: "icon-button", title: "Upload files", "aria-label": "Upload files", onclick: () => input.click() }, icon("upload"))
    : el("button", { type: "button", class: "secondary", onclick: () => input.click() }, icon("upload"), "Upload");
  return el("span", {}, input, btn);
}

// Files dropped on a folder (in the explorer or a table) go into it.
function dropTarget(node, dir) {
  node.addEventListener("dragover", (e) => {
    if (!e.dataTransfer || ![...e.dataTransfer.types].includes("Files")) return;
    e.preventDefault();
    e.stopPropagation();
    hideDropOverlay();
    node.classList.add("drop");
  });
  node.addEventListener("dragleave", (e) => {
    if (!node.contains(e.relatedTarget)) node.classList.remove("drop");
  });
  node.addEventListener("drop", (e) => {
    node.classList.remove("drop");
    hideDropOverlay();
    if (!e.dataTransfer || !e.dataTransfer.files.length) return;
    e.preventDefault();
    e.stopPropagation();
    upload(dir, [...e.dataTransfer.files]);
  });
}

// Files dropped anywhere else go to the folder in view.
let overlay = null;
function hideDropOverlay() {
  if (overlay) overlay.remove();
  overlay = null;
}
function pageDrop(area) {
  area.addEventListener("dragover", (e) => {
    if (!state.space || !e.dataTransfer || ![...e.dataTransfer.types].includes("Files")) return;
    e.preventDefault();
    if (!overlay) {
      const dir = currentFolder();
      overlay = el("div", { class: "page-drop-overlay" }, `Drop to upload to ${dir ? dir + "/" : "the top of the space"}`);
      document.body.append(overlay);
    }
  });
  area.addEventListener("dragleave", (e) => {
    if (!area.contains(e.relatedTarget)) hideDropOverlay();
  });
  area.addEventListener("drop", (e) => {
    hideDropOverlay();
    if (!state.space || !e.dataTransfer || !e.dataTransfer.files.length) return;
    e.preventDefault();
    upload(currentFolder(), [...e.dataTransfer.files]);
  });
}

async function moveItem(path, isFolder) {
  const to = prompt(`Rename or move ${path} to:`, path);
  if (!to || to.trim() === path) return;
  try {
    await state.api.move(state.space, path, to.trim());
    await reload();
    go(state.user, state.space, isFolder ? "d" : "f", to.trim());
  } catch (e) {
    toast(e.message);
  }
}

async function deleteItem(path, isFolder, stay = false) {
  if (isFolder) {
    const inside = state.files.filter((f) => f.type !== "folder" && f.path.startsWith(path + "/")).length;
    if (!confirm(inside ? `Delete the folder ${path} and the ${count(inside, "file")} in it?` : `Delete the empty folder ${path}?`)) return;
  } else if (!confirm(`Delete ${path}?`)) return;
  try {
    await state.api.remove(state.space, path, isFolder);
    await reload();
    if (stay) return route();
    const parent = path.split("/").slice(0, -1).join("/");
    go(state.user, state.space, parent ? "d" : "", parent);
  } catch (e) {
    toast(e.message);
  }
}

// Starts a run and opens it. A writing run is refused while another one is
// active (409): open that one instead.
async function startRun(path, parameters, readOnly, button) {
  if (button) button.disabled = true;
  try {
    const run = await state.api.startRun(state.space, path, parameters, readOnly);
    await reload();
    go(state.user, state.space, "r", run.id);
  } catch (e) {
    if (e instanceof ApiError && e.code === "invalid_parameters" && parameters === undefined) {
      // A quick run (default parameters) of a notebook whose contract needs
      // some: open it so the parameters can be filled in.
      toast(`${path.split("/").pop()} needs parameters: ${e.message.replace(/^parameters don't match the notebook's input contract: /, "").replace("(the whole value): ", "")}. Fill them in next to Run.`, "info");
      go(state.user, state.space, "f", path);
    } else if (e instanceof ApiError && e.status === 409) {
      const active = state.runs.find((r) => ACTIVE.has(r.status) && !r.read_only_space);
      toast("This space already has a run that can change its files" + (active ? ": opening it." : "."), "info");
      if (active) go(state.user, state.space, "r", active.id);
    } else toast(e.message);
  } finally {
    if (button) button.disabled = false;
  }
}

// ---------- pages ----------

function showSpaces() {
  const page = newPage();
  page.append(
    el("div", { class: "hero" }, el("h1", {}, "Spaces"),
      el("p", {}, "A space holds notebooks, data files and runs. Runs in one space can't see another's files.")),
    el("div", { class: "section" }, el("div", { class: "cards" },
      state.spaces.map((s) => el("button", { type: "button", class: "card space-card", onclick: () => go(state.user, s.id) },
        el("span", { class: "title" }, icon("box"), s.name || s.id.slice(0, 8)),
        el("span", { class: "muted" }, `created ${timeAgo(s.created_at)}`))),
      el("button", { type: "button", class: "card space-card new", onclick: createSpace }, icon("plus"), "New space"),
    )),
  );
}

function lastRunOf(path) {
  return state.runs.find((r) => !r.function && r.notebook === path);
}

function notebookCard(f) {
  const last = lastRunOf(f.path);
  const dir = f.path.split("/").slice(0, -1).join("/");
  const run = el("button", { type: "button", class: "small secondary", title: "Run with its default parameters" }, icon("play"), "Run");
  run.addEventListener("click", (e) => {
    e.stopPropagation();
    startRun(f.path, undefined, false, run);
  });
  const card = el("div", { class: "card nb-card", role: "link", tabindex: "0", title: `Open ${f.path}` },
    el("div", { class: "title" }, icon("notebook"), el("span", {}, f.path.split("/").pop())),
    dir ? el("div", { class: "folder" }, `${dir}/`) : null,
    el("div", { class: "meta" }, `edited ${timeAgo(f.modified)} · ${formatBytes(f.size)}`),
    el("div", { class: "foot" },
      last ? badge(last.status) : el("span", { class: "muted" }, "Never run"),
      last ? el("span", { class: "muted" }, timeAgo(last.created_at)) : null,
      el("span", { class: "spacer" }), run),
  );
  const open = () => go(state.user, state.space, "f", f.path);
  card.addEventListener("click", open);
  card.addEventListener("keydown", (e) => {
    if (e.key === "Enter" && e.target === card) open();
  });
  return card;
}

// The files in one folder as a table, folders first.
function filesTable(dir) {
  const inside = state.files
    .filter((f) => f.path.startsWith(dir ? dir + "/" : "") && f.path !== dir && !f.path.slice(dir ? dir.length + 1 : 0).includes("/"))
    .sort((a, b) => ((a.type === "folder") === (b.type === "folder") ? a.path.localeCompare(b.path) : a.type === "folder" ? -1 : 1));
  if (inside.length === 0) {
    const drop = el("div", { class: "dropzone" }, icon("upload"), el("strong", {}, dir ? "This folder is empty" : "No files yet"),
      el("span", {}, "Drop files anywhere on this page, or use Upload."));
    dropTarget(drop, dir);
    return drop;
  }
  const stop = (fn) => (e) => {
    e.stopPropagation();
    fn();
  };
  const rows = inside.map((f) => {
    const folder = f.type === "folder";
    const name = f.path.split("/").pop();
    const tr = el("tr", { class: "clickable" },
      el("td", {}, el("div", { class: "name-cell" }, folder ? icon("folder") : fileIcon(kindOf(f.path)), el("span", {}, folder ? `${name}/` : name))),
      el("td", { class: "num hide-narrow" }, folder ? "" : formatBytes(f.size)),
      el("td", { class: "when" }, f.modified ? timeAgo(f.modified) : ""),
      el("td", { class: "row-actions" },
        el("button", { type: "button", class: "icon-button", title: "Rename or move", "aria-label": `Rename or move ${f.path}`, onclick: stop(() => moveItem(f.path, folder)) }, icon("pencil")),
        folder ? null : el("a", { class: "icon-button", href: state.api.fileUrl(state.space, f.path), download: name, title: "Download", "aria-label": `Download ${f.path}`, onclick: (e) => e.stopPropagation() }, icon("download")),
        el("button", { type: "button", class: "icon-button danger-hover", title: "Delete", "aria-label": `Delete ${f.path}`, onclick: stop(() => deleteItem(f.path, folder, true)) }, icon("trash"))),
    );
    tr.addEventListener("click", () => go(state.user, state.space, folder ? "d" : "f", f.path));
    if (folder) dropTarget(tr, f.path);
    return tr;
  });
  return el("div", { class: "card table-card" }, el("table", { class: "list" },
    el("thead", {}, el("tr", {}, el("th", {}, "Name"), el("th", { class: "hide-narrow" }, "Size"), el("th", {}, "Modified"), el("th", {}, el("span", { class: "visually-hidden" }, "Actions")))),
    el("tbody", {}, rows)));
}

function createActions(dir) {
  return el("div", { class: "quick-actions" },
    el("button", { type: "button", onclick: () => newNotebook(dir) }, icon("plus"), "New notebook"),
    uploadButton(() => dir),
    menuButton([
      { label: "New file", icon: "file", onclick: () => newFile(dir) },
      { label: "New folder", icon: "folder", onclick: () => newFolder(dir) },
      dir ? "-" : null,
      dir ? { label: "Rename or move folder", icon: "pencil", onclick: () => moveItem(dir, true) } : null,
      dir ? { label: "Delete folder", icon: "trash", danger: true, onclick: () => deleteItem(dir, true) } : null,
    ], "More"),
  );
}

// The space's home: notebooks first, then recent runs, functions and files.
function showOverview() {
  const sp = state.spaces.find((s) => s.id === state.space);
  const page = newPage();
  const files = state.files.filter((f) => f.type !== "folder");
  const notebooks = files.filter((f) => kindOf(f.path) === "notebook")
    .sort((a, b) => (b.modified || "").localeCompare(a.modified || ""));
  const active = state.runs.filter((r) => ACTIVE.has(r.status)).length;
  const bytes = files.reduce((n, f) => n + (f.size || 0), 0);
  const stat = (n, word, ...rest) => el("span", { class: "stat" }, el("strong", {}, String(n)), n === 1 ? word : `${word}s`, ...rest);

  page.append(
    el("div", { class: "page-head" }, el("h1", {}, sp ? sp.name || sp.id : "Space"), el("span", { class: "spacer" }), createActions("")),
    el("div", { class: "stats" },
      stat(notebooks.length, "notebook"),
      stat(files.length, "file", el("span", { class: "muted" }, `· ${formatBytes(bytes)}`)),
      stat(state.functions.length, "function"),
      active ? el("span", { class: "stat" }, badge("running"), `${active} active`) : null,
    ),
  );

  const nbSection = el("section", { class: "section" }, el("div", { class: "section-head" }, el("h2", {}, "Notebooks")));
  if (notebooks.length) nbSection.append(el("div", { class: "cards" }, notebooks.map(notebookCard)));
  else {
    nbSection.append(el("div", { class: "card empty-state" }, icon("notebook"), el("h2", {}, "No notebooks yet"),
      el("p", {}, "Create one here, or upload notebooks and the data files they read. Notebooks run as Python 3, without network access."),
      el("div", { class: "quick-actions" },
        el("button", { type: "button", onclick: () => newNotebook("") }, icon("plus"), "New notebook"), uploadButton(() => ""))));
  }
  page.append(nbSection);

  if (state.runs.length) {
    const recent = state.runs.slice(0, 6);
    page.append(el("section", { class: "section" },
      el("div", { class: "section-head" }, el("h2", {}, "Recent runs"), el("span", { class: "spacer" }),
        state.runs.length > recent.length ? el("button", { type: "button", class: "link", onclick: () => openDrawer("runs", false) }, `All ${state.runs.length}`) : null),
      el("div", { class: "card table-card" }, el("table", { class: "list" },
        el("thead", {}, el("tr", {}, el("th", {}, "Status"), el("th", {}, "Notebook or function"), el("th", { class: "hide-narrow" }, "Duration"), el("th", {}, "Started"))),
        el("tbody", {}, recent.map((r) => {
          const tr = el("tr", { class: "clickable", title: r.id },
            el("td", {}, badge(r.status)),
            el("td", {}, el("div", { class: "name-cell" }, icon(r.function ? "function" : "notebook"), el("span", {}, runTitle(r)))),
            el("td", { class: "num hide-narrow" }, duration(r)),
            el("td", { class: "when" }, timeAgo(r.created_at)));
          tr.addEventListener("click", () => go(state.user, state.space, "r", r.id));
          return tr;
        }))))));
  }

  if (state.functions.length) {
    page.append(el("section", { class: "section" },
      el("div", { class: "section-head" }, el("h2", {}, "Functions"), el("span", { class: "muted" }, "notebooks other services call with JSON")),
      el("div", { class: "cards" }, state.functions.map((f) =>
        el("button", { type: "button", class: "card space-card", onclick: () => go(state.user, state.space, "fn", f.name) },
          el("span", { class: "title" }, icon("function"), f.name),
          el("span", { class: "muted" }, `latest v${f.latest_version}`))))));
  }

  page.append(el("section", { class: "section" },
    el("div", { class: "section-head" }, el("h2", {}, "Files"), el("span", { class: "muted" }, "top level")),
    filesTable("")));
}

// A folder: breadcrumbs, what's in it, and actions.
function openFolder(dir) {
  const sp = state.spaces.find((s) => s.id === state.space);
  const page = newPage();
  const parts = dir.split("/");
  const crumbs = el("nav", { class: "breadcrumbs", "aria-label": "Folder" },
    el("button", { type: "button", onclick: () => go(state.user, state.space) }, sp ? sp.name || "Space" : "Space"));
  parts.slice(0, -1).forEach((p, i) => {
    const path = parts.slice(0, i + 1).join("/");
    crumbs.append(icon("chevron-right"), el("button", { type: "button", onclick: () => go(state.user, state.space, "d", path) }, p));
  });
  page.append(
    crumbs,
    el("div", { class: "page-head" }, el("h1", {}, `${parts[parts.length - 1]}/`), el("span", { class: "spacer" }), createActions(dir)),
    filesTable(dir),
  );
}

function openFile(path) {
  const file = state.files.find((f) => f.path === path && f.type !== "folder");
  const notebook = kindOf(path) === "notebook";
  const publish = () => publishDialog(state, path, (name) => reload().then(() => go(state.user, state.space, "fn", name)));
  // Next to Save and Download: the rest of what can be done with the file.
  const head = () => [menuButton([
    notebook ? { label: "Publish as function…", icon: "function", onclick: publish } : null,
    { label: "Rename or move…", icon: "pencil", onclick: () => moveItem(path, false) },
    "-",
    { label: "Delete", icon: "trash", danger: true, onclick: () => deleteItem(path, false) },
  ])];
  const extra = () => (notebook ? [runForm(path)] : []);
  state.page = openFilePage(newPage(), {
    api: state.api, space: state.space, path, file, modules: state.modules, headActions: head, extraActions: extra,
    onSaved: () => reload(),
    onMoved: (to) => reload().then(() => go(state.user, state.space, "f", to)),
  });
}

function runForm(path) {
  const params = el("textarea", {
    "aria-label": "Parameters (JSON)",
    placeholder: 'Parameters (optional JSON), e.g. {"region": "jakarta", "cfg": {"months": 6}}',
    spellcheck: "false",
  });
  const readOnly = el("input", { type: "checkbox" });
  const button = el("button", { type: "button", title: "Runs the saved version of the notebook" }, icon("play"), "Run");
  button.addEventListener("click", async () => {
    let parameters;
    const text = params.value.trim();
    if (text) {
      try {
        parameters = JSON.parse(text);
      } catch {
        return toast('Parameters must be valid JSON, e.g. {"months": 6}');
      }
      if (typeof parameters !== "object" || Array.isArray(parameters) || parameters === null) {
        return toast("Parameters must be a JSON object.");
      }
    }
    if (state.page && state.page.dirty && state.page.dirty() && !confirm("The run uses the saved notebook, not your unsaved changes. Run anyway?")) return;
    startRun(path, parameters, readOnly.checked, button);
  });
  const last = lastRunOf(path);
  return el("div", { class: "run-form" }, button, params,
    el("label", { title: "The run can't change the space's files, and may run beside other runs" }, readOnly, "Read-only"),
    last ? el("button", { type: "button", class: "ghost small", title: "Open the last run", onclick: () => go(state.user, state.space, "r", last.id) },
      "Last run", badge(last.status)) : null);
}

function openRun(runId) {
  const stop = showRun(newPage(), state.api, state.space, runId, () => reload());
  state.page = { stop };
}

function openFunction(name) {
  state.page = showFunction(newPage(), state, name, {
    openRun: (id) => reload().then(() => go(state.user, state.space, "r", id)),
    deleted: () => reload().then(() => go(state.user, state.space)),
  });
}

// While runs are active, keep their statuses (drawer, counts) current.
function pollRuns() {
  setInterval(async () => {
    if (!state.space || document.hidden || !state.runs.some((r) => ACTIVE.has(r.status))) return;
    try {
      state.runs = await state.api.runs(state.space);
    } catch {
      return;
    }
    renderChrome();
  }, 4000);
}

// ---------- start ----------

async function start() {
  try {
    state.users = await listUsers();
  } catch (e) {
    toast(e.message);
    return;
  }
  const sel = $("user-select");
  for (const u of state.users) sel.append(el("option", { value: u }, u));
  sel.addEventListener("change", () => go(sel.value));
  $("space-select").addEventListener("change", (e) => {
    if (e.target.value === NEW_SPACE) {
      e.target.value = state.space;
      return createSpace();
    }
    go(state.user, e.target.value);
  });
  $("explorer-toggle").addEventListener("click", () => setExplorer(document.body.classList.contains("explorer-closed")));
  $("runs-toggle").addEventListener("click", () => openDrawer("runs"));
  $("fns-toggle").addEventListener("click", () => openDrawer("fns"));
  $("tab-runs").addEventListener("click", () => openDrawer("runs", false));
  $("tab-fns").addEventListener("click", () => openDrawer("fns", false));
  $("drawer-close").addEventListener("click", closeDrawer);
  // Becoming narrow: the explorer and drawer turn into overlays, so close them.
  window.matchMedia("(max-width: 860px)").addEventListener("change", (e) => {
    if (e.matches) {
      setExplorer(false, false);
      closeDrawer();
    } else setExplorer(recall("nbsandbox.explorer", true), false);
  });
  // On a narrow screen the explorer covers the page: a tap on the page closes it.
  $("main").addEventListener("click", () => {
    if (narrow() && !document.body.classList.contains("explorer-closed")) setExplorer(false, false);
  });
  pageDrop($("main"));
  dropTarget($("explorer"), "");
  pollRuns();
  window.addEventListener("hashchange", route);
  await route();
}

start();
