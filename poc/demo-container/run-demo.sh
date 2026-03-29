#!/usr/bin/env bash
# run-demo.sh — Run the PoC 2 container demo.
#
# Prerequisites:
#   - Docker daemon running
#   - Linux with kernel 5.6+ (or Lima VM)
#   - faultbox + faultbox-shim built for the target platform
#
# Usage:
#   bash poc/demo-container/run-demo.sh          # from project root
#   make demo-container                           # via Makefile

set -euo pipefail
cd "$(dirname "$0")/../.."

LINUX_BIN="${LINUX_BIN:-bin/linux-arm64}"

echo "=== Faultbox PoC 2: Container Demo ==="
echo

# 1. Build binaries (if not already built).
if [ ! -f "$LINUX_BIN/faultbox" ] || [ ! -f "$LINUX_BIN/faultbox-shim" ]; then
    echo "Building faultbox + faultbox-shim..."
    make demo-build
fi

# 2. Run the test suite.
echo "Running container demo tests..."
echo
"$LINUX_BIN/faultbox" test poc/demo-container/faultbox.star

echo
echo "=== Demo complete ==="
