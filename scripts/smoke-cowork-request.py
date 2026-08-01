#!/usr/bin/env python3
"""Smoke test for an AGENT asking for desktop access mid-session.

This is the agent-facing half of the mid-session Cowork story: a thread that
finds it needs the desktop calls the `enable_cowork` MCP tool, the HUMAN
approves (an agent can never grant this to itself), and the desktop tools become
usable in that same session — plus the OS-level permission is requested straight
away instead of at first use.

  1. start a thread with Cowork off; ask it to call enable_cowork
  2. expect a cowork.enableRequested prompt carrying the agent's stated reason
  3. approve it (permission.respond)
  4. expect a cowork.portalRequest of kind "preflight"  -> OS permission asked
     for at enable time, which is the point (answered "no desktop" here)
  5. expect the desktop tools in the very next turn

Requires: a built ./build/akcore and an authenticated `claude` CLI.
Run unbuffered:  python3 -u scripts/smoke-cowork-request.py
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
SOCK = os.path.join(tempfile.gettempdir(), "ak-smoke-cowork-req.sock")
DEADLINE_SECS = 300
REASON_HINT = "drive a browser"


def log(*a):
    print(*a, flush=True)


def events_of(params):
    if "event" in params:
        return [params["event"]]
    return params.get("events", [])


def main():
    if not os.path.exists(AKCORE):
        sys.exit("build akcore first: scripts/ak build")
    try:
        os.unlink(SOCK)
    except FileNotFoundError:
        pass

    env = dict(os.environ, XDG_DATA_HOME=tempfile.mkdtemp(prefix="ak-smoke-creq-data-"))
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

    workspace = tempfile.mkdtemp(prefix="ak-smoke-creq-ws-")
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

    call("handshake", {})  # be the primary UI: consent + portal requests land here
    start_id = call("agent.start", {
        "workspacePath": workspace,
        "prompt": (f"Call the cooperation MCP tool enable_cowork with reason "
                   f"'{REASON_HINT}'. Then reply with the single word ASKED."),
        "model": "claude-haiku-4-5-20251001",
    })

    thread_id = None
    phase = "asking"
    saw_enable_prompt = False
    prompt_reason = ""
    saw_preflight = False
    tools_after = None
    send_id = None

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

                method = msg.get("method")

                if method == "cowork.enableRequested":
                    p = msg["params"]
                    saw_enable_prompt = True
                    prompt_reason = p.get("reason", "")
                    log(f"  >> agent asks for desktop access: reason={prompt_reason!r} "
                        f"self={p.get('self')} — approving")
                    call("permission.respond", {"requestId": p["requestId"], "allow": True})
                    continue

                if method == "cowork.portalRequest":
                    p = msg["params"]
                    log(f"  >> portal request: kind={p.get('kind')}")
                    if p.get("kind") == "preflight":
                        saw_preflight = True
                    # No desktop in a smoke run — answer honestly so the core's
                    # round-trip completes instead of timing out.
                    call("cowork.portalResult", {
                        "corrId": p.get("corrId"), "kind": p.get("kind"),
                        "ok": False, "error": "no desktop session in the smoke test",
                    })
                    continue

                if method == "permission.requested":
                    call("permission.respond",
                         {"requestId": msg["params"]["requestId"], "allow": True})
                    continue

                if method != "agent.event":
                    continue

                for ev in events_of(msg["params"]):
                    if ev.get("type") == "system" and ev.get("subtype") == "init":
                        found = sorted(t for t in ev.get("tools", [])
                                       if t.startswith("mcp__cowork"))
                        log(f"  init ({phase}): cowork tools={len(found)}")
                        if phase == "after":
                            tools_after = found
                    elif ev.get("type") == "assistant":
                        for b in ev.get("message", {}).get("content", []):
                            if b.get("type") == "tool_use":
                                log(f"  assistant -> tool_use: {b.get('name')}")
                            elif b.get("type") == "text" and b["text"].strip():
                                log(f"  assistant: {' '.join(b['text'].split())[:160]}")
                    elif ev.get("type") == "result":
                        if phase == "asking":
                            phase = "after"
                            send_id = call("agent.send", {
                                "threadId": thread_id,
                                "text": "Reply with the single word NEXT and nothing else.",
                            })
                        elif phase == "after" and tools_after is not None:
                            log("  done — stopping agent")
                            call("agent.stop", {"threadId": thread_id})
                    elif ev.get("type") == "_lifecycle":
                        log(f"  lifecycle: {ev.get('phase')} - {ev.get('detail')}")
                        if ev.get("phase") == "exited":
                            raise StopIteration
    except StopIteration:
        pass
    finally:
        s.close()
        core.terminate()
        try:
            core.wait(timeout=4)
        except subprocess.TimeoutExpired:
            core.kill()

    log("\n--- verdict ---")
    checks = {
        "agent thread started": thread_id is not None,
        "the agent's enable_cowork reached the human as a prompt": saw_enable_prompt,
        "the prompt carried the agent's stated reason":
            REASON_HINT.split()[0] in prompt_reason.lower(),
        "the OS permission was requested at enable time (preflight)": saw_preflight,
        "desktop tools present on the next turn": bool(tools_after),
    }
    for name, passed in checks.items():
        log(f"  [{'PASS' if passed else 'FAIL'}] {name}")
    log(f"  cowork tools after: {len(tools_after or [])}")

    ok = all(checks.values())
    log("\nSMOKE TEST PASSED" if ok else "\nSMOKE TEST FAILED")
    sys.exit(0 if ok else 1)


if __name__ == "__main__":
    main()
