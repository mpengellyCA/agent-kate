#!/usr/bin/env python3
"""End-to-end smoke test for interrupt-keeps-session-hot (plan 13 phase 2).

Two scenarios against a real akcore + authenticated `claude`:

SCENARIO A — in-band interrupt keeps the process alive:
  Starts an agent on a long, slow-to-produce turn, waits until it is *actively
  generating*, then fires agent.interrupt. A passing run proves:
    * the in-flight turn halts fast (a `turn_aborted` lifecycle arrives within a
      couple seconds — not after the whole count finishes),
    * the thread is left RUNNING (not dormant — the CLI stays resident), and
    * a plain agent.send (no resume) into the same process still recalls a secret
      the agent was told before the interrupt — i.e. no resume cost.

SCENARIO B — escalation backstop for a hung tool:
  Starts an agent that runs `sleep 600` in bash (a tool the CLI can't cancel
  in-band), interrupts it, and proves:
    * the escalation backstop kills the process (an `interrupted` lifecycle
      arrives) and the thread goes dormant, and
    * the next agent.send auto-resumes and answers with prior context.

SCENARIO C — stop & close on a dormant agent archives (does not error):
  Starts an agent, lets its turn finish, agent.stop's it to dormant (the CLI
  exits and the supervisor forgets the thread), then calls agent.stopClose and
  proves it archives the thread out of the live roster instead of erroring with
  "unknown thread" — the regression guard for the dormant stop-close fix.

Exercises the real RPC path: agent.interrupt -> Supervisor.Interrupt ->
in-band frame (stdin kept open) -> pumpStdout sees the aborted result ->
`turn_aborted`; and the signal-escalation fallback -> reap() "interrupted".

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
            if p.get("threadId") != thread_id:
                continue
            # Events arrive coalesced as an ordered batch under "events".
            for ev in p.get("events", []):
                yield ev

    def wait_until_generating(self, thread_id, timeout=90):
        """Block until the agent's turn is genuinely in flight and worth
        interrupting — the first assistant text chunk, or (if the model is still
        thinking) the first assistant message of any kind. Returns a short label
        describing what we saw."""
        deadline = time.time() + timeout
        for ev in self._events(thread_id, deadline):
            t = ev.get("type")
            if t == "assistant":
                for b in ev.get("message", {}).get("content", []):
                    if b.get("type") == "text" and b.get("text", "").strip():
                        return "text:" + b["text"][:40]
                    if b.get("type") == "tool_use":
                        return "tool_use:" + str(b.get("name", ""))
                return "assistant"
            elif t == "_lifecycle" and ev.get("phase") == "error":
                raise RuntimeError(f"{self.label}: agent error: {ev.get('detail')}")

    def wait_for_lifecycle(self, thread_id, phases, timeout=40):
        deadline = time.time() + timeout
        try:
            for ev in self._events(thread_id, deadline):
                if ev.get("type") == "_lifecycle" and ev.get("phase") in phases:
                    return ev.get("phase"), ev.get("detail", "")
        except RuntimeError:
            pass
        return None, None

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


HAIKU = "claude-haiku-4-5-20251001"


def scenario_inband(c, workspace, checks):
    """Interrupt mid-generation keeps the process alive; a plain send (no
    resume) into the same process recalls the pre-interrupt secret."""
    log("\n=== SCENARIO A: in-band interrupt keeps the session hot ===")
    start = c.call("agent.start", {
        "workspacePath": workspace,
        "prompt": (f"Our project's build reference number is {SECRET}; please keep "
                   f"it in mind. Now, without using any tools, count slowly from 1 "
                   f"to 500, writing one number per line followed by a short "
                   f"sentence about it. Do not stop early."),
        "model": HAIKU,
    })
    if start.get("error"):
        sys.exit(f"agent.start failed: {start['error']}")
    thread_id = start["result"]["threadId"]
    log(f"thread {thread_id}  session {start['result'].get('sessionId','')}")

    what = c.wait_until_generating(thread_id)
    log(f"agent is generating ({what!r}) — interrupting now")

    t0 = time.time()
    resp = c.call("agent.interrupt", {"threadId": thread_id})
    checks["A: agent.interrupt accepted"] = not resp.get("error")

    # In-band: expect turn_aborted (process stays alive), NOT interrupted.
    phase, detail = c.wait_for_lifecycle(
        thread_id, {"turn_aborted", "interrupted", "exited", "error"}, timeout=15)
    dt = time.time() - t0
    log(f"lifecycle after interrupt: {phase} — {detail}  ({dt:.2f}s)")
    checks["A: turn halted as 'turn_aborted'"] = phase == "turn_aborted"
    checks["A: halted promptly (< 8s)"] = phase == "turn_aborted" and dt < 8.0

    rec = next((t for t in c.call("session.listThreads", {})["result"]["threads"]
                if t["threadId"] == thread_id), None)
    checks["A: thread still running (process alive)"] = bool(
        rec and rec.get("status") == "running")

    # No resume — send straight into the SAME process.
    c.call("agent.send", {
        "threadId": thread_id,
        "text": ("Never mind the counting. What was the build reference number I "
                 "gave you at the start? Reply with only the digits."),
    })
    result2, texts2 = c.wait_for_result(thread_id)
    haystack = result2 + " " + " ".join(texts2)
    log(f"follow-up (no resume) result: {result2!r}")
    checks["A: follow-up recalls the secret (no resume)"] = SECRET in haystack

    # Terminal close: stopClose must archive it out of the live roster.
    close = c.call("agent.stopClose", {"threadId": thread_id}, timeout=90)
    checks["A: agent.stopClose accepted"] = not close.get("error")
    still = next((t for t in c.call("session.listThreads", {})["result"]["threads"]
                  if t["threadId"] == thread_id), None)
    checks["A: stop & close clears the roster entry"] = still is None
    arch = c.call("cleanup.listArchived", {})["result"].get("archived", [])
    checks["A: stop & close archives (restorable)"] = any(
        a.get("threadId") == thread_id for a in arch)


def scenario_blocking_tool(c, workspace, checks):
    """Interrupt during a blocking foreground bash tool (`tail -f`, which the CLI
    can't return from on its own) recovers and the next send continues with
    context. Empirically claude 2.1.185 cancels even a running tool IN-BAND — the
    turn aborts (turn_aborted, process stays hot) and a plain send continues on
    the same session with no resume. If the CLI ever fails to ack in-band, the
    supervisor's signal-escalation backstop kills the process (interrupted →
    dormant) and the next send auto-resumes with context. This scenario asserts
    the recover-and-continue contract for BOTH outcomes."""
    log("\n=== SCENARIO B: interrupt a blocking bash tool, then continue ===")
    start = c.call("agent.start", {
        "workspacePath": workspace,
        "prompt": (f"Our build reference number is {SECRET}. Please set up a file "
                   f"watcher: run this bash command in the foreground to watch "
                   f"README.md and block until I edit it: tail -f README.md"),
        "model": HAIKU,
        # bypassPermissions so the agent actually runs the blocking bash call
        # without a gate. `tail -f` blocks in the foreground indefinitely; a
        # legitimate task framing reliably gets the model to run it (a bare
        # `sleep 600` is refused/blocked by the Bash tool's own guard).
        "permissionMode": "bypassPermissions",
    })
    if start.get("error"):
        sys.exit(f"agent.start (B) failed: {start['error']}")
    thread_id = start["result"]["threadId"]
    log(f"thread {thread_id}")

    # Wait for the bash tool_use to actually start, so the process is genuinely
    # stuck in the blocking `tail -f` when we interrupt.
    deadline = time.time() + 90
    started = False
    try:
        for ev in c._events(thread_id, deadline):
            t = ev.get("type")
            if t == "assistant":
                for b in ev.get("message", {}).get("content", []):
                    if b.get("type") == "tool_use":
                        started = True
                        break
                    if b.get("type") == "text" and b.get("text", "").strip():
                        log(f"  B assistant text: {b['text'][:70]!r}")
            if started:
                break
    except RuntimeError:
        pass
    checks["B: blocking bash tool started"] = started
    if not started:
        log("bash tool never started — cannot exercise the blocking-tool path")
        return
    # Let the tool genuinely block in the foreground before interrupting.
    time.sleep(1.0)
    log("blocking tool in flight — interrupting now")

    t0 = time.time()
    resp = c.call("agent.interrupt", {"threadId": thread_id})
    checks["B: agent.interrupt accepted"] = not resp.get("error")

    # Either outcome recovers: turn_aborted (in-band cancel, stays alive) or
    # interrupted (escalation, goes dormant). Both must halt promptly.
    phase, detail = c.wait_for_lifecycle(
        thread_id, {"turn_aborted", "interrupted", "exited", "error"}, timeout=20)
    dt = time.time() - t0
    log(f"lifecycle after interrupt: {phase} — {detail}  ({dt:.2f}s)")
    checks["B: blocking tool halted (aborted or escalated)"] = phase in (
        "turn_aborted", "interrupted")
    checks["B: halted promptly (< 10s)"] = phase in (
        "turn_aborted", "interrupted") and dt < 10.0

    escalated = phase == "interrupted"
    rec = next((t for t in c.call("session.listThreads", {})["result"]["threads"]
                if t["threadId"] == thread_id), None)
    if escalated:
        # Escalation path: dormant, then auto-resume-on-send brings it back.
        log("escalated to signals — thread went dormant; resuming")
        checks["B: escalated thread is dormant"] = bool(
            rec and rec.get("status") == "dormant")
        resume = c.call("agent.resume", {"threadId": thread_id})
        checks["B: agent.resume accepted"] = not resume.get("error")
        rphase, _ = c.wait_for_lifecycle(thread_id, {"resumed", "error"})
        if rphase != "resumed":
            checks["B: resumed"] = False
            return
        checks["B: resumed"] = True
    else:
        # In-band cancel: the process stays hot — no resume needed.
        log("cancelled in-band — process stays resident; sending on same session")
        checks["B: in-band cancel keeps process alive"] = bool(
            rec and rec.get("status") == "running")

    # In both cases a follow-up must continue with the pre-interrupt context.
    c.call("agent.send", {
        "threadId": thread_id,
        "text": ("Stop watching the file. What was the build reference number I "
                 "gave you at the start? Reply with only the digits."),
    })
    result3, texts3 = c.wait_for_result(thread_id)
    haystack = result3 + " " + " ".join(texts3)
    log(f"follow-up result: {result3!r}")
    checks["B: follow-up continues with context"] = SECRET in haystack


def scenario_dormant_stopclose(c, workspace, checks):
    """Stop & close on a DORMANT agent must archive, not error. A dormant thread
    has already been reaped, so the supervisor no longer tracks it and Stop()
    reports "unknown thread" — the normal dormant state. Regression guard for the
    core lifecycle fix: agent.stopClose must ignore that and archive anyway."""
    log("\n=== SCENARIO C: stop & close on a dormant agent archives ===")
    start = c.call("agent.start", {
        "workspacePath": workspace,
        "prompt": "Reply with only the word: ready.",
        "model": HAIKU,
    })
    if start.get("error"):
        sys.exit(f"agent.start (C) failed: {start['error']}")
    thread_id = start["result"]["threadId"]
    log(f"thread {thread_id}")

    # Let the opening turn complete so there's a real session on disk.
    c.wait_for_result(thread_id)

    # agent.stop closes stdin; the CLI exits and reap() marks the thread dormant
    # and drops it from the supervisor's tracking map.
    stop = c.call("agent.stop", {"threadId": thread_id})
    checks["C: agent.stop accepted"] = not stop.get("error")
    phase, _ = c.wait_for_lifecycle(thread_id, {"exited", "interrupted"}, timeout=20)
    log(f"lifecycle after stop: {phase}")

    # Poll until the record actually reads dormant (reap → UpdateQuiet is async).
    dormant = False
    for _ in range(30):
        rec = next((t for t in c.call("session.listThreads", {})["result"]["threads"]
                    if t["threadId"] == thread_id), None)
        if rec and rec.get("status") == "dormant":
            dormant = True
            break
        time.sleep(0.2)
    checks["C: thread is dormant before stop & close"] = dormant

    # The fix under test: stopClose on the dormant thread must NOT error even
    # though the supervisor no longer tracks it.
    close = c.call("agent.stopClose", {"threadId": thread_id}, timeout=90)
    checks["C: agent.stopClose on dormant accepted (no error)"] = not close.get("error")
    if close.get("error"):
        log(f"  stopClose error: {close['error']}")

    still = next((t for t in c.call("session.listThreads", {})["result"]["threads"]
                  if t["threadId"] == thread_id), None)
    checks["C: dormant stop & close clears the roster entry"] = still is None
    arch = c.call("cleanup.listArchived", {})["result"].get("archived", [])
    checks["C: dormant stop & close archives (restorable)"] = any(
        a.get("threadId") == thread_id for a in arch)


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
        scenario_inband(c, workspace, checks)
        scenario_blocking_tool(c, workspace, checks)
        scenario_dormant_stopclose(c, workspace, checks)
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
