# Chapter 1: Your First Fault

**Duration:** 15 minutes
**Prerequisites:** [Chapter 0 (Setup)](00-setup.md) completed

## Goals & Purpose

Every program trusts the operating system. When your code calls `write()`,
it assumes the bytes reach the disk. When it calls `connect()`, it assumes
the network is there. But in production, these assumptions break: disks
fill up, networks partition, I/O errors corrupt data.

**The question you should be asking:** *"What happens to my program when
the OS returns an error it doesn't expect?"*

Most teams discover the answer in production — at 3am, during a traffic
spike. Faultbox lets you discover it now, on your laptop.

In this chapter you'll build the core intuition: **the OS is an API, and
like any API, it can return errors**. Faultbox intercepts that API at the
kernel level, so your program has no way to avoid or detect the interception.
This is not mocking — it's real.

## How it works

```
Your program                    Kernel                     Faultbox
    |                             |                           |
    |-- write(fd, buf, n) ------->|                           |
    |                             |-- seccomp notification -->|
    |                             |                           |-- check rules
    |                             |                           |-- deny: EIO
    |                             |<-- return DENY(EIO) ------|
    |<-- errno = EIO -------------|                           |
```

This works on **any binary** — Go, Rust, C, Java, Python. No code changes,
no special libraries. The kernel is the interception point.

## Run the target program normally

The `target` binary is a simple Go program that writes a file, reads it back,
and makes an HTTP request.

**Linux:**
```bash
bin/target
```

**macOS (Lima):**
```bash
vm bin/linux-arm64/target
```

You'll see faultbox engine logs followed by the target's output:
```
... [engine] target started  pid=50318
=== Faultbox PoC Target ===
PID: 1
FS: wrote and read 14 bytes (took 78us)
net failed: ... connect: network is unreachable (took 2ms)
=== Target done ===
... [engine] session completed  exit_code=0
```

> **Why "network is unreachable"?** This is NOT a fault injection.
> `faultbox run` creates an **isolated network namespace** for the target
> process (PID, mount, and network are all sandboxed). The target has no
> network access — only loopback. This is a safety feature: the target
> can't make real external calls during testing.
>
> The filesystem still works because it uses the mount namespace which
> shares `/tmp/`. The network error is normal and expected — ignore it
> for now. In later chapters, we'll inject network faults explicitly
> with `connect=deny("ECONNREFUSED")`.

The filesystem write+read succeeded. Now break it.

## Inject a write fault

**Linux:**
```bash
bin/faultbox run --fault "write=EIO:100%" bin/target
```

**macOS (Lima):**
```bash
vm bin/linux-arm64/faultbox run --fault "write=EIO:100%" bin/linux-arm64/target
```

Now look at the output:
```
... [engine] seccomp filter installed  syscalls=[64]
... [engine] target started  pid=50274  listener_fd=8
... [engine] syscall intercepted  name=write  decision=deny(input/output error)
... [engine] syscall intercepted  name=write  decision=deny(input/output error)
... [engine] syscall intercepted  name=write  decision=deny(input/output error)
... [engine] syscall intercepted  name=write  decision=deny(input/output error)
... [engine] target exited with error  exit_code=1
```

Notice what happened:
- The seccomp filter was installed for syscall 64 (`write` on arm64)
- **Four** write syscalls were intercepted and denied with EIO
- The target exited with error code 1
- **The target's own output is missing** — it tried to `write()` to stdout
  but that write was also denied! The program couldn't even print its error.

This demonstrates a key point: `write=EIO:100%` denies ALL writes — to files,
to stdout, to network sockets. The program had no way to report what went wrong
because the reporting mechanism (stdout) was itself broken.

**Why this matters:** In production, if your disk I/O fails, can your service
still log the error? Can it still respond to healthchecks? A blanket write
failure exposes these dependencies.

**Why this matters:** Your program's error handling for disk I/O is now
testable. Does it retry? Crash? Corrupt state? Log the error? You can
answer these questions before production does.

## Probabilistic faults

Real failures are intermittent. Make writes fail 30% of the time:

**Linux:**
```bash
bin/faultbox run --fault "write=EIO:30%" bin/target
```

**macOS (Lima):**
```bash
vm bin/linux-arm64/faultbox run --fault "write=EIO:30%" bin/linux-arm64/target
```

Run it several times. Sometimes it works, sometimes it fails. This is how
real disk errors behave. **The intuition:** if your error handling only
works when failures are 100%, it might not work when they're 5%.

## Path-targeted faults

Deny opens only for files under `/data/`:

**Linux:**
```bash
bin/faultbox run --fault "openat=ENOENT:100%:/data/*" bin/target
```

**macOS (Lima):**
```bash
vm bin/linux-arm64/faultbox run --fault "openat=ENOENT:100%:/data/*" bin/linux-arm64/target
```

The target writes to `/tmp/` (not `/data/`), so the filesystem test succeeds.
(You'll still see "network is unreachable" — that's the namespace sandbox,
not your fault rule.) **The intuition:** production failures are usually
localized — one volume fails, not all storage. Path targeting simulates
exactly that.

> **fd→path resolution:** Path filtering works for both file-path syscalls
> (`openat`) and fd-based syscalls (`write`, `read`, `fsync`). For fd-based
> syscalls, Faultbox resolves the file descriptor to a path via `/proc/PID/fd/N`,
> so `--fault "write=EIO:100%:/tmp/faultbox*"` correctly targets only writes
> to files matching that glob. System paths (libc, ld-linux, /proc, etc.)
> are automatically excluded.

## Delay faults

Slow down every write by 500ms:

**Linux:**
```bash
bin/faultbox run --fault "write=delay:500ms:100%" bin/target
```

**macOS (Lima):**
```bash
vm bin/linux-arm64/faultbox run --fault "write=delay:500ms:100%" bin/linux-arm64/target
```

The filesystem operation now takes >500ms. **The intuition:** slow I/O is
often worse than failed I/O. A timeout at 5s might mask a 4.9s delay that
cascades into downstream timeouts. Delays let you find these problems.

## What you learned

- `faultbox run` wraps any binary with syscall interception
- `--fault "syscall=ERRNO:PROB%"` denies syscalls with specific errors
- `--fault "syscall=delay:DURATION:PROB%"` introduces latency
- Path globs (`:/data/*`) target specific files
- seccomp-notify works at the kernel level — no code changes needed
- **The mental model:** think of every syscall as an API call that can fail

## What's next

Running `faultbox run` with manual flags works for exploration, but it doesn't
scale. You need:

- **Repeatable tests** — run the same scenario every time, in CI
- **Multi-service topologies** — your API depends on a database, which depends on storage
- **Assertions** — not just "did it crash?" but "did it return the right error code?"

Chapter 2 introduces Starlark spec files — a way to codify your system
topology and test scenarios as code.

## Exercises

> Note: `faultbox run` isolates the network namespace, so network-related
> exercises won't show interesting results here. We'll test network faults
> in Chapter 3 with multi-service topologies where services connect to each
> other on localhost.

1. **Disk full**: Run with `--fault "write=ENOSPC:100%"`. What errno message
   do you see in the engine logs? How is it different from EIO?

2. **Slow filesystem**: Run with `--fault "write=delay:1s:100%"`. How long
   does the session take? Now try `delay:3s`. Does the target handle the
   slowdown or does it time out?

3. **Permission denied**: Run with `--fault "openat=EPERM:100%"`. The target
   can't open any files. How many `openat` syscalls get denied? (Count the
   `deny(operation not permitted)` lines.) Are they all for `/tmp/faultbox-target-test`
   or are there other files?

4. **Selective denial**: Run with `--fault "openat=ENOENT:100%:/tmp/faultbox*"`.
   The glob targets only the test file. Does the target still run? What about
   its other operations? Now try `--fault "write=EIO:100%:/tmp/faultbox*"` —
   does path filtering work for `write` the same way as `openat`? (Yes — Faultbox
   resolves fd→path via `/proc`, so write path targeting works.)
