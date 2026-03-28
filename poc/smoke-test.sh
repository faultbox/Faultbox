#!/bin/bash
# Smoke test: build and run the target binary inside the Lima VM
# Verifies that the dev environment can compile and run Go code
# that exercises filesystem, network, and syscalls.
#
# Usage: make env-exec CMD="cd ~/git/Faultbox && bash poc/smoke-test.sh"

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
PROJECT_DIR="$(cd "$SCRIPT_DIR/.." && pwd)"

echo "=== Faultbox PoC Smoke Test ==="
echo "Host: $(uname -s) $(uname -r)"
echo "Go: $(/usr/local/go/bin/go version)"
echo

cd "$PROJECT_DIR"

echo "--- Building target binary ---"
/usr/local/go/bin/go build -o /tmp/faultbox-target ./poc/target/

echo "--- Running target binary ---"
/tmp/faultbox-target

echo
echo "--- Checking kernel capabilities ---"

# seccomp: check if SECCOMP_RET_USER_NOTIF is supported
echo -n "seccomp-notify support: "
if grep -q "SECCOMP_FILTER" /boot/config-"$(uname -r)" 2>/dev/null; then
    echo "YES"
else
    echo "UNKNOWN (no kernel config found, may still work)"
fi

# eBPF: try to list BPF programs
echo -n "eBPF support: "
if sudo bpftool prog list >/dev/null 2>&1; then
    echo "YES ($(sudo bpftool prog list 2>/dev/null | wc -l) programs loaded)"
else
    echo "NO or needs root"
fi

# Network namespace: create and delete a test namespace
echo -n "Network namespaces: "
if sudo ip netns add faultbox-test 2>/dev/null; then
    sudo ip netns delete faultbox-test
    echo "YES"
else
    echo "NO or needs root"
fi

# FUSE
echo -n "FUSE: "
if [ -e /dev/fuse ]; then
    echo "YES (/dev/fuse present)"
else
    echo "NO"
fi

# tc/netem
echo -n "tc/netem: "
if sudo tc qdisc show >/dev/null 2>&1; then
    echo "YES"
else
    echo "NO or needs root"
fi

echo
echo "=== Smoke test complete ==="
