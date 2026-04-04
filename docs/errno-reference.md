# Errno Reference

When injecting faults with `deny()`, you specify an errno — the error code
the kernel returns to the target process. This reference lists the most
useful errnos for fault injection testing, grouped by failure scenario.

## Quick reference

| Errno | Code | Meaning | Common use |
|-------|------|---------|------------|
| `EIO` | 5 | Input/output error | Disk corruption, hardware failure |
| `ENOSPC` | 28 | No space left on device | Disk full |
| `EROFS` | 30 | Read-only file system | Mounted read-only, immutable volume |
| `ENOENT` | 2 | No such file or directory | Missing file, deleted config |
| `EACCES` | 13 | Permission denied | Wrong file permissions |
| `EPERM` | 1 | Operation not permitted | Missing capability, security policy |
| `ECONNREFUSED` | 111 | Connection refused | Service down, port not listening |
| `ECONNRESET` | 104 | Connection reset by peer | Remote service crashed mid-request |
| `ETIMEDOUT` | 110 | Connection timed out | Network unreachable, firewall drop |
| `EHOSTUNREACH` | 113 | No route to host | Network partition, DNS failure |
| `ENETUNREACH` | 101 | Network is unreachable | Interface down, routing failure |
| `EAGAIN` | 11 | Resource temporarily unavailable | Socket buffer full, non-blocking I/O |
| `ENOMEM` | 12 | Out of memory | Memory pressure, OOM conditions |
| `EMFILE` | 24 | Too many open files | File descriptor exhaustion |
| `ENFILE` | 23 | Too many open files in system | System-wide fd limit |
| `EEXIST` | 17 | File exists | Lock file contention, create-exclusive |
| `ENOTEMPTY` | 39 | Directory not empty | Cleanup failure |
| `ENOSYS` | 38 | Function not implemented | Missing kernel feature, seccomp block |

> **Note:** Errno codes shown are for Linux (amd64/arm64). They're the same
> across architectures for the common ones listed here.

## Disk & storage failures

### EIO — I/O error
```python
fault(db, write=deny("EIO"), run=scenario)
```
**Simulates:** Disk corruption, bad sectors, SAN disconnection, NFS timeout.
The most generic I/O error — the storage layer failed but doesn't say why.

**What to test:**
- Does the service retry or fail fast?
- Is the error surfaced to the caller (not swallowed)?
- Does partial write leave corrupted state?

### ENOSPC — No space left on device
```python
fault(db, write=deny("ENOSPC"), run=scenario)
```
**Simulates:** Disk full, volume quota exceeded, WAL growth beyond capacity.
One of the most common production failures — logs or data fill the disk.

**What to test:**
- Does the service return a meaningful error (not just "internal error")?
- Can the service still respond to healthchecks?
- Does it stop accepting writes gracefully?

### EROFS — Read-only file system
```python
fault(db, write=deny("EROFS"), run=scenario)
```
**Simulates:** Filesystem remounted read-only after corruption detection,
immutable container layers, read-only volume mount.

**What to test:**
- Does the service distinguish "read-only" from "broken"?
- Can it still serve read requests?

## File access failures

### ENOENT — No such file or directory
```python
fault(db, openat=deny("ENOENT"), run=scenario)
```
**Simulates:** Missing config file, deleted data directory, unmounted volume,
symlink target removed.

**What to test:**
- Does the service fail with a clear error message naming the missing file?
- Does it retry or fail immediately?

### EACCES — Permission denied
```python
fault(db, openat=deny("EACCES"), run=scenario)
```
**Simulates:** Wrong file ownership after deployment, restrictive SELinux/AppArmor
policy, missing group membership.

**What to test:**
- Does the error message mention permissions (not just "failed to open")?
- Can the service recover if permissions are fixed?

### EPERM — Operation not permitted
```python
fault(db, openat=deny("EPERM"), run=scenario)
```
**Simulates:** Missing Linux capability (e.g., `CAP_NET_BIND_SERVICE`),
seccomp policy blocking the operation, mandatory access control denial.

> **EACCES vs EPERM:** `EACCES` is "you don't have permission for this
> specific resource." `EPERM` is "you're not allowed to do this operation
> at all." In practice, many programs don't distinguish them.

## Network failures

### ECONNREFUSED — Connection refused
```python
fault(api, connect=deny("ECONNREFUSED"), run=scenario)
```
**Simulates:** Target service not running, port not listening, service
crashed during deployment.

**What to test:**
- Does the caller return 503 (not 500)?
- Does it retry with backoff?
- Does the error message name the target service?

### ECONNRESET — Connection reset by peer
```python
fault(api, read=deny("ECONNRESET"), run=scenario)
```
**Simulates:** Remote service crashed mid-response, load balancer killed
the connection, TCP RST from firewall.

**What to test:**
- Does the caller handle partial reads?
- Does it retry the full request (idempotent) or fail?

### ETIMEDOUT — Connection timed out
```python
fault(api, connect=deny("ETIMEDOUT"), run=scenario)
```
**Simulates:** Firewall silently dropping packets (no RST), network
congestion, DNS resolution timeout.

> **Tip:** For testing timeout behavior, `delay("5s")` is often more
> realistic than `deny("ETIMEDOUT")`. A deny returns instantly — a real
> timeout makes the caller wait.

### EHOSTUNREACH — No route to host
```python
fault(api, connect=deny("EHOSTUNREACH"), run=scenario)
```
**Simulates:** Network partition, host down, routing table misconfiguration.

### ENETUNREACH — Network is unreachable
```python
fault(api, connect=deny("ENETUNREACH"), run=scenario)
```
**Simulates:** Interface down, default route missing, VPN disconnected.

## Resource exhaustion

### EAGAIN — Resource temporarily unavailable
```python
fault(db, write=deny("EAGAIN"), run=scenario)
```
**Simulates:** Socket send buffer full, non-blocking I/O would block,
file lock temporarily held by another process.

**What to test:**
- Does the caller retry?
- Is there a retry limit to prevent infinite loops?

### ENOMEM — Out of memory
```python
fault(db, write=deny("ENOMEM"), run=scenario)
```
**Simulates:** Memory pressure, `mmap` failure, large allocation rejected.

### EMFILE — Too many open files
```python
fault(db, openat=deny("EMFILE"), run=scenario)
```
**Simulates:** File descriptor exhaustion in the process. Common when
connection pools or file handles leak.

**What to test:**
- Does the service report fd exhaustion clearly?
- Can it still handle healthcheck requests?

### ENFILE — Too many open files in system
```python
fault(db, openat=deny("ENFILE"), run=scenario)
```
**Simulates:** System-wide fd limit hit. Affects all processes on the host.

## Data integrity

### fsync failures
```python
fault(db, fsync=deny("EIO"), run=scenario)
```
**Simulates:** Postgres `fsync` failure — data written to page cache but
not persisted to disk. This is how real data loss happens: the write
succeeds but the sync fails, and the application thinks data is durable.

**What to test:**
- Does the database detect the sync failure?
- Does it refuse to confirm the transaction?
- Does it enter a crash-safe recovery state?

> **Critical:** Postgres historically panicked on `fsync` failure because
> retrying might silently return success even though data was lost.
> This is exactly the kind of bug Faultbox was built to find.

## Combining errnos with probability

Not all failures are 100%. Use probability for intermittent errors:

```python
# 10% of writes fail — tests retry logic
fault(db, write=deny("EIO", probability="10%"), run=scenario)

# 50% connection failures — tests circuit breaker
fault(api, connect=deny("ECONNREFUSED", probability="50%"), run=scenario)
```

## Combining errnos with delay

Real failures often start with slowness before errors:

```python
# Slow then broken — cascade simulation
fault(db,
    write=delay("2s"),
    fsync=deny("EIO"),
    run=scenario,
)
```
