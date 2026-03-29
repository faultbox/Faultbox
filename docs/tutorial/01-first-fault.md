# Chapter 1: Your First Fault

In this chapter you'll inject your first syscall fault into a running process
and see what happens when the operating system lies to your program.

## What is Faultbox?

Faultbox intercepts system calls using Linux's **seccomp-notify** mechanism.
When your program calls `write()`, `connect()`, or `read()`, the kernel pauses
the program and asks Faultbox: "should I allow this?" Faultbox can:

- **Allow** — let the syscall proceed normally
- **Deny** — return an error code (EIO, ENOSPC, ECONNREFUSED, ...)
- **Delay** — wait, then allow

This isn't mocking. Your program runs for real. The filesystem, network, and
kernel are all real. Faultbox only changes what the kernel *returns*.

## Build

```bash
make build
# For Lima VM:
make demo-build
```

This creates `bin/faultbox` (or `bin/linux-arm64/faultbox` for Lima).

## The target program

`poc/target/main.go` is a simple Go program that:
1. Writes a file to `/tmp/faultbox-target-test`
2. Reads it back
3. Makes an HTTP request to `httpbin.org`

Build it:
```bash
go build -o /tmp/target ./poc/target/
```

Run it normally:
```bash
/tmp/target
```

You'll see output like:
```
PID: 12345
filesystem: write+read OK (2ms)
network: HTTP 200 OK (150ms)
```

## Inject a write fault

Now run it through Faultbox, denying every `write()` syscall with EIO (I/O error):

```bash
faultbox run --fault "write=EIO:100%" /tmp/target
```

The program fails! `write()` returns EIO, and the file operation errors out.
Faultbox intercepted the syscall at the kernel level — the program had no way
to avoid it.

## Probabilistic faults

Real failures aren't 100%. Make writes fail 30% of the time:

```bash
faultbox run --fault "write=EIO:30%" /tmp/target
```

Run it several times. Sometimes it works, sometimes it fails. This is how
real disk errors behave — intermittent and unpredictable.

## Path-targeted faults

Deny opens only for files under `/data/`:

```bash
faultbox run --fault "openat=ENOENT:100%:/data/*" /tmp/target
```

The target writes to `/tmp/` (not `/data/`), so it succeeds. Path targeting
lets you fault specific files without breaking the whole filesystem.

## Delay faults

Slow down every write by 500ms:

```bash
faultbox run --fault "write=delay:500ms:100%" /tmp/target
```

The filesystem operation now takes >500ms. Delay faults simulate slow disks,
overloaded storage, or network congestion.

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
    |                             |                           |
```

The key insight: this works on **any binary**. C, Rust, Java, Python — anything
that makes syscalls. No code changes, no special libraries, no mocking frameworks.

## What you learned

- `faultbox run` wraps a binary with syscall interception
- `--fault "syscall=ERRNO:PROBABILITY%"` denies syscalls
- `--fault "syscall=delay:DURATION:PROBABILITY%"` slows syscalls
- Path globs (`:/data/*`) target specific files
- seccomp-notify works at the kernel level — programs can't bypass it

## Exercises

1. **Network fault**: Run the target with `--fault "connect=ECONNREFUSED:100%"`.
   What happens to the HTTP request? Does the program crash or handle the error?

2. **Slow network**: Run with `--fault "connect=delay:2s:100%"`. How long does
   the HTTP request take now? (The target has a 5s timeout — will it succeed?)

3. **Combined faults**: Run with two faults at once:
   ```bash
   faultbox run --fault "write=EIO:50%" --fault "connect=delay:1s:100%" /tmp/target
   ```
   What's the combined effect?

4. **Explore errno values**: Try `ENOSPC` (disk full), `EPERM` (permission denied),
   `ETIMEDOUT` (timeout). Which ones does the target handle gracefully?
