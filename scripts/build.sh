#!/usr/bin/env bash
# Configure and build both agentkate (C++/Qt UI) and akcore (Go core).
set -euo pipefail
cd "$(dirname "$0")/.."

BUILD_DIR="${BUILD_DIR:-build}"
BUILD_TYPE="${BUILD_TYPE:-Debug}"

cmake -S . -B "$BUILD_DIR" -G Ninja -DCMAKE_BUILD_TYPE="$BUILD_TYPE"
cmake --build "$BUILD_DIR"

echo
echo "Built:"
echo "  UI:   $BUILD_DIR/agentkate"
echo "  Core: $BUILD_DIR/akcore"
