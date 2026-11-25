// Copyright 2026 The nbsandbox Authors
// SPDX-License-Identifier: Apache-2.0
//
// A notebook's interactive session (docs/sessions.md): its live kernel,
// reached over the session's WebSocket as Jupyter messages. Runs cells and
// streams their outputs, asks the kernel for completions and docs (with a
// deadline: typing never waits for a busy kernel), and follows the kernel's
// state for the status indicator. A dropped connection reconnects by
// itself; the session (and its variables) is still there.

const enc = encodeURIComponent;

// Python, run silently in the kernel for the variables panel: the user's
// variables (not modules, functions, classes, or IPython's own), each
// summarised (type, shape or length, a DataFrame's columns, the start of
// its repr) with any failure left blank, as JSON. The reply reads it back
// through user_expressions; its helper names start with "_", so they are
// never listed.
const VARIABLES_CODE = `def _nbsandbox_variables():
    import json
    ip = get_ipython()
    out = []
    for k, v in sorted(ip.user_ns.items()):
        if k.startswith("_") or k in ip.user_ns_hidden or k in ("In", "Out", "exit", "quit", "get_ipython"):
            continue
        t = type(v).__name__
        if t in ("module", "function", "builtin_function_or_method", "type", "method"):
            continue
        e = {"name": k, "type": t, "shape": "", "columns": [], "value": ""}
        try:
            if hasattr(v, "shape") and not isinstance(v, (bool, int, float, complex)):
                e["shape"] = " x ".join(str(n) for n in v.shape)
            elif hasattr(v, "__len__") and not isinstance(v, (str, bytes)):
                e["shape"] = str(len(v))
        except Exception:
            pass
        try:
            cols = getattr(v, "columns", None)
            if cols is not None:
                e["columns"] = [str(c) for c in list(cols)[:30]]
        except Exception:
            pass
        try:
            e["value"] = repr(v)[:100]
        except Exception:
            pass
        out.append(e)
        if len(out) >= 200:
            break
    return json.dumps(out)
class _NbsandboxJSON(str):
    def __repr__(self):
        return str(self)
_nbsandbox_vars = _NbsandboxJSON(_nbsandbox_variables())
del _nbsandbox_variables, _NbsandboxJSON`;

function msgId() {
  const b = new Uint8Array(8);
  crypto.getRandomValues(b);
  return Array.from(b, (x) => x.toString(16).padStart(2, "0")).join("");
}

export class KernelSession {
  /**
   * api: the UI's Api; space, notebook: what it is for.
   * onState({status, kernel, mode, reason}): the session's state changed.
   * onNotice(text): something to tell the user (a file write-back).
   */
  constructor(api, space, notebook, { onState, onNotice } = {}) {
    this.api = api;
    this.space = space;
    this.notebook = notebook;
    this.onState = onState || (() => {});
    this.onNotice = onNotice || (() => {});
    this.id = "";
    this.mode = "";
    this.status = "none"; // none | starting | connecting | ready | ended
    this.kernel = ""; // idle | busy | starting | restarting | dead
    this.reason = "";
    this.ws = null;
    this.pending = new Map(); // msg_id -> {onMessage(msg), done(reply)}
    this.closedByUs = false;
    this.retry = 0;
    this.clientSession = msgId();
  }

  path(rest = "") {
    return `/spaces/${enc(this.space)}/sessions${rest}`;
  }

  setState(changes) {
    Object.assign(this, changes);
    this.onState({ status: this.status, kernel: this.kernel, mode: this.mode, reason: this.reason });
  }

  /** The notebook's open session, if it has one (null otherwise). */
  async find() {
    const list = await this.api.request("GET", this.path());
    return (list.items || []).find((s) => s.notebook === this.notebook) || null;
  }

  /** Opens (or joins) the notebook's session and connects to it. */
  async start(mode) {
    this.setState({ status: "starting", reason: "" });
    const s = await this.api.request("POST", this.path(), { notebook: this.notebook, mode: mode || undefined,
      environment: this.environment || undefined });
    this.attach(s);
  }

  /** Connects to an open session (from find or start). */
  attach(s) {
    this.id = s.id;
    this.closedByUs = false;
    this.setState({ mode: s.mode, status: "connecting" });
    this.connect();
  }

  connect() {
    if (!this.id || this.closedByUs) return;
    const proto = location.protocol === "https:" ? "wss:" : "ws:";
    const ws = new WebSocket(`${proto}//${location.host}${this.api.url(this.path(`/${enc(this.id)}/channel`))}`);
    this.ws = ws;
    ws.onmessage = (ev) => {
      let msg;
      try {
        msg = JSON.parse(ev.data);
      } catch {
        return;
      }
      this.receive(msg);
    };
    ws.onclose = (ev) => {
      if (this.ws !== ws) return;
      this.ws = null;
      if (this.closedByUs || this.status === "ended") return;
      // A dropped connection (a restart of nbsandboxd, the network): the
      // session is still there; try again, more slowly each time.
      this.failPending("the connection to the session dropped");
      this.retry++;
      if (this.retry > 8) {
        this.setState({ status: "ended", reason: ev.reason || "the session can't be reached" });
        return;
      }
      this.setState({ status: "connecting" });
      setTimeout(() => this.reconnect(), Math.min(500 * 2 ** this.retry, 15000));
    };
  }

  async reconnect() {
    if (this.closedByUs) return;
    try {
      const s = await this.api.request("GET", this.path(`/${enc(this.id)}`));
      if (s.status === "ended") return this.setState({ status: "ended", reason: s.end_reason || "" });
    } catch {
      // try the WebSocket anyway
    }
    this.connect();
  }

  receive(msg) {
    const content = msg.content || {};
    if (msg.channel === "bridge") {
      if (content.status === "attached") {
        this.retry = 0;
        this.setState({ status: "ready", kernel: content.kernel || "idle", mode: content.mode || this.mode });
      } else if (content.status === "ended") {
        this.failPending("the session has ended");
        this.setState({ status: "ended", kernel: "", reason: content.reason || "" });
      } else if (content.status === "ready") {
        this.setState({ kernel: "idle" });
      } else if (content.status === "restarting" || content.status === "dead") {
        this.failPending(content.status === "dead" ? "the kernel died" : "the kernel restarted");
        this.setState({ kernel: content.status });
      } else if (content.notice) {
        this.onNotice(content.notice);
      } else if (content.refused) {
        this.onNotice(`Refused: ${content.error || content.refused}`);
      }
      return;
    }
    // The request's own handler first: a cell that just finished is no
    // longer pending when the kernel's state is worked out.
    const parentId = msg.parent_header && msg.parent_header.msg_id;
    const p = parentId && this.pending.get(parentId);
    if (p) p.onMessage(msg);
    if (msg.channel === "iopub" && msg.header && msg.header.msg_type === "status") {
      const parent = msg.parent_header || {};
      // A completion's busy-idle isn't the kernel being busy.
      if (parent.msg_type === "execute_request" || content.execution_state !== "busy") {
        this.setState({ kernel: this.anyRunning() ? "busy" : content.execution_state });
      }
    }
  }

  anyRunning() {
    for (const p of this.pending.values()) if (p.kind === "execute" && !p.finished) return true;
    return false;
  }

  failPending(why) {
    for (const [id, p] of this.pending) {
      p.fail(why);
      this.pending.delete(id);
    }
  }

  get ready() {
    return this.status === "ready" && this.ws && this.ws.readyState === WebSocket.OPEN;
  }

  get busy() {
    return this.kernel !== "idle";
  }

  send(channel, msgType, content) {
    const id = msgId();
    this.ws.send(JSON.stringify({
      channel,
      header: { msg_id: id, msg_type: msgType, session: this.clientSession, username: "", version: "5.3" },
      parent_header: {}, metadata: {}, content,
    }));
    return id;
  }

  /**
   * Runs code. on: {output(msg), input(executionCount), done(reply), error(why)}.
   * Outputs arrive as Jupyter iopub messages; done gets the execute_reply's
   * content once the kernel is idle again (every output has arrived).
   */
  execute(code, on) {
    if (!this.ready) throw new Error("not connected to the session");
    let reply = null;
    const entry = { kind: "execute", finished: false };
    const id = this.send("shell", "execute_request", {
      code, silent: false, store_history: true, user_expressions: {}, allow_stdin: false, stop_on_error: true,
    });
    entry.onMessage = (msg) => {
      const t = msg.header.msg_type;
      if (msg.channel === "shell" && t === "execute_reply") reply = msg.content;
      else if (t === "execute_input") on.input && on.input(msg.content.execution_count);
      else if (t === "status" && msg.content.execution_state === "idle") {
        entry.finished = true;
        this.pending.delete(id);
        on.done && on.done(reply || { status: "ok" });
        if (this.onExecuted) setTimeout(() => this.onExecuted(), 0); // after the state update
      } else if (msg.channel === "iopub" && t !== "status") on.output && on.output(msg);
    };
    entry.fail = (why) => on.error && on.error(why);
    this.pending.set(id, entry);
    return id;
  }

  /**
   * The kernel's variables, for the variables panel: [{name, type, shape,
   * columns, value}], from one silent request (no output, no execution
   * count), or null if the kernel can't say now.
   */
  async variables(deadlineMs = 3000) {
    if (!this.ready || this.busy) return null;
    const reply = await this.request("execute_request", {
      code: VARIABLES_CODE, silent: true, store_history: false, allow_stdin: false, stop_on_error: false,
      user_expressions: { v: "_nbsandbox_vars" },
    }, deadlineMs);
    const v = reply && reply.user_expressions && reply.user_expressions.v;
    if (!v || v.status !== "ok" || !v.data || typeof v.data["text/plain"] !== "string") return null;
    try {
      return JSON.parse(v.data["text/plain"]);
    } catch {
      return null;
    }
  }

  /** A shell request answered by one reply, or null after deadlineMs. */
  request(msgType, content, deadlineMs) {
    if (!this.ready) return Promise.resolve(null);
    return new Promise((resolve) => {
      const id = this.send("shell", msgType, content);
      const timer = setTimeout(() => {
        this.pending.delete(id);
        resolve(null); // a late answer is dropped, as JupyterLab does
      }, deadlineMs);
      this.pending.set(id, {
        kind: "request",
        onMessage: (msg) => {
          if (msg.channel !== "shell") return;
          clearTimeout(timer);
          this.pending.delete(id);
          resolve(msg.content);
        },
        fail: () => {
          clearTimeout(timer);
          resolve(null);
        },
      });
    });
  }

  /** The kernel's completions at pos in code (null if it can't answer in time). */
  complete(code, pos, deadlineMs = 300) {
    if (this.busy) return Promise.resolve(null); // a busy kernel answers after its cell
    return this.request("complete_request", { code, cursor_pos: pos }, deadlineMs);
  }

  /** The kernel's docs for what is at pos (detail 0: signature and docstring). */
  inspect(code, pos, deadlineMs = 800) {
    if (this.busy) return Promise.resolve(null);
    return this.request("inspect_request", { code, cursor_pos: pos, detail_level: 0 }, deadlineMs);
  }

  interrupt() {
    return this.api.request("POST", this.path(`/${enc(this.id)}/interrupt`));
  }

  restart() {
    return this.api.request("POST", this.path(`/${enc(this.id)}/restart`));
  }

  /** Writes a read-write session's file changes to the space now. */
  syncFiles() {
    return this.api.request("POST", this.path(`/${enc(this.id)}/sync`));
  }

  /** Ends the session: its kernel stops. */
  async end() {
    this.closedByUs = true;
    try {
      await this.api.request("DELETE", this.path(`/${enc(this.id)}`));
    } finally {
      this.close();
      this.setState({ status: "ended", kernel: "", reason: "you ended it" });
    }
  }

  /** Disconnects this page; the session goes on (it ends when idle). */
  close() {
    this.closedByUs = true;
    this.failPending("closed");
    if (this.ws) {
      const ws = this.ws;
      this.ws = null;
      ws.close();
    }
  }
}
