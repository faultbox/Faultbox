#!/bin/bash
# Faultbox Demo Day Script
# Usage: make demo  (from project root — builds + runs in Lima)
#   or:  ./run-demo.sh  (inside Lima, with binaries already built)
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
  sleep 2
}
trap cleanup EXIT

# --- Auto-detect binaries ---
BIN_DIR=""
for candidate in "$DEMO_DIR/../../bin/linux-arm64" "/tmp"; do
  if [[ -x "$candidate/faultbox" && -x "$candidate/inventory-svc" && -x "$candidate/order-svc" ]]; then
    BIN_DIR="$candidate"
    break
  fi
done

if [[ -z "$BIN_DIR" ]]; then
  echo -e "${RED}Error: binaries not found.${NC}"
  echo "Run 'make demo' from the project root, or build manually:"
  echo "  GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -o bin/linux-arm64/faultbox ./cmd/faultbox/"
  echo "  GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -o bin/linux-arm64/inventory-svc ./poc/demo/inventory-svc/"
  echo "  GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -o bin/linux-arm64/order-svc ./poc/demo/order-svc/"
  exit 1
fi

# Copy binaries to /tmp/ (the .star file references /tmp/ paths).
if [[ "$BIN_DIR" != "/tmp" ]]; then
  cp "$BIN_DIR/faultbox"      /tmp/faultbox
  cp "$BIN_DIR/inventory-svc" /tmp/inventory-svc
  cp "$BIN_DIR/order-svc"     /tmp/order-svc
fi

FAULTBOX=/tmp/faultbox
step "Using binaries from $BIN_DIR"

cleanup

# ============================================================================
# ACT 1: The System
# ============================================================================
header "ACT 1: The System Under Test"

step "Topology: order-svc (HTTP :8080) → inventory-svc (TCP :5432 + WAL)"
echo ""
head -24 "$STAR_FILE"
echo ""

# ============================================================================
# ACT 2: Run All Tests
# ============================================================================
header "ACT 2: Run All Tests"

step "faultbox test faultbox.star --output trace.json --shiviz trace.shiviz"
echo ""
cleanup
$FAULTBOX test "$STAR_FILE" \
  --log-format=console \
  --output /tmp/demo-trace.json \
  --shiviz /tmp/demo-trace.shiviz \
  --normalize /tmp/demo-run1.norm

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
$FAULTBOX test "$STAR_FILE" \
  --log-format=console \
  --normalize /tmp/demo-run2.norm

echo ""
step "Comparing normalized traces..."
$FAULTBOX diff /tmp/demo-run1.norm /tmp/demo-run2.norm && echo ""

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
