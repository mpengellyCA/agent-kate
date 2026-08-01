#!/usr/bin/env python3
"""End-to-end smoke test for switching Cowork on MID-SESSION.

The bug this guards: a thread that was started (or forked) without desktop access
could be "enabled" afterwards, but its running CLI had been launched without the
Cowork MCP server — so the desktop tools never appeared and the agent sat there
with nothing to call and no OS permission dialog ever raised.

The fix wires the Cowork bridge into every thread and lets it advertise nothing
until the thread opts in. This test proves the visible half of that on a LIVE
claude session:

  1. start a thread with Cowork OFF          -> no mcp__cowork__* tools
  2. cowork.setEnabled                        -> bridge re-advertises
  3. send another message                     -> mcp__cowork__* tools ARE there

It reads the tool list out of each turn's system/init event, so no model
reasoning is required and the run costs a couple of Haiku turns.

Requires: a built ./build/akcore and an authenticated `claude` CLI.
Run unbuffered for live output:  python3 -u scripts/smoke-cowork-enable.py
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
SOCK = os.path.join(tempfile.gettempdir(), "ak-smoke-cowork.sock")
DEADLINE_SECS = 240


def log(*a):
    print(*a, flush=True)


def events_of(params):
    if "event" in params:
        return [params["event"]]
    return params.get("events", [])


def cowork_tools(ev):
    return sorted(t for t in ev.get("tools", []) if t.startswith("mcp__cowork"))


def main():
    if not os.path.exists(AKCORE):
        sys.exit("build akcore first: scripts/ak build")
    try:
        os.unlink(SOCK)
    except FileNotFoundError:
        pass

    env = dict(os.environ, XDG_DATA_HOME=tempfile.mkdtemp(prefix="ak-smoke-cowork-data-"))
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

    workspace = tempfile.mkdtemp(prefix="ak-smoke-cowork-ws-")
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

    # cowork.setEnabled is UI-only, so this connection has to identify as the UI.
    call("handshake", {})
    start_id = call("agent.start", {
        "workspacePath": workspace,
        "prompt": "Reply with the single word READY and nothing else.",
        "model": "claude-haiku-4-5-20251001",
        "coworkEnabled": False,
    })

    thread_id = None
    phase = "before"           # before -> enabling -> after
    tools_before = None
    tools_after = None
    enable_reply = None
    setenabled_id = None
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

                if setenabled_id is not None and msg.get("id") == setenabled_id:
                    if msg.get("error"):
                        sys.exit(f"cowork.setEnabled failed: {msg['error']}")
                    enable_reply = msg.get("result", {})
                    log(f"  cowork.setEnabled -> {enable_reply}")
                    # Second turn: its system/init event carries the tool list the
                    # CLI now holds — which is the whole point of the test.
                    send_id = call("agent.send", {
                        "threadId": thread_id,
                        "text": "Reply with the single word AGAIN and nothing else.",
                    })
                    continue

                if msg.get("method") == "permission.requested":
                    call("permission.respond",
                         {"requestId": msg["params"]["requestId"], "allow": True})
                    continue

                if msg.get("method") != "agent.event":
                    continue

                for ev in events_of(msg["params"]):
                    if ev.get("type") == "system" and ev.get("subtype") == "init":
                        found = cowork_tools(ev)
                        servers = ", ".join(f"{x.get('name')}={x.get('status')}"
                                            for x in ev.get("mcp_servers", []))
                        log(f"  init ({phase}): mcp=[{servers}] cowork tools={len(found)}")
                        if phase == "before":
                            tools_before = found
                        elif phase == "after":
                            tools_after = found
                    elif ev.get("type") == "result":
                        if phase == "before":
                            phase = "enabling"
                            log("  turn 1 done — switching Cowork ON mid-session")
                            setenabled_id = call("cowork.setEnabled",
                                                 {"threadId": thread_id, "enabled": True})
                            phase = "after"
                        elif phase == "after" and tools_after is not None:
                            log("  turn 2 done — stopping agent")
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
        "cowork bridge wired in but SILENT before enabling":
            tools_before == [],
        "cowork.setEnabled reported a live apply (no restart)":
            (enable_reply or {}).get("applied") == "live",
        "the CLI confirmed it re-listed before setEnabled returned":
            (enable_reply or {}).get("revealed") is True,
        # The strict half: the tools are there on the VERY NEXT turn, not one
        # turn later. Without the re-list ack this raced and failed here.
        "desktop tools present on the next turn after enabling":
            bool(tools_after),
    }
    for name, passed in checks.items():
        log(f"  [{'PASS' if passed else 'FAIL'}] {name}")
    log(f"  cowork tools before: {tools_before}")
    log(f"  cowork tools after:  {len(tools_after or [])} "
        f"(e.g. {(tools_after or ['-'])[:3]})")

    ok = all(checks.values())
    log("\nSMOKE TEST PASSED" if ok else "\nSMOKE TEST FAILED")
    sys.exit(0 if ok else 1)


if __name__ == "__main__":
    main()
