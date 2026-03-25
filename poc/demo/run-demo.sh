#!/bin/bash
# Faultbox Demo Day Script
# Usage: ./run-demo.sh [--build]
#
# Requires: faultbox, inventory-svc, order-svc binaries in /tmp/
# Pass --build to rebuild from source.
set -euo pipefail

DEMO_DIR="$(cd "$(dirname "$0")" && pwd)"
STAR_FILE="$DEMO_DIR/faultbox.star"

# Colors
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
CYAN='\033[0;36m'
BOLD='\033[1m'
NC='\033[0m'

header() { echo -e "\n${BOLD}${CYAN}$1${NC}\n"; }
step()   { echo -e "${YELLOW}▶ $1${NC}"; }

cleanup() {
  pkill -9 -f inventory-svc 2>/dev/null || true
  pkill -9 -f order-svc 2>/dev/null || true
  rm -f /tmp/inventory.wal
  sleep 1
}

# --- Build (optional) ---
if [[ "${1:-}" == "--build" ]]; then
  header "Building binaries..."
  GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -o /tmp/faultbox       ./cmd/faultbox/
  GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -o /tmp/inventory-svc  ./poc/demo/inventory-svc/
  GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -o /tmp/order-svc      ./poc/demo/order-svc/
  echo "Done."
fi

cleanup

# ============================================================================
# ACT 1: The System
# ============================================================================
header "ACT 1: The System Under Test"

step "Topology: order-svc (HTTP :8080) → inventory-svc (TCP :5432 + WAL)"
echo ""
cat "$STAR_FILE" | head -22
echo ""

# ============================================================================
# ACT 2: Run All Tests
# ============================================================================
header "ACT 2: Run All Tests"

step "faultbox test faultbox.star --output trace.json --shiviz trace.shiviz"
echo ""
cleanup
/tmp/faultbox test "$STAR_FILE" \
  --output /tmp/demo-trace.json \
  --shiviz /tmp/demo-trace.shiviz \
  --normalize /tmp/demo-run1.norm \
  2>&1 | grep -v '^{' # filter JSON log lines

echo ""
step "Trace written to /tmp/demo-trace.json ($(wc -l < /tmp/demo-trace.json) lines)"
step "ShiViz trace written to /tmp/demo-trace.shiviz"
step "Open https://bestchai.bitbucket.io/shiviz/ and paste the .shiviz file"

# ============================================================================
# ACT 3: Determinism
# ============================================================================
header "ACT 3: Deterministic Simulation"

step "Running the same tests again..."
echo ""
cleanup
/tmp/faultbox test "$STAR_FILE" \
  --normalize /tmp/demo-run2.norm \
  2>&1 | grep -v '^{' # filter JSON log lines

echo ""
step "Comparing normalized traces..."
/tmp/faultbox diff /tmp/demo-run1.norm /tmp/demo-run2.norm && echo ""

# ============================================================================
# ACT 4: Temporal Properties
# ============================================================================
header "ACT 4: Temporal Property Verification"

step "From faultbox.star:"
echo ""
echo '    def test_happy_path():'
echo '        resp = orders.post(path="/orders", body=...)'
echo '        assert_eq(resp.status, 200)'
echo ''
echo '        # Temporal: WAL must have been written.'
echo '        assert_eventually(service="inventory", syscall="openat", path="/tmp/inventory.wal")'
echo ''
echo '    def test_inventory_unreachable():'
echo '        def scenario():'
echo '            resp = orders.post(path="/orders", body=...)'
echo '            assert_eq(resp.status, 503)'
echo ''
echo '            # No WAL write should occur.'
echo '            assert_never(service="inventory", syscall="openat", path="/tmp/inventory.wal")'
echo '        fault(orders, connect=deny("ECONNREFUSED"), run=scenario)'
echo ""

step "We don't just break things — we prove properties over the syscall trace."

# ============================================================================
header "Demo Complete"
echo -e "Files:"
echo -e "  ${GREEN}trace.json${NC}   — full syscall trace with PObserve-compatible events"
echo -e "  ${GREEN}trace.shiviz${NC} — ShiViz visualization with vector clocks"
echo -e "  ${GREEN}*.norm${NC}       — normalized traces for determinism comparison"

cleanup
