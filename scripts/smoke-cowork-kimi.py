#!/usr/bin/env python3
"""Smoke test for switching Cowork on mid-session on the KIMI backend.

Kimi lists a server's tools once, at session/new, and ignores the MCP
notifications/tools/list_changed notification (probed on 0.30.0) — so the live
reveal claude gets is not available here. Enabling Cowork on a running kimi
thread therefore RE-ATTACHES it: the core waits for the current turn, stops the
process and resumes the same kimi session with the desktop bridge now
advertising its tools. session/resume keeps the conversation, so nothing is lost.

This verifies exactly that:

  1. start a kimi thread, teach it a codeword       -> running, Cowork off
  2. cowork.setEnabled                              -> applied == "reattach"
  3. the thread comes back (lifecycle: resumed)
  4. it still remembers the codeword                -> context survived
  5. it reports having mcp__cowork__* tools         -> the tools really arrived

Requires: a built ./build/akcore and an authenticated `kimi` CLI.
Run unbuffered:  python3 -u scripts/smoke-cowork-kimi.py
"""
import json
import os
import socket
import subprocess
import sys
import tempfile
import threading
import time

ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
AKCORE = os.path.join(ROOT, "build", "akcore")
SOCK = os.path.join(tempfile.gettempdir(), "ak-smoke-cowork-kimi.sock")
DEADLINE_SECS = 420
CODEWORD = "PLUM-4271"


def log(*a):
    print(*a, flush=True)


def events_of(params):
    if "event" in params:
        return [params["event"]]
    return params.get("events", [])


def texts_of(ev):
    out = []
    if ev.get("type") == "assistant":
        for b in ev.get("message", {}).get("content", []):
            if b.get("type") == "text":
                out.append(b["text"])
    return out


def main():
    if not os.path.exists(AKCORE):
        sys.exit("build akcore first: scripts/ak build")
    try:
        os.unlink(SOCK)
    except FileNotFoundError:
        pass

    env = dict(os.environ, XDG_DATA_HOME=tempfile.mkdtemp(prefix="ak-smoke-ckimi-data-"))
    core = subprocess.Popen([AKCORE, "--socket", SOCK], env=env,
                            stderr=subprocess.PIPE, text=True)
    threading.Thread(target=lambda: [log("  [core]", ln.rstrip()) for ln in core.stderr],
                     daemon=True).start()

    for _ in range(50):
        if os.path.exists(SOCK):
            break
        time.sleep(0.1)
    else:
        core.kill()
        sys.exit("akcore socket never appeared")

    s = socket.socket(socket.AF_UNIX)
    s.connect(SOCK)
    s.settimeout(5.0)

    workspace = tempfile.mkdtemp(prefix="ak-smoke-ckimi-ws-")
    for cmd in (["git", "init", "-q"],
                ["git", "config", "user.email", "smoke@agentkate"],
                ["git", "config", "user.name", "Agent Kate Smoke"]):
        subprocess.run(cmd, cwd=workspace, check=True)
    with open(os.path.join(workspace, "README.md"), "w") as fh:
        fh.write("smoke test repo\n")
    subprocess.run(["git", "add", "."], cwd=workspace, check=True)
    subprocess.run(["git", "commit", "-q", "-m", "init"], cwd=workspace, check=True)
    log(f"workspace: {workspace} (git repo)")

    next_id = [0]

    def call(method, params):
        next_id[0] += 1
        s.sendall((json.dumps({"jsonrpc": "2.0", "id": next_id[0],
                               "method": method, "params": params}) + "\n").encode())
        return next_id[0]

    call("handshake", {})  # cowork.setEnabled is UI-only
    start_id = call("agent.start", {
        "workspacePath": workspace,
        "prompt": f"Remember this codeword: {CODEWORD}. Reply with just the word OK.",
        "backend": "kimi",
        "permissionMode": "default",
    })

    thread_id = None
    enable_reply = None
    setenabled_id = None
    phase = "first-turn"
    saw_resumed = False
    recall_text = ""

    buf = b""
    deadline = time.time() + DEADLINE_SECS
    last_event = time.time()

    try:
        while time.time() < deadline:
            try:
                chunk = s.recv(65536)
            except (socket.timeout, TimeoutError):
                log(f"  ... waiting ({int(time.time() - last_event)}s since last event)")
                continue
            if not chunk:
                log("core connection closed")
                break
            buf += chunk
            while b"\n" in buf:
                line, buf = buf.split(b"\n", 1)
                if not line.strip():
                    continue
                try:
                    msg = json.loads(line)
                except json.JSONDecodeError:
                    continue
                last_event = time.time()

                if msg.get("id") == start_id:
                    if msg.get("error"):
                        sys.exit(f"agent.start failed: {msg['error']}")
                    thread_id = msg.get("result", {}).get("threadId")
                    log(f"thread started: {thread_id}")
                    continue

                if setenabled_id is not None and msg.get("id") == setenabled_id:
                    if msg.get("error"):
                        sys.exit(f"cowork.setEnabled failed: {msg['error']}")
                    enable_reply = msg.get("result", {})
                    log(f"  cowork.setEnabled -> {enable_reply}")
                    continue

                if msg.get("method") == "permission.requested":
                    call("permission.respond",
                         {"requestId": msg["params"]["requestId"], "allow": True})
                    continue

                if msg.get("method") != "agent.event":
                    continue

                for ev in events_of(msg["params"]):
                    for t in texts_of(ev):
                        if t.strip():
                            log(f"  assistant: {' '.join(t.split())[:160]}")
                        if phase == "recalling":
                            recall_text += t

                    if ev.get("type") == "_lifecycle":
                        log(f"  lifecycle: {ev.get('phase')} - {ev.get('detail')}")
                        if ev.get("phase") == "resumed" and phase == "reattaching":
                            saw_resumed = True
                            phase = "recalling"
                            # Ask the re-attached session for BOTH facts at once:
                            # its memory (context survived) and its tool list (the
                            # desktop bridge arrived).
                            call("agent.send", {
                                "threadId": thread_id,
                                "text": "Answer in one line, no tool calls: the codeword I "
                                        "gave you earlier, then YES or NO for whether you "
                                        "now have MCP tools whose names start with "
                                        "mcp__cowork__ (for example "
                                        "mcp__cowork__desktop_list_windows).",
                            })
                        if ev.get("phase") == "exited" and phase == "done":
                            raise StopIteration
                    elif ev.get("type") == "result":
                        if phase == "first-turn":
                            phase = "reattaching"
                            log("  turn 1 done — switching Cowork ON (expect a re-attach)")
                            setenabled_id = call("cowork.setEnabled",
                                                 {"threadId": thread_id, "enabled": True})
                        elif phase == "recalling":
                            phase = "done"
                            log("  recall turn done — stopping agent")
                            call("agent.stop", {"threadId": thread_id})
    except StopIteration:
        pass
    finally:
        s.close()
        core.terminate()
        try:
            core.wait(timeout=4)
        except subprocess.TimeoutExpired:
            core.kill()

    answer = " ".join(recall_text.split())
    log("\n--- verdict ---")
    checks = {
        "kimi thread started": thread_id is not None,
        "cowork.setEnabled chose the re-attach path":
            (enable_reply or {}).get("applied") == "reattach",
        "the thread came back up (resumed)": saw_resumed,
        "the conversation survived the re-attach": CODEWORD in answer.upper(),
        "the re-attached session has the desktop tools": "YES" in answer.upper(),
    }
    for name, passed in checks.items():
        log(f"  [{'PASS' if passed else 'FAIL'}] {name}")
    log(f"  recall answer: {answer[:300]!r}")

    ok = all(checks.values())
    log("\nSMOKE TEST PASSED" if ok else "\nSMOKE TEST FAILED")
    sys.exit(0 if ok else 1)


if __name__ == "__main__":
    main()
