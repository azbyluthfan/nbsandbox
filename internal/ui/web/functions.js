// Copyright 2026 The nbsandbox Authors
// SPDX-License-Identifier: Apache-2.0
//
// Functions in the UI: publish a notebook as a function, see its versions
// and contract, call it with a form generated from the input schema, and
// watch the call live (the run's status event stream) until its result.

import { ApiError } from "./api.js";
import { el, clear, toast, badge, timeAgo, formatBytes, icon } from "./dom.js";
import { codeBlock } from "./viewers.js";

const SIMPLE = new Set(["string", "number", "integer", "boolean"]);

/** Shows the function `name`. Returns {stop()}. */
export function showFunction(main, state, name, { openRun, deleted }) {
  const { api, space } = state;
  let source = null;
  let stopped = false;
  clear(main).append(el("p", { class: "muted" }, "Loading…"));

  (async () => {
    let fn;
    let latest;
    try {
      fn = await api.fn(space, name);
      latest = await api.fnVersion(space, name, fn.latest_version);
    } catch (e) {
      clear(main).append(el("p", { class: "message bad" }, e instanceof ApiError && e.status === 404 ? `There is no function "${name}".` : e.message));
      return;
    }
    if (stopped) return;
    const contract = latest.contract || { input: null, output: null };
    const head = el(
      "div",
      { class: "file-head" },
      el("h1", {}, name),
      badge("v" + fn.latest_version),
      el("span", { class: "spacer" }),
      el("button", {
        type: "button", class: "danger",
        onclick: async () => {
          if (!confirm(`Delete the function ${name} and all its versions? Its past calls stay.`)) return;
          try {
            await api.deleteFunction(space, name);
            deleted();
          } catch (e) {
            toast(e.message);
          }
        },
      }, icon("trash"), "Delete function"),
    );
    const versions = el(
      "table",
      { class: "list" },
      el("thead", {}, el("tr", {}, ["Version", "Notebook", "Files", "Size", "Published"].map((h) => el("th", {}, h)))),
      el("tbody", {}, fn.versions.map((v) => el("tr", {},
        el("td", {}, `v${v.version}`), el("td", {}, v.notebook), el("td", { title: v.files.join("\n") }, String(v.files.length)),
        el("td", {}, formatBytes(v.bundle_bytes)), el("td", {}, `${timeAgo(v.created_at)} by ${v.created_by}`)))),
    );
    const result = el("div", { class: "fn-result" });
    const section = (title, ...body) => el("section", { class: "section" }, el("div", { class: "section-head" }, el("h2", {}, title)), ...body);
    const calls = state.runs.filter((r) => r.function === name).slice(0, 8);
    main.replaceChildren(...[
      head,
      el("p", { class: "page-sub" }, `Published from ${latest.notebook}. Other services call it with JSON and get JSON back; each call runs in a fresh, isolated container.`),
      section("Try it", el("div", { class: "card invoke" }, invokeForm(contract.input, fn, result), result)),
      calls.length ? section("Recent calls", el("div", { class: "card table-card" }, el("table", { class: "list" },
        el("thead", {}, el("tr", {}, el("th", {}, "Status"), el("th", {}, "Version"), el("th", {}, "Call"), el("th", {}, "Started"))),
        el("tbody", {}, calls.map((r) => {
          const tr = el("tr", { class: "clickable" }, el("td", {}, badge(r.status)), el("td", { class: "num" }, `v${r.version}`),
            el("td", { class: "num" }, r.id.slice(0, 8)), el("td", { class: "when" }, timeAgo(r.created_at)));
          tr.addEventListener("click", () => openRun(r.id));
          return tr;
        }))))) : null,
      section("Call it from your service", snippet(curlExample(space, name, contract.input)),
        el("p", { class: "muted" }, "Add ", el("code", {}, "?wait=30s"), " to get the result in the same response. See docs/functions.md for statuses, retries and streaming.")),
      section("Contract", contract.input || contract.output
        ? el("div", { class: "contract-view" },
          el("h3", {}, "Input"), contract.input ? codeBlock(JSON.stringify(contract.input, null, 2), "json") : el("p", { class: "muted" }, "Any JSON object."),
          el("h3", {}, "Output"), contract.output ? codeBlock(JSON.stringify(contract.output, null, 2), "json") : el("p", { class: "muted" }, "Any JSON value."))
        : el("p", { class: "muted" }, "None: any parameters are accepted and any value may be returned. Add one in the notebook's Contract section, then publish again.")),
      section("Versions", el("div", { class: "card table-card" }, versions),
        el("p", { class: "muted" }, "Each version is a copy of the notebook and its files as they were when published; editing the space doesn't change them.")),
    ].filter(Boolean));
  })();

  // A form for the input schema's top-level fields, plus raw JSON.
  function invokeForm(schema, fn, result) {
    const props = schema && schema.type === "object" && schema.properties ? schema.properties : null;
    const required = new Set((schema && schema.required) || []);
    const fields = [];
    const grid = el("div", { class: "form-grid" });
    if (props) {
      // Required fields first, then the rest, each group in schema order.
      const keys = Object.keys(props);
      keys.sort((a, b) => Number(required.has(b)) - Number(required.has(a)));
      for (const key of keys) {
        const sub = props[key] || {};
        const t = sub.type;
        let input;
        if (Array.isArray(sub.enum) && sub.enum.every((v) => typeof v === "string")) {
          input = el("select", {}, required.has(key) ? null : el("option", { value: "" }, "—"), sub.enum.map((v) => el("option", { value: v }, v)));
        } else if (t === "boolean") input = el("input", { type: "checkbox" });
        else if (SIMPLE.has(t)) {
          input = el("input", {
            type: t === "string" ? "text" : "number", step: t === "integer" ? "1" : "any",
            min: sub.minimum, max: sub.maximum, placeholder: t,
          });
        } else input = el("textarea", { rows: "2", spellcheck: "false", placeholder: `${t || "any"} as JSON` });
        input.id = `fn-field-${key}`;
        if (sub.default !== undefined) {
          if (t === "boolean") input.checked = !!sub.default;
          else input.value = SIMPLE.has(t) || input.tagName === "SELECT" ? String(sub.default) : JSON.stringify(sub.default);
        }
        fields.push({ key, t, input, required: required.has(key) });
        grid.append(el("label", { for: input.id, title: sub.description || "" }, key, required.has(key) ? el("span", { class: "req", title: "required" }, "*") : null), input);
        const hint = hintFor(sub);
        if (hint) grid.append(el("span", { class: "hint" }, hint));
      }
    }
    const raw = el("textarea", { rows: "4", spellcheck: "false", "aria-label": "Parameters (JSON)", placeholder: '{"region": "jakarta", "cfg": {"months": 6}}' });
    const useRaw = el("input", { type: "checkbox", checked: !props });
    const rawBox = el("div", { hidden: !!props }, raw);
    useRaw.addEventListener("change", () => {
      if (useRaw.checked) {
        try {
          raw.value = JSON.stringify(collect(), null, 2);
        } catch {
          /* keep what's there */
        }
      }
      rawBox.hidden = !useRaw.checked;
      grid.hidden = useRaw.checked;
    });
    grid.hidden = !props;
    const versionSel = el("select", { "aria-label": "Version" },
      el("option", { value: "0" }, `latest (v${fn.latest_version})`),
      fn.versions.map((v) => el("option", { value: String(v.version) }, `v${v.version}`)));

    function collect() {
      const out = {};
      for (const f of fields) {
        if (f.t === "boolean") {
          out[f.key] = f.input.checked;
          continue;
        }
        const v = f.input.value.trim();
        if (v === "") {
          if (f.required) throw new Error(`${f.key} is required`);
          continue;
        }
        if (f.t === "string") out[f.key] = f.input.value;
        else if (f.t === "number" || f.t === "integer") out[f.key] = Number(v);
        else {
          try {
            out[f.key] = JSON.parse(v);
          } catch {
            throw new Error(`${f.key} must be JSON`);
          }
        }
      }
      return out;
    }

    const go = el("button", { type: "button" }, icon("play"), "Call");
    go.addEventListener("click", async () => {
      let params;
      try {
        if (useRaw.checked) {
          params = raw.value.trim() ? JSON.parse(raw.value) : {};
          if (typeof params !== "object" || params === null || Array.isArray(params)) throw new Error("Parameters must be a JSON object.");
        } else params = collect();
      } catch (e) {
        return toast(e.message);
      }
      go.disabled = true;
      try {
        const run = await api.invoke(space, name, Number(versionSel.value), params);
        follow(run, result);
      } catch (e) {
        if (e instanceof ApiError && e.body && e.body.error && e.body.error.problems) {
          clear(result).append(el("p", { class: "message bad" }, e.message),
            el("ul", {}, e.body.error.problems.map((p) => el("li", {}, `${p.pointer || "(whole object)"}: ${p.message}`))));
        } else toast(e.message);
      } finally {
        go.disabled = false;
      }
    });
    return el("div", {}, grid, rawBox,
      el("div", { class: "actions" }, go, versionSel, el("span", { class: "spacer" }), el("label", { class: "muted" }, useRaw, " Edit as JSON")));
  }

  // Watches a call through its status stream until the result.
  function follow(run, result) {
    if (source) source.close();
    const status = el("span", {}, badge(run.status));
    const out = el("div", {});
    clear(result).append(
      el("p", {}, "Call ", el("button", { type: "button", class: "link", onclick: () => openRun(run.id) }, run.id.slice(0, 8)), ": ", status),
      out,
    );
    source = new EventSource(api.eventsUrl(space, run.id));
    source.addEventListener("status", (ev) => {
      let e;
      try {
        e = JSON.parse(ev.data);
      } catch {
        return;
      }
      clear(status).append(badge(e.status), e.queue_position ? ` position ${e.queue_position}` : "");
      if (e.result) {
        source.close();
        source = null;
        const r = e.result;
        clear(out).append(...[
          r.error ? el("p", { class: "message bad" }, `${r.error.kind}${r.error.retryable ? " (retryable)" : ""}: ${r.error.message}`) : null,
          codeBlock(JSON.stringify(r.output !== undefined ? r.output : r.error || null, null, 2), "json"),
        ].filter(Boolean));
      }
    });
  }

  return {
    stop() {
      stopped = true;
      if (source) source.close();
    },
  };
}

/** Asks which name and files to publish the notebook with, then publishes. */
export function publishDialog(state, path, done) {
  const { api, space } = state;
  const base = path.split("/").pop().replace(/\.ipynb$/i, "").toLowerCase().replace(/[^a-z0-9_-]+/g, "-").replace(/^-+/, "") || "function";
  const name = el("input", { type: "text", value: base, pattern: "[a-z0-9][a-z0-9_-]{0,62}", "aria-label": "Function name" });
  const choices = state.files
    .filter((f) => f.path !== path)
    .sort((a, b) => a.path.localeCompare(b.path))
    .map((f) => ({ f, box: el("input", { type: "checkbox" }) }));
  const dialog = el("dialog", { class: "publish" });
  const publish = el("button", { type: "button" }, "Publish");
  publish.addEventListener("click", async () => {
    const files = choices.filter((c) => c.box.checked).map((c) => c.f.path);
    publish.disabled = true;
    try {
      const v = await api.publish(space, name.value.trim(), path, files);
      toast(`Published ${v.function} v${v.version}.`, "info");
      dialog.close();
      dialog.remove();
      done(v.function);
    } catch (e) {
      toast(e.message);
    } finally {
      publish.disabled = false;
    }
  });
  dialog.append(
    el("h2", {}, `Publish ${path}`),
    el("p", { class: "muted" }, "Publishing copies the saved notebook (not unsaved edits) and the files you pick into a new, immutable version. Calls use that copy until you publish again."),
    el("label", {}, "Function name ", name),
    el("p", {}, "Files and folders the notebook reads:"),
    choices.length
      ? el("ul", { class: "choices" }, choices.map((c) => el("li", {}, el("label", {}, c.box, ` ${c.f.path}${c.f.type === "folder" ? "/" : ""}`))))
      : el("p", { class: "muted" }, "The space has no other files."),
    el("div", { class: "actions" }, el("button", { type: "button", class: "secondary", onclick: () => { dialog.close(); dialog.remove(); } }, "Cancel"), publish),
  );
  document.body.append(dialog);
  dialog.showModal();
}

// A short description of a schema's constraints, shown under its field.
function hintFor(sub) {
  const parts = [];
  if (sub.description) parts.push(sub.description);
  if (sub.minimum !== undefined && sub.maximum !== undefined) parts.push(`${sub.minimum} to ${sub.maximum}`);
  else if (sub.minimum !== undefined) parts.push(`at least ${sub.minimum}`);
  else if (sub.maximum !== undefined) parts.push(`at most ${sub.maximum}`);
  if (sub.minLength) parts.push(`at least ${sub.minLength} character${sub.minLength === 1 ? "" : "s"}`);
  if (sub.type === "object" && sub.properties) parts.push(`fields: ${Object.keys(sub.properties).join(", ")}`);
  if (sub.type === "array") parts.push("a JSON list");
  return parts.join(" · ");
}

// An example call with placeholder credentials and example values from the
// input schema.
function curlExample(space, name, schema) {
  const example = {};
  const props = schema && schema.properties ? schema.properties : {};
  for (const key of (schema && schema.required) || []) {
    const sub = props[key] || {};
    example[key] = sub.default !== undefined ? sub.default : Array.isArray(sub.enum) ? sub.enum[0]
      : { string: "…", integer: 1, number: 1.5, boolean: true, array: [], object: {} }[sub.type] ?? null;
  }
  const body = JSON.stringify({ parameters: example });
  return [
    `curl -u "$NBSANDBOX_USER:$NBSANDBOX_PASSWORD" \\`,
    "  -H 'Content-Type: application/json' \\",
    `  -d '${body.replace(/'/g, "'\\''")}' \\`,
    `  "$NBSANDBOX_URL/v1/spaces/${space}/functions/${name}/invoke?wait=30s"`,
  ].join("\n");
}

// A code block with a Copy button.
function snippet(text) {
  const copy = el("button", { type: "button", class: "small secondary" }, icon("copy"), "Copy");
  copy.addEventListener("click", async () => {
    try {
      await navigator.clipboard.writeText(text);
      clear(copy).append(icon("check"), "Copied");
      setTimeout(() => clear(copy).append(icon("copy"), "Copy"), 1500);
    } catch {
      toast("Couldn't copy: select the text instead.");
    }
  });
  return el("div", { class: "snippet" }, el("pre", {}, text), copy);
}
