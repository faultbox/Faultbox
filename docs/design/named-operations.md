# Design Document: Named Operations

## Problem

Users think in **operations** ("WAL write", "accept connection", "persist
to disk") but fault injection works at the **syscall level** ("write",
"fsync", "connect"). This mismatch means:

- Users must know which syscalls map to which operations
- Path filters require knowing exact file paths
- Multiple related syscalls (write + fsync for durability) must be
  specified separately
- Trace output shows syscall names, not business operations

## Goal

Let users define **named operations** on services that group syscalls +
path filters into a reusable, human-readable unit:

```python
db = service("db", BIN + "/mock-db",
    interface("main", "tcp", 5432),
    ops = {
        "persist": op(syscalls=["write", "fsync"], path="/tmp/*.wal"),
        "accept":  op(syscalls=["connect", "read"]),
        "log":     op(syscalls=["write"], path="/var/log/*"),
    },
)

# Use operation name instead of syscall:
fault(db, persist=deny("EIO"), run=scenario)

# Trace output shows:
#   #48  db  persist(write)   deny(EIO)  /tmp/inventory.wal
#   #49  db  persist(fsync)   deny(EIO)
```

## Starlark API

### Defining operations

```python
db = service("db", BIN + "/mock-db",
    interface("main", "tcp", 5432),
    ops = {
        "persist": op(syscalls=["write", "fsync"], path="/tmp/*.wal"),
        "accept":  op(syscalls=["connect", "read"]),
    },
)
```

The `op()` builtin creates an operation definition:

| Parameter | Type | Required | Description |
|-----------|------|----------|-------------|
| `syscalls` | list | **yes** | Syscall names (expanded by family) |
| `path` | string | no | Path glob for file syscalls |

### Using operations in fault()

Operations become valid kwargs on `fault()`, just like syscall names:

```python
# Named operation — expands to write + fsync with path filter:
fault(db, persist=deny("EIO"), run=scenario)

# Equivalent to (but clearer):
# fault(db, write=deny("EIO"), fsync=deny("EIO"), run=scenario)
# ... plus path filtering for /tmp/*.wal

# Mix operations and raw syscalls:
fault(db, persist=deny("EIO"), connect=delay("2s"), run=scenario)
```

### Operations in trace output

Faulted syscalls show the operation name:

```
--- PASS: test_wal_failure (200ms, seed=0) ---
  syscall trace (3 events):
    #48  db    persist(write)    deny(EIO)  [WAL write]  /tmp/inventory.wal
    #49  db    persist(fsync)    deny(EIO)  [WAL write]
  fault rule on db: persist=deny(EIO) → filter:[write,writev,pwrite64,fsync,fdatasync] path=/tmp/*.wal (2 hits)
```

### Operations in assertions

```python
# Assert on operation name:
assert_eventually(service="db", op="persist")

# Lambda with operation:
assert_eventually(where=lambda e: e.op == "persist" and e.data.get("path", "").endswith(".wal"))
```

### Operations in generator

The generator uses operation names when available:

```python
# Generated:
def test_gen_order_flow_db_persist_failure():
    """order_flow with db persist denied."""
    fault(db, persist=deny("EIO", label="persist failure"), run=order_flow)
```

## Technical Implementation

### New type: `OpDef`

```go
// internal/star/types.go
type OpDef struct {
    Name     string   // set when attached to service
    Syscalls []string // ["write", "fsync"]
    Path     string   // glob pattern (optional)
}
```

Starlark value — returned by `op()` builtin, stored in `ServiceDef.Ops`.

### Changes to `ServiceDef`

```go
type ServiceDef struct {
    // ... existing fields ...
    Ops map[string]*OpDef  // operation name → definition
}
```

### `op()` builtin

```go
func builtinOp(thread *starlark.Thread, fn *starlark.Builtin,
    args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
    // Parse syscalls= (required list of strings)
    // Parse path= (optional string)
    // Return *OpDef
}
```

### Changes to `fault()` builtin

In `builtinFault()`, when a kwarg key matches a service's operation name
instead of a syscall name:

```go
for _, kv := range kwargs {
    key := kv[0]  // could be "write" (syscall) or "persist" (operation)

    if opDef, isOp := svc.Ops[key]; isOp {
        // Expand operation to multiple FaultDefs:
        for _, sc := range opDef.Syscalls {
            faults[sc] = fd  // same FaultDef for each syscall
            // Also set PathGlob from opDef.Path
        }
    } else {
        faults[key] = fd  // raw syscall (existing behavior)
    }
}
```

### Changes to `FaultRule`

```go
type FaultRule struct {
    // ... existing fields ...
    Op string  // operation name (for trace display), empty for raw syscalls
}
```

### Changes to diagnostic output

When `Op` is set on a fired rule, show it in the trace:

```
#48  db    persist(write)    deny(EIO)    /tmp/inventory.wal
```

Format: `<op>(<syscall>)` instead of just `<syscall>`.

### Changes to `StarlarkEvent`

Add `.op` attribute — resolved from the FaultRule that matched:

```go
func (e *StarlarkEvent) Attr(name string) (starlark.Value, error) {
    case "op":
        return starlark.String(e.ev.Fields["op"]), nil
}
```

### Integration with generator

The analyzer extracts `Ops` from services. The matrix uses operation
names when generating mutations:

```go
// If service has ops, generate per-operation mutations:
for opName, opDef := range svc.Ops {
    mutations = append(mutations, Mutation{
        Name:     fmt.Sprintf("test_gen_%s_%s_%s_failure", scenario, svc.Name, opName),
        FaultKey: opName,  // use operation name as fault kwarg
        // ...
    })
}
```

## Backward Compatibility

- Raw syscall names still work: `fault(db, write=deny("EIO"))` is unchanged
- Operations are optional — services without `ops=` work exactly as before
- Operations and raw syscalls can be mixed in the same `fault()` call
- Existing test files don't need changes

## Key Files

| File | Change |
|------|--------|
| `internal/star/types.go` | Add `OpDef` type, `ServiceDef.Ops` field |
| `internal/star/builtins.go` | Add `op()` builtin, update `fault()` to resolve ops |
| `internal/star/runtime.go` | Update `applyFaults()` to expand ops → rules with PathGlob |
| `internal/engine/fault.go` | Add `Op` field to `FaultRule` |
| `internal/engine/launch_linux.go` | Pass `Op` to emitSyscallEvent |
| `cmd/faultbox/main.go` | Show `op(syscall)` in diagnostic output |
| `internal/generate/matrix.go` | Generate per-operation mutations |

## Rollout

1. `OpDef` type + `op()` builtin + `ServiceDef.Ops`
2. `fault()` resolves operation names to syscall+path rules
3. `FaultRule.Op` field + diagnostic output
4. `StarlarkEvent.op` attribute
5. Generator integration
6. Docs + tutorial update
