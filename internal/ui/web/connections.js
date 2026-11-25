// Copyright 2026 The nbsandbox Authors
// SPDX-License-Identifier: Apache-2.0
//
// Connections (docs/connections.md): the user's data sources, a space's
// names for them per environment, and the environment a run, session or
// function call reads in. A secret is write-only: typed in, never shown.

import { ApiError } from "./api.js";
import { el, clear, toast, timeAgo, icon } from "./dom.js";

// The settings of each type, as the server checks them.
export const TYPES = {
  postgresql: { label: "PostgreSQL", secret: "Password", fields: [["host", "Host"], ["port", "Port", "5432"], ["database", "Database"], ["user", "User"]] },
  mysql: { label: "MySQL", secret: "Password", fields: [["host", "Host"], ["port", "Port", "3306"], ["database", "Database"], ["user", "User"]] },
  sqlserver: { label: "SQL Server", secret: "Password", fields: [["host", "Host"], ["port", "Port", "1433"], ["database", "Database"], ["user", "User (a SQL login)"]] },
  s3: {
    label: "S3-compatible storage", secret: "Secret access key", optionalSecret: true,
    fields: [["endpoint", "Endpoint (host[:port])", "s3.amazonaws.com"], ["bucket", "Bucket"], ["region", "Region", "us-east-1"],
      ["access_key_id", "Access key ID"], ["url_style", "URL style (vhost or path)", "vhost"], ["use_ssl", "HTTPS (true or false)", "true"]],
  },
  azureblob: { label: "Azure Blob Storage", secret: "Connection string, account key or SAS",
    fields: [["account", "Account"], ["container", "Container"], ["endpoint", "Endpoint URL (optional)"], ["auth", "Secret kind (connection_string, account_key or sas)", "connection_string"]] },
  http: { label: "HTTP API", secret: "Token or API key", optionalSecret: true,
    fields: [["url", "URL"], ["auth_header", "Header", "Authorization"], ["auth_prefix", "Prefix", "Bearer "]] },
};

const NAME = /^[a-z][a-z0-9_]{0,62}$/;

/** Whether connections are on: their list answers (404 when off). */
export async function connectionsOn(api) {
  try {
    await api.connections();
    return true;
  } catch (e) {
    if (e instanceof ApiError && e.status === 404) return false;
    return true; // another problem: show the pages, which say what it is
  }
}

// ---------- the environment a space reads in ----------

const envKey = (space) => `nbsandbox.env.${space}`;

/** The environment chosen for a space in this browser ("": its default). */
export function chosenEnvironment(space) {
  try {
    return localStorage.getItem(envKey(space)) || "";
  } catch {
    return "";
  }
}

function chooseEnvironment(space, env) {
  try {
    if (env) localStorage.setItem(envKey(space), env);
    else localStorage.removeItem(envKey(space));
  } catch {
    /* only a convenience */
  }
}

/**
 * A picker for the environment the space's names are read in: shown only
 * when the space has more than one. Runs, sessions and function calls use
 * chosenEnvironment().
 */
export function environmentPicker(api, space) {
  const select = el("select", { "aria-label": "Environment" });
  const label = el("label", { class: "env-picker", hidden: true, title: "Which environment's connections the space's names mean" },
    icon("table"), select);
  select.addEventListener("change", () => chooseEnvironment(space, select.value));
  api.spaceConnections(space).then((sc) => {
    const envs = Object.keys(sc.environments || {}).sort();
    if (envs.length < 2) return;
    let current = chosenEnvironment(space);
    if (!envs.includes(current)) current = "";
    select.append(...envs.map((e) => el("option", { value: e === sc.default_environment ? "" : e, selected: e === (current || sc.default_environment) },
      e + (e === sc.default_environment ? " (default)" : ""))));
    label.hidden = false;
  }).catch(() => {});
  return label;
}

// ---------- the user's connections ----------

/** The Connections page: the user's connections, to add, change or delete. */
export async function showConnections(api, page) {
  clear(page).append(el("div", { class: "hero" }, el("h1", {}, "Connections"),
    el("p", {}, "Your databases and object storage. A space gives each a short name (crm, lake) that notebooks query: ",
      el("code", {}, 'nbsandbox.sql("SELECT … FROM crm.orders")'), ". Secrets are kept by nbsandbox's secret provider and never shown again.")));
  const list = el("div", { class: "section" }, el("p", { class: "muted" }, "Loading…"));
  page.append(list);
  const render = async () => {
    let items;
    try {
      items = await api.connections();
    } catch (e) {
      return clear(list).append(el("p", { class: "error-text" }, e.message));
    }
    clear(list).append(
      el("div", { class: "section-head" }, el("h2", {}, `${items.length} connection${items.length === 1 ? "" : "s"}`),
        el("button", { type: "button", onclick: () => editDialog(api, null, render) }, icon("plus"), "New connection")),
      items.length ? el("table", { class: "list connections" }, el("tbody", {}, items.map((c) => el("tr", {},
        el("td", {}, el("strong", {}, c.name)),
        el("td", { class: "muted" }, (TYPES[c.type] || { label: c.type }).label),
        el("td", { class: "muted" }, endpointOf(c)),
        el("td", { class: "muted" }, secretOf(c)),
        el("td", { class: "muted" }, `changed ${timeAgo(c.updated_at)}`),
        el("td", {},
          el("button", { type: "button", class: "ghost small", title: "Change", onclick: () => editDialog(api, c, render) }, icon("pencil")),
          el("button", { type: "button", class: "ghost small", title: "Delete", onclick: () => remove(api, c, render) }, icon("trash")))))))
        : el("p", { class: "muted" }, "None yet. Add one, then give it a name in a space."));
  };
  await render();
}

function endpointOf(c) {
  const s = c.settings || {};
  if (s.host) return `${s.host}${s.port ? ":" + s.port : ""}/${s.database || ""}`;
  if (s.bucket) return `${s.endpoint || ""}/${s.bucket}`;
  if (s.container) return `${s.account}/${s.container}`;
  return s.url || "";
}

function secretOf(c) {
  const s = c.secret || {};
  if (s.mode === "referenced") return `secret: ${s.ref}`;
  if (s.mode === "managed") return "secret: stored";
  return "no secret";
}

async function remove(api, c, done) {
  if (!confirm(`Delete the connection ${c.name}? Its stored secret is deleted too.`)) return;
  try {
    await api.deleteConnection(c.id);
    done();
  } catch (e) {
    toast(e.status === 409 ? `${c.name} is used by a space: give that name another connection first. ${e.message}` : e.message);
  }
}

/** Creates (c null) or changes a connection, in a dialog. */
function editDialog(api, c, done) {
  const dialog = el("dialog", { class: "dialog connection-dialog" });
  const type = el("select", { "aria-label": "Type", disabled: !!c },
    Object.entries(TYPES).map(([k, t]) => el("option", { value: k, selected: c ? c.type === k : k === "postgresql" }, t.label)));
  const name = el("input", { "aria-label": "Name", value: c ? c.name : "", placeholder: "e.g. crm-prod", autocomplete: "off" });
  const fields = el("div", { class: "fields" });
  const secretMode = el("select", { "aria-label": "Secret" },
    c ? el("option", { value: "keep" }, "Keep the current secret") : null,
    el("option", { value: "value" }, "Enter it (stored by nbsandbox)"),
    el("option", { value: "ref" }, "Reference one your platform keeps"),
    el("option", { value: "none" }, "None"));
  const secret = el("input", { type: "password", autocomplete: "new-password", "aria-label": "Secret" });
  const ref = el("input", { "aria-label": "Reference", placeholder: "name#key (a Kubernetes Secret) or path#key (Vault)", hidden: true });
  const inputs = {};
  const drawFields = () => {
    const t = TYPES[type.value];
    clear(fields);
    for (const [key, label, placeholder] of t.fields) {
      const v = c && c.settings[key] !== undefined ? String(c.settings[key]) : "";
      inputs[key] = el("input", { value: v, placeholder: placeholder || "", "aria-label": label });
      fields.append(el("label", {}, label, inputs[key]));
    }
    secret.placeholder = t.secret;
  };
  const drawSecret = () => {
    secret.hidden = secretMode.value !== "value";
    ref.hidden = secretMode.value !== "ref";
  };
  type.addEventListener("change", drawFields);
  secretMode.addEventListener("change", drawSecret);
  drawFields();
  drawSecret();
  const save = el("button", { type: "button" }, icon("save"), c ? "Save" : "Create");
  save.addEventListener("click", async () => {
    const settings = {};
    for (const [key] of TYPES[type.value].fields) {
      const v = inputs[key].value.trim();
      if (v === "") continue;
      settings[key] = key === "port" ? Number(v) : key === "use_ssl" ? v !== "false" : v;
    }
    let sec;
    if (secretMode.value === "value") sec = { value: secret.value };
    if (secretMode.value === "ref") sec = { ref: ref.value.trim() };
    if (secretMode.value === "none") sec = { none: true };
    save.disabled = true;
    try {
      if (c) {
        const patch = { settings };
        if (name.value.trim() !== c.name) patch.name = name.value.trim();
        if (sec) patch.secret = sec;
        await api.updateConnection(c.id, patch);
      } else {
        await api.createConnection({ name: name.value.trim(), type: type.value, settings, secret: sec || { none: true } });
      }
      secret.value = "";
      dialog.close();
      done();
    } catch (e) {
      toast(e.message);
    } finally {
      save.disabled = false;
    }
  });
  dialog.append(
    el("h2", {}, c ? `Change ${c.name}` : "New connection"),
    el("label", {}, "Type", type), el("label", {}, "Name", name), fields,
    el("label", {}, "Secret", secretMode), secret, ref,
    el("p", { class: "muted" }, "Give nbsandbox a read-only user: the database's own permissions are what really limits a query."),
    el("div", { class: "actions" }, save, el("button", { type: "button", class: "ghost", onclick: () => dialog.close() }, "Cancel")));
  dialog.addEventListener("close", () => dialog.remove());
  document.body.append(dialog);
  dialog.showModal();
}

// ---------- a space's names ----------

/**
 * The space's names per environment, editable: a table with a row per
 * name and a column per environment, each cell one of the user's
 * connections. Every environment binds every name.
 */
export function spaceConnectionsSection(api, space, onConnections) {
  const section = el("section", { class: "section space-connections", hidden: true });
  let model; // {default, envs: [names], names: [names], cells: {env: {name: connId}}}
  let conns = [];
  const draw = () => {
    const opts = (env, name) => el("select", { "aria-label": `${name} in ${env}`, onchange: (e) => { model.cells[env][name] = e.target.value; } },
      el("option", { value: "" }, "—"),
      conns.map((c) => el("option", { value: c.id, selected: model.cells[env][name] === c.id }, `${c.name} (${(TYPES[c.type] || { label: c.type }).label})`)));
    const table = el("table", { class: "list bindings" },
      el("thead", {}, el("tr", {}, el("th", {}, "Name"), model.envs.map((env) => el("th", {},
        el("label", { title: "The default environment" }, el("input", { type: "radio", name: "default-env", checked: env === model.default,
          onchange: () => { model.default = env; } }), env),
        el("button", { type: "button", class: "ghost small", title: `Remove ${env}`, onclick: () => {
          model.envs = model.envs.filter((e) => e !== env);
          delete model.cells[env];
          if (model.default === env) model.default = model.envs[0] || "";
          draw();
        } }, icon("x")))), el("th", {}))),
      el("tbody", {}, model.names.map((name) => el("tr", {}, el("td", {}, el("code", {}, name)),
        model.envs.map((env) => el("td", {}, opts(env, name))),
        el("td", {}, el("button", { type: "button", class: "ghost small", title: `Remove ${name}`, onclick: () => {
          model.names = model.names.filter((n) => n !== name);
          for (const env of model.envs) delete model.cells[env][name];
          draw();
        } }, icon("trash")))))));
    const add = (what) => {
      const v = (prompt(what === "name" ? "A name notebooks will use (lowercase, e.g. crm):" : "An environment (lowercase, e.g. prod):") || "").trim();
      if (!v) return;
      if (!NAME.test(v)) return toast("Lowercase letters, digits and _; starting with a letter.");
      if (what === "name" && !model.names.includes(v)) model.names.push(v);
      if (what === "env" && !model.envs.includes(v)) {
        model.envs.push(v);
        model.cells[v] = {};
        if (!model.default) model.default = v;
      }
      draw();
    };
    const save = el("button", { type: "button" }, icon("save"), "Save");
    save.addEventListener("click", async () => {
      const environments = {};
      for (const env of model.envs) environments[env] = { ...model.cells[env] };
      save.disabled = true;
      try {
        await api.setSpaceConnections(space, { default_environment: model.default, environments });
        toast("Saved.", "info");
        load();
      } catch (e) {
        toast(e.message);
      } finally {
        save.disabled = false;
      }
    });
    clear(section).append(
      el("div", { class: "section-head" }, el("h2", {}, "Connections"),
        el("span", { class: "muted" }, "names notebooks query, per environment")),
      el("div", { class: "card" },
        model.names.length && model.envs.length ? table : el("p", { class: "muted" },
          "Add a name (crm, lake) and an environment (dev, prod), then choose which of your connections each name means there."),
        el("div", { class: "actions" },
          el("button", { type: "button", class: "ghost", onclick: () => add("name") }, icon("plus"), "Name"),
          el("button", { type: "button", class: "ghost", onclick: () => add("env") }, icon("plus"), "Environment"),
          save,
          conns.length ? null : el("button", { type: "button", class: "ghost", onclick: onConnections }, "Add a connection first"))));
  };
  const load = async () => {
    try {
      const [sc, list] = await Promise.all([api.spaceConnections(space), api.connections()]);
      conns = list;
      const envs = Object.keys(sc.environments || {}).sort();
      const names = [...new Set(envs.flatMap((e) => Object.keys(sc.environments[e])))].sort();
      const cells = {};
      for (const env of envs) {
        cells[env] = {};
        for (const [n, b] of Object.entries(sc.environments[env])) cells[env][n] = b.id;
      }
      model = { default: sc.default_environment || envs[0] || "", envs, names, cells };
      section.hidden = false;
      draw();
    } catch (e) {
      if (!(e instanceof ApiError && e.status === 404)) toast(e.message);
    }
  };
  load();
  return section;
}
