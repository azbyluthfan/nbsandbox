# Copyright 2026 The nbsandbox Authors
# SPDX-License-Identifier: Apache-2.0
"""The session bridge: the main process of an interactive session's
container (docs/sessions.md). It starts an IPython kernel over Unix sockets
inside the container and relays Jupyter messages between the kernel and
nbsandboxd, which speaks to it over the container's stdin and stdout (an
attach: the container has no network). One JSON message per line, in
Jupyter's own message format (channel, header, parent_header, metadata,
content), without binary buffers.

Only some message types pass, both ways (ALLOWED_IN, ALLOWED_OUT); anything
else is refused with a "bridge" message. input() raises (allow_stdin is
always off), and comms (widgets) are refused. Messages of its own are on the
"bridge" channel:

    {"channel": "bridge", "content": {"status": "ready" | "restarting" | "dead", ...}}
    {"channel": "bridge", "content": {"refused": "<msg_type>", "msg_id": "..."}}

nbsandboxd restarts the kernel with a control shutdown_request whose content
has "restart": true; a browser can't send one (nbsandboxd filters it out).
Interrupts are SIGINT to the kernel (they also stop code in C extensions),
answered with an interrupt_reply. A kernel that dies is started again."""

import asyncio
import datetime
import json
import os
import queue
import sys
import uuid

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
from nbsandbox_runner import _ipc_kernel_manager, install_kernel_startup  # noqa: E402

ALLOWED_IN = {
    "shell": {"execute_request", "complete_request", "inspect_request", "is_complete_request", "kernel_info_request"},
    "control": {"interrupt_request", "shutdown_request"},
}
ALLOWED_OUT = {
    "shell": {"execute_reply", "complete_reply", "inspect_reply", "is_complete_reply", "kernel_info_reply"},
    "control": {"interrupt_reply", "shutdown_reply"},
    "iopub": {"status", "stream", "display_data", "update_display_data", "execute_result", "error",
              "execute_input", "clear_output"},
}
# A single message larger than this (as JSON) is replaced by a short notice:
# a cell that displays a huge object must not flood nbsandboxd or the browser.
MAX_MESSAGE = int(os.environ.get("NBSANDBOX_SESSION_MAX_MESSAGE", str(2 << 20)))
OUTPUT_TYPES = {"stream", "display_data", "update_display_data", "execute_result", "error"}
# How long a pump waits on one poll of a kernel socket before polling again.
PUMP_POLL = 1.0


def _default(o):
    if isinstance(o, (datetime.datetime, datetime.date)):
        return o.isoformat()
    if isinstance(o, (bytes, bytearray, memoryview)):
        return None  # binary buffers aren't carried
    return str(o)


class Bridge:
    def __init__(self, out=None):
        self.out = out or sys.stdout.buffer
        self.write_lock = asyncio.Lock()
        self.km = None
        self.kc = None
        self.pumps = []
        self.restarting = False

    async def emit(self, msg):
        line = json.dumps(msg, default=_default, separators=(",", ":"))
        if len(line) > MAX_MESSAGE and msg.get("header", {}).get("msg_type") in OUTPUT_TYPES:
            line = json.dumps(_too_large(msg, len(line)), default=_default, separators=(",", ":"))
        async with self.write_lock:
            self.out.write(line.encode() + b"\n")
            self.out.flush()

    async def bridge(self, **content):
        await self.emit({"channel": "bridge", "content": content})

    async def start_kernel(self):
        km = _ipc_kernel_manager()(kernel_name="python3")
        # The kernel's own stdout and stderr must never reach ours: ours is
        # the protocol.
        devnull = open(os.devnull, "w")
        await km.start_kernel(stdout=devnull, stderr=devnull)
        kc = km.client()
        kc.start_channels()
        await kc.wait_for_ready(timeout=120)
        self.km, self.kc = km, kc
        self.pumps = [asyncio.create_task(self.pump(kc.shell_channel, "shell")),
                      asyncio.create_task(self.pump(kc.iopub_channel, "iopub")),
                      asyncio.create_task(self.pump(kc.control_channel, "control"))]

    async def stop_kernel(self):
        for t in self.pumps:
            t.cancel()
        self.pumps = []
        if self.kc is not None:
            self.kc.stop_channels()
        if self.km is not None:
            try:
                await self.km.shutdown_kernel(now=True)
            except Exception:
                pass
        self.km = self.kc = None

    async def restart(self, reason, parent=None):
        self.restarting = True
        try:
            await self.bridge(status="restarting", reason=reason)
            await self.stop_kernel()
            await self.start_kernel()
            if parent is not None:
                await self.emit(_reply("control", "shutdown_reply", parent, {"status": "ok", "restart": True}))
            await self.bridge(status="ready")
        finally:
            self.restarting = False

    async def pump(self, channel, name):
        """Relays one of the kernel's channels to nbsandboxd, until cancelled.

        It never waits on a single poll for good: ZeroMQ signals a socket
        readable on edges only, and the shell socket is also written to (and
        polled with timeouts while the kernel starts), so an endless poll can
        miss a reply already queued, and the session's shell channel goes
        silent while iopub carries on (seen under load in the soak). A new
        poll every second sees what is queued. And an error never ends the
        pump: it is reported, and the pump goes on."""
        while True:
            try:
                msg = await channel.get_msg(timeout=PUMP_POLL)
            except queue.Empty:
                continue
            except asyncio.CancelledError:
                raise
            except Exception as e:
                await self.bridge(notice="a message from the kernel's %s channel was lost: %s" % (name, e))
                await asyncio.sleep(PUMP_POLL / 10)
                continue
            mt = msg["header"]["msg_type"]
            if mt not in ALLOWED_OUT[name]:
                continue
            await self.emit({"channel": name, "header": msg["header"], "parent_header": msg["parent_header"],
                             "metadata": msg.get("metadata", {}), "content": msg["content"]})

    async def watch(self):
        """Starts the kernel again if it dies (out of memory, os._exit)."""
        while True:
            await asyncio.sleep(1)
            if self.restarting or self.km is None:
                continue
            if not await self.km.is_alive():
                await self.bridge(status="dead")
                await self.restart("the kernel died")

    async def handle(self, line):
        try:
            m = json.loads(line)
            ch, header = m["channel"], m["header"]
            mt = header["msg_type"]
        except Exception:
            return
        if mt not in ALLOWED_IN.get(ch, set()):
            await self.bridge(refused=mt, msg_id=header.get("msg_id"))
            return
        content = m.get("content") or {}
        if mt == "interrupt_request":
            if self.km is not None:
                await self.km.interrupt_kernel()
            await self.emit(_reply("control", "interrupt_reply", header, {"status": "ok"}))
            return
        if mt == "shutdown_request":
            if not content.get("restart"):
                await self.bridge(refused=mt, msg_id=header.get("msg_id"))
                return
            await self.restart("restart requested", parent=header)
            return
        if self.kc is None:
            await self.bridge(refused=mt, msg_id=header.get("msg_id"), reason="the kernel is restarting")
            return
        if mt == "execute_request":
            content = dict(content, allow_stdin=False)  # input() raises
        msg = self.kc.session.msg(mt, content)
        msg["header"]["msg_id"] = header.get("msg_id") or msg["header"]["msg_id"]
        if header.get("session"):
            msg["header"]["session"] = header["session"]
        getattr(self.kc, ch + "_channel").send(msg)

    async def serve(self, reader):
        await self.start_kernel()
        await self.bridge(status="ready")
        watcher = asyncio.create_task(self.watch())
        try:
            while True:
                line = await reader.readline()
                if not line:
                    # Docker keeps a session's stdin open between attaches;
                    # an end means nobody will ever write again.
                    break
                await self.handle(line)
        finally:
            watcher.cancel()
            await self.stop_kernel()


def _reply(channel, msg_type, parent, content):
    return {"channel": channel,
            "header": {"msg_id": uuid.uuid4().hex, "msg_type": msg_type, "session": parent.get("session", ""),
                       "username": "nbsandbox", "date": datetime.datetime.now(datetime.timezone.utc).isoformat(),
                       "version": "5.3"},
            "parent_header": parent, "metadata": {}, "content": content}


def _too_large(msg, size):
    note = "[nbsandbox] output not shown: %d bytes, over the session's %d-byte limit per output\n" % (size, MAX_MESSAGE)
    out = {k: msg.get(k) for k in ("channel", "header", "parent_header", "metadata")}
    out["header"] = dict(msg["header"], msg_type="stream")
    out["content"] = {"name": "stderr", "text": note}
    return out


def session_folders():
    """A scratch run folder for the nbsandbox module, as a run has /run (here
    in /tmp: nothing in it is kept), so notebooks written for runs work in a
    session: output() and emit() write there and show what they wrote."""
    run_dir = os.path.join(os.environ.get("TMPDIR", "/tmp"), "nbsandbox-session")
    os.makedirs(os.path.join(run_dir, "artifacts"), exist_ok=True)
    os.environ.setdefault("NBSANDBOX_SESSION", "1")
    os.environ.setdefault("NBSANDBOX_RUN_DIR", run_dir)
    os.environ.setdefault("NBSANDBOX_OUTPUT_DIR", os.path.join(run_dir, "artifacts"))


async def main():
    session_folders()
    install_kernel_startup()
    loop = asyncio.get_running_loop()
    reader = asyncio.StreamReader(limit=16 << 20)
    await loop.connect_read_pipe(lambda: asyncio.StreamReaderProtocol(reader), sys.stdin)
    await Bridge().serve(reader)


if __name__ == "__main__":
    asyncio.run(main())
