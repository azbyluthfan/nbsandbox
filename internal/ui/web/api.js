// Copyright 2026 The nbsandbox Authors
// SPDX-License-Identifier: Apache-2.0
//
// API client for the local UI. Every call goes through /ui/api/as/{user}/v1,
// which runs the normal API as the chosen user.

export class ApiError extends Error {
  constructor(status, code, message, body) {
    super(message);
    this.status = status;
    this.code = code;
    this.body = body;
  }
}

const enc = encodeURIComponent;

// Server messages are written for API callers; a few read better in the UI.
function friendly(err) {
  if (err.code === "docker_unavailable") {
    return "Runs can't start right now: Docker isn't reachable from nbsandboxd. Nothing was queued; try again in a few seconds.";
  }
  return err.message;
}

function unquote(etag) {
  return (etag || "").replace(/^W\//, "").replace(/^"|"$/g, "");
}

/** Encodes a relative file path segment by segment, keeping the slashes. */
export function encodePath(p) {
  return p.split("/").map(enc).join("/");
}

export async function listUsers() {
  const r = await fetch("/ui/api/users");
  if (!r.ok) throw new ApiError(r.status, "", "cannot load users", null);
  return (await r.json()).users;
}

export class Api {
  constructor(user) {
    this.user = user;
    this.base = `/ui/api/as/${enc(user)}/v1`;
  }

  url(path) {
    return this.base + path;
  }

  async request(method, path, body, headers = {}) {
    const init = { method, headers: { ...headers } };
    if (body !== undefined) {
      init.headers["Content-Type"] = "application/json";
      init.body = JSON.stringify(body);
    }
    const r = await fetch(this.url(path), init);
    const text = await r.text();
    let data = null;
    try {
      data = text ? JSON.parse(text) : null;
    } catch {
      data = null;
    }
    if (!r.ok) {
      const err = data && data.error ? data.error : { code: "", message: text || r.statusText };
      throw new ApiError(r.status, err.code, friendly(err), data);
    }
    return data;
  }

  spaces() {
    return this.request("GET", "/spaces?limit=200").then((d) => d.items);
  }

  createSpace(name) {
    return this.request("POST", "/spaces", { name });
  }

  /** Files and folders (type "file" | "folder"), each file with its version. */
  files(space) {
    return this.request("GET", `/spaces/${enc(space)}/files`).then((d) => d.items);
  }

  /** A file's bytes and the version they are (from the ETag). */
  async fileWithVersion(space, path) {
    const r = await fetch(this.fileUrl(space, path), { cache: "no-store" });
    if (!r.ok) throw new ApiError(r.status, "", r.status === 404 ? `${path} no longer exists` : `cannot open ${path}`, null);
    return { bytes: new Uint8Array(await r.arrayBuffer()), version: unquote(r.headers.get("ETag")) };
  }

  /** "same", "changed" or "gone": whether the file is still at version. */
  async checkVersion(space, path, version) {
    const r = await fetch(this.fileUrl(space, path), { method: "GET", cache: "no-store", headers: { "If-None-Match": `"${version}"` } });
    if (r.status === 304) return "same";
    if (r.status === 404) return "gone";
    return r.ok ? "changed" : "same"; // other errors: don't alarm the editor
  }

  /**
   * Saves text to path. version: the version being replaced, or null to
   * create (fails if the file exists). A 412 ApiError carries
   * body.current_version.
   */
  async saveFile(space, path, text, version) {
    const headers = { "Content-Type": "application/octet-stream" };
    if (version) headers["If-Match"] = `"${version}"`;
    else headers["If-None-Match"] = "*";
    const r = await fetch(this.fileUrl(space, path), { method: "PUT", headers, body: text });
    const data = await r.json().catch(() => null);
    if (!r.ok) {
      const err = data && data.error ? data.error : { code: "", message: r.statusText };
      throw new ApiError(r.status, err.code, err.message, data);
    }
    return data;
  }

  move(space, path, to) {
    return this.request("PATCH", `/spaces/${enc(space)}/files/${encodePath(path)}`, { path: to });
  }

  createFolder(space, path) {
    return this.request("POST", `/spaces/${enc(space)}/folders`, { path });
  }

  remove(space, path, recursive) {
    return this.request("DELETE", `/spaces/${enc(space)}/files/${encodePath(path)}${recursive ? "?recursive=true" : ""}`);
  }

  /** Uploads File objects into folder ("" = top level), all or nothing. */
  async upload(space, folder, files) {
    const form = new FormData();
    for (const f of files) form.append("file", f, f.name);
    const q = folder ? `?folder=${enc(folder)}` : "";
    const r = await fetch(this.url(`/spaces/${enc(space)}/files${q}`), { method: "POST", body: form });
    const data = await r.json().catch(() => null);
    if (!r.ok) throw new ApiError(r.status, data && data.error ? data.error.code : "", data && data.error ? data.error.message : r.statusText, data);
    return data;
  }

  runtime() {
    return this.request("GET", "/runtime");
  }

  functions(space) {
    return this.request("GET", `/spaces/${enc(space)}/functions`).then((d) => d.items);
  }

  fn(space, name) {
    return this.request("GET", `/spaces/${enc(space)}/functions/${enc(name)}`);
  }

  fnVersion(space, name, version) {
    return this.request("GET", `/spaces/${enc(space)}/functions/${enc(name)}/versions/${version}`);
  }

  publish(space, name, notebook, files) {
    return this.request("POST", `/spaces/${enc(space)}/functions/${enc(name)}/versions`, { notebook, files });
  }

  invoke(space, name, version, parameters) {
    const path = version ? `/versions/${version}/invoke` : "/invoke";
    return this.request("POST", `/spaces/${enc(space)}/functions/${enc(name)}${path}`, { parameters });
  }

  deleteFunction(space, name) {
    return this.request("DELETE", `/spaces/${enc(space)}/functions/${enc(name)}`);
  }

  result(space, id) {
    return this.request("GET", `/spaces/${enc(space)}/runs/${enc(id)}/result`);
  }

  eventsUrl(space, id) {
    return this.url(`/spaces/${enc(space)}/runs/${enc(id)}/events`);
  }

  fileUrl(space, path) {
    return this.url(`/spaces/${enc(space)}/files/${encodePath(path)}`);
  }

  async fileBytes(space, path) {
    const r = await fetch(this.fileUrl(space, path));
    if (!r.ok) throw new ApiError(r.status, "", `cannot open ${path}`, null);
    return new Uint8Array(await r.arrayBuffer());
  }

  runs(space) {
    return this.request("GET", `/spaces/${enc(space)}/runs?limit=30`).then((d) => d.items);
  }

  run(space, id) {
    return this.request("GET", `/spaces/${enc(space)}/runs/${enc(id)}`);
  }

  startRun(space, notebook, parameters, readOnly) {
    const body = { notebook };
    if (parameters) body.parameters = parameters;
    if (readOnly) body.space = "read-only";
    return this.request("POST", `/spaces/${enc(space)}/runs`, body);
  }

  cancelRun(space, id, reason) {
    return this.request("POST", `/spaces/${enc(space)}/runs/${enc(id)}/cancel`, reason ? { reason } : undefined);
  }

  artifacts(space, id) {
    return this.request("GET", `/spaces/${enc(space)}/runs/${enc(id)}/artifacts`).then((d) => d.items);
  }

  runUrl(space, id, rest) {
    return this.url(`/spaces/${enc(space)}/runs/${enc(id)}${rest}`);
  }
}
