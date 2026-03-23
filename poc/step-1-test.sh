#!/bin/bash
# Automated checks for Step 1: Linux Launcher
# Usage: make env-exec CMD="bash poc/step-1-test.sh"
set -euo pipefail

PASS=0
FAIL=0

check() {
    local name="$1"
    local result="$2"  # "true" or "false"
    if [ "$result" = "true" ]; then
        echo "  PASS: $name"
        PASS=$((PASS + 1))
    else
        echo "  FAIL: $name"
        FAIL=$((FAIL + 1))
    fi
}

has() {
    echo "$OUTPUT" | grep -q "$1" && echo "true" || echo "false"
}

echo "=== Step 1: Build ==="
/usr/local/go/bin/go build -o /tmp/faultbox ./cmd/faultbox/
/usr/local/go/bin/go build -o /tmp/faultbox-target ./poc/target/
echo "  Build OK"

echo ""
echo "=== Step 1: Namespace Isolation ==="
OUTPUT=$(/tmp/faultbox run /tmp/faultbox-target 2>&1)

check "PID namespace (PID: 1)"          "$(has 'PID: 1')"
check "Mount namespace (FS read/write)"  "$(has 'FS: wrote and read')"
check "NET namespace (network isolated)" "$(has 'network is unreachable')"
check "All 4 namespaces enabled"         "$(has '"type":"USER"')"
check "Session lifecycle complete"       "$(has '"state":"stopped"')"
check "Exit code 0"                      "$(has '"exit_code":0')"

echo ""
echo "=== Step 1: Exit Code Propagation ==="
set +e
/tmp/faultbox run /bin/false >/dev/null 2>&1; CODE=$?
set -e
check "Non-zero exit forwarded" "$([ "$CODE" -eq 1 ] && echo true || echo false)"

echo ""
echo "=== Step 1: Error Handling ==="
set +e
OUTPUT=$(/tmp/faultbox run /tmp/does-not-exist 2>&1); CODE=$?
set -e
check "Bad binary exits 1"   "$([ "$CODE" -eq 1 ] && echo true || echo false)"
check "Bad binary logs error" "$(has '"level":"ERROR"')"

echo ""
echo "=== Results: $PASS passed, $FAIL failed ==="
[ "$FAIL" -eq 0 ] || exit 1
