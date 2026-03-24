# Step 3: Manual Test Instructions

## Prerequisites

- Lima VM running (`make env-status` should show `faultbox-dev Running`)
- If not running: `make env-start`

## 1. Build binaries inside the VM

```bash
make env-exec CMD="bash -c '/usr/local/go/bin/go build -o /tmp/faultbox ./cmd/faultbox/ && \
  /usr/local/go/bin/go build -o /tmp/faultbox-target ./poc/target/ && echo BUILD_OK'"
```

Expected: prints `BUILD_OK`, no errors.

## 2. Baseline: namespaces only, no faults

```bash
make env-exec CMD="/tmp/faultbox run --log-format=console /tmp/faultbox-target"
```

**What to check:**

| Check | Expected | Why |
|---|---|---|
| All 4 namespaces enabled | PID, NET, MNT, USER logged | Unified shim sets up namespaces |
| Target sees PID 1 | `PID: 1` in output | PID namespace works |
| FS works | `FS: wrote and read 14 bytes` | No faults, filesystem normal |
| NET isolated | `net failed: ...` | Network namespace active |
| `seccomp=false` in session log | No seccomp filter logged | No faults requested |
| Exit code 0 | `exit_code=0` | Clean exit |

## 3. Namespaces + seccomp combined (the Step 3 unification)

```bash
make env-exec CMD="/tmp/faultbox run --fault 'openat=ENOENT:100%' --log-format=console /tmp/faultbox-target"
```

**What to check:**

| Check | Expected | Why |
|---|---|---|
| All 4 namespaces enabled | PID, NET, MNT, USER logged | Namespaces active even with faults |
| Seccomp filter installed | `seccomp filter installed` logged | Filter active |
| Target sees PID 1 | `PID: 1` in output | PID namespace works WITH seccomp |
| Dynamic linker loads | Target starts (not exit 127) | System path exclusion protects `/lib/*` |
| openat denied | `deny(no such file or directory)` | Fault injection works |
| Target file op fails | `fs write failed: ...` | Fault reached the target |

**This is the key improvement over Step 2** — namespaces and seccomp work together.

## 4. Path-based fault filtering with glob

```bash
make env-exec CMD="/tmp/faultbox run --fault 'openat=ENOENT:100%:/tmp/faultbox-*' --debug --log-format=console /tmp/faultbox-target"
```

**What to check:**

| Check | Expected | Why |
|---|---|---|
| System paths not tagged | No `system path` in debug log | Path glob bypasses system exclusion |
| Matching path denied | `/tmp/faultbox-target-test` denied | Glob matches `/tmp/faultbox-*` |
| Non-matching paths allowed | `/etc/ld.so.cache` allowed | Doesn't match glob |
| Target starts | `PID: 1` visible | Dynamic linker not affected by glob |

## 5. System path exclusion (100% fault without glob)

```bash
make env-exec CMD="/tmp/faultbox run --fault 'openat=ENOENT:100%' --debug --log-format=console /tmp/faultbox-target"
```

**What to check:**
- `allow (system path)` appears for `/lib/*`, `/proc/*`, `/sys/*`, `/etc/ld.so.*`
- Dynamic linker loads successfully (target starts, shows `PID: 1`)
- Application paths (`/tmp/faultbox-target-test`) are still denied
- **Contrast with Step 2:** previously, 100% openat fault killed the dynamic linker (exit 127)

## 6. ENV variable passthrough

```bash
make env-exec CMD="/tmp/faultbox run --env MY_VAR=hello_from_faultbox --log-format=console /tmp/faultbox-target"
```

**What to check:**
- Target starts and runs normally (env vars are passed through)
- No errors related to environment

## 7. Write fault with namespaces (no deadlock)

```bash
make env-exec CMD="bash -c 'timeout 10 /tmp/faultbox run --fault \"write=EIO:100%\" --log-format=console /tmp/faultbox-target; echo exit_code=\$?'"
```

**What to check:**
- Completes within 10 seconds (no deadlock)
- Write calls denied with EIO
- Shim pipe write was NOT affected (BPF fd exception works)

## 8. Multiple fault rules with namespaces

```bash
make env-exec CMD="/tmp/faultbox run --fault 'openat=EACCES:50%' --fault 'write=EIO:100%' --log-format=console /tmp/faultbox-target"
```

**What to check:**
- Both syscalls appear in the `syscalls` log
- `rule_count=2`
- `namespaces=true` and `seccomp=true` both logged

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

## 11. Path glob in fault rule parsing

```bash
make env-exec CMD="bash -c '/tmp/faultbox run --fault \"openat=ENOENT:100%:/nonexistent/*\" /tmp/faultbox-target; echo exit_code=\$?'"
```

**What to check:**
- Target runs successfully (`exit_code=0`) — no files match `/nonexistent/*`
- Path glob doesn't cause errors
- System paths load normally

## 12. Verify host tests pass (macOS)

```bash
go test ./...
```

**What to check:**
- All tests pass including new path filtering tests
- No compilation errors on macOS (stubs compile)

## Quick one-liner (all VM checks)

```bash
limactl shell --workdir /host-home/git/Faultbox faultbox-dev -- bash -c '
/usr/local/go/bin/go build -o /tmp/faultbox ./cmd/faultbox/
/usr/local/go/bin/go build -o /tmp/faultbox-target ./poc/target/
echo "BUILD_OK"

echo "=== Test 1: Baseline ==="
/tmp/faultbox run /tmp/faultbox-target 2>/dev/null && echo "PASS" || echo "FAIL"

echo "=== Test 2: NS + fault ==="
output=$(/tmp/faultbox run --fault "openat=ENOENT:100%" /tmp/faultbox-target 2>&1 || true)
echo "$output" | grep -q "deny" && echo "$output" | grep -q "PID: 1" && echo "PASS" || echo "FAIL"

echo "=== Test 3: Path glob ==="
output=$(/tmp/faultbox run --fault "openat=ENOENT:100%:/tmp/faultbox-*" --debug /tmp/faultbox-target 2>&1 || true)
echo "$output" | grep -q "deny" && ! echo "$output" | grep -q "system path" && echo "PASS" || echo "FAIL"

echo "=== Test 4: System path exclusion ==="
output=$(/tmp/faultbox run --fault "openat=ENOENT:100%" --debug /tmp/faultbox-target 2>&1 || true)
echo "$output" | grep -q "system path" && echo "$output" | grep -q "PID: 1" && echo "PASS" || echo "FAIL"

echo "=== Test 5: Write (no deadlock) ==="
timeout 10 /tmp/faultbox run --fault "write=EIO:100%" /tmp/faultbox-target 2>/dev/null || true
code=$?; [ "$code" -ne 124 ] && echo "PASS" || echo "FAIL"

echo "=== Test 6: Bad syscall ==="
/tmp/faultbox run --fault "fakesyscall=EIO:100%" /tmp/faultbox-target 2>/dev/null; code=$?
[ "$code" -eq 1 ] && echo "PASS" || echo "FAIL"

echo "=== Test 7: Bad syntax ==="
/tmp/faultbox run --fault "garbage" /tmp/faultbox-target 2>/dev/null; code=$?
[ "$code" -eq 1 ] && echo "PASS" || echo "FAIL"

echo "=== Done ==="
'
```
