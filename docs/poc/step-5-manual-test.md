# Step 5: Manual Test Instructions

## Prerequisites

- Lima VM running (`make env-status` should show `faultbox-dev Running`)
- If not running: `make env-start`

## 1. Build binaries inside the VM

```bash
make env-exec CMD="bash -c '/usr/local/go/bin/go build -o /tmp/faultbox ./cmd/faultbox/ && \
  /usr/local/go/bin/go build -o /tmp/faultbox-target ./poc/target/ && echo BUILD_OK'"
```

## 2. after=N trigger: fail after N calls succeed

```bash
make env-exec CMD="/tmp/faultbox run --fault 'openat=ENOENT:100%:after=2' --debug --log-format=console /tmp/faultbox-target"
```

**What to check:**
- First 2 openat calls are allowed (counter increments but trigger doesn't fire)
- 3rd+ openat calls are denied with ENOENT
- Target starts (dynamic linker opens count toward the counter)

## 3. nth=N trigger: fail only the Nth call

```bash
make env-exec CMD="/tmp/faultbox run --fault 'openat=ENOENT:100%:/tmp/*:nth=1' --debug --log-format=console /tmp/faultbox-target"
```

**What to check:**
- Exactly 1 deny — only the 1st openat matching `/tmp/*` fails
- Subsequent openat calls to `/tmp/*` succeed (nth fires only once)

## 4. after=0: fail all from the start

```bash
make env-exec CMD="/tmp/faultbox run --fault 'openat=ENOENT:100%:/tmp/*:after=0' --debug --log-format=console /tmp/faultbox-target"
```

**What to check:**
- All openat calls matching `/tmp/*` are denied immediately
- Equivalent to no trigger (but explicit)

## 5. Delay with trigger

```bash
make env-exec CMD="/tmp/faultbox run --fault 'openat=delay:500ms:100%:/tmp/*:nth=2' --debug --log-format=console /tmp/faultbox-target"
```

**What to check:**
- Only the 2nd openat to `/tmp/*` is delayed by 500ms
- 1st openat to `/tmp/*` proceeds normally
- Total time ~500ms (not 1s)

## 6. --fs-fault convenience flag

```bash
make env-exec CMD="/tmp/faultbox run --fs-fault 'open=ENOENT:100%:/tmp/*' --log-format=console /tmp/faultbox-target"
```

**What to check:**
- `open` is mapped to `openat` syscall
- `/tmp/faultbox-target-test` is denied

## 7. Path glob + trigger combined

```bash
make env-exec CMD="/tmp/faultbox run --fault 'openat=ENOENT:100%:/tmp/*:after=1' --debug --log-format=console /tmp/faultbox-target"
```

**What to check:**
- 1st openat to `/tmp/*` succeeds (write succeeds)
- 2nd openat to `/tmp/*` fails (read fails)
- System paths unaffected (path glob overrides system exclusion)

## 8. Regression: existing features work

```bash
make env-exec CMD="/tmp/faultbox run --fault 'openat=ENOENT:100%' --debug --log-format=console /tmp/faultbox-target"
```

**What to check:**
- System path exclusion still works
- Target starts (PID: 1)
- Application openat denied

## 9. Bad trigger syntax

```bash
make env-exec CMD="bash -c '/tmp/faultbox run --fault \"openat=EIO:100%:nth=0\" /tmp/faultbox-target; echo exit_code=\$?'"
make env-exec CMD="bash -c '/tmp/faultbox run --fault \"openat=EIO:100%:nth=abc\" /tmp/faultbox-target; echo exit_code=\$?'"
```

**What to check:**
- Both return `exit_code=1` with error messages
- No crash or panic

## 10. Verify host tests pass (macOS)

```bash
go test ./...
```

## Quick one-liner

```bash
limactl shell --workdir /host-home/git/Faultbox faultbox-dev -- bash -c '
/usr/local/go/bin/go build -o /tmp/faultbox ./cmd/faultbox/
/usr/local/go/bin/go build -o /tmp/faultbox-target ./poc/target/

echo "=== T1: Baseline ==="
/tmp/faultbox run /tmp/faultbox-target 2>/dev/null && echo "PASS" || echo "FAIL"

echo "=== T2: after=2 ==="
output=$(/tmp/faultbox run --fault "openat=ENOENT:100%:after=2" --debug /tmp/faultbox-target 2>&1 || true)
echo "$output" | grep -q "deny" && echo "PASS" || echo "FAIL"

echo "=== T3: nth=1 ==="
output=$(/tmp/faultbox run --fault "openat=ENOENT:100%:/tmp/*:nth=1" --debug /tmp/faultbox-target 2>&1 || true)
echo "$output" | grep -q "deny" && echo "PASS" || echo "FAIL"

echo "=== T4: delay+trigger ==="
output=$(/tmp/faultbox run --fault "openat=delay:500ms:100%:/tmp/*:nth=2" --debug /tmp/faultbox-target 2>&1 || true)
echo "$output" | grep -q "delay" && echo "PASS" || echo "FAIL"

echo "=== T5: fs-fault ==="
output=$(/tmp/faultbox run --fs-fault "open=ENOENT:100%:/tmp/*" /tmp/faultbox-target 2>&1 || true)
echo "$output" | grep -q "deny" && echo "PASS" || echo "FAIL"

echo "=== T6: regression ==="
output=$(/tmp/faultbox run --fault "openat=ENOENT:100%" --debug /tmp/faultbox-target 2>&1 || true)
echo "$output" | grep -q "system path" && echo "PASS" || echo "FAIL"

echo "=== T7: bad trigger ==="
/tmp/faultbox run --fault "openat=EIO:100%:nth=0" /tmp/faultbox-target 2>/dev/null; code=$?
[ "$code" -eq 1 ] && echo "PASS" || echo "FAIL"

echo "=== Done ==="
'
```
