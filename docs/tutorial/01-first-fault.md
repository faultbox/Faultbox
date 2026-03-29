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

You'll see:
```
PID: 12345
filesystem: write+read OK (2ms)
network: HTTP 200 OK (150ms)
```

Everything works. Now break it.

## Inject a write fault

**Linux:**
```bash
bin/faultbox run --fault "write=EIO:100%" bin/target
```

**macOS (Lima):**
```bash
vm bin/linux-arm64/faultbox run --fault "write=EIO:100%" bin/linux-arm64/target
```

The program fails! Every `write()` syscall returns EIO (I/O error). The file
operation errors out because the kernel told the program "I/O error" — and
the program believed it.

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

The target writes to `/tmp/` (not `/data/`), so it succeeds. **The intuition:**
production failures are usually localized — one volume fails, not all storage.
Path targeting lets you simulate exactly that.

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

1. **Network fault**: Run with `--fault "connect=ECONNREFUSED:100%"`.
   What happens to the HTTP request? Does the program crash or handle it?

2. **Slow network**: Run with `--fault "connect=delay:2s:100%"`. The target
   has a 5s timeout — does the request succeed? What about `delay:6s`?

3. **Combined faults**:
   ```bash
   bin/faultbox run --fault "write=EIO:50%" --fault "connect=delay:1s:100%" bin/target
   ```
   (On macOS, use `vm bin/linux-arm64/faultbox ...` with `bin/linux-arm64/target`)

   What's the combined effect? Which fails first?

4. **Explore errno values**: Try `ENOSPC` (disk full), `EPERM` (permission
   denied), `ETIMEDOUT` (timeout). For each one, ask: "would my production
   service handle this correctly?"
