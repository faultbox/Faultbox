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
# Linux (native): BIN = "bin"
# macOS (Lima): BIN = "bin/linux"
BIN = "bin/linux"

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

## Using operations in fault assumptions

Use the operation name as the fault keyword in a `fault_assumption`:

```python
# --- Scenario ---

def place_order():
    return orders.post(path="/orders", body='{"sku":"widget","qty":1}')
scenario(place_order)

# --- Fault assumptions using named operations ---

persist_broken = fault_assumption("persist_broken",
    target = inventory,
    persist = deny("EIO", label="persist broken"),
)

fault_scenario("persist_failure",
    scenario = place_order,
    faults = persist_broken,
    expect = lambda r: assert_true(r.status != 200, "expected failure on persist"),
)
```

This is equivalent to:
```python
fault_assumption("persist_broken_expanded",
    target = inventory,
    write = deny("EIO"),
    fsync = deny("EIO"),
)
```

But clearer — you're faulting the *persist operation*, not individual syscalls.

## Path-filtered operations

With `path=`, only writes to matching files are faulted:

```python
wal_broken = fault_assumption("wal_broken",
    target = inventory,
    wal_write = deny("EIO", label="WAL broken"),
)

fault_scenario("wal_failure",
    scenario = place_order,
    faults = wal_broken,
    expect = lambda r: assert_eq(r.status, 409, "expected 409 on WAL failure"),
)
```

The inventory service can still log to stdout and respond via TCP — only
writes to `/tmp/*.wal` are denied.

## Fault matrix: cross-product testing

Named operations shine with `fault_matrix` — test every scenario against
every fault automatically:

```python
def check_inventory():
    return orders.get(path="/inventory/widget")
scenario(check_inventory)

fault_matrix(
    scenarios = [place_order, check_inventory],
    faults = [persist_broken, wal_broken],
    expect = lambda r: assert_true(r.status != 200, "expected failure under fault"),
)
```

This generates 4 test cases (2 scenarios x 2 faults) automatically.

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

All three can be combined in the same `fault_scenario`.

## What you learned

- `ops={"name": op(syscalls=[...], path=...)}` defines named operations
- Use operation names in `fault_assumption()`: `persist=deny("EIO")`
- `fault_scenario()` composes scenarios + fault assumptions + expectations
- `fault_matrix()` generates cross-product tests from scenarios and faults
- Path filters target specific files without affecting stdout/network
- Trace output shows `op(syscall)` for clarity
