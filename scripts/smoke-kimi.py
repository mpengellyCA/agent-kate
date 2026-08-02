#!/usr/bin/env python3
"""End-to-end smoke test for the Kimi Code backend of the Agent Kate core.

Starts akcore, runs one headless `kimi acp` agent thread through the JSON-RPC
bus, and verifies ACP translation, the permission bridge, and the Cooperation
MCP round-trip. The agent is asked to run a gated Bash command and to call the
cooperation MCP `request_review` tool, so a passing run proves the ACP client,
the event translator, the permission bridge, and the MCP forwarding all work.

Requires: a built ./build/akcore and an authenticated `kimi` CLI.
Run unbuffered for live output:  python3 -u scripts/smoke-kimi.py
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
SOCK = os.path.join(tempfile.gettempdir(), "ak-smoke-kimi.sock")
DEADLINE_SECS = 200


def log(*a):
    print(*a, flush=True)


def summarize(ev):
    """Print a one-line summary of a stream-json (or synthetic) event."""
    t = ev.get("type")
    if t == "system":
        log(f"  system/{ev.get('subtype')}  model={ev.get('model')}  session={ev.get('session_id')}")
    elif t == "assistant":
        for block in ev.get("message", {}).get("content", []):
            if block.get("type") == "text":
                txt = " ".join(block["text"].split())
                if txt:
                    log(f"  assistant: {txt[:140]}")
            elif block.get("type") == "tool_use":
                log(f"  assistant -> tool_use: {block.get('name')}")
    elif t == "user":
        for block in ev.get("message", {}).get("content", []):
            if block.get("type") == "tool_result":
                content = block.get("content")
                if isinstance(content, list):
                    content = " ".join(c.get("text", "") for c in content if isinstance(c, dict))
                log(f"  tool_result: {str(content)[:140]}")
    elif t == "result":
        log(f"  RESULT  subtype={ev.get('subtype')}  is_error={ev.get('is_error')}")
    elif t == "_stderr":
        log(f"  agent-stderr: {ev.get('text')}")
    elif t == "_lifecycle":
        log(f"  lifecycle: {ev.get('phase')} - {ev.get('detail')}")
    else:
        log(f"  event: {t}")


def events_of(params):
    """agent.event arrives either as a single event or a batch."""
    if "event" in params:
        return [params["event"]]
    return params.get("events", [])


def main():
    if not os.path.exists(AKCORE):
        sys.exit("build akcore first: scripts/build.sh")
    for stale in (SOCK,):
        try:
            os.unlink(stale)
        except FileNotFoundError:
            pass

    # Isolate the thread store so the smoke run does not touch real user data.
    env = dict(os.environ, XDG_DATA_HOME=tempfile.mkdtemp(prefix="ak-smoke-data-"))
    core = subprocess.Popen([AKCORE, "--socket", SOCK], env=env,
                            stderr=subprocess.PIPE, text=True)
    threading.Thread(
        target=lambda: [log("  [core]", ln.rstrip()) for ln in core.stderr],
        daemon=True,
    ).start()

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

    # A committed git repo, so the agent runs in an isolated worktree.
    workspace = tempfile.mkdtemp(prefix="ak-smoke-ws-")
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

    # Identify as the primary UI before anything else: agent.start, agent.stop
    # and permission.respond are UI-only handlers, so an unidentified connection
    # is refused ("only be performed from the Agent Kate window").
    call("handshake", {})

    # Writing OUTSIDE the workspace is gated by kimi's permission rules, so the
    # command routes through the ACP permission bridge.
    perm_file = os.path.join(tempfile.gettempdir(), f"ak-perm-kimi-{os.getpid()}.txt")
    if os.path.exists(perm_file):
        os.unlink(perm_file)
    prompt = (f"Use the Bash tool to run exactly this command: "
              f"echo agentkate-perm-ok > {perm_file} && cat {perm_file}. Then use the "
              f"cooperation MCP tool request_review with summary 'kimi smoke test'. "
              f"Finally reply with the single word DONE and nothing else.")
    # Pin the approval mode: this machine's kimi config may default to "auto"
    # (auto-approve), which would bypass the very permission bridge this smoke
    # exists to prove. "default" = manual approvals, routed to the human.
    start_id = call("agent.start", {
        "workspacePath": workspace,
        "prompt": prompt,
        "backend": "kimi",
        "permissionMode": "default",
    })

    saw_backend = False
    saw_permission = False
    saw_bash_output = False
    saw_review = False
    saw_result = False
    saw_isolated = False
    saw_session_id = False
    thread_id = None

    buf = b""
    deadline = time.time() + DEADLINE_SECS
    last_event = time.time()
    ok = False

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
                        log(f"agent.start failed: {msg['error']}")
                        raise SystemExit(1)
                    result = msg.get("result", {})
                    thread_id = result.get("threadId")
                    saw_backend = result.get("backend") == "kimi"
                    log(f"thread started: {thread_id}  backend={result.get('backend')!r}")
                    continue

                if msg.get("method") == "permission.requested":
                    p = msg["params"]
                    log(f"  >> permission requested: {p.get('toolName')} — auto-approving")
                    saw_permission = True
                    call("permission.respond",
                         {"requestId": p["requestId"], "allow": True})
                    continue

                if msg.get("method") == "agent.reviewRequested":
                    log(f"  >> review requested: {msg['params'].get('summary')!r}")
                    saw_review = True
                    continue

                if msg.get("method") == "agent.event":
                    for ev in events_of(msg["params"]):
                        summarize(ev)
                        if ev.get("type") == "system" and ev.get("session_id"):
                            saw_session_id = True
                        if ev.get("type") == "user":
                            for block in ev.get("message", {}).get("content", []):
                                if block.get("type") == "tool_result":
                                    c = block.get("content")
                                    txt = c if isinstance(c, str) else " ".join(
                                        x.get("text", "") for x in c if isinstance(x, dict))
                                    if "agentkate-perm-ok" in txt:
                                        saw_bash_output = True
                        if ev.get("type") == "result":
                            saw_result = True
                            if thread_id:
                                log("  (turn complete; stopping agent)")
                                call("agent.stop", {"threadId": thread_id})
                        if ev.get("type") == "_lifecycle":
                            if ev.get("phase") == "started" and ev.get("isolated"):
                                saw_isolated = True
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
        try:
            os.unlink(perm_file)
        except OSError:
            pass

    log("\n--- verdict ---")
    checks = {
        "agent thread started": thread_id is not None,
        "start reply reports backend=kimi": saw_backend,
        "agent ran in an isolated worktree": saw_isolated,
        "session id reported (init event)": saw_session_id,
        "per-tool permission requested + approved": saw_permission,
        "approved Bash command executed": saw_bash_output,
        "agent requested review (Cooperation MCP)": saw_review,
        "agent produced a result": saw_result,
    }
    for name, passed in checks.items():
        log(f"  [{'PASS' if passed else 'FAIL'}] {name}")

    ok = all(checks.values())
    log("\nSMOKE TEST PASSED" if ok else "\nSMOKE TEST FAILED")
    sys.exit(0 if ok else 1)


if __name__ == "__main__":
    main()
