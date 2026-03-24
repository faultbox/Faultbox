# Faultbox CLI Reference

## Commands

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

**Format:**

```
--fault "SYSCALL=ERRNO:PROBABILITY%"
```

| Part | Description | Example |
|---|---|---|
| `SYSCALL` | Linux syscall name to intercept | `openat`, `write`, `connect` |
| `ERRNO` | Error code to return (case-insensitive) | `EIO`, `ENOENT`, `ECONNREFUSED` |
| `PROBABILITY%` | Chance the fault fires (0-100) | `100%` = always, `50%` = half, `1%` = rare |

Both `--fault "spec"` and `--fault="spec"` syntax are supported.

**Examples:**

```bash
# Fail every file open with "no such file"
faultbox run --fault "openat=ENOENT:100%" ./my-service

# Fail 20% of writes with I/O error
faultbox run --fault "write=EIO:20%" ./my-service

# Reject all outbound connections
faultbox run --fault "connect=ECONNREFUSED:100%" ./my-service

# Multiple faults at once
faultbox run \
  --fault "openat=ENOSPC:10%" \
  --fault "write=EIO:5%" \
  --fault "connect=ETIMEDOUT:30%" \
  ./my-service
```

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
4. Faultbox checks fault rules:
   - Match? Roll probability → deny with errno OR allow
   - No match? Allow (kernel handles normally)
5. Target resumes (sees either success or errno)
```

Only the syscalls you specify in `--fault` are intercepted. All others run at
full native speed with zero overhead.

#### Important Notes

- **Dynamic linker:** 100% fault on `openat` will prevent the dynamic linker
  from loading shared libraries (`libc.so.6`), killing the target before `main()`.
  Use lower probabilities or target specific paths (future feature).
- **Go runtime:** Faulting `futex`, `sigaction`, or `clone` can break the Go
  runtime's goroutine scheduler. Use with caution on Go targets.
- **TOCTOU:** Path arguments (e.g., the filename in `openat`) are read from the
  target's memory via `/proc/pid/mem`. The target could theoretically modify this
  memory between Faultbox's inspection and the kernel's execution. For fault
  injection this is harmless; for security-critical use it is not sufficient.
- **Namespaces:** When `--fault` is specified, the target runs without namespace
  isolation (PID/NET/MNT/USER). This will be combined in a future step.

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
| `decision` | string | `allow` or `deny(errno description)` |
| `exit_code` | int | Target's exit code (on completion) |
| `duration` | int | Session duration in nanoseconds |

---

### `--debug`

Enable debug-level logging. Shows every intercepted syscall (including allowed
ones), internal state transitions, and detailed namespace/seccomp setup info.

Without `--debug`, only denied (faulted) syscalls are logged at INFO level.

```bash
faultbox run --debug --fault "openat=ENOENT:50%" ./my-service
```

---

## Exit Codes

| Code | Meaning |
|---|---|
| 0 | Target exited successfully |
| 1 | Faultbox error (bad args, failed to start, etc.) |
| 1-125 | Target's own non-zero exit code (passed through) |
| 127 | Target could not be executed (e.g., shared library load failed) |
| 128+N | Target killed by signal N (e.g., 137 = SIGKILL) |

Faultbox faithfully propagates the target's exit code. If the target exits 42,
faultbox exits 42.

---

## Execution Modes

### Namespace Isolation (default, no `--fault`)

When no fault rules are specified, the target runs in isolated Linux namespaces:

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

### Fault Injection (with `--fault`)

When fault rules are specified, the target runs with a seccomp filter attached.
Namespace isolation is currently disabled in this mode (will be combined in a
future step).

```bash
faultbox run --fault "write=EIO:10%" ./my-service
# Target runs with seccomp filter, write() fails 10% of the time
```

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
