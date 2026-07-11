#!/usr/bin/env python3
"""End-to-end smoke test for forking an agent (plan 13 phase 6).

Starts an agent thread on one model, tells it a secret, then FORKS it to a
different model tier via agent.fork. A passing run proves:

  * the fork is a distinct roster thread with its own id and worktree branch,
  * it inherited the conversation (it recalls the secret it was never told
    directly on its own session — --resume <source> --fork-session carried it),
  * the fork runs on the requested different tier, and
  * the ORIGINAL agent is untouched and still answers on its own session.

Both source and fork are pinned to cheap/fast tiers (Haiku ↔ Fable) — the tier
CHANGE is what matters, not model intelligence, so the run stays quick and cheap.

Requires: a built ./build/akcore and an authenticated `claude` CLI.
Run unbuffered for live output:  python3 -u scripts/smoke-fork.py
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
SOCK = os.path.join(tempfile.gettempdir(), "ak-fork.sock")
SECRET = "8492"
SOURCE_MODEL = "haiku"
FORK_MODEL = "fable"
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
        """Yield individual agent.event objects for thread_id until the deadline.

        The core delivers events as a coalesced batch under the `events` key;
        we flatten it and yield each event in order.
        """
        while True:
            msg = self._read(deadline)
            if msg is None:
                raise RuntimeError(f"{self.label}: timeout waiting for an agent event")
            if msg.get("method") != "agent.event":
                continue
            p = msg.get("params", {})
            if p.get("threadId") != thread_id:
                continue
            for ev in p.get("events", []):
                yield ev

    def wait_for_lifecycle(self, thread_id, phases, timeout=60):
        deadline = time.time() + timeout
        for ev in self._events(thread_id, deadline):
            if ev.get("type") == "_lifecycle" and ev.get("phase") in phases:
                return ev.get("phase"), ev.get("detail", "")
        return None, ""

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
        raise RuntimeError(f"{self.label}: no result before timeout")

    def stop(self):
        self.proc.terminate()
        try:
            self.proc.wait(timeout=8)
        except subprocess.TimeoutExpired:
            self.proc.kill()


def make_repo():
    ws = tempfile.mkdtemp(prefix="ak-fork-ws-")
    for cmd in (["git", "init", "-q"],
                ["git", "config", "user.email", "fork@agentkate"],
                ["git", "config", "user.name", "Agent Kate Fork"]):
        subprocess.run(cmd, cwd=ws, check=True)
    with open(os.path.join(ws, "README.md"), "w") as fh:
        fh.write("fork smoke test\n")
    subprocess.run(["git", "add", "."], cwd=ws, check=True)
    subprocess.run(["git", "commit", "-q", "-m", "init"], cwd=ws, check=True)
    return ws


def main():
    if not os.path.exists(AKCORE):
        sys.exit("build akcore first: scripts/build.sh")
    if not shutil.which("claude"):
        sys.exit("the `claude` CLI is required")

    data_dir = tempfile.mkdtemp(prefix="ak-fork-data-")  # isolated thread store
    env = dict(os.environ, XDG_DATA_HOME=data_dir)
    workspace = make_repo()
    log(f"workspace: {workspace}")

    checks = {}
    c = None
    try:
        c = Core(SOCK, env, "core")
        if c.call("handshake", {})["result"].get("name") != "akcore":
            sys.exit("handshake failed")

        # --- source agent: start it and tell it a secret ---------------------
        start = c.call("agent.start", {
            "workspacePath": workspace,
            "prompt": (f"Let's play a memory game. My favourite number is {SECRET}. "
                       f"Please keep it in mind for later. Do not use any tools. "
                       f"Reply with only the word OK."),
            "model": SOURCE_MODEL,
            "isolation": "isolated",
        })
        if start.get("error"):
            sys.exit(f"agent.start failed: {start['error']}")
        src_thread = start["result"]["threadId"]
        src_session = start["result"].get("sessionId", "")
        log(f"source thread {src_thread}  session {src_session}  model {SOURCE_MODEL}")

        r1, _ = c.wait_for_result(src_thread)
        log(f"source turn 1 result: {r1!r}")
        checks["source turn 1 completed"] = bool(r1)

        # --- fork it to a different tier -------------------------------------
        fork = c.call("agent.fork", {
            "threadId": src_thread,
            "model": FORK_MODEL,
            "title": "Fork of secret-keeper",
        })
        if fork.get("error"):
            sys.exit(f"agent.fork failed: {fork['error']}")
        fork_thread = fork["result"]["threadId"]
        log(f"fork thread {fork_thread}  requested model {FORK_MODEL}")
        checks["fork returned a new distinct thread id"] = (
            bool(fork_thread) and fork_thread != src_thread)

        phase, detail = c.wait_for_lifecycle(fork_thread, {"started", "error"})
        log(f"fork lifecycle: {phase} — {detail}")
        checks["fork started"] = phase == "started"
        if phase != "started":
            sys.exit(f"fork did not start: {detail}")

        # The fork must recall the secret it was never told on its own turns —
        # it inherited the context via --resume <source> --fork-session.
        c.call("agent.send", {
            "threadId": fork_thread,
            "text": ("In our memory game, what was my favourite number? "
                     "Reply with only the digits, nothing else."),
        })
        rf, tf = c.wait_for_result(fork_thread)
        hay = rf + " " + " ".join(tf)
        log(f"fork result: {rf!r}")
        checks["fork recalls the inherited secret"] = SECRET in hay

        # --- the fork must be its own thread on its own session/branch -------
        listed = c.call("session.listThreads", {})["result"]["threads"]
        src_rec = next((t for t in listed if t["threadId"] == src_thread), None)
        fork_rec = next((t for t in listed if t["threadId"] == fork_thread), None)
        log(f"threads: source={src_rec is not None} fork={fork_rec is not None}")
        checks["both source and fork persisted"] = bool(src_rec and fork_rec)
        checks["fork runs on the requested different model"] = bool(
            fork_rec and fork_rec.get("model")
            and fork_rec["model"] != (src_rec or {}).get("model"))
        checks["fork has its own new session id"] = bool(
            fork_rec and fork_rec.get("sessionId")
            and fork_rec["sessionId"] != src_session)
        src_branch = (src_rec or {}).get("worktree", {}).get("branch", "")
        fork_branch = (fork_rec or {}).get("worktree", {}).get("branch", "")
        log(f"branches: source={src_branch!r} fork={fork_branch!r}")
        checks["fork has its own worktree branch"] = bool(
            fork_branch and fork_branch != src_branch)

        # --- the original agent is untouched and still answers ---------------
        c.call("agent.send", {
            "threadId": src_thread,
            "text": ("In our memory game, what was my favourite number? "
                     "Reply with only the digits, nothing else."),
        })
        rs, ts = c.wait_for_result(src_thread)
        hay_s = rs + " " + " ".join(ts)
        log(f"source follow-up result: {rs!r}")
        # The original must still answer on its own hot session (proving the fork
        # left it untouched) and recall the number it was given.
        checks["original still answers on its own session"] = bool(rs or ts)
        checks["original still recalls the number"] = SECRET in hay_s
    finally:
        if c:
            c.stop()
        shutil.rmtree(data_dir, ignore_errors=True)
        shutil.rmtree(workspace, ignore_errors=True)

    log("\n--- verdict ---")
    for name, ok in checks.items():
        log(f"  [{'PASS' if ok else 'FAIL'}] {name}")
    if checks and all(checks.values()):
        log("\nFORK SMOKE TEST PASSED")
        sys.exit(0)
    log("\nFORK SMOKE TEST FAILED")
    sys.exit(1)


if __name__ == "__main__":
    main()
