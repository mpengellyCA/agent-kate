#!/usr/bin/env bash
# Build the pinned Remote Access web bundle. Standard CMake builds call this
# before Go embeds the result; Node is a build-time tool, never a runtime one.
# A raw `go build` may still embed the committed stub for narrow core-only work.
set -euo pipefail

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
webui="${root}/core/internal/remote/webui"
dist="${webui}/dist"
force=0
check=0

for arg in "$@"; do
    case "$arg" in
        --force) force=1 ;;
        --check) check=1 ;;
        -h|--help)
            echo "usage: scripts/build-webui.sh [--force|--check]"
            exit 0
            ;;
        *) echo "unknown option: $arg" >&2; exit 2 ;;
    esac
done

if [[ "$check" == 1 ]]; then
    if [[ -f "${dist}/index.html" ]]; then
        echo "webui: built (${dist})"
    else
        echo "webui: stub only (${dist})"
    fi
    exit 0
fi

if [[ "$force" == 0 && -f "${dist}/index.html" && -z "$(find "${webui}/src" "${webui}/index.html" "${webui}/vite.config.js" "${webui}/package.json" -newer "${dist}/index.html" -print -quit 2>/dev/null)" ]]; then
    echo "webui: already current"
    exit 0
fi

command -v npm >/dev/null 2>&1 || {
    echo "webui: npm is required to build a missing or stale embedded web bundle" >&2
    exit 1
}
[[ -f "${webui}/package-lock.json" ]] || {
    echo "webui: package-lock.json is required for a pinned build" >&2
    exit 1
}

if [[ ! -d "${webui}/node_modules" || "${webui}/package-lock.json" -nt "${webui}/node_modules/.package-lock.json" ]]; then
    echo "webui: installing pinned dependencies…"
    (cd "${webui}" && npm ci --no-audit --no-fund --silent)
fi

echo "webui: building the mobile bundle…"
(cd "${webui}" && npm run build --silent)
# Vite clears dist. Keep its committed sentinel so //go:embed works in a clean
# checkout where this explicit build was never run.
touch "${dist}/.gitkeep"
