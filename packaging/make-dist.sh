#!/usr/bin/env bash
# Produce a source tarball with vendored Go dependencies, suitable for
# offline RPM/DEB builds. Output: dist/agentkate-<version>.tar.gz
set -euo pipefail

cd "$(dirname "$0")/.."
ROOT="$(pwd)"

VERSION="${VERSION:-$(awk -F'[ )]' '/^project\(AgentKate VERSION/ {print $3}' CMakeLists.txt)}"
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

tar -C "${DIST_DIR}" -czf "${DIST_DIR}/${NAME}.tar.gz" "${NAME}"
rm -rf "${STAGE}"

echo "wrote ${DIST_DIR}/${NAME}.tar.gz"
