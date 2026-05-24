#!/usr/bin/env python3
"""Smoke test for the Claude Code session browser (resume increment 3).

Plants a synthetic Claude Code transcript under a throwaway CLAUDE_CONFIG_DIR,
then drives the core's session.browse / session.attach IPC to prove a past
conversation — one Agent Kate never started — can be discovered and attached as
a resumable thread, with de-duplication.

Requires a built ./build/akcore. No `claude` CLI or network needed.
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
SOCK = os.path.join(tempfile.gettempdir(), "ak-browse.sock")
SESSION_ID = "aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee"
PROJECT = "/tmp/ak-browse-fake-project"
TITLE = "Refactor the parser"


def log(*a):
    print(*a, flush=True)


class Core:
    def __init__(self, env):
        try:
            os.unlink(SOCK)
        except FileNotFoundError:
            pass
        self.proc = subprocess.Popen([AKCORE, "--socket", SOCK], env=env,
                                     stderr=subprocess.PIPE, text=True)
        threading.Thread(target=self._drain, daemon=True).start()
        for _ in range(60):
            if os.path.exists(SOCK):
                break
            time.sleep(0.1)
        else:
            raise RuntimeError("akcore socket never appeared")
        self.s = socket.socket(socket.AF_UNIX)
        self.s.connect(SOCK)
        self.s.settimeout(2.0)
        self.buf = b""
        self.next_id = 0

    def _drain(self):
        for line in self.proc.stderr:
            if "level=ERROR" in line:
                log("  [core]", line.rstrip())

    def call(self, method, params, timeout=15):
        self.next_id += 1
        rid = self.next_id
        self.s.sendall((json.dumps({"jsonrpc": "2.0", "id": rid,
                                    "method": method, "params": params}) + "\n").encode())
        deadline = time.time() + timeout
        while time.time() < deadline:
            while b"\n" in self.buf:
                line, self.buf = self.buf.split(b"\n", 1)
                if not line.strip():
                    continue
                msg = json.loads(line)
                if msg.get("id") == rid:
                    return msg
            try:
                chunk = self.s.recv(65536)
            except (socket.timeout, TimeoutError):
                continue
            if not chunk:
                raise RuntimeError("akcore closed the connection")
            self.buf += chunk
        raise RuntimeError(f"timeout on {method}")

    def stop(self):
        self.proc.terminate()
        try:
            self.proc.wait(timeout=8)
        except subprocess.TimeoutExpired:
            self.proc.kill()


def main():
    if not os.path.exists(AKCORE):
        sys.exit("build akcore first: scripts/build.sh")

    cfg = tempfile.mkdtemp(prefix="ak-browse-cfg-")
    data = tempfile.mkdtemp(prefix="ak-browse-data-")
    # Plant a synthetic Claude Code transcript under CLAUDE_CONFIG_DIR/projects.
    proj_dir = os.path.join(cfg, "projects", "-tmp-ak-browse-fake-project")
    os.makedirs(proj_dir, exist_ok=True)
    with open(os.path.join(proj_dir, SESSION_ID + ".jsonl"), "w") as fh:
        fh.write(json.dumps({"type": "user", "cwd": PROJECT,
                             "message": {"role": "user", "content": TITLE}}) + "\n")
        fh.write(json.dumps({"type": "assistant", "cwd": PROJECT,
                             "message": {"role": "assistant",
                                         "content": [{"type": "text", "text": "Done"}]}})
                 + "\n")

    env = dict(os.environ, CLAUDE_CONFIG_DIR=cfg, XDG_DATA_HOME=data)
    checks = {}
    core = None
    try:
        core = Core(env)
        if core.call("handshake", {})["result"].get("name") != "akcore":
            sys.exit("handshake failed")

        browsed = core.call("session.browse", {})["result"]["sessions"]
        ours = next((s for s in browsed if s["sessionId"] == SESSION_ID), None)
        log(f"session.browse -> {len(browsed)} session(s); ours found: {ours is not None}")
        checks["session discovered"] = ours is not None
        checks["title read from transcript"] = bool(ours and ours.get("title") == TITLE)
        checks["project read from cwd"] = bool(ours and ours.get("project") == PROJECT)
        checks["not yet attached"] = bool(ours and ours.get("attached") is False)

        attach = core.call("session.attach", {"sessionId": SESSION_ID,
                                              "project": PROJECT, "title": TITLE})
        if attach.get("error"):
            sys.exit(f"session.attach failed: {attach['error']}")
        thread_id = attach["result"]["threadId"]
        log(f"attached as thread {thread_id}")
        checks["attach returns a thread id"] = bool(thread_id)

        threads = core.call("session.listThreads", {})["result"]["threads"]
        rec = next((t for t in threads if t["threadId"] == thread_id), None)
        checks["attached thread is tracked"] = rec is not None
        checks["tracked with the right session"] = bool(rec and rec["sessionId"] == SESSION_ID)
        checks["tracked as dormant"] = bool(rec and rec["status"] == "dormant")

        rebrowse = core.call("session.browse", {})["result"]["sessions"]
        ours2 = next((s for s in rebrowse if s["sessionId"] == SESSION_ID), None)
        checks["browse now marks it attached"] = bool(ours2 and ours2.get("attached") is True)

        again = core.call("session.attach", {"sessionId": SESSION_ID,
                                             "project": PROJECT, "title": TITLE})
        checks["re-attach is deduplicated"] = bool(
            again["result"].get("alreadyAttached") is True
            and again["result"].get("threadId") == thread_id)
    finally:
        if core:
            core.stop()
        shutil.rmtree(cfg, ignore_errors=True)
        shutil.rmtree(data, ignore_errors=True)

    log("\n--- verdict ---")
    for name, ok in checks.items():
        log(f"  [{'PASS' if ok else 'FAIL'}] {name}")
    if checks and all(checks.values()):
        log("\nBROWSE SMOKE TEST PASSED")
        sys.exit(0)
    log("\nBROWSE SMOKE TEST FAILED")
    sys.exit(1)


if __name__ == "__main__":
    main()
