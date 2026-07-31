#!/usr/bin/env python3
"""End-to-end smoke test for cross-harness orchestration (plan 16 P1).

Starts akcore and runs BOTH orchestration directions through the JSON-RPC bus:

  A. a Claude controller thread calls the Cooperation MCP tool `launch_agent`
     to start a Kimi worker with wait=true, and reports the worker's reply;
  B. a Kimi controller does the same in reverse, launching a Claude worker.

Both launches also request the plan 16 P3 persona channels (a system prompt
and a custom subagent profile), which Claude supports and Kimi does not — so
the same call must come back APPLIED in one direction and NOT APPLIED, named,
in the other.

A third leg covers ensembles (plan 16 P4): the mode.* catalogue round-trip, and
a mode.apply whose controller orchestrates from its rendered master prompt
alone — nothing in the human's task names a tool, a backend or a model.

A passing run proves the launch_agent/wait_agent bridge tools, the synchronous
agent.launchWorker path, the agent.wait turn tracker, the ParentThreadID/Role
linkage in agent.list, the core-broadcast `mcp.activity` feed (plan 16 P2), the
persona applied-truth (plan 16 P3) and the ensemble apply path (plan 16 P4) —
for both harnesses in both roles.

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
                  worker_backend, magic_word, persona_applied):
    """Start a controller, let it orchestrate one worker, verify the outcome.

    persona_applied says whether the WORKER's engine supports the P3 persona
    channels, i.e. whether launch_agent must report them applied or must name
    them in its NOT APPLIED list.
    """
    log(f"\n=== direction {name}: {controller_backend or 'claude'} controller "
        f"-> {worker_backend} worker ===")
    checks = {}
    state = {"thread": None, "done": False, "tool_use": False,
             "launched": False, "magic": False, "launch_text": ""}
    acts = []  # mcp.activity notifications (plan 16 P2's core-side MCP feed)

    start = bus.call("agent.start", dict(controller_extra,
                                         prompt=prompt, backend=controller_backend))
    state["thread"] = start["threadId"]
    log(f"controller thread: {state['thread']} backend={start.get('backend')!r}")

    def on_notify(msg):
        if msg.get("method") == "mcp.activity":
            p = msg["params"]
            acts.append(p)
            log(f"  ++ mcp.activity {p.get('tool')} thread={p.get('threadId')} "
                f"ok={p.get('ok')} {p.get('durationMs')}ms "
                f"args={p.get('argsSummary')!r}")
            return
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
                            state["launch_text"] = txt
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

    # Persona applied-truth (P3): the same request, honestly reported per
    # engine — applied where the CLI has the channel, NAMED where it does not.
    launch_text = state["launch_text"]
    if persona_applied:
        checks["launch_agent applied the system prompt"] = \
            "System prompt:" in launch_text
        checks["launch_agent applied the 'scout' subagent profile"] = \
            "Subagent profiles available to the worker: scout" in launch_text
        checks["launch_agent reported nothing NOT APPLIED"] = \
            bool(launch_text) and "NOT APPLIED" not in launch_text
    else:
        checks["NOT APPLIED names system_prompt"] = \
            "NOT APPLIED: system_prompt" in launch_text
        checks["NOT APPLIED names agents[scout]"] = \
            "NOT APPLIED: agents[scout]" in launch_text
        checks["no persona reported as applied"] = \
            bool(launch_text) and "System prompt:" not in launch_text \
            and "Subagent profiles available" not in launch_text

    # The core broadcasts every bridge call to the UI as mcp.activity, tagged
    # with the calling thread — this is what the live MCP view (P5) consumes.
    def activity(tool):
        return [a for a in acts if a.get("tool") == tool
                and a.get("threadId") == state["thread"] and a.get("ok")]

    checks["mcp.activity: launch_agent, controller thread, ok"] = bool(activity("launch_agent"))
    checks["mcp.activity: wait_agent, controller thread, ok"] = bool(activity("wait_agent"))
    checks[f"mcp.activity: launch_agent summary names {worker_backend!r}"] = \
        any(worker_backend in (a.get("argsSummary") or "") for a in activity("launch_agent"))

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


def run_ensemble(bus, workspace):
    """Apply an ensemble and let its briefing alone drive the orchestration.

    This is plan 16 P4's end-to-end claim: mode.apply starts ONE thread (the
    controller), already briefed with the rendered master prompt, and that
    prompt is what teaches it to launch a worker with the roster's exact
    arguments. Nothing here tells the controller which tool to call or which
    backend/model to use — if the roster or the tool names in the master prompt
    were wrong, the run fails.
    """
    log("\n=== ensembles: mode.save -> mode.apply -> the controller orchestrates ===")
    checks = {}

    listing = bus.call("mode.list", {})
    catalogue = listing.get("modes", [])
    checks["mode.list serves the built-in ensembles"] = \
        bool(catalogue) and all(m.get("builtIn") for m in catalogue)
    checks["mode.list serves the default master prompt"] = \
        "{{worker_roster}}" in listing.get("defaultMasterPrompt", "")

    # Both roles pinned to haiku: this exercises the machinery, not the model.
    haiku = "claude-haiku-4-5-20251001"
    ensemble = {
        "name": "Smoke Ensemble",
        "description": "orchestration smoke",
        "controller": {"backend": "claude", "model": haiku, "isolation": "workspace"},
        "workers": [{"role": "ponger", "backend": "claude", "model": haiku,
                     "isolation": "workspace",
                     "notes": "Replies with one word. Give it the exact words to say."}],
    }
    bus.call("mode.save", {"mode": ensemble})
    saved = [m for m in bus.call("mode.list", {}).get("modes", [])
             if m.get("name") == "Smoke Ensemble"]
    checks["mode.save round-trips into the catalogue"] = \
        bool(saved) and not saved[0].get("builtIn") \
        and saved[0].get("workers", [{}])[0].get("role") == "ponger"

    task = ('Launch exactly one "ponger" worker, using the backend, model and '
            'isolation its roster row gives, and set its prompt to: "Reply with '
            'exactly the single word ENSEMBLEPONG and nothing else. Do not use any '
            'tools." Wait for that worker, then reply with a single line: '
            'WORKER SAID: <the worker\'s reply text>. Do not post notes, do not '
            'claim files, and do not call any other tools.')
    applied = bus.call("mode.apply",
                       {"name": "Smoke Ensemble", "workDir": workspace, "task": task},
                       deadline_secs=90)
    controller_id = applied.get("threadId", "")
    log(f"controller thread: {controller_id} backend={applied.get('backend')!r}")
    checks["mode.apply started a controller thread"] = bool(controller_id)
    checks["mode.apply reported the ensemble name"] = applied.get("ensemble") == "Smoke Ensemble"
    # Claude has the persona channel, so the briefing is ALSO pinned as a system
    # prompt — and that has to be reported, not assumed.
    checks["mode.apply pinned the briefing as a system prompt (claude)"] = \
        applied.get("systemPromptApplied") is True
    checks["mode.apply reported nothing unapplied"] = not applied.get("unapplied")

    state = {"done": False, "magic": False, "launched": False}
    acts = []

    def on_notify(msg):
        if msg.get("method") == "mcp.activity":
            p = msg["params"]
            acts.append(p)
            log(f"  ++ mcp.activity {p.get('tool')} thread={p.get('threadId')} "
                f"ok={p.get('ok')} args={p.get('argsSummary')!r}")
            return
        if msg.get("method") == "permission.requested":
            p = msg["params"]
            log(f"  >> permission requested: {p.get('toolName')} — auto-approving")
            bus.call_async("permission.respond",
                           {"requestId": p["requestId"], "allow": True})
            return
        if msg.get("method") != "agent.event":
            return
        params = msg["params"]
        prefix = "[ctrl]" if params.get("threadId") == controller_id else "[wrkr]"
        for ev in events_of(params):
            summarize(prefix, ev)
            if params.get("threadId") != controller_id:
                continue
            if ev.get("type") == "user":
                for block in ev.get("message", {}).get("content", []):
                    if block.get("type") == "tool_result":
                        c = block.get("content")
                        txt = c if isinstance(c, str) else " ".join(
                            x.get("text", "") for x in c if isinstance(x, dict))
                        if "Launched worker" in txt:
                            state["launched"] = True
            if ev.get("type") == "assistant":
                for block in ev.get("message", {}).get("content", []):
                    if block.get("type") == "text" and "ENSEMBLEPONG" in block.get("text", ""):
                        state["magic"] = True
            if ev.get("type") == "result":
                if "ENSEMBLEPONG" in str(ev.get("result", "")):
                    state["magic"] = True
                state["done"] = True

    bus.pump(on_notify, lambda: state["done"], time.time() + DEADLINE_SECS)

    checks["the briefing alone made the controller launch a worker"] = state["launched"]
    checks["controller relayed the worker's reply (ENSEMBLEPONG)"] = state["magic"]
    checks["mcp.activity: launch_agent from the controller"] = any(
        a.get("tool") == "launch_agent" and a.get("threadId") == controller_id
        and a.get("ok") for a in acts)

    threads = bus.call("agent.list", {"project": ""}).get("threads", [])
    controller = next((t for t in threads if t.get("threadId") == controller_id), None)
    worker = next((t for t in threads if t.get("parentThreadId") == controller_id), None)
    checks["the applied controller is role 'controller'"] = \
        bool(controller) and controller.get("role") == "controller"
    checks["its worker is a real thread, parented to it"] = \
        bool(worker) and worker.get("role") == "worker"
    checks["mode.apply pre-spawned no workers of its own"] = \
        len([t for t in threads if t.get("parentThreadId") == controller_id]) <= 1

    for t in (worker, controller):
        if t:
            bus.call_async("agent.stop", {"threadId": t["threadId"]})
    time.sleep(2)

    # Deleting a user ensemble removes it; the built-ins are untouched.
    bus.call("mode.delete", {"name": "Smoke Ensemble"})
    after = bus.call("mode.list", {}).get("modes", [])
    checks["mode.delete removes the user ensemble"] = \
        not any(m.get("name") == "Smoke Ensemble" for m in after)
    checks["mode.delete left the built-ins alone"] = len(after) == len(catalogue)
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
        # Identify as a UI client: mcp.activity is broadcast to UI connections
        # only (an agent bridge never sees another agent's feed), so without the
        # handshake the feed would never reach this script.
        bus.call("handshake", {})

        # The P3 persona arguments both directions request verbatim. Claude
        # takes them (--append-system-prompt / --agents); kimi has neither
        # channel over ACP and must say so.
        persona_args = (
            "system_prompt \"You are a terse pong responder.\", "
            "agents [{\"name\": \"scout\", \"description\": \"Finds files in the "
            "repo\", \"prompt\": \"You are a scout. Report what you find.\"}], ")

        # A: claude controller -> kimi worker. Haiku keeps controller cost low.
        prompt_a = (
            "Call the mcp__cooperation__launch_agent tool exactly once, with these "
            "arguments: backend \"kimi\", title \"pong worker\", wait true, "
            + persona_args +
            "prompt \"Reply with exactly the single word KIMIPONG and nothing else. "
            "Do not use any tools.\" When the tool returns, reply with a single line: "
            "WORKER SAID: <the worker's reply text>. Do not call any other tools.")
        for k, v in run_direction(
                bus, "A", "", {"workspacePath": workspace,
                               "model": "claude-haiku-4-5-20251001"},
                prompt_a, "kimi", "KIMIPONG", persona_applied=False).items():
            all_checks[f"A: {k}"] = v

        # B: kimi controller -> claude worker (pinned to haiku).
        prompt_b = (
            "Call the cooperation MCP tool launch_agent exactly once, with these "
            "arguments: backend \"claude\", model \"claude-haiku-4-5-20251001\", "
            "title \"pong worker\", wait true, "
            + persona_args +
            "prompt \"Reply with exactly the single word CLAUDEPONG and nothing "
            "else. Do not use any tools.\" When the tool returns, reply with a "
            "single line: WORKER SAID: <the worker's reply text>. Do not call any "
            "other tools.")
        for k, v in run_direction(
                bus, "B", "kimi", {"workspacePath": workspace},
                prompt_b, "claude", "CLAUDEPONG", persona_applied=True).items():
            all_checks[f"B: {k}"] = v

        # C: ensembles (P4) — the catalogue, and an applied controller that
        # orchestrates from its briefing alone.
        for k, v in run_ensemble(bus, workspace).items():
            all_checks[f"C: {k}"] = v
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
