# Step 4: Manual Test Instructions

## Prerequisites

- Lima VM running (`make env-status` should show `faultbox-dev Running`)
- If not running: `make env-start`

## 1. Build binaries inside the VM

```bash
make env-exec CMD="bash -c '/usr/local/go/bin/go build -o /tmp/faultbox ./cmd/faultbox/ && \
  /usr/local/go/bin/go build -o /tmp/faultbox-target ./poc/target/ && echo BUILD_OK'"
```

Expected: prints `BUILD_OK`, no errors.

## 2. Baseline: no faults

```bash
make env-exec CMD="/tmp/faultbox run --log-format=console /tmp/faultbox-target"
```

**What to check:**
- Target shows `FS: wrote and read 14 bytes (took <1ms)` — no delay
- Exit code 0

## 3. Delay injection on openat (500ms)

```bash
make env-exec CMD="/tmp/faultbox run --fault 'openat=delay:500ms:100%:/tmp/faultbox-*' --log-format=console /tmp/faultbox-target"
```

**What to check:**

| Check | Expected | Why |
|---|---|---|
| Delay logged | `decision=delay(500ms)` for `/tmp/faultbox-target-test` | Delay action works |
| FS operation slow | `took ~1s` (two openat calls × 500ms) | Delays accumulate |
| System paths not delayed | `/lib/*`, `/proc/*` not in log | Path glob limits scope |
| Target completes | `exit_code=0` | Delays don't break execution |

## 4. Delay injection on write (200ms)

```bash
make env-exec CMD="/tmp/faultbox run --fault 'write=delay:200ms:100%' --log-format=console /tmp/faultbox-target"
```

**What to check:**
- Write calls show `decision=delay(200ms)`
- Target output appears slowly (each `fmt.Print` → write → 200ms delay)

## 5. Deny still works (regression)

```bash
make env-exec CMD="/tmp/faultbox run --fault 'openat=ENOENT:100%' --log-format=console /tmp/faultbox-target"
```

**What to check:**
- `decision=deny(no such file or directory)` — deny action unchanged
- Dynamic linker loads (system path exclusion)
- Exit code 1 (target file op fails)

## 6. Deny + delay combined

```bash
make env-exec CMD="/tmp/faultbox run --fault 'openat=delay:100ms:100%:/tmp/*' --fault 'write=EIO:100%' --log-format=console /tmp/faultbox-target"
```

**What to check:**
- openat on `/tmp/*` shows `delay(100ms)` — delay action
- write shows `deny(input/output error)` — deny action
- Both actions compose correctly in the same session

## 7. Delay with probabilistic firing

```bash
make env-exec CMD="/tmp/faultbox run --fault 'openat=delay:500ms:50%' --debug --log-format=console /tmp/faultbox-target"
```

**What to check:**
- Some openat calls show `delay(500ms)`, others show `allow`
- Run multiple times to see probabilistic behavior

## 8. Bad delay syntax errors cleanly

```bash
make env-exec CMD="bash -c '/tmp/faultbox run --fault \"connect=delay:badtime:100%\" /tmp/faultbox-target; echo exit_code=\$?'"
```

**What to check:**
- Error message about bad duration
- `exit_code=1`

## 9. Verify host tests pass (macOS)

```bash
go test ./...
```

**What to check:**
- All tests pass including new delay parsing tests
- No compilation errors on macOS

## Quick one-liner (all VM checks)

```bash
limactl shell --workdir /host-home/git/Faultbox faultbox-dev -- bash -c '
/usr/local/go/bin/go build -o /tmp/faultbox ./cmd/faultbox/
/usr/local/go/bin/go build -o /tmp/faultbox-target ./poc/target/
echo "BUILD_OK"

echo "=== Test 1: Baseline ==="
/tmp/faultbox run /tmp/faultbox-target 2>/dev/null && echo "PASS" || echo "FAIL"

echo "=== Test 2: Deny ==="
output=$(/tmp/faultbox run --fault "openat=ENOENT:100%" /tmp/faultbox-target 2>&1 || true)
echo "$output" | grep -q "deny" && echo "PASS" || echo "FAIL"

echo "=== Test 3: Delay ==="
output=$(/tmp/faultbox run --fault "openat=delay:500ms:100%:/tmp/faultbox-*" /tmp/faultbox-target 2>&1 || true)
echo "$output" | grep -q "delay(500ms)" && echo "PASS" || echo "FAIL"

echo "=== Test 4: Deny+Delay ==="
output=$(/tmp/faultbox run --fault "openat=delay:100ms:100%:/tmp/*" --fault "write=EIO:100%" /tmp/faultbox-target 2>&1 || true)
echo "$output" | grep -q "delay" && echo "$output" | grep -q "deny" && echo "PASS" || echo "FAIL"

echo "=== Test 5: Bad delay syntax ==="
/tmp/faultbox run --fault "connect=delay:bad:100%" /tmp/faultbox-target 2>/dev/null; code=$?
[ "$code" -eq 1 ] && echo "PASS" || echo "FAIL"

echo "=== Done ==="
'
```
