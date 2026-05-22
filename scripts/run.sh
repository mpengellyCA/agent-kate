#!/usr/bin/env bash
# Launch AgentKate. The UI spawns akcore itself, so only the UI is started here.
# Any extra arguments are passed through (e.g. a file or project path to open).
set -euo pipefail
cd "$(dirname "$0")/.."

BUILD_DIR="${BUILD_DIR:-build}"

if [[ ! -x "$BUILD_DIR/agentkate" || ! -x "$BUILD_DIR/akcore" ]]; then
    echo "Build artifacts missing; building first..."
    scripts/build.sh
fi

exec "$BUILD_DIR/agentkate" "$@"
