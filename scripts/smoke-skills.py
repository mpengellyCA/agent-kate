#!/usr/bin/env python3
"""End-to-end smoke test for the Claude Skills manager.

Starts akcore against a temp XDG_DATA_HOME, drops two skills into the central
catalog, then drives skills.listCatalog / skills.install / skills.listInstalled
/ skills.uninstall over JSON-RPC and verifies the symlinks land in the target's
.claude/skills directory.

Requires: a built ./build/akcore. Run unbuffered for live output:
  python3 -u scripts/smoke-skills.py
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


def log(*a):
    print(*a, flush=True)


class Core:
    def __init__(self, sock_path):
        self.s = socket.socket(socket.AF_UNIX)
        self.s.connect(sock_path)
        self.s.settimeout(3.0)
        self.buf = b""
        self.next_id = 0

    def call(self, method, params, timeout=5):
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
        raise RuntimeError(f"timeout waiting for {method}")


def main():
    if not os.path.exists(AKCORE):
        sys.exit("build akcore first: scripts/build.sh")

    sock = os.path.join(tempfile.gettempdir(), "ak-skills-smoke.sock")
    try:
        os.unlink(sock)
    except FileNotFoundError:
        pass

    data = tempfile.mkdtemp(prefix="ak-skills-data-")
    target = tempfile.mkdtemp(prefix="ak-skills-target-")

    # Seed the catalog directory the core will look in.
    catalog = os.path.join(data, "agentkate", "skills")
    os.makedirs(os.path.join(catalog, "review"), exist_ok=True)
    with open(os.path.join(catalog, "review", "SKILL.md"), "w") as fh:
        fh.write("---\nname: review\ndescription: Review the current diff\n---\nbody\n")
    with open(os.path.join(catalog, "quickfix.md"), "w") as fh:
        fh.write("---\ndescription: \"One-off fix helper\"\n---\nbody\n")

    env = dict(os.environ, XDG_DATA_HOME=data)
    core_proc = subprocess.Popen([AKCORE, "--socket", sock], env=env,
                                 stderr=subprocess.PIPE, text=True)
    threading.Thread(
        target=lambda: [log("  [core]", ln.rstrip()) for ln in core_proc.stderr],
        daemon=True).start()

    for _ in range(50):
        if os.path.exists(sock):
            break
        time.sleep(0.1)
    else:
        core_proc.kill()
        sys.exit("akcore socket never appeared")

    checks = {}
    try:
        core = Core(sock)
        hs = core.call("handshake", {})
        if hs.get("result", {}).get("name") != "akcore":
            sys.exit(f"bad handshake: {hs}")

        resp = core.call("skills.listCatalog", {})
        skills = resp["result"]["skills"]
        names = sorted(s["name"] for s in skills)
        checks["catalog lists both skills"] = names == ["quickfix", "review"]
        checks["catalogDir reported"] = resp["result"]["catalogDir"].endswith("agentkate/skills")
        checks["description parsed"] = any(
            s["name"] == "review" and s["description"] == "Review the current diff"
            for s in skills)

        # No skills installed yet.
        resp = core.call("skills.listInstalled", {"target": target})
        checks["empty install list before install"] = resp["result"]["installed"] == []

        # Install both.
        for n in ("review", "quickfix"):
            resp = core.call("skills.install", {"name": n, "target": target})
            if resp.get("error"):
                sys.exit(f"install {n} failed: {resp['error']}")

        review_link = os.path.join(target, ".claude", "skills", "review")
        quickfix_link = os.path.join(target, ".claude", "skills", "quickfix.md")
        checks["review symlink exists"] = os.path.islink(review_link)
        checks["quickfix symlink exists"] = os.path.islink(quickfix_link)
        checks["review symlink resolves to catalog"] = (
            os.path.realpath(review_link) == os.path.realpath(
                os.path.join(catalog, "review")))

        resp = core.call("skills.listInstalled", {"target": target})
        listed = resp["result"]["installed"]
        checks["listInstalled finds two entries"] = len(listed) == 2
        checks["listInstalled flags inCatalog"] = all(e["inCatalog"] for e in listed)

        # Uninstall one and confirm.
        core.call("skills.uninstall", {"name": "review", "target": target})
        checks["review symlink removed"] = not os.path.exists(review_link)
        checks["quickfix symlink still there"] = os.path.islink(quickfix_link)

        # Refuse to remove a non-symlink with the same name.
        os.makedirs(os.path.join(target, ".claude", "skills", "mine"), exist_ok=True)
        with open(os.path.join(target, ".claude", "skills", "mine", "SKILL.md"), "w") as fh:
            fh.write("---\n---\n")
        resp = core.call("skills.uninstall", {"name": "mine", "target": target})
        checks["uninstall refuses non-symlink"] = "error" in resp
    finally:
        core_proc.terminate()
        try:
            core_proc.wait(timeout=3)
        except subprocess.TimeoutExpired:
            core_proc.kill()
        shutil.rmtree(data, ignore_errors=True)
        shutil.rmtree(target, ignore_errors=True)

    log("\n--- verdict ---")
    for name, ok in checks.items():
        log(f"  [{'PASS' if ok else 'FAIL'}] {name}")
    if checks and all(checks.values()):
        log("\nSKILLS SMOKE TEST PASSED")
        sys.exit(0)
    log("\nSKILLS SMOKE TEST FAILED")
    sys.exit(1)


if __name__ == "__main__":
    main()
