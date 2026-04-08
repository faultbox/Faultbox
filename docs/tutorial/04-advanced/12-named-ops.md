# Chapter 12: Named Operations

**Duration:** 15 minutes
**Prerequisites:** [Chapter 3 (Fault Injection)](../02-syscall-level/03-fault-injection.md) completed

## Goals & Purpose

Syscall names (`write`, `fsync`, `connect`) are technical — they describe
*how* the kernel works, not *what* your service does. Named operations
bridge this gap: you define operations in terms of your service's behavior,
then fault those operations by name.

## Defining operations

Add `ops=` to a service declaration:

```python
# Linux: BIN = "bin"
# macOS (Lima): BIN = "bin/linux-arm64"
BIN = "bin/linux-arm64"

inventory = service("inventory", BIN + "/inventory-svc",
    interface("main", "tcp", 5432),
    env = {"PORT": "5432", "WAL_PATH": "/tmp/inventory.wal"},
    healthcheck = tcp("localhost:5432"),
    ops = {
        "persist": op(syscalls=["write", "fsync"]),
        "wal_write": op(syscalls=["write", "fsync"], path="/tmp/*.wal"),
    },
)

orders = service("orders", BIN + "/order-svc",
    interface("public", "http", 8080),
    env = {"PORT": "8080", "INVENTORY_ADDR": inventory.main.addr},
    depends_on = [inventory],
    healthcheck = http("localhost:8080/health"),
)
```

`op()` takes:
- `syscalls=` — list of syscall names (expanded by family: `write` → write, writev, pwrite64)
- `path=` — optional glob filter (only fault writes to matching files)

## Using operations in fault()

Use the operation name as the fault keyword:

```python
def test_persist_failure():
    """All persist syscalls fail — inventory can't write or sync."""
    def scenario():
        resp = orders.post(path="/orders", body='{"sku":"widget","qty":1}')
        assert_true(resp.status != 200, "expected failure on persist")
    fault(inventory, persist=deny("EIO", label="persist broken"), run=scenario)
```

This is equivalent to:
```python
fault(inventory, write=deny("EIO"), fsync=deny("EIO"), run=scenario)
```

But clearer — you're faulting the *persist operation*, not individual syscalls.

## Path-filtered operations

With `path=`, only writes to matching files are faulted:

```python
def test_wal_failure():
    """Only WAL writes fail — stdout and TCP responses still work."""
    def scenario():
        resp = orders.post(path="/orders", body='{"sku":"widget","qty":1}')
        assert_eq(resp.status, 409, "expected 409 on WAL failure")
    fault(inventory, wal_write=deny("EIO", label="WAL broken"), run=scenario)
```

The inventory service can still log to stdout and respond via TCP — only
writes to `/tmp/*.wal` are denied.

## Trace output

Faulted syscalls show the operation name:

```
  syscall trace (3 events):
    #48  inventory    persist(write)    deny(EIO)  [persist broken]
    #49  inventory    persist(fsync)    deny(EIO)  [persist broken]
```

Format: `operation(syscall)` instead of just `syscall`.

## When to use operations

| Approach | When |
|----------|------|
| Raw syscalls: `write=deny("EIO")` | Quick tests, simple services |
| Named ops: `persist=deny("EIO")` | Multiple related syscalls, path filtering, readable traces |
| Protocol faults: `fault(db.main, error(query="INSERT*"))` | Protocol-specific (HTTP status, SQL error, Redis command) |

All three can be combined in the same test.

## What you learned

- `ops={"name": op(syscalls=[...], path=...)}` defines named operations
- Use operation names in `fault()`: `fault(svc, persist=deny("EIO"))`
- Path filters target specific files without affecting stdout/network
- Trace output shows `op(syscall)` for clarity
