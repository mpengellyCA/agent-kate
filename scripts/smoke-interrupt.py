#!/usr/bin/env python3
"""End-to-end smoke test for hard interrupt + resume (plan 04).

Starts an agent on a long, slow-to-produce turn, waits until it is *actively
generating*, then fires agent.interrupt. A passing run proves:

  * the in-flight turn halts fast (an `interrupted` lifecycle arrives within a
    couple seconds — not after the whole count-to-500 finishes),
  * the thread is left dormant-but-resumable, and
  * `agent.resume` + a follow-up restores the session: the resumed agent still
    recalls a secret it was told before the interrupt.

This exercises the real RPC path: agent.interrupt -> Supervisor.Interrupt ->
in-band stream-json interrupt frame -> reap() "interrupted" -> resume.

Requires: a built ./build/akcore and an authenticated `claude` CLI.
Run unbuffered for live output:  python3 -u scripts/smoke-interrupt.py
"""
import json
import os
import shutil
import socket
import subprocess
import sys
import tempfile
import threading
import time

ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
AKCORE = os.path.join(ROOT, "build", "akcore")
SOCK = os.path.join(tempfile.gettempdir(), "ak-interrupt.sock")
SECRET = "4731"


def log(*a):
    print(*a, flush=True)


class Core:
    """One akcore process plus a JSON-RPC connection to it."""

    def __init__(self, sock, env, label):
        self.label = label
        try:
            os.unlink(sock)
        except FileNotFoundError:
            pass
        self.proc = subprocess.Popen([AKCORE, "--socket", sock], env=env,
                                     stderr=subprocess.PIPE, text=True)
        threading.Thread(target=self._drain, daemon=True).start()
        for _ in range(60):
            if os.path.exists(sock):
                break
            time.sleep(0.1)
        else:
            raise RuntimeError(f"{label}: socket never appeared")
        self.s = socket.socket(socket.AF_UNIX)
        self.s.connect(sock)
        self.s.settimeout(2.0)
        self.buf = b""
        self.next_id = 0

    def _drain(self):
        for line in self.proc.stderr:
            text = line.rstrip()
            if "level=ERROR" in text or "level=WARN" in text:
                log(f"  [{self.label}] {text}")

    def _read(self, deadline):
        while True:
            if b"\n" in self.buf:
                line, self.buf = self.buf.split(b"\n", 1)
                if line.strip():
                    return json.loads(line)
                continue
            if time.time() > deadline:
                return None
            try:
                chunk = self.s.recv(65536)
            except (socket.timeout, TimeoutError):
                continue
            if not chunk:
                return None
            self.buf += chunk

    def call(self, method, params, timeout=20):
        self.next_id += 1
        rid = self.next_id
        self.s.sendall((json.dumps({"jsonrpc": "2.0", "id": rid,
                                    "method": method, "params": params}) + "\n").encode())
        deadline = time.time() + timeout
        while True:
            msg = self._read(deadline)
            if msg is None:
                raise RuntimeError(f"{self.label}: timeout on {method}")
            if msg.get("id") == rid:
                return msg

    def _events(self, thread_id, deadline):
        while True:
            msg = self._read(deadline)
            if msg is None:
                raise RuntimeError(f"{self.label}: timeout waiting for an agent event")
            if msg.get("method") != "agent.event":
                continue
            p = msg.get("params", {})
            if p.get("threadId") == thread_id:
                yield p.get("event", {})

    def wait_for_first_assistant_text(self, thread_id, timeout=60):
        """Block until the agent emits its first chunk of assistant text —
        i.e. the turn is genuinely in flight and worth interrupting."""
        deadline = time.time() + timeout
        for ev in self._events(thread_id, deadline):
            if ev.get("type") == "assistant":
                for b in ev.get("message", {}).get("content", []):
                    if b.get("type") == "text" and b.get("text", "").strip():
                        return b["text"]
            elif ev.get("type") == "_lifecycle" and ev.get("phase") == "error":
                raise RuntimeError(f"{self.label}: agent error: {ev.get('detail')}")

    def wait_for_lifecycle(self, thread_id, phases, timeout=40):
        deadline = time.time() + timeout
        for ev in self._events(thread_id, deadline):
            if ev.get("type") == "_lifecycle" and ev.get("phase") in phases:
                return ev.get("phase"), ev.get("detail", "")

    def wait_for_result(self, thread_id, timeout=200):
        deadline = time.time() + timeout
        texts = []
        for ev in self._events(thread_id, deadline):
            t = ev.get("type")
            if t == "assistant":
                for b in ev.get("message", {}).get("content", []):
                    if b.get("type") == "text" and b.get("text"):
                        texts.append(b["text"])
            elif t == "result":
                return str(ev.get("result", "")), texts
            elif t == "_lifecycle" and ev.get("phase") == "error":
                raise RuntimeError(f"{self.label}: agent error: {ev.get('detail')}")

    def stop(self):
        self.proc.terminate()
        try:
            self.proc.wait(timeout=8)
        except subprocess.TimeoutExpired:
            self.proc.kill()


def make_repo():
    ws = tempfile.mkdtemp(prefix="ak-interrupt-ws-")
    for cmd in (["git", "init", "-q"],
                ["git", "config", "user.email", "interrupt@agentkate"],
                ["git", "config", "user.name", "Agent Kate Interrupt"]):
        subprocess.run(cmd, cwd=ws, check=True)
    with open(os.path.join(ws, "README.md"), "w") as fh:
        fh.write("interrupt smoke test\n")
    subprocess.run(["git", "add", "."], cwd=ws, check=True)
    subprocess.run(["git", "commit", "-q", "-m", "init"], cwd=ws, check=True)
    return ws


def main():
    if not os.path.exists(AKCORE):
        sys.exit("build akcore first: scripts/build.sh")
    if not shutil.which("claude"):
        sys.exit("the `claude` CLI is required")

    data_dir = tempfile.mkdtemp(prefix="ak-interrupt-data-")
    env = dict(os.environ, XDG_DATA_HOME=data_dir)
    workspace = make_repo()
    log(f"workspace: {workspace}")

    checks = {}
    c = None
    try:
        c = Core(SOCK, env, "core")
        if c.call("handshake", {})["result"].get("name") != "akcore":
            sys.exit("handshake failed")

        # A long, slow turn so there's a real in-flight response to interrupt.
        start = c.call("agent.start", {
            "workspacePath": workspace,
            "prompt": (f"First, remember this secret code for later: {SECRET}. "
                       f"Then, without using any tools, count slowly from 1 to "
                       f"500, writing one number per line followed by a short "
                       f"sentence about it. Do not stop early."),
            "model": "claude-haiku-4-5-20251001",
        })
        if start.get("error"):
            sys.exit(f"agent.start failed: {start['error']}")
        thread_id = start["result"]["threadId"]
        session_id = start["result"].get("sessionId", "")
        log(f"thread {thread_id}  session {session_id}")

        # Wait until it's genuinely generating, then interrupt.
        first = c.wait_for_first_assistant_text(thread_id)
        log(f"agent is generating (first text: {first[:60]!r}…) — interrupting now")

        t0 = time.time()
        resp = c.call("agent.interrupt", {"threadId": thread_id})
        checks["agent.interrupt accepted"] = not resp.get("error")

        phase, detail = c.wait_for_lifecycle(thread_id, {"interrupted", "exited", "error"},
                                             timeout=15)
        dt = time.time() - t0
        log(f"lifecycle after interrupt: {phase} — {detail}  ({dt:.2f}s)")
        checks["turn halted as 'interrupted'"] = phase == "interrupted"
        checks["halted promptly (< 8s)"] = dt < 8.0

        rec = next((t for t in c.call("session.listThreads", {})["result"]["threads"]
                    if t["threadId"] == thread_id), None)
        checks["thread left dormant (resumable)"] = bool(rec and rec.get("status") == "dormant")

        # --- resume and confirm the session survived ------------------------
        resume = c.call("agent.resume", {"threadId": thread_id})
        checks["agent.resume accepted"] = not resume.get("error")
        phase, detail = c.wait_for_lifecycle(thread_id, {"resumed", "error"})
        log(f"resume lifecycle: {phase} — {detail}")
        if phase != "resumed":
            sys.exit(f"resume did not start the thread: {detail}")

        c.call("agent.send", {
            "threadId": thread_id,
            "text": ("Stop counting. What was the secret code I told you at the "
                     "start? Reply with only the digits, nothing else."),
        })
        result2, texts2 = c.wait_for_result(thread_id)
        haystack = result2 + " " + " ".join(texts2)
        log(f"resumed turn result: {result2!r}")
        log(f"resumed agent said: {' '.join(texts2)[:120]!r}")
        checks["resumed agent recalls the secret"] = SECRET in haystack
    finally:
        if c:
            c.stop()
        shutil.rmtree(data_dir, ignore_errors=True)
        shutil.rmtree(workspace, ignore_errors=True)

    log("\n--- verdict ---")
    for name, ok in checks.items():
        log(f"  [{'PASS' if ok else 'FAIL'}] {name}")
    if checks and all(checks.values()):
        log("\nINTERRUPT SMOKE TEST PASSED")
        sys.exit(0)
    log("\nINTERRUPT SMOKE TEST FAILED")
    sys.exit(1)


if __name__ == "__main__":
    main()
