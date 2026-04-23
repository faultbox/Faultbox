# Seccomp Cheatsheet — Go Operations → Syscalls

Faultbox intercepts at the syscall level. When you write
`fault(svc, write=deny("EIO"))`, you're saying *"intercept the
write syscall family on this service."* The customer report
(FB §2.1 #6) called out that figuring out which Go stdlib operation
maps to which syscall is hours of grep-and-strace. This page is the
shortcut.

> **Ground truth.** Go's syscall surface depends on the runtime
> version (changes between minor releases) and arch. When in doubt,
> run `strace -fce trace=%network ./your-binary` and confirm. The
> tables below cover Go 1.22+ on Linux/amd64 and arm64; the families
> Faultbox auto-expands smooth over the differences.

## How family expansion works

`write=deny("EIO")` doesn't only intercept `write(2)`. The runtime
expands to the full family because Go programs use them
interchangeably depending on buffer shape and runtime mood:

| You write | Faultbox intercepts |
|---|---|
| `write` | `write`, `writev`, `pwrite64` |
| `read` | `read`, `readv`, `pread64` |
| `open` | `open`, `openat` (Go uses openat universally) |
| `sendto` | `sendto`, `sendmsg` (and `sendmmsg` where present) |
| `recvfrom` | `recvfrom`, `recvmsg` |
| `fsync` | `fsync`, `fdatasync` |

Family names (`write`, `read`, `open`, `sendto`, `recvfrom`, `fsync`)
are canonicals — typing one of them in a fault rule pulls in the
whole expansion. Listing a single sub-syscall (e.g. `pwrite64`) only
intercepts that one.

## Network operations

### TCP socket lifecycle

| Go operation | Syscalls (in order) |
|---|---|
| `net.Dial("tcp", addr)` | `socket`, `connect` |
| `conn.Write(buf)` | `write` (small) or `writev` (split-buffer) |
| `conn.Read(buf)` | `read` (or `readv`) |
| `conn.Close()` | `shutdown`, `close` |
| Client-side TLS handshake | `read`, `write` (handshake bytes), `epoll_pwait` |

**To fault outbound TCP connect:** `connect=deny("ECONNREFUSED")`.
Family is `connect` only; no expansion needed.

**To fault outbound TCP send:** `write=deny("EIO")` or, if you only
care about message sockets (gRPC, MongoDB), `sendto=deny("EIO")`
which expands to `sendto + sendmsg`.

### UDP

| Go operation | Syscalls |
|---|---|
| `net.ListenPacket("udp", …)` | `socket`, `bind` |
| `conn.WriteTo(buf, addr)` | `sendto` (or `sendmsg` for batched) |
| `conn.ReadFrom(buf)` | `recvfrom` (or `recvmsg`) |

UDP "denies" land via `sendto=deny("ECONNREFUSED")`.

### HTTP

`net/http` rides on the TCP table above. `http.Get(url)` performs
DNS (see below) → `socket` → `connect` → TLS handshake (`read`/`write`
loop) → request `write` → response `read` → `close`. To fault HTTP
specifically — say, "fail every POST" — use the protocol-level proxy
(RFC-024) instead of syscall faults; it gives you method/path-aware
matching.

### DNS

Go's resolver has two modes — `cgo` (uses libc) and `pure Go` (custom).
The pure-Go resolver is the default in modern releases.

| Resolver | Syscalls observed |
|---|---|
| Pure-Go | `connect` to DNS server (UDP 53), `sendto`, `recvfrom` |
| cgo | `connect` (UDP/TCP), plus libc internals via the standard syscalls |

To "break DNS" for a service, fault `connect=deny("ENETUNREACH")`
filtered to UDP port 53 (currently a manual filter; full DNS-aware
faults are a future RFC).

## Filesystem operations

| Go operation | Syscalls |
|---|---|
| `os.Open(path)` | `openat` (Go uses `openat` even when you write `open`) |
| `os.Create(path)` | `openat` (with `O_CREAT`), maybe `fchown` |
| `f.Write(buf)` | `write` |
| `f.Sync()` | `fsync` (plus `fdatasync` in some paths) |
| `f.Close()` | `close` |
| `os.Remove(path)` | `unlinkat` |
| `os.Rename(old, new)` | `renameat2` (or `rename` on older kernels) |

**To fault file writes:** `write=deny("EIO")` covers `write + writev
+ pwrite64`. WAL-style append-and-fsync flows need both `write` and
`fsync` faulted to fully simulate disk failure — typical pattern:

```python
ops = {"persist": op(syscalls = ["write", "fsync"], path = "/data/*.wal")},
```

then `fault(svc, persist = deny("EIO"))`.

## Process / signal operations

| Go operation | Syscalls |
|---|---|
| `os.Exit(n)` | `exit_group(n)` |
| `runtime.SetFinalizer` | none observable |
| Signal handling (`signal.Notify`) | `rt_sigprocmask`, `rt_sigaction` |
| `time.Sleep(d)` | `nanosleep` (or `clock_nanosleep`) |
| `runtime.Gosched` | `sched_yield` (rare) |

Most spec authors don't fault these directly; they're listed for
debugging — when a trace shows a syscall you don't recognise,
this table is the first stop.

## Per-protocol quick lookup

| Protocol family | Syscalls usually faulted |
|---|---|
| HTTP / HTTP/2 / gRPC client send | `write` (covers writev/pwrite64) |
| HTTP / HTTP/2 / gRPC client recv | `read` |
| Datagram protocols (DNS, syslog) | `sendto`, `recvfrom` |
| Database client (Postgres, MySQL) connect | `connect` |
| Database write path | `write` |
| Database fsync (WAL, commit) | `fsync` |
| Kafka producer send | `write` (or `sendto` on some configurations) |
| Kafka consumer receive | `read` |
| Cache (Redis, Memcached) | `connect`, `read`, `write` |

## Debugging when a fault doesn't fire

`v0.9.4` introduced `fault_zero_traffic` events (surfaced to terminal
in v0.9.7). If you see one for your fault, the rule installed but
matched no syscalls during the fault window. Three causes ranked by
frequency:

1. **The SUT cached the upstream connection** — first request
   established it, subsequent requests reuse the open TCP socket.
   `connect` won't fire again. Fault `write` instead, or use the
   data-path proxy (RFC-024) for protocol-level faults.
2. **You faulted a syscall the SUT doesn't actually use** — e.g.
   `fault(svc, sendto=deny(...))` against a service that uses plain
   `write` over a stream socket. Add the family canonical or both.
3. **The SUT's traffic happens before/after your fault window** —
   client-side request batching, late shutdown flushes. Widen the
   window with `fault_start`/`fault_stop` instead of the
   block-scoped `fault(..., run=…)`.

## Debugging strategy

When a Go binary's syscall pattern is unclear, attach:

```sh
strace -fce trace=%network -p $(pgrep your-app) 2>&1 | head
strace -fce trace=write,read,fsync,openat -p $(pgrep your-app)
```

The `%network` filter set covers connect/send/recv variants. Look
for the syscalls Go actually emits and put **those** into the
Faultbox rule.

## See also

- [errno-reference.md](errno-reference.md) — full list of error codes
  the runtime accepts (`EIO`, `ECONNREFUSED`, `EPIPE`, …).
- [spec-language.md §named-operations](spec-language.md) — `ops=` for
  protocol-aware naming so spec authors don't have to think in
  syscalls at all.
- [Linux syscall man-pages online](https://man7.org/linux/man-pages/dir_section_2.html)
  — authoritative behaviour reference for any syscall you're
  uncertain about.
