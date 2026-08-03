#!/usr/bin/env bash
# Produce a source tarball with vendored Go dependencies, suitable for
# offline RPM/DEB builds. Output: dist/agentkate-<version>.tar.gz
set -euo pipefail

cd "$(dirname "$0")/.."
ROOT="$(pwd)"

# Version: MAJOR.MINOR from CMakeLists.txt, patch = TOTAL commit count (git rev-list
# --count HEAD) — matches scripts/lib.sh project_version and CMakeLists.txt's stamped
# version. Must NOT use --first-parent (it under-counts merged branches). Override with
# VERSION=...
if [[ -z "${VERSION:-}" ]]; then
    _full="$(awk -F'[ )]' '/^project\(AgentKate VERSION/ {print $3}' CMakeLists.txt)"
    IFS=. read -r _maj _min _ <<<"$_full"
    if _cnt="$(git -C "$ROOT" rev-list --count HEAD 2>/dev/null)" && [[ -n "$_cnt" ]]; then
        VERSION="${_maj}.${_min}.${_cnt}"
    else
        VERSION="$_full"
    fi
fi
if [[ -z "$VERSION" ]]; then
    echo "could not detect version from CMakeLists.txt" >&2
    exit 1
fi

NAME="agentkate-${VERSION}"
DIST_DIR="${ROOT}/dist"
STAGE="${DIST_DIR}/${NAME}"

rm -rf "${STAGE}"
mkdir -p "${STAGE}"

# Use `git archive` when available to honour .gitignore; fall back to rsync.
if git -C "${ROOT}" rev-parse --is-inside-work-tree >/dev/null 2>&1; then
    git -C "${ROOT}" archive --format=tar HEAD | tar -x -C "${STAGE}"
else
    rsync -a --exclude=build --exclude=dist --exclude=.git \
        "${ROOT}/" "${STAGE}/"
fi

# Vendor Go modules so the package build can run offline.
( cd "${STAGE}/core" && go mod vendor )

# The source tree deliberately carries only the embedded web stub. A release
# tarball bakes the pinned browser bundle once, so ordinary distro builds do
# not acquire a Node/npm dependency. Refusing by default prevents a release
# from silently advertising remote access but serving a development stub.
WEBUI="${STAGE}/core/internal/remote/webui"
if [[ -f "${WEBUI}/package.json" ]]; then
    if command -v npm >/dev/null 2>&1; then
        echo "building the web UI into the release tarball…"
        "${STAGE}/scripts/build-webui.sh" --force
        rm -rf "${WEBUI}/node_modules"
    else
        echo "ERROR: npm is required to build the release web bundle." >&2
        echo "       Set ALLOW_WEBUI_STUB=1 only for an intentionally stubbed tarball." >&2
        [[ "${ALLOW_WEBUI_STUB:-0}" == "1" ]] || exit 1
        echo "WARNING: release tarball will contain the Remote Access stub." >&2
    fi
fi

tar -C "${DIST_DIR}" -czf "${DIST_DIR}/${NAME}.tar.gz" "${NAME}"
rm -rf "${STAGE}"

echo "wrote ${DIST_DIR}/${NAME}.tar.gz"
