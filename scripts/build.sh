#!/usr/bin/env bash
# Configure and build both agentkate (C++/Qt UI) and akcore (Go core) into a
# local build directory — for development and running from the tree.
#
#   scripts/build.sh                # Debug build into ./build
#   scripts/build.sh --release      # optimised (RelWithDebInfo)
#   scripts/build.sh --clean        # wipe the build dir first
#
# Honours BUILD_DIR (default: build) and BUILD_TYPE (default: Debug).
set -euo pipefail
source "$(dirname "${BASH_SOURCE[0]}")/lib.sh"

BUILD_DIR="${BUILD_DIR:-build}"
BUILD_TYPE="${BUILD_TYPE:-Debug}"
CLEAN=0

while [[ $# -gt 0 ]]; do
    case "$1" in
        --release)   BUILD_TYPE="RelWithDebInfo" ;;
        --debug)     BUILD_TYPE="Debug" ;;
        --clean)     CLEAN=1 ;;
        -h|--help)
            grep '^#' "${BASH_SOURCE[0]}" | sed 's/^# \{0,1\}//; 1d'
            exit 0 ;;
        *) die "unknown option: $1 (try --help)" ;;
    esac
    shift
done

cd "$ROOT"

step "Checking build tools"
require_cmd cmake "$(pkg_install_hint cmake)"
require_cmd ninja "$(pkg_install_hint ninja)"
require_cmd go    "$(pkg_install_hint go)"
require_cmd git
ok "cmake, ninja, go, git present"

if [[ "$CLEAN" == 1 && -d "$BUILD_DIR" ]]; then
    step "Cleaning $BUILD_DIR"
    rm -rf "$BUILD_DIR"
fi

step "Configuring ($BUILD_TYPE)"
cmake -S . -B "$BUILD_DIR" -G Ninja -DCMAKE_BUILD_TYPE="$BUILD_TYPE"

step "Building"
cmake --build "$BUILD_DIR"

step "Done"
ok "UI:   ${C_BOLD}$BUILD_DIR/agentkate${C_RESET}"
ok "Core: ${C_BOLD}$BUILD_DIR/akcore${C_RESET}"
info "Run it with ${C_BOLD}scripts/run.sh${C_RESET} (or ${C_BOLD}scripts/ak run${C_RESET})."
