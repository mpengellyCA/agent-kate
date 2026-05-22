#!/usr/bin/env python3
"""End-to-end smoke test for M4: VS Code extension-server reuse.

Starts akcore, installs the Intelephense PHP extension from Open VSX over the
JSON-RPC bus, then launches the language server akcore detected inside the
downloaded .vsix and drives it over LSP. A passing run proves the whole
pipeline: Open VSX download -> unpack -> server-recipe detection -> the server
actually runs and serves PHP intelligence.

Requires: a built ./build/akcore, `node` on PATH, network access to
open-vsx.org. Run unbuffered for live output:  python3 -u scripts/smoke-vsix.py
"""
import json
import os
import select
import shutil
import socket
import subprocess
import sys
import tempfile
import threading
import time

ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
AKCORE = os.path.join(ROOT, "build", "akcore")
EXTENSION_ID = "bmewburn.vscode-intelephense-client"


def log(*a):
    print(*a, flush=True)


# --- akcore JSON-RPC client -------------------------------------------------
class Core:
    def __init__(self, sock_path):
        self.s = socket.socket(socket.AF_UNIX)
        self.s.connect(sock_path)
        self.s.settimeout(3.0)
        self.buf = b""
        self.next_id = 0

    def call(self, method, params, timeout):
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
                log(f"  ... waiting on {method} ({int(deadline - time.time())}s left)")
                continue
            if not chunk:
                raise RuntimeError("akcore closed the connection")
            self.buf += chunk
        raise RuntimeError(f"timeout waiting for {method}")


# --- minimal LSP client -----------------------------------------------------
class Lsp:
    def __init__(self, cmd):
        self.p = subprocess.Popen(cmd, stdin=subprocess.PIPE,
                                  stdout=subprocess.PIPE, stderr=subprocess.DEVNULL)
        self.buf = bytearray()
        self.rid = 0

    def _send(self, msg):
        body = json.dumps(msg).encode()
        self.p.stdin.write(b"Content-Length: %d\r\n\r\n%s" % (len(body), body))
        self.p.stdin.flush()

    def request(self, method, params):
        self.rid += 1
        self._send({"jsonrpc": "2.0", "id": self.rid, "method": method, "params": params})
        return self.rid

    def notify(self, method, params):
        self._send({"jsonrpc": "2.0", "method": method, "params": params})

    def _answer(self, req):
        # Auto-answer server->client requests so the server never blocks.
        if req.get("method") == "workspace/configuration":
            items = req.get("params", {}).get("items", [])
            result = [None] * max(1, len(items))
        else:
            result = None
        self._send({"jsonrpc": "2.0", "id": req["id"], "result": result})

    def read(self, deadline):
        while True:
            sep = self.buf.find(b"\r\n\r\n")
            if sep >= 0:
                length = 0
                for line in bytes(self.buf[:sep]).decode(errors="replace").split("\r\n"):
                    if line.lower().startswith("content-length:"):
                        length = int(line.split(":", 1)[1])
                if len(self.buf) >= sep + 4 + length:
                    body = bytes(self.buf[sep + 4:sep + 4 + length])
                    del self.buf[:sep + 4 + length]
                    msg = json.loads(body)
                    if "method" in msg and "id" in msg:  # server -> client request
                        self._answer(msg)
                        continue
                    return msg
            timeout = deadline - time.time()
            if timeout <= 0:
                return None
            if not select.select([self.p.stdout], [], [], timeout)[0]:
                return None
            chunk = os.read(self.p.stdout.fileno(), 65536)
            if not chunk:
                return None
            self.buf += chunk

    def await_result(self, rid, deadline):
        while time.time() < deadline:
            msg = self.read(deadline)
            if msg is None:
                return None
            if msg.get("id") == rid:
                return msg
        return None

    def close(self):
        try:
            self.p.terminate()
            self.p.wait(timeout=3)
        except Exception:
            self.p.kill()


def main():
    if not os.path.exists(AKCORE):
        sys.exit("build akcore first: scripts/build.sh")
    if not shutil.which("node"):
        sys.exit("node not found on PATH (needed to run the bundled server)")

    sock = os.path.join(tempfile.gettempdir(), "ak-vsix-smoke.sock")
    try:
        os.unlink(sock)
    except FileNotFoundError:
        pass

    # An isolated cache dir so the test never touches the real extension cache.
    cache = tempfile.mkdtemp(prefix="ak-vsix-cache-")
    env = dict(os.environ, XDG_CACHE_HOME=cache)

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
    lsp = None
    try:
        core = Core(sock)
        hs = core.call("handshake", {}, 5)
        if hs.get("result", {}).get("name") != "akcore":
            sys.exit(f"bad handshake: {hs}")

        log(f"installing {EXTENSION_ID} from Open VSX ...")
        resp = core.call("vsix.install", {"extensionId": EXTENSION_ID}, 180)
        if resp.get("error"):
            sys.exit(f"vsix.install failed: {resp['error']}")
        ext = resp["result"]
        log(f"  installed {ext.get('id')} v{ext.get('version')}")

        checks["install succeeded"] = True
        checks["version reported"] = bool(ext.get("version"))

        server = ext.get("server")
        checks["server recipe detected"] = bool(server and server.get("command"))
        if server:
            log(f"  server: {server.get('command')} {' '.join(server.get('args', []))}")
            log(f"  source={server.get('source')}  languageIds={server.get('languageIds')}")
        checks["registry recipe for php"] = bool(
            server and server.get("source") == "registry"
            and "php" in (server.get("languageIds") or []))

        listed = core.call("vsix.list", {}, 5)["result"].get("extensions", [])
        checks["extension appears in vsix.list"] = any(
            e.get("id") == EXTENSION_ID for e in listed)

        # --- launch the detected server and drive it over LSP --------------
        if not (server and server.get("command")):
            raise RuntimeError("no server recipe — cannot probe the language server")

        ws = tempfile.mkdtemp(prefix="ak-vsix-php-")
        php = os.path.join(ws, "broken.php")
        with open(php, "w") as fh:
            fh.write("<?php\n\n$x = ;\n")  # deliberate parse error
        uri = "file://" + php

        log("launching the bundled language server ...")
        lsp = Lsp([server["command"]] + server["args"])
        storage = tempfile.mkdtemp(prefix="ak-vsix-store-")
        rid = lsp.request("initialize", {
            "processId": os.getpid(),
            "rootUri": "file://" + ws,
            "initializationOptions": {"storagePath": storage, "clearCache": True},
            "capabilities": {"textDocument": {"publishDiagnostics": {}}},
        })
        init = lsp.await_result(rid, time.time() + 40)
        caps = (init or {}).get("result", {}).get("capabilities")
        checks["server launches + LSP initialize"] = bool(caps)

        if caps:
            lsp.notify("initialized", {})
            lsp.notify("textDocument/didOpen", {"textDocument": {
                "uri": uri, "languageId": "php", "version": 1,
                "text": open(php).read()}})
            diags = None
            deadline = time.time() + 40
            while time.time() < deadline and not diags:
                msg = lsp.read(deadline)
                if msg is None:
                    break
                if (msg.get("method") == "textDocument/publishDiagnostics"
                        and msg["params"].get("uri") == uri):
                    items = msg["params"].get("diagnostics", [])
                    if items:
                        diags = items
            if diags:
                log(f"  diagnostics: {diags[0].get('message')}")
            checks["server reports PHP diagnostics"] = bool(diags)
        else:
            checks["server reports PHP diagnostics"] = False
    finally:
        if lsp:
            lsp.close()
        core_proc.terminate()
        try:
            core_proc.wait(timeout=3)
        except subprocess.TimeoutExpired:
            core_proc.kill()
        shutil.rmtree(cache, ignore_errors=True)

    log("\n--- verdict ---")
    for name, ok in checks.items():
        log(f"  [{'PASS' if ok else 'FAIL'}] {name}")
    if checks and all(checks.values()):
        log("\nVSIX SMOKE TEST PASSED")
        sys.exit(0)
    log("\nVSIX SMOKE TEST FAILED")
    sys.exit(1)


if __name__ == "__main__":
    main()
