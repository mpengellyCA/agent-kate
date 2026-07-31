#!/usr/bin/env python3
"""End-to-end smoke test for cross-harness orchestration (plan 16 P1).

Starts akcore and runs BOTH orchestration directions through the JSON-RPC bus:

  A. a Claude controller thread calls the Cooperation MCP tool `launch_agent`
     to start a Kimi worker with wait=true, and reports the worker's reply;
  B. a Kimi controller does the same in reverse, launching a Claude worker.

A passing run proves the launch_agent/wait_agent bridge tools, the synchronous
agent.launchWorker path, the agent.wait turn tracker, and the ParentThreadID/
Role linkage in agent.list — for both harnesses in both roles.

Requires: a built ./build/akcore and authenticated `claude` + `kimi` CLIs.
Run unbuffered for live output:  python3 -u scripts/smoke-orchestrate.py
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
SOCK = os.path.join(tempfile.gettempdir(), "ak-smoke-orch.sock")
DEADLINE_SECS = 420  # two controller turns, each nesting a worker launch + turn


def log(*a):
    print(*a, flush=True)


def summarize(prefix, ev):
    t = ev.get("type")
    if t == "assistant":
        for block in ev.get("message", {}).get("content", []):
            if block.get("type") == "text":
                txt = " ".join(block["text"].split())
                if txt:
                    log(f"  {prefix} assistant: {txt[:140]}")
            elif block.get("type") == "tool_use":
                log(f"  {prefix} assistant -> tool_use: {block.get('name')}")
    elif t == "user":
        for block in ev.get("message", {}).get("content", []):
            if block.get("type") == "tool_result":
                content = block.get("content")
                if isinstance(content, list):
                    content = " ".join(c.get("text", "") for c in content if isinstance(c, dict))
                log(f"  {prefix} tool_result: {str(content)[:160]}")
    elif t == "result":
        log(f"  {prefix} RESULT subtype={ev.get('subtype')} result={str(ev.get('result'))[:120]!r}")
    elif t == "_lifecycle":
        log(f"  {prefix} lifecycle: {ev.get('phase')} - {ev.get('detail')}")


def events_of(params):
    if "event" in params:
        return [params["event"]]
    return params.get("events", [])


class Bus:
    """Tiny JSON-RPC client: sends calls, pumps notifications to a callback."""

    def __init__(self, sock_path):
        self.s = socket.socket(socket.AF_UNIX)
        self.s.connect(sock_path)
        self.s.settimeout(5.0)
        self.buf = b""
        self.next_id = 0
        self.replies = {}

    def call_async(self, method, params):
        self.next_id += 1
        frame = {"jsonrpc": "2.0", "id": self.next_id, "method": method, "params": params}
        self.s.sendall((json.dumps(frame) + "\n").encode())
        return self.next_id

    def pump(self, on_notify, until, deadline):
        """Read frames until `until()` is true or the deadline passes."""
        while time.time() < deadline:
            if until():
                return True
            try:
                chunk = self.s.recv(65536)
            except (socket.timeout, TimeoutError):
                continue
            if not chunk:
                sys.exit("core connection closed unexpectedly")
            self.buf += chunk
            while b"\n" in self.buf:
                line, self.buf = self.buf.split(b"\n", 1)
                if not line.strip():
                    continue
                try:
                    msg = json.loads(line)
                except json.JSONDecodeError:
                    continue
                if msg.get("id") is not None and "method" not in msg:
                    self.replies[msg["id"]] = msg
                else:
                    on_notify(msg)
        return until()

    def call(self, method, params, deadline_secs=30):
        rid = self.call_async(method, params)
        deadline = time.time() + deadline_secs
        self.pump(lambda m: None, lambda: rid in self.replies, deadline)
        reply = self.replies.pop(rid, None)
        if reply is None:
            sys.exit(f"{method} never answered")
        if reply.get("error"):
            sys.exit(f"{method} failed: {reply['error']}")
        return reply.get("result", {})


def run_direction(bus, name, controller_backend, controller_extra, prompt,
                  worker_backend, magic_word):
    """Start a controller, let it orchestrate one worker, verify the outcome."""
    log(f"\n=== direction {name}: {controller_backend or 'claude'} controller "
        f"-> {worker_backend} worker ===")
    checks = {}
    state = {"thread": None, "done": False, "tool_use": False,
             "launched": False, "magic": False}

    start = bus.call("agent.start", dict(controller_extra,
                                         prompt=prompt, backend=controller_backend))
    state["thread"] = start["threadId"]
    log(f"controller thread: {state['thread']} backend={start.get('backend')!r}")

    def on_notify(msg):
        if msg.get("method") == "permission.requested":
            p = msg["params"]
            log(f"  >> permission requested: {p.get('toolName')} — auto-approving")
            bus.call_async("permission.respond",
                           {"requestId": p["requestId"], "allow": True})
            return
        if msg.get("method") != "agent.event":
            return
        params = msg["params"]
        prefix = "[ctrl]" if params.get("threadId") == state["thread"] else "[wrkr]"
        for ev in events_of(params):
            summarize(prefix, ev)
            if params.get("threadId") != state["thread"]:
                continue
            if ev.get("type") == "assistant":
                for block in ev.get("message", {}).get("content", []):
                    if block.get("type") == "tool_use" and \
                            block.get("name", "").endswith("launch_agent"):
                        state["tool_use"] = True
                    if block.get("type") == "text" and magic_word in block.get("text", ""):
                        state["magic"] = True
            if ev.get("type") == "user":
                for block in ev.get("message", {}).get("content", []):
                    if block.get("type") == "tool_result":
                        c = block.get("content")
                        txt = c if isinstance(c, str) else " ".join(
                            x.get("text", "") for x in c if isinstance(x, dict))
                        if "Launched worker" in txt:
                            state["launched"] = True
            if ev.get("type") == "result":
                if magic_word in str(ev.get("result", "")):
                    state["magic"] = True
                state["done"] = True

    deadline = time.time() + DEADLINE_SECS
    bus.pump(on_notify, lambda: state["done"], deadline)

    checks["controller started"] = state["thread"] is not None
    checks["controller called launch_agent"] = state["tool_use"]
    checks["launch_agent reported a launched worker"] = state["launched"]
    checks[f"controller relayed the worker's reply ({magic_word})"] = state["magic"]

    # The worker must be a real roster thread, parented to the controller.
    threads = bus.call("agent.list", {"project": ""}).get("threads", [])
    worker = next((t for t in threads
                   if t.get("parentThreadId") == state["thread"]), None)
    controller = next((t for t in threads
                       if t.get("threadId") == state["thread"]), None)
    checks["worker visible in agent.list with parent linkage"] = worker is not None
    checks["worker role is 'worker'"] = bool(worker) and worker.get("role") == "worker"
    checks[f"worker backend is {worker_backend!r}"] = \
        bool(worker) and worker.get("backend") == worker_backend
    checks["controller role is 'controller'"] = \
        bool(controller) and controller.get("role") == "controller"

    # Wind both threads down so the next direction starts clean.
    for t in (worker, controller):
        if t:
            bus.call_async("agent.stop", {"threadId": t["threadId"]})
    time.sleep(2)
    return checks


def main():
    if not os.path.exists(AKCORE):
        sys.exit("build akcore first: scripts/build.sh")
    try:
        os.unlink(SOCK)
    except FileNotFoundError:
        pass

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

    workspace = tempfile.mkdtemp(prefix="ak-smoke-ws-")
    for cmd in (["git", "init", "-q"],
                ["git", "config", "user.email", "smoke@agentkate"],
                ["git", "config", "user.name", "Agent Kate Smoke"]):
        subprocess.run(cmd, cwd=workspace, check=True)
    with open(os.path.join(workspace, "README.md"), "w") as fh:
        fh.write("orchestration smoke repo\n")
    subprocess.run(["git", "add", "."], cwd=workspace, check=True)
    subprocess.run(["git", "commit", "-q", "-m", "init"], cwd=workspace, check=True)
    log(f"workspace: {workspace} (git repo)")

    all_checks = {}
    try:
        bus = Bus(SOCK)

        # A: claude controller -> kimi worker. Haiku keeps controller cost low.
        prompt_a = (
            "Call the mcp__cooperation__launch_agent tool exactly once, with these "
            "arguments: backend \"kimi\", title \"pong worker\", wait true, prompt "
            "\"Reply with exactly the single word KIMIPONG and nothing else. Do not "
            "use any tools.\" When the tool returns, reply with a single line: "
            "WORKER SAID: <the worker's reply text>. Do not call any other tools.")
        for k, v in run_direction(
                bus, "A", "", {"workspacePath": workspace,
                               "model": "claude-haiku-4-5-20251001"},
                prompt_a, "kimi", "KIMIPONG").items():
            all_checks[f"A: {k}"] = v

        # B: kimi controller -> claude worker (pinned to haiku).
        prompt_b = (
            "Call the cooperation MCP tool launch_agent exactly once, with these "
            "arguments: backend \"claude\", model \"claude-haiku-4-5-20251001\", "
            "title \"pong worker\", wait true, prompt \"Reply with exactly the "
            "single word CLAUDEPONG and nothing else. Do not use any tools.\" When "
            "the tool returns, reply with a single line: WORKER SAID: <the worker's "
            "reply text>. Do not call any other tools.")
        for k, v in run_direction(
                bus, "B", "kimi", {"workspacePath": workspace},
                prompt_b, "claude", "CLAUDEPONG").items():
            all_checks[f"B: {k}"] = v
    finally:
        core.terminate()
        try:
            core.wait(timeout=8)
        except subprocess.TimeoutExpired:
            core.kill()

    log("\n--- verdict ---")
    for name, passed in all_checks.items():
        log(f"  [{'PASS' if passed else 'FAIL'}] {name}")
    ok = bool(all_checks) and all(all_checks.values())
    log("\nSMOKE TEST PASSED" if ok else "\nSMOKE TEST FAILED")
    sys.exit(0 if ok else 1)


if __name__ == "__main__":
    main()
