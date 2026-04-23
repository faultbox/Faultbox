# Faultbox CLI Reference

## Commands

### `faultbox test`

Run multi-service tests defined in a Starlark `.star` file.

```
faultbox test [flags] <file.star>
```

| Flag | Description |
|------|-------------|
| `--test <name>` | Run only the matching test function |
| `--runs <N>` | Run each test N times (stops on first failure per test) |
| `--seed <N>` | Use specific seed for deterministic replay. Since v0.9.8 every run persists its seed to `.faultbox/last-seed` next to the spec; subsequent no-flag runs reuse it. Stderr prints `Seed: N (cli\|cached\|generated)` to show which tier fired. |
| `--show all\|fail` | Filter output: `all` (default) or `fail` (only failures) |
| `--format json` | Output structured JSON to stdout (human output on stderr) |
| `--output <file>` | Write JSON trace results to file |
| `--bundle <path>` | Override `.fb` bundle filename (default: `run-<ts>-<seed>.fb` in cwd; see [bundles.md](bundles.md)) |
| `--no-bundle` | Skip `.fb` bundle emission entirely |
| `--shiviz <file>` | Write ShiViz-compatible visualization trace |
| `--normalize <file>` | Write normalized trace for determinism comparison |
| `--explore all\|sample` | Explore all interleavings or sample randomly |
| `--virtual-time` | Use virtual time (delays advance clock instead of sleeping) |
| `--log-format=console\|json` | Log output format |
| `--debug` | Enable debug logging |
| `--dry-run` | Validate spec and show test plan without running (no Docker needed) |

**Examples:**

```bash
faultbox test faultbox.star                             # run all tests
faultbox test faultbox.star --test happy_path           # run one test
faultbox test faultbox.star --format json               # structured JSON to stdout
faultbox test faultbox.star --output trace.json         # JSON trace to file
faultbox test faultbox.star --dry-run                   # validate without Docker
faultbox test faultbox.star --runs 100 --show fail      # counterexample discovery
faultbox test faultbox.star --seed 42                   # deterministic replay
faultbox test faultbox.star --normalize run.norm        # normalized trace
faultbox test faultbox.star --explore all               # all interleavings
```

**JSON output (`--format json`):**

Writes machine-parseable JSON to stdout with:
- Per-test results with pass/fail, seed, duration
- Scenario return value (`return_value` field, when non-None)
- Fault info: service, syscall, action, errno, hits, label
- Syscall summary: per-service total, faulted, breakdown
- Diagnostics: actionable hints (FAULT_NOT_FIRED, SERVICE_CRASHED, etc.)
- Full event trace with vector clocks
- **Matrix section** (when `fault_matrix()` tests are present): scenarios,
  faults, per-cell results, pass/fail counts

Human-readable output (logs, summaries) goes to stderr, keeping stdout
clean for piping to `jq`, LLM agents, or CI systems.

```bash
# Parse with jq
faultbox test spec.star --format json | jq '.tests[] | {name, result}'

# Check diagnostics
faultbox test spec.star --format json | jq '.tests[].diagnostics[]'

# Matrix results
faultbox test spec.star --format json | jq '.matrix'

# Scenario return values
faultbox test spec.star --format json | jq '.tests[] | {name, return_value}'
```

**Test naming conventions:**

| Source | Name pattern | Example |
|--------|-------------|---------|
| `def test_*()` | `test_<name>` | `test_happy_path` |
| `scenario(fn)` | `test_<fn_name>` | `test_order_flow` |
| `fault_scenario("x", ...)` | `test_<x>` | `test_order_db_down` |
| `fault_matrix(...)` | `test_matrix_<scenario>_<fault>` | `test_matrix_order_flow_db_down` |

**Debug output (`--debug`):**

Shows scenario return values after each test:

```
--- PASS: test_order_flow (12ms, seed=42) ---
  return: response(status=200, body="confirmed", duration_ms=8)
```

**Counterexample discovery (P-lang style):**

Run each test N times with auto-incrementing seeds (0..N-1). With `--show fail`,
only failing tests are printed. Each failure includes the seed for replay:

```bash
faultbox test faultbox.star --runs 1000 --show fail
# --- FAIL: test_flaky_network (215ms, seed=7) ---
#   reason: assert_true: expected 200 or 503, got 0
#   replay: faultbox test faultbox.star --test flaky_network --seed 7
```

**Dry-run mode (`--dry-run`):**

Validates the spec and shows the test plan without starting any services
or Docker containers. Useful for CI lint steps, macOS without Lima, and
fast feedback:

```bash
faultbox test faultbox.star --dry-run
# Dry-run: loaded faultbox.star
# Services: 3
# Tests: test_order_flow, test_health_check, test_matrix_order_flow_db_down, ...
# Fault matrix: 6 cells (2 scenarios × 3 faults)
```

**Container behavior:**

- **Auto-cleanup:** At the start of each test suite, Faultbox removes all
  stale containers and networks with the `faultbox-` prefix. No manual
  `docker rm -f` needed between runs.
- **Multi-process containers:** Containers with fork-based entrypoints
  (Java/shell, e.g., Confluent Kafka, Zookeeper) automatically fall back
  to no-seccomp mode. Fault injection is skipped for these services with a
  warning — they still work for topology, healthchecks, and protocol steps.
- **Image pull timeout:** Default 120 seconds. Large images (MySQL, Kafka)
  should pull within this window on most connections.

See [Spec Language Reference](spec-language.md) for `.star` file syntax.

---

### `faultbox init`

Generate a starter `.star` file for a service, from docker-compose, or set up
Claude Code integration.

```
faultbox init [flags] <binary>
faultbox init --from-compose [docker-compose.yml]
faultbox init --claude
faultbox init --vscode
```

| Flag | Default | Description |
|------|---------|-------------|
| `--name <name>` | `myapp` | Service name |
| `--port <port>` | `8080` | Port number |
| `--protocol http\|tcp` | `http` | Protocol type |
| `--output <file>` | stdout | Write to file instead of printing |
| `--from-compose [file]` | `docker-compose.yml` | Generate spec from docker-compose |
| `--claude` | | Set up Claude Code integration (commands + MCP config) |
| `--vscode` | | Generate VS Code autocomplete stubs |

**Examples:**

```bash
# From a binary
faultbox init --name orders --port 8080 ./order-svc
faultbox init --name db --port 5432 --protocol tcp ./db-svc
faultbox init --name api --output faultbox.star ./api-svc

# From docker-compose.yml
faultbox init --from-compose                               # auto-detect compose file
faultbox init --from-compose docker-compose.prod.yml       # specific file
faultbox init --from-compose --output faultbox.star         # write to file

# Claude Code integration
faultbox init --claude        # creates .claude/commands/ + .mcp.json

# VS Code autocomplete
faultbox init --vscode        # creates typings/ + .vscode/ stubs
```

**From compose:** Parses docker-compose.yml services, detects protocols from
image names and ports (postgres→5432, redis→6379, etc.), generates service
declarations with correct depends_on ordering, healthchecks, and a happy-path
test with `scenario()` registration.

**Claude integration:** Creates three slash commands (`/fault-test`,
`/fault-generate`, `/fault-diagnose`) and `.mcp.json` for automatic MCP
server connection.

---

### `faultbox inspect`

Examine a `.fb` bundle produced by `faultbox test`. Read-only — never
modifies the bundle, never refuses based on producer version (warns
instead). See [bundles.md](bundles.md) for the archive layout.

```
faultbox inspect <bundle.fb>                    # summary + file list
faultbox inspect <bundle.fb> <path-in-archive>  # dump one file to stdout
faultbox inspect <bundle.fb> --extract <dir>    # extract all to dir
```

**Summary mode** prints a scannable header (producer version, run
ID, seed, host OS/arch, Go toolchain), a pass/fail test list, and
the archive file list.

**Dump mode** writes one internal file to stdout — pipe to `jq`,
`less`, or a diff tool.

**Extract mode** writes every file to a directory, preserving the
`spec/`, `services/` substructure and setting `replay.sh` executable.

**Examples:**

```bash
faultbox inspect run-2026-04-22-42.fb
faultbox inspect run-*.fb manifest.json | jq '.summary'
faultbox inspect run-*.fb trace.json | jq '.tests[0].diagnostics'
faultbox inspect run-*.fb --extract ./unpacked/
```

**Version banner:** if the bundle was produced by a different
Faultbox version, `inspect` prints a one-line warning to stderr
before the summary. Major-version mismatch (`0.x` ↔ `1.x`) says
`replay will refuse`; minor/patch drift is informational only.

---

### `faultbox replay` (v0.10.0)

Re-run a `.fb` bundle. Reads the spec tree captured under `spec/`,
extracts to a temp dir, and re-invokes `faultbox test` with the
recorded seed so probabilistic faults reproduce.

```
faultbox replay <bundle.fb>                      # rerun every test
faultbox replay <bundle.fb> --test <name>        # rerun one test
faultbox replay <bundle.fb> --extract-only <dir> # extract spec/ but don't run
```

**Examples:**

```bash
faultbox replay run-2026-04-22-42.fb
faultbox replay run-*.fb --test test_fault_scenario
faultbox replay run-*.fb --extract-only ./debug/
```

**Version compatibility:**

| Bundle vs current | Behaviour |
|---|---|
| Same X.Y.Z | Silent, replays |
| Same major, minor/patch differs | Warns, replays |
| Major version differs (0.x → 1.x) | **Refuses** — install bundle's version, or use `--extract-only` |

Replay never emits its own `.fb` bundle (`--no-bundle` is implied)
since the user is reproducing an existing run, not creating new
evidence. Override by passing `--bundle <path>` if you do want one.

---

### `faultbox lock` (v0.10.0)

Pin the byte-exact identity of every container image the spec
references so two runs reach the same bytes. RFC-030.

```
faultbox lock [spec.star]              # generate / overwrite faultbox.lock
faultbox lock --check [spec.star]      # exit 0 if matches; 2 if drifted
faultbox lock --update [spec.star]     # explicit alias for the default
```

Spec defaults to `faultbox.star` in cwd. The lock file lives next
to the spec.

**Format** (`faultbox.lock`):

```json
{
  "schema_version": 1,
  "lock_version": "0.10.0",
  "generated_at": "2026-04-23T12:00:00Z",
  "images": {
    "mysql:8.0.32":   "sha256:abc...",
    "redis:7-alpine": "sha256:def..."
  }
}
```

**CI integration:**

```bash
faultbox lock --check                  # fail build if lock is stale
FAULTBOX_LOCK_STRICT=1 faultbox test   # fail tests if no lock present
```

`faultbox test` always loads the lock if present and prints
`Lock: faultbox.lock (faultbox <ver>, N images)` alongside the
`Seed:` line. With `FAULTBOX_LOCK_STRICT=1` set, a missing lock
file becomes a hard error — set this in CI to ensure every spec
has a committed lock.

**Examples:**

```bash
faultbox lock                          # write lock from cwd's spec
faultbox lock infra/specs/auth.star    # non-default spec path
faultbox lock --check                  # CI hook
```

**Out of scope for v0.10.0** (reserved fields in the schema):

- Stdlib content hash (Phase 3)
- Binary checksums for `service(binary=…)` (Phase 2)
- Strict-mode comparison wired into the image pull path (Phase 2)

The current preflight reports the lock's presence but doesn't yet
fail the test when image digests drift; that's coming in v0.10.1.
Use `faultbox lock --check` as a CI hook in the meantime.

---

### `faultbox diff`

Compare two normalized trace files. Returns exit code 0 if identical, 2 if different.

```
faultbox diff <trace1.norm> <trace2.norm>
```

**Example:**

```bash
faultbox test faultbox.star --normalize run1.norm
faultbox test faultbox.star --normalize run2.norm
faultbox diff run1.norm run2.norm
# traces are identical
```

---

### `faultbox generate`

Generate failure scenarios from registered `scenario()` functions.

```
faultbox generate [flags] <file.star>
```

| Flag | Description |
|------|-------------|
| `--output <file>` | Write all mutations to a single file (default: one file per scenario) |
| `--scenario <name>` | Generate only for this scenario |
| `--service <name>` | Generate only for this dependency |
| `--category <cat>` | Filter: `network`, `disk`, `all` (default: `all`) |
| `--dry-run` | List mutations without generating code |

**Default output:** one file per scenario, named `<scenario>.faults.star`.

**Examples:**

```bash
faultbox generate faultbox.star                            # per-scenario files
faultbox generate faultbox.star --output all-failures.star # single file
faultbox generate faultbox.star --scenario order_flow      # one scenario
faultbox generate faultbox.star --service db               # one dependency
faultbox generate faultbox.star --dry-run                  # preview
```

**How it works:**

1. Loads the `.star` file and finds all `scenario()` registrations
2. Analyzes the topology (services, dependencies, protocols)
3. Generates `fault_assumption()` definitions — one per unique fault mode
4. Generates a `fault_matrix()` call composing scenarios × assumptions
5. Generated files use `load()` to import topology and scenario functions

**Generated output format:**

```python
load("faultbox.star", "orders", "inventory", "order_flow")

# network faults
inventory_down = fault_assumption("inventory_down",
    target = orders,
    connect = deny("ECONNREFUSED"),
)

# disk faults
disk_eio = fault_assumption("disk_eio",
    target = inventory,
    write = deny("EIO"),
)

fault_matrix(
    scenarios = [order_flow],
    faults = [inventory_down, disk_eio],
)
```

Add `overrides=` to `fault_matrix()` for per-cell expected behavior.
Network partitions are generated as standalone `test_*` functions.

---

### `faultbox run`

Launch a binary under Faultbox's control with process isolation and optional
syscall-level fault injection.

```
faultbox run [flags] <binary> [args...]
```

**Arguments:**

| Argument | Description |
|---|---|
| `<binary>` | Path to the executable to run (must be absolute or relative to CWD) |
| `[args...]` | Arguments passed to the target binary |

Use `--` to separate Faultbox flags from the target's flags:

```bash
faultbox run --debug -- ./my-service --port 8080
```

---

## Flags

### `--fault "SPEC"`

Inject a fault on a specific syscall. Can be specified multiple times.

**Deny format (return an error):**

```
--fault "SYSCALL=ERRNO:PROBABILITY%[:PATH_GLOB][:TRIGGER]"
```

**Delay format (sleep then allow):**

```
--fault "SYSCALL=delay:DURATION:PROBABILITY%[:TRIGGER]"
```

| Part | Description | Example |
|---|---|---|
| `SYSCALL` | Linux syscall name to intercept | `openat`, `write`, `connect` |
| `ERRNO` | Error code to return (case-insensitive) | `EIO`, `ENOENT`, `ECONNREFUSED` |
| `delay` | Delay action keyword | `delay` (literal) |
| `DURATION` | Go duration string | `200ms`, `1s`, `500us` |
| `PROBABILITY%` | Chance the fault fires (0-100) | `100%` = always, `50%` = half, `1%` = rare |
| `PATH_GLOB` | *(Optional, deny only)* Glob pattern for file syscalls | `/data/*`, `/tmp/test-*` |
| `TRIGGER` | *(Optional)* Stateful trigger | `nth=3`, `after=5` |

**Triggers:**

| Trigger | Description | Example |
|---|---|---|
| *(omitted)* | Fire on every matching call | Default behavior |
| `nth=N` | Fire only on the Nth matching call (1-indexed) | `nth=3` = fail 3rd call only |
| `after=N` | Allow first N calls, fail all subsequent | `after=2` = first 2 succeed, rest fail |

Both `--fault "spec"` and `--fault="spec"` syntax are supported.

**Examples:**

```bash
# Fail every file open with "no such file"
faultbox run --fault "openat=ENOENT:100%" ./my-service

# Fail opens only under /data/
faultbox run --fault "openat=ENOENT:100%:/data/*" ./my-service

# Fail 20% of writes with I/O error
faultbox run --fault "write=EIO:20%" ./my-service

# Reject all outbound connections
faultbox run --fault "connect=ECONNREFUSED:100%" ./my-service

# Delay every connect() by 200ms (simulates slow network)
faultbox run --fault "connect=delay:200ms:100%" ./my-service

# Delay 20% of sends by 50ms
faultbox run --fault "sendto=delay:50ms:20%" ./my-service

# Combination: slow file opens + occasional disk errors
faultbox run \
  --fault "openat=delay:100ms:100%:/data/*" \
  --fault "write=EIO:5%" \
  ./my-service

# Combination: reject 30% of connections, delay the rest
faultbox run \
  --fault "connect=ECONNREFUSED:30%" \
  --fault "connect=delay:200ms:70%" \
  ./my-service

# Fail fsync after first 2 succeed (classic WAL durability test)
faultbox run --fault "fsync=EIO:100%:after=2" ./my-service

# Fail only the 3rd open under /data/
faultbox run --fault "openat=ENOENT:100%:/data/*:nth=3" ./my-service
```

#### Path Filtering

For file-related syscalls (`openat`, `mkdirat`, `unlinkat`, etc.), an optional
path glob can narrow which files are affected:

- **With path glob:** Only syscalls targeting paths matching the glob are faulted.
  System paths are NOT auto-excluded (the glob is your filter).
- **Without path glob:** All paths are faulted EXCEPT system paths (see below).

**System path exclusion** (automatic when no path glob is specified):

| Path prefix | Why excluded |
|---|---|
| `/lib/`, `/lib64/`, `/usr/lib/`, `/usr/lib64/` | Shared libraries (dynamic linker) |
| `/proc/`, `/sys/` | Virtual filesystems |
| `/dev/` | Device nodes |
| `/etc/ld.so.*` | Dynamic linker configuration |

This means `--fault "openat=ENOENT:100%"` now works safely — the dynamic linker
can still load shared libraries, and only application file opens are faulted.

#### Supported Syscalls

These are the syscalls that can be targeted with `--fault`. The syscall numbers
are architecture-specific (ARM64 and AMD64 are supported).

**File/IO:**

| Syscall | Description | Typical Use |
|---|---|---|
| `openat` | Open a file | Fail file access: `openat=ENOENT`, `openat=EACCES` |
| `read` | Read from fd | Corrupt reads: `read=EIO` |
| `write` | Write to fd | Fail writes: `write=EIO`, `write=ENOSPC` |
| `writev` | Write scatter/gather | Same as write (Go uses this for network I/O) |
| `readv` | Read scatter/gather | Same as read |
| `close` | Close fd | Rare: `close=EIO` (triggers on flush) |
| `fsync` | Flush to disk | Durability test: `fsync=EIO` |
| `mkdirat` | Create directory | `mkdirat=ENOSPC`, `mkdirat=EACCES` |
| `unlinkat` | Delete file/dir | `unlinkat=EPERM` |
| `faccessat` | Check permissions | `faccessat=EACCES` |
| `fstatat` | Stat a file | `fstatat=ENOENT` |
| `getdents64` | Read directory | `getdents64=EIO` |
| `readlinkat` | Read symlink | `readlinkat=ENOENT` |

**Network:**

| Syscall | Description | Typical Use |
|---|---|---|
| `connect` | Outbound TCP/UDP | `connect=ECONNREFUSED`, `connect=ETIMEDOUT` |
| `socket` | Create socket | `socket=ENOMEM` |
| `bind` | Bind to address | `bind=EADDRINUSE` |
| `listen` | Listen for connections | `listen=EADDRINUSE` |
| `accept` | Accept connection | `accept=ENOMEM` |
| `sendto` | Send data | `sendto=ECONNRESET`, `sendto=EPIPE` |
| `recvfrom` | Receive data | `recvfrom=ECONNRESET` |
| `getsockname` | Get socket address | Rarely faulted |

**Process/System:**

| Syscall | Description | Typical Use |
|---|---|---|
| `clone` | Fork/thread create | `clone=ENOMEM` |
| `execve` | Execute program | `execve=ENOENT` |
| `wait4` | Wait for child | Rarely faulted |
| `getpid` | Get process ID | Rarely faulted |
| `uname` | System info | Rarely faulted |
| `getrandom` | Random bytes | `getrandom=EAGAIN` |

**Memory:**

| Syscall | Description | Typical Use |
|---|---|---|
| `mmap` | Map memory | `mmap=ENOMEM` |
| `mprotect` | Set memory protection | `mprotect=ENOMEM` |
| `brk` | Adjust heap | `brk=ENOMEM` |

**Low-level (use with caution):**

| Syscall | Description | Note |
|---|---|---|
| `ioctl` | Device control | May break target in unexpected ways |
| `fcntl` | File descriptor control | May break target in unexpected ways |
| `futex` | Userspace locking | **Dangerous:** Go runtime depends on this |
| `ppoll` / `pselect6` | I/O multiplexing | May cause hangs |
| `sigaction` / `sigprocmask` / `sigreturn` | Signal handling | **Dangerous:** may crash target |

#### Supported Error Codes

**File/IO errors:**

| Errno | Meaning | Simulates |
|---|---|---|
| `ENOENT` | No such file or directory | Missing file, deleted config |
| `EACCES` | Permission denied | Wrong file permissions |
| `EPERM` | Operation not permitted | Privilege escalation blocked |
| `EIO` | I/O error | Disk failure, corrupted storage |
| `ENOSPC` | No space left on device | Full disk |
| `EROFS` | Read-only filesystem | Mounted read-only, immutable infra |
| `EEXIST` | File exists | Race condition on create |
| `ENOTEMPTY` | Directory not empty | Failed cleanup |
| `ENFILE` | Too many open files (system) | System-wide fd exhaustion |
| `EMFILE` | Too many open files (process) | Per-process fd exhaustion |
| `EFBIG` | File too large | Size limit hit |

**Network errors:**

| Errno | Meaning | Simulates |
|---|---|---|
| `ECONNREFUSED` | Connection refused | Target service down |
| `ECONNRESET` | Connection reset by peer | Dropped connection mid-request |
| `ECONNABORTED` | Connection aborted | Aborted handshake |
| `ETIMEDOUT` | Connection timed out | Network partition, slow peer |
| `ENETUNREACH` | Network unreachable | Network down, routing failure |
| `EHOSTUNREACH` | Host unreachable | DNS resolved but host down |
| `EADDRINUSE` | Address already in use | Port conflict |
| `EADDRNOTAVAIL` | Address not available | Bind to invalid interface |

**Generic errors:**

| Errno | Meaning | Simulates |
|---|---|---|
| `EINTR` | Interrupted system call | Signal during syscall |
| `EAGAIN` | Resource temporarily unavailable | Non-blocking I/O, try again |
| `ENOMEM` | Out of memory | Memory pressure, OOM |
| `EBUSY` | Device or resource busy | Locked resource |
| `EINVAL` | Invalid argument | Bad parameter to syscall |

#### How Fault Injection Works

Faultbox uses Linux's **seccomp user notifications** (`SECCOMP_RET_USER_NOTIF`)
to intercept syscalls with zero overhead on non-targeted syscalls:

```
1. Faultbox installs a BPF filter in the target process (before exec)
2. Target calls openat("/data/file", ...)
3. Kernel pauses the target, notifies Faultbox
4. Faultbox checks fault rules (in a goroutine per notification):
   a. File syscall? Read path from /proc/pid/mem
   b. Path glob set? Check path matches glob
   c. No glob? Check path against system exclusion list
   d. Match? Roll probability:
      - Deny rule → return errno immediately
      - Delay rule → sleep(duration), then allow
   e. No match? Allow (kernel handles normally)
5. Target resumes (sees either success, errno, or delayed success)
```

Only the syscalls you specify in `--fault` are intercepted. All others run at
full native speed with zero overhead.

#### Important Notes

- **Dynamic linker:** When no path glob is specified, system paths (`/lib/*`,
  `/usr/lib/*`, `/proc/*`, etc.) are automatically excluded from file-related
  faults. This allows `--fault "openat=ENOENT:100%"` to work safely without
  breaking shared library loading. Use a path glob for precise targeting.
- **Go runtime:** Faulting `futex`, `sigaction`, or `clone` can break the Go
  runtime's goroutine scheduler. Use with caution on Go targets.
- **TOCTOU:** Path arguments (e.g., the filename in `openat`) are read from the
  target's memory via `/proc/pid/mem`. The target could theoretically modify this
  memory between Faultbox's inspection and the kernel's execution. For fault
  injection this is harmless; for security-critical use it is not sufficient.

---

### `--log-format=FORMAT`

Control log output format.

| Value | Description |
|---|---|
| `console` | Colored, human-readable. Timestamps as `HH:MM:SS.mmm`, levels colored (green INF, yellow WRN, red ERR, gray DBG), component in brackets. |
| `json` | Structured JSON lines (one object per event). Each line is valid JSON with `time`, `level`, `msg`, `component`, plus event-specific fields. |
| *(omitted)* | Auto-detect: `console` if stderr is a TTY, `json` if piped. |

**Examples:**

```bash
# Force colored output even when piped
faultbox run --log-format=console ./my-service | tee output.log

# Force JSON for machine consumption
faultbox run --log-format=json ./my-service 2>events.jsonl

# Filter intercepted syscalls with jq
faultbox run --fault "openat=EIO:10%" --log-format=json ./my-service 2>&1 | \
  jq 'select(.msg == "syscall intercepted")'
```

**JSON log fields:**

| Field | Type | Description |
|---|---|---|
| `time` | string | ISO 8601 timestamp with nanoseconds |
| `level` | string | `DEBUG`, `INFO`, `WARN`, `ERROR` |
| `msg` | string | Event message |
| `component` | string | `engine` |
| `session_id` | string | Hex session identifier |
| `state` | string | Session state: `starting`, `running`, `stopped`, `failed` |
| `name` | string | Syscall name (on interception events) |
| `pid` | int | Target process PID |
| `decision` | string | `allow`, `allow (system path)`, `deny(errno description)`, or `delay(duration)` |
| `path` | string | File path (on file syscall interception events) |
| `exit_code` | int | Target's exit code (on completion) |
| `duration` | int | Session duration in nanoseconds |

---

### `--env KEY=VALUE`

Set an environment variable for the target process. Can be specified multiple
times. These are appended to the current environment.

```bash
faultbox run --env DB_URL=postgres://localhost/db --env PORT=8080 ./my-service
```

Both `--env KEY=VALUE` and `--env=KEY=VALUE` syntax are supported.

---

### `--fs-fault "SPEC"`

Convenience flag that maps filesystem operation names to the correct syscall(s).
Same syntax as `--fault` but with human-friendly operation names.

| fs-fault op | Maps to syscall(s) |
|---|---|
| `open` | `openat` |
| `read` | `read`, `readv` |
| `write` | `write`, `writev` |
| `sync` / `fsync` | `fsync` |
| `mkdir` | `mkdirat` |
| `delete` | `unlinkat` |
| `stat` | `fstatat` |

```bash
# These are equivalent:
faultbox run --fs-fault "sync=EIO:100%:after=2" ./my-service
faultbox run --fault "fsync=EIO:100%:after=2" ./my-service

# "write" expands to both write and writev:
faultbox run --fs-fault "write=EIO:10%" ./my-service
```

---

### `--debug`

Enable debug-level logging. Shows every intercepted syscall (including allowed
ones), internal state transitions, and detailed namespace/seccomp setup info.

Without `--debug`, only denied (faulted) syscalls are logged at INFO level.

```bash
faultbox run --debug --fault "openat=ENOENT:50%" ./my-service
```

---

### `faultbox self-update`

Update the faultbox binary to the latest release.

```
faultbox self-update
```

Downloads the latest release from GitHub, verifies the SHA-256 checksum,
and replaces the current binary in-place. Also updates `faultbox-shim`
if present in the release archive.

```bash
faultbox self-update          # update to latest
faultbox --version            # verify new version
```

---

### `faultbox mcp`

Start a Model Context Protocol (MCP) server on stdio for LLM agent integration.

```
faultbox mcp
```

The server exposes 6 tools over JSON-RPC 2.0:

| Tool | Description |
|------|-------------|
| `run_test` | Run all tests in a .star file, return structured JSON |
| `run_single_test` | Run a specific test by name |
| `list_tests` | Discover test functions in a .star file |
| `generate_faults` | Run the failure scenario generator |
| `init_from_compose` | Generate a .star spec from docker-compose.yml |
| `init_spec` | Generate a starter .star spec for a binary |

**Usage with Claude Code / Claude Desktop:**

```json
{
  "mcpServers": {
    "faultbox": {
      "command": "faultbox",
      "args": ["mcp"]
    }
  }
}
```

Or run `faultbox init --claude` to auto-configure.

---

### `faultbox --version`

Print the version and exit.

```bash
faultbox --version
# faultbox 0.7.0
```

---

### `faultbox recipes`

Browse the embedded standard recipe library (see [RFC-019](rfcs/0019-recipe-distribution.md)).
Recipes ship with the binary and are loaded via the `@faultbox/` prefix in specs.

#### `faultbox recipes list`

```
faultbox recipes list
```

Prints every stdlib recipe available in the installed binary along with the
canonical `load()` syntax.

```
Available stdlib recipes (load via @faultbox/recipes/<name>.star):
  cassandra
  clickhouse
  http2
  mongodb
  udp

Example:
  load("@faultbox/recipes/cassandra.star", "cassandra")
```

#### `faultbox recipes show <name>`

```
faultbox recipes show mongodb
```

Prints the source of a recipe file. Useful for:
- Understanding what a recipe injects before loading it
- Copying a recipe into your project as a starting point for customization:
  ```
  faultbox recipes show mongodb > recipes/mongodb-custom.star
  ```

---

## Exit Codes

### `faultbox test` / `faultbox diff`

| Code | Meaning |
|---|---|
| 0 | All tests passed / traces identical |
| 1 | Faultbox error (bad config, load failure, etc.) |
| 2 | One or more tests failed / traces differ |

### `faultbox run`

| Code | Meaning |
|---|---|
| 0 | Target exited successfully |
| 1 | Faultbox error (bad args, failed to start, etc.) |
| 1-125 | Target's own non-zero exit code (passed through) |
| 127 | Target could not be executed (e.g., shared library load failed) |
| 128+N | Target killed by signal N (e.g., 137 = SIGKILL) |

`faultbox run` faithfully propagates the target's exit code. If the target exits 42,
faultbox exits 42.

---

## Execution Modes

### Unified Launch

The target always runs via a re-exec shim that sets up both namespace isolation
and (optionally) seccomp filtering in a single launch path.

**Without `--fault`** — namespace isolation only:

| Namespace | Effect |
|---|---|
| PID | Target sees itself as PID 1 |
| NET | Empty network stack (no interfaces, no connectivity) |
| MNT | Private mount table (writes to `/tmp` don't affect host) |
| USER | Unprivileged — maps current user to root inside namespace |

```bash
faultbox run ./my-service
# Target sees: PID 1, no network, private filesystem
```

**With `--fault`** — namespace isolation + seccomp filter:

```bash
faultbox run --fault "write=EIO:10%" ./my-service
# Target sees: PID 1, no network, private filesystem
# AND write() fails 10% of the time with EIO
```

Both modes are always combined — you never lose isolation when adding faults.

---

## Environment

- **Requires Linux.** On macOS, use the Lima VM:
  ```bash
  make env-exec CMD="faultbox run ..."
  ```
  Running on macOS directly prints an error and exits 1.

- **No root required.** Namespace isolation uses `CLONE_NEWUSER` (unprivileged).
  Seccomp filter uses `PR_SET_NO_NEW_PRIVS` (no root needed).

- **Kernel 5.6+** required for fault injection (`pidfd_getfd`).
  Kernel 5.19+ recommended for Go targets (`SECCOMP_FILTER_FLAG_WAIT_KILLABLE_RECV`
  avoids SIGURG spin loops).

---

## Troubleshooting

### Container name conflicts

```
Error: Conflict. The container name "/faultbox-postgres" is already in use
```

Faultbox auto-cleans stale containers at suite start (v0.5.0+). If you
see this after upgrading, run once manually:

```bash
docker rm -f $(docker ps -aq --filter name=faultbox) 2>/dev/null
docker network rm faultbox-net 2>/dev/null
```

### Multi-process container warning

```
WARN: seccomp listener failed — falling back to no-seccomp mode
WARN: fault rules will NOT apply — service running without seccomp
```

Containers with shell/Java entrypoints (Confluent Kafka, Zookeeper,
Cassandra, Elasticsearch) can't use seccomp-notify because the
entrypoint forks. The container runs normally but **fault injection is
skipped** for this service.

**What works:** healthchecks, protocol steps, event sources, topology.
**What doesn't work:** `fault(service, write=deny(...))` — the fault
rule is installed but never fires.

**Workaround:** Use protocol-level faults instead (these use a proxy,
not seccomp):

```python
# Instead of: fault(kafka, write=deny("EIO"), run=scenario)
# Use:        fault(kafka.broker, drop(topic="orders"), run=scenario)
```

### Image pull timeout

```
Error: pull image postgres:16 (timeout 120s): context deadline exceeded
```

Large images may exceed the 120s default on slow connections. Pre-pull
images before running tests:

```bash
docker pull postgres:16
docker pull confluentinc/cp-kafka:7.6
faultbox test faultbox.star  # images already cached
```

### Seccomp not available

```
Error: install seccomp filter: operation not permitted
```

Requires Linux kernel 5.6+ with `CONFIG_SECCOMP_FILTER=y`. Check:

```bash
grep SECCOMP /boot/config-$(uname -r)
```

On macOS, use the Lima VM — seccomp is not available natively.
