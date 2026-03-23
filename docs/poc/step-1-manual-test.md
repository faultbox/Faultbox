# Step 1: Manual Test Instructions

## Prerequisites

- Lima VM running (`make env-status` should show `faultbox-dev Running`)
- If not running: `make env-start`

## 1. Build binaries inside the VM

```bash
make env-exec CMD="bash -c '/usr/local/go/bin/go build -o /tmp/faultbox ./cmd/faultbox/ && \
  /usr/local/go/bin/go build -o /tmp/faultbox-target ./poc/target/ && echo BUILD_OK'"
```

Expected: prints `BUILD_OK`, no errors.

## 2. Run target under faultbox (JSON logs)

```bash
make env-exec CMD="/tmp/faultbox run /tmp/faultbox-target"
```

**What to check:**

| Check | Expected | Why |
|---|---|---|
| Target prints `PID: 1` | PID namespace works — target is init process in its own PID tree |
| Target prints `FS: wrote and read 14 bytes` | Mount namespace works — target can write to `/tmp` in its private mount |
| Target prints `net failed: ... network is unreachable` | NET namespace works — target has empty network stack, no interfaces |
| Logs show `"state":"starting"` → `"state":"running"` → `"state":"stopped"` | Session lifecycle state machine works correctly |
| Logs show `"type":"PID"`, `"type":"NET"`, `"type":"MNT"`, `"type":"USER"` | All 4 namespaces are activated |
| `"exit_code":0` in final log line | Faultbox correctly captures target exit code |
| Each log line is valid JSON | Structured logging works (pipe-friendly) |

## 3. Run with console (colored) logs

```bash
make env-exec CMD="/tmp/faultbox run --log-format=console /tmp/faultbox-target"
```

**What to check:**
- Log lines show colored level: green `INF`
- Timestamps in `HH:MM:SS.mmm` format
- Component shown as `[engine]`
- Session ID and attributes after message text
- Target stdout (the `===` lines) is NOT mixed into log format — it appears as plain text

## 4. Run with debug logging

```bash
make env-exec CMD="/tmp/faultbox run --debug --log-format=console /tmp/faultbox-target"
```

**What to check:**
- Additional `DBG` lines appear (gray color)
- Debug lines show UID/GID mapping details for USER namespace

## 5. Verify exit code propagation

```bash
make env-exec CMD="bash -c '/tmp/faultbox run /bin/false; echo exit_code=\$?'"
```

**What to check:**
- `exit_code=1` — faultbox forwards the target's non-zero exit code
- Logs show `"exit_code":1` and a `WARN` level "target exited with error"

## 6. Verify error on non-existent binary

```bash
make env-exec CMD="bash -c '/tmp/faultbox run /tmp/does-not-exist; echo exit_code=\$?'"
```

**What to check:**
- `exit_code=1` — faultbox reports failure
- Error log with `"level":"ERROR"` and message about failed start

## 7. Verify macOS behavior (run on host, not in VM)

```bash
make build
./bin/faultbox run /bin/echo hello
```

**What to check:**
- Error message: `namespace isolation requires Linux (current OS: darwin); use the Lima VM: make env-exec CMD="..."`
- Exits with code 1
- Does NOT crash or panic

## 8. Verify host tests pass

```bash
go test ./...
```

**What to check:**
- All tests pass on macOS host

## Quick one-liner (all VM checks)

```bash
make env-exec CMD="bash poc/step-1-test.sh"
```

See `poc/step-1-test.sh` for the automated checks.
