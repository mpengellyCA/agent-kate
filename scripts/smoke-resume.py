#!/usr/bin/env python3
"""End-to-end smoke test for session resume (M-resume increment 1).

Starts an agent thread, tells it a secret, then *kills the core entirely* —
simulating an AgentKate restart — starts a fresh core, and resumes the thread
from its persisted Claude Code session. A passing run proves the thread record
survived on disk and that `claude --resume` restored the conversation: the
resumed agent still recalls the secret.

Requires: a built ./build/akcore and an authenticated `claude` CLI.
Run unbuffered for live output:  python3 -u scripts/smoke-resume.py
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
SOCK = os.path.join(tempfile.gettempdir(), "ak-resume.sock")
SECRET = "7321"
TURN_TIMEOUT = 200


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
        """Yield agent.event payloads for thread_id until the deadline."""
        while True:
            msg = self._read(deadline)
            if msg is None:
                raise RuntimeError(f"{self.label}: timeout waiting for an agent event")
            if msg.get("method") != "agent.event":
                continue
            p = msg.get("params", {})
            if p.get("threadId") == thread_id:
                yield p.get("event", {})

    def wait_for_lifecycle(self, thread_id, phases, timeout=40):
        deadline = time.time() + timeout
        for ev in self._events(thread_id, deadline):
            if ev.get("type") == "_lifecycle" and ev.get("phase") in phases:
                return ev.get("phase"), ev.get("detail", "")

    def wait_for_result(self, thread_id, timeout=TURN_TIMEOUT):
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
    ws = tempfile.mkdtemp(prefix="ak-resume-ws-")
    for cmd in (["git", "init", "-q"],
                ["git", "config", "user.email", "resume@agentkate"],
                ["git", "config", "user.name", "AgentKate Resume"]):
        subprocess.run(cmd, cwd=ws, check=True)
    with open(os.path.join(ws, "README.md"), "w") as fh:
        fh.write("resume smoke test\n")
    subprocess.run(["git", "add", "."], cwd=ws, check=True)
    subprocess.run(["git", "commit", "-q", "-m", "init"], cwd=ws, check=True)
    return ws


def main():
    if not os.path.exists(AKCORE):
        sys.exit("build akcore first: scripts/build.sh")
    if not shutil.which("claude"):
        sys.exit("the `claude` CLI is required")

    data_dir = tempfile.mkdtemp(prefix="ak-resume-data-")  # isolated thread store
    env = dict(os.environ, XDG_DATA_HOME=data_dir)
    workspace = make_repo()
    log(f"workspace: {workspace}")

    checks = {}
    c1 = c2 = None
    try:
        # --- core #1: start a thread and tell it a secret --------------------
        c1 = Core(SOCK, env, "core#1")
        if c1.call("handshake", {})["result"].get("name") != "akcore":
            sys.exit("core#1 handshake failed")

        # Pin to Haiku — this test verifies resume/persistence, not model
        # intelligence; Haiku is ~5x cheaper and still remembers a 4-digit
        # code across a restart.
        start = c1.call("agent.start", {
            "workspacePath": workspace,
            "prompt": (f"Remember this secret code for later: {SECRET}. "
                       f"Do not use any tools. Reply with only the word OK."),
            "model": "claude-haiku-4-5-20251001",
        })
        if start.get("error"):
            sys.exit(f"agent.start failed: {start['error']}")
        thread_id = start["result"]["threadId"]
        session_id = start["result"].get("sessionId", "")
        log(f"thread {thread_id}  session {session_id}")
        checks["session id assigned at start"] = bool(session_id)

        result1, _ = c1.wait_for_result(thread_id)
        log(f"turn 1 result: {result1!r}")
        checks["turn 1 completed"] = bool(result1)

        c1.stop()
        c1 = None
        log("core #1 stopped — simulating an AgentKate restart")

        # --- core #2: fresh process, resume the thread -----------------------
        c2 = Core(SOCK, env, "core#2")
        if c2.call("handshake", {})["result"].get("name") != "akcore":
            sys.exit("core#2 handshake failed")

        listed = c2.call("session.listThreads", {})["result"]["threads"]
        rec = next((t for t in listed if t["threadId"] == thread_id), None)
        log(f"session.listThreads -> {len(listed)} thread(s); ours: {rec is not None}")
        checks["thread persisted across restart"] = rec is not None
        checks["session id persisted"] = bool(rec and rec.get("sessionId") == session_id)
        checks["restored as dormant"] = bool(rec and rec.get("status") == "dormant")

        resume = c2.call("agent.resume", {"threadId": thread_id})
        if resume.get("error"):
            sys.exit(f"agent.resume failed: {resume['error']}")
        checks["resume accepted"] = True

        phase, detail = c2.wait_for_lifecycle(thread_id, {"resumed", "error"})
        log(f"resume lifecycle: {phase} — {detail}")
        if phase != "resumed":
            sys.exit(f"resume did not start the thread: {detail}")

        c2.call("agent.send", {
            "threadId": thread_id,
            "text": ("What is the secret code I told you earlier? "
                     "Reply with only the digits, nothing else."),
        })
        result2, texts2 = c2.wait_for_result(thread_id)
        haystack = result2 + " " + " ".join(texts2)
        log(f"turn 2 result: {result2!r}")
        checks["resumed agent recalls the secret"] = SECRET in haystack
    finally:
        if c1:
            c1.stop()
        if c2:
            c2.stop()
        shutil.rmtree(data_dir, ignore_errors=True)
        shutil.rmtree(workspace, ignore_errors=True)

    log("\n--- verdict ---")
    for name, ok in checks.items():
        log(f"  [{'PASS' if ok else 'FAIL'}] {name}")
    if checks and all(checks.values()):
        log("\nRESUME SMOKE TEST PASSED")
        sys.exit(0)
    log("\nRESUME SMOKE TEST FAILED")
    sys.exit(1)


if __name__ == "__main__":
    main()
