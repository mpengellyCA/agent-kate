#!/usr/bin/env python3
"""Smoke test for the LSP wiring.

Drives clangd through the sequence AgentKate's LspClient implements and checks
every LSP feature wired into the editor: diagnostics, completion, hover,
go-to-definition, find-references, and the document-symbol outline.

Requires clangd on PATH.
"""
import json
import os
import select
import shutil
import subprocess
import sys
import tempfile
import time

# helper_func is called from a *clean* expression on line 1 so clang's AST
# fully resolves it (hover/definition/references need a resolved node). The
# undefined `broken` lives in its own function on line 2 — it still yields a
# diagnostic, but its error recovery cannot poison the helper_func call.
SOURCE = ("int helper_func(void) { return 0; }\n"
          "int main(void) { return helper_func(); }\n"
          "int decoy(void) { return broken; }\n")
LINE1 = SOURCE.split("\n")[1]
HELPER_COL = LINE1.index("helper_func")  # start of the call on line 1


def frame(msg):
    body = json.dumps(msg).encode()
    return b"Content-Length: " + str(len(body)).encode() + b"\r\n\r\n" + body


def symbol_names(items):
    names = []
    for it in items or []:
        names.append(it.get("name", ""))
        names += symbol_names(it.get("children"))
    return names


def main():
    if not shutil.which("clangd"):
        sys.exit("clangd not found on PATH")

    root = tempfile.mkdtemp(prefix="ak-lsp-")
    src = os.path.join(root, "probe.c")
    with open(src, "w") as fh:
        fh.write(SOURCE)
    uri = "file://" + src

    proc = subprocess.Popen(["clangd"], stdin=subprocess.PIPE, stdout=subprocess.PIPE,
                            stderr=subprocess.DEVNULL)
    buf = bytearray()

    def send(method, params, request_id=None):
        msg = {"jsonrpc": "2.0", "method": method, "params": params}
        if request_id is not None:
            msg["id"] = request_id
        proc.stdin.write(frame(msg))
        proc.stdin.flush()

    def read_message(deadline):
        nonlocal buf
        while True:
            sep = buf.find(b"\r\n\r\n")
            if sep >= 0:
                length = 0
                for line in bytes(buf[:sep]).decode().split("\r\n"):
                    if line.lower().startswith("content-length:"):
                        length = int(line.split(":", 1)[1])
                if len(buf) >= sep + 4 + length:
                    body = bytes(buf[sep + 4:sep + 4 + length])
                    del buf[:sep + 4 + length]
                    return json.loads(body)
            timeout = deadline - time.time()
            if timeout <= 0:
                return None
            if not select.select([proc.stdout], [], [], timeout)[0]:
                return None
            chunk = os.read(proc.stdout.fileno(), 65536)
            if not chunk:
                return None
            buf += chunk

    def tdoc(line, char):
        return {"textDocument": {"uri": uri},
                "position": {"line": line, "character": char}}

    send("initialize",
         {"processId": os.getpid(), "rootUri": "file://" + root, "capabilities": {}},
         request_id=1)
    deadline = time.time() + 30
    while True:
        msg = read_message(deadline)
        if msg is None:
            proc.terminate()
            sys.exit("no initialize response from clangd")
        if msg.get("id") == 1 and "result" in msg:
            break

    send("initialized", {})
    send("textDocument/didOpen",
         {"textDocument": {"uri": uri, "languageId": "c", "version": 1, "text": SOURCE}})

    # Wait for the first diagnostics — clangd has finished parsing the TU and
    # can now answer position-based requests reliably.
    diagnostics = None
    deadline = time.time() + 30
    while time.time() < deadline and diagnostics is None:
        msg = read_message(deadline)
        if msg is None:
            break
        if msg.get("method") == "textDocument/publishDiagnostics":
            items = msg["params"].get("diagnostics", [])
            if items:
                diagnostics = items

    send("textDocument/completion", tdoc(1, HELPER_COL + 8), request_id=2)
    send("textDocument/hover", tdoc(1, HELPER_COL + 2), request_id=3)
    send("textDocument/definition", tdoc(1, HELPER_COL + 2), request_id=4)
    send("textDocument/references",
         {**tdoc(1, HELPER_COL + 2), "context": {"includeDeclaration": True}}, request_id=5)
    send("textDocument/documentSymbol", {"textDocument": {"uri": uri}}, request_id=6)

    results = {}
    deadline = time.time() + 40
    while time.time() < deadline and len(results) < 5:
        msg = read_message(deadline)
        if msg is None:
            break
        if "id" in msg and "result" in msg and msg["id"] in (2, 3, 4, 5, 6):
            results[msg["id"]] = msg["result"]

    proc.terminate()
    try:
        proc.wait(timeout=3)
    except subprocess.TimeoutExpired:
        proc.kill()

    if os.environ.get("LSP_DEBUG"):
        for rid in (2, 3, 4, 5, 6):
            print(f"--- id {rid} ---")
            print(json.dumps(results.get(rid), indent=2)[:1200])

    # --- evaluate -----------------------------------------------------------
    completion = results.get(2) or []
    comp_items = completion if isinstance(completion, list) else completion.get("items", [])
    comp_ok = any("helper_func" in c.get("label", "") for c in comp_items)

    hover = results.get(3) or {}
    hover_ok = bool(hover) and bool(hover.get("contents"))

    def first_loc(res):
        if isinstance(res, list):
            res = res[0] if res else {}
        rng = res.get("range") or res.get("targetRange") or {}
        return rng.get("start", {}).get("line")

    def_ok = first_loc(results.get(4)) == 0

    refs = results.get(5) or []
    ref_lines = sorted({r.get("range", {}).get("start", {}).get("line") for r in refs})
    refs_ok = 0 in ref_lines and 1 in ref_lines

    names = symbol_names(results.get(6))
    sym_ok = "helper_func" in names and "main" in names

    checks = {
        "diagnostics": bool(diagnostics),
        "completion (helper_func)": comp_ok,
        "hover": hover_ok,
        "go-to-definition (line 0)": def_ok,
        "find-references (def + use)": refs_ok,
        "document symbols (helper_func, main)": sym_ok,
    }
    print("\n--- verdict ---")
    for name, ok in checks.items():
        print(f"  [{'PASS' if ok else 'FAIL'}] {name}")

    if all(checks.values()):
        print("\nLSP SMOKE TEST PASSED")
        sys.exit(0)
    print("\nLSP SMOKE TEST FAILED")
    sys.exit(1)


if __name__ == "__main__":
    main()
