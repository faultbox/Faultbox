# Step 2: Manual Test Instructions

## Prerequisites

- Lima VM running (`make env-status` should show `faultbox-dev Running`)
- If not running: `make env-start`

## 1. Build binaries inside the VM

```bash
make env-exec CMD="bash -c '/usr/local/go/bin/go build -o /tmp/faultbox ./cmd/faultbox/ && \
  /usr/local/go/bin/go build -o /tmp/faultbox-target ./poc/target/ && echo BUILD_OK'"
```

Expected: prints `BUILD_OK`, no errors.

## 2. Run target WITHOUT fault injection (baseline)

```bash
make env-exec CMD="/tmp/faultbox run /tmp/faultbox-target"
```

**What to check:**

| Check | Expected | Why |
|---|---|---|
| Target runs successfully | `exit_code=0` | Baseline: no faults, everything works |
| PID namespace active | Target sees `PID: 1` | Step 1 isolation still works |
| FS works | `FS: wrote and read 14 bytes` | No filesystem faults |
| NET isolated | `net failed: ...` | Network namespace still active |

## 3. Run with 100% openat fault injection

```bash
make env-exec CMD="/tmp/faultbox run --fault 'openat=ENOENT:100%' --log-format=console /tmp/faultbox-target"
```

**What to check:**

| Check | Expected | Why |
|---|---|---|
| Seccomp filter installed | Log: `seccomp filter installed` with syscalls list | Filter was built and loaded |
| Target started | Log: `target started with seccomp filter` with pid and listener_fd | Re-exec shim worked, pidfd_getfd succeeded |
| Syscall intercepted and denied | Log: `syscall intercepted name=openat decision=deny(ENOENT)` | Notification loop receives and denies |
| Target's file open fails | Target prints file error (not `FS: wrote and read`) | Fault was injected successfully |
| Session completes | Log: `session completed` with exit_code | Clean shutdown of notification loop |

## 4. Run with 100% write fault injection (EIO)

```bash
make env-exec CMD="/tmp/faultbox run --fault 'write=EIO:100%' --log-format=console /tmp/faultbox-target"
```

**What to check:**
- Target's `write()` calls fail with EIO
- The shim pipe write (fd communication to parent) was NOT affected (BPF fd exception works)
- Session still completes cleanly (no deadlock)

## 5. Run with debug logging to see all intercepted syscalls

```bash
make env-exec CMD="/tmp/faultbox run --fault 'openat=ENOENT:50%' --debug --log-format=console /tmp/faultbox-target"
```

**What to check:**
- `DBG` lines show every intercepted syscall with `decision=allow`
- `INF` lines show only denied syscalls with `decision=deny(ENOENT)`
- Roughly half the `openat` calls are denied (probabilistic — run a few times)

## 6. Run with JSON logs (machine-readable)

```bash
make env-exec CMD="/tmp/faultbox run --fault 'openat=ENOENT:100%' --log-format=json /tmp/faultbox-target" 2>&1 | head -20
```

**What to check:**
- Every log line is valid JSON
- Each line has `"level"`, `"msg"`, `"time"` fields
- Intercepted syscalls have `"name"`, `"pid"`, `"decision"` fields
- Could be piped to `jq` for filtering

## 7. Run with multiple fault rules

```bash
make env-exec CMD="/tmp/faultbox run --fault 'openat=EACCES:100%' --fault 'write=EIO:100%' --log-format=console /tmp/faultbox-target"
```

**What to check:**
- Both `openat` and `write` syscalls are intercepted
- `openat` → denied with EACCES
- `write` → denied with EIO
- Separate rules applied independently

## 8. Verify exit code propagation with faults

```bash
make env-exec CMD="bash -c '/tmp/faultbox run --fault \"openat=ENOENT:100%\" /tmp/faultbox-target; echo exit_code=\$?'"
```

**What to check:**
- Target exits non-zero (file operations fail)
- Faultbox exit code reflects the target's exit code

## 9. Verify bad syscall name errors cleanly

```bash
make env-exec CMD="bash -c '/tmp/faultbox run --fault \"fakesyscall=EIO:100%\" /tmp/faultbox-target; echo exit_code=\$?'"
```

**What to check:**
- Error message: `unknown syscall "fakesyscall"`
- `exit_code=1`
- No crash or panic

## 10. Verify bad fault rule syntax errors cleanly

```bash
make env-exec CMD="bash -c '/tmp/faultbox run --fault \"garbage\" /tmp/faultbox-target; echo exit_code=\$?'"
```

**What to check:**
- Error message about invalid fault rule format
- `exit_code=1`
- No crash or panic

## 11. Verify host tests pass (macOS)

```bash
go test ./...
```

**What to check:**
- All tests pass on macOS host (fault_test.go parses rules without Linux)
- No compilation errors on macOS (seccomp_other.go stubs compile)

## Quick one-liner (all VM checks)

```bash
make env-exec CMD="bash -c '
set -e
echo "=== Build ==="
/usr/local/go/bin/go build -o /tmp/faultbox ./cmd/faultbox/
/usr/local/go/bin/go build -o /tmp/faultbox-target ./poc/target/
echo "BUILD_OK"

echo ""
echo "=== Test 1: Baseline (no faults) ==="
/tmp/faultbox run /tmp/faultbox-target 2>/dev/null && echo "PASS: baseline" || echo "FAIL: baseline"

echo ""
echo "=== Test 2: 100% openat fault ==="
output=\$(/tmp/faultbox run --fault "openat=ENOENT:100%" /tmp/faultbox-target 2>&1)
echo "\$output" | grep -q "deny" && echo "PASS: openat fault" || echo "FAIL: openat fault"

echo ""
echo "=== Test 3: write fault (no deadlock) ==="
timeout 10 /tmp/faultbox run --fault "write=EIO:100%" /tmp/faultbox-target 2>&1; code=\$?
[ "\$code" -ne 124 ] && echo "PASS: no deadlock" || echo "FAIL: deadlock (timeout)"

echo ""
echo "=== Test 4: Bad syscall name ==="
/tmp/faultbox run --fault "fakesyscall=EIO:100%" /tmp/faultbox-target 2>&1; code=\$?
[ "\$code" -eq 1 ] && echo "PASS: bad syscall" || echo "FAIL: bad syscall (got \$code)"

echo ""
echo "=== Test 5: Bad rule syntax ==="
/tmp/faultbox run --fault "garbage" /tmp/faultbox-target 2>&1; code=\$?
[ "\$code" -eq 1 ] && echo "PASS: bad syntax" || echo "FAIL: bad syntax (got \$code)"

echo ""
echo "=== All Step 2 manual tests done ==="
'"
```
