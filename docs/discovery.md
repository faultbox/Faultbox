# Faultbox — Discovery & Positioning

## One-Liner

**Faultbox verifies distributed systems by controlling what the kernel returns to your code.**

---

## The Problem

Distributed systems fail in ways that are hard to test:

- **Network partitions** — what happens when service A can't reach service B?
- **Disk failures** — what if fsync returns EIO after your WAL write?
- **Race conditions** — two concurrent requests for the last item in stock — who wins?
- **Cascading failures** — one slow service makes the entire system unresponsive

Engineers discover these bugs in production. The existing tools don't solve this well:

| Tool | What it does | What's missing |
|------|-------------|----------------|
| **Unit tests** | Test one function | Can't test inter-service failures |
| **E2E tests** | Test happy paths | Can't inject infrastructure failures |
| **Chaos engineering** | Kill pods in staging | Not reproducible, no assertions, expensive |
| **TLA+** | Prove protocol correctness | Doesn't test your actual code |
| **Antithesis** | Autonomous exploration | Managed cloud only, expensive, opaque |

---

## What Faultbox Does

Faultbox intercepts syscalls via Linux seccomp-notify — the same kernel mechanism
that containers use for security. When your service calls `write()`, `connect()`,
or `fsync()`, Faultbox decides: allow it, fail it, delay it, or hold it.

This gives you **infrastructure-level fault injection on unmodified binaries**:

```python
# faultbox.star — a complete distributed systems test

inventory = service("inventory", "/usr/bin/inventory-svc",
    interface("main", "tcp", 5432),
    healthcheck = tcp("localhost:5432"),
)

orders = service("orders", "/usr/bin/order-svc",
    interface("public", "http", 8080),
    env = {"INVENTORY_ADDR": inventory.main.addr},
    depends_on = [inventory],
    healthcheck = http("localhost:8080/health"),
)

def test_wal_durability():
    """When fsync fails, the order must NOT be confirmed."""
    def scenario():
        resp = orders.post(path="/orders", body='{"sku":"widget","qty":1}')
        assert_true(resp.status != 200, "must not confirm on fsync failure")

        # Prove the fsync was actually denied (not just a timeout).
        assert_eventually(service="inventory", syscall="fsync", decision="deny*")
    fault(inventory, fsync=deny("EIO"), run=scenario)

def test_no_double_spend():
    """Two concurrent orders for the last widget — exactly one succeeds."""
    results = parallel(
        lambda: orders.post(path="/orders", body='{"sku":"widget","qty":1}'),
        lambda: orders.post(path="/orders", body='{"sku":"widget","qty":1}'),
    )
    ok_count = sum(1 for r in results if r.status == 200)
    assert_eq(ok_count, 1, "exactly one order should succeed")
```

```bash
faultbox test faultbox.star                          # run all tests
faultbox test faultbox.star --explore=all             # try ALL interleavings
faultbox test faultbox.star --virtual-time            # instant delays
faultbox test faultbox.star --runs 1000 --show fail   # find counterexamples
```

---

## Key Capabilities

### 1. Syscall-Level Fault Injection
Intercept any syscall — `write`, `connect`, `fsync`, `openat` — and deny it
with a specific errno, delay it, or hold it. Path-filtered: fail writes to
`/data/wal` but not `/proc/self/status`.

### 2. Temporal Assertions
Not just "did the request return 200?" but "did the WAL open happen before
the WAL write?" and "was fsync never called during this scenario?"

```python
assert_before(
    first={"service": "inventory", "syscall": "openat", "path": "/tmp/inventory.wal"},
    then={"service": "inventory", "syscall": "write", "path": "/tmp/inventory.wal"},
)
assert_never(service="inventory", syscall="openat", path="/tmp/inventory.wal")
```

### 3. Exhaustive Interleaving Exploration
`parallel()` runs concurrent requests. `--explore=all` tries every permutation
of syscall ordering. For 4 concurrent syscalls, that's 24 runs. Each ordering
is deterministic and replayable with `--seed`.

### 4. Virtual Time
Fault delays (`delay("2s")`) advance a virtual clock instead of sleeping.
A test that would take 30 minutes with real time completes in seconds.
This makes exhaustive exploration practical.

### 5. Continuous Monitors
Safety properties checked on every syscall event during execution, not just
at the end. "No two concurrent WAL writes" is a monitor, not an assertion.

### 6. Network Partition Modeling
`partition(orders, inventory)` blocks connections between specific services
by inspecting the destination address of `connect()` syscalls. Other services
remain unaffected.

### 7. Deterministic Replay
Every test has a seed. Same seed = same fault decisions, same release ordering.
When `--runs 1000` finds a failure at seed 42, `--seed 42` reproduces it every time.

### 8. Causal Tracing
Vector clocks on every event. ShiViz visualization shows causal arrows between
services. PObserve-compatible trace format for integration with formal methods tools.

---

## How It Compares

|  | Faultbox | Antithesis | P-lang | Chaos Eng |
|--|----------|-----------|--------|-----------|
| **Tests real code** | Yes | Yes | No (model) | Yes |
| **Syscall-level faults** | Yes | Yes (hypervisor) | N/A | No (pod-level) |
| **Exhaustive exploration** | Yes (K!) | Partial | Yes (all states) | No |
| **Deterministic replay** | Yes (seed) | Yes (hypervisor) | Yes | No |
| **Temporal assertions** | Yes | "Sometimes true" | Yes (LTL) | No |
| **Network partitions** | Yes (per-dest) | Yes | Yes (model) | Yes (coarse) |
| **Virtual time** | Yes (delays) | Yes (full) | Yes (logical) | No |
| **Setup** | One binary, local | Managed cloud | Rewrite as P | Staging env |
| **Cost** | Free / open source | Enterprise | Free | Infra cost |
| **Feedback loop** | Seconds | Hours | Minutes | Manual |

### Positioning vs Antithesis

Antithesis controls the entire execution environment via a custom hypervisor.
Faultbox controls syscall boundaries via seccomp-notify. This means:

- **Antithesis advantage:** Total control (thread scheduling, memory, time).
  Can find in-process data races.
- **Faultbox advantage:** No managed infrastructure, works on any Linux binary,
  runs locally in seconds, directed + exhaustive (not just autonomous).

Faultbox is for engineers who know what failure modes to verify.
Antithesis is for teams who want autonomous bug discovery on their infra.

**They're complementary.** Faultbox runs in CI on every commit (fast, directed).
Antithesis runs continuously in the background (slow, exploratory).

---

## Architecture

```
┌─────────────────────────────────────────────────────────┐
│                    faultbox test                         │
│               Starlark Runtime (.star)                  │
│  service() / fault() / parallel() / assert_*()          │
└──────────┬──────────────────────────────────────────────┘
           │
┌──────────▼──────────────────────────────────────────────┐
│                   Engine (Go)                           │
│  Session per service │ HoldQueue │ VirtualClock │ RNG   │
└──────────┬──────────────────────────────────────────────┘
           │
┌──────────▼──────────────────────────────────────────────┐
│              seccomp-notify (kernel)                     │
│  BPF filter → notification → Allow / Deny / ReturnValue │
│  Per-process: PID/MNT/USER namespaces                   │
└─────────────────────────────────────────────────────────┘
           │
┌──────────▼──────────────────────────────────────────────┐
│               Target Services (unmodified)              │
│  order-svc ←→ inventory-svc ←→ database-svc            │
└─────────────────────────────────────────────────────────┘
```

**~7,600 lines of Go.** No external dependencies beyond the Go stdlib,
`go.starlark.net`, and `golang.org/x/sys/unix`.

---

## Target Users

### 1. Engineers Building Distributed Systems
- "What happens when my database's fsync fails?"
- "Is my retry logic correct under partial network failure?"
- "Can two concurrent requests corrupt shared state?"
- Write a `.star` file, run `faultbox test`, get a pass/fail in seconds.

### 2. LLM Agents in CI/CD
- Agent writes code → generates `.star` spec → runs `faultbox test` → reads JSON results → fixes code
- Structured JSON output with `replay_command` and `failure_type` for machine consumption
- `faultbox init` generates starter specs from binary metadata
- Exit codes: 0 = pass, 1 = error, 2 = test failure

### 3. Platform/SRE Teams
- Define failure scenarios as `.star` files in the repo
- Run in CI: `faultbox test --explore=all --virtual-time`
- Catches regressions before they reach production
- Replaces ad-hoc chaos experiments with reproducible tests

---

## What's Built (PoC — 35 commits)

| Capability | Status |
|-----------|--------|
| Process isolation (PID/MNT/USER namespaces) | Done |
| Seccomp-notify fault injection (deny/delay/hold) | Done |
| Path-filtered file faults | Done |
| Network fault injection (connect deny/delay) | Done |
| Starlark config language | Done |
| Multi-service orchestration with dependency ordering | Done |
| Healthcheck-based readiness (TCP/HTTP) | Done |
| Temporal assertions (eventually/never/before) | Done |
| Continuous monitors | Done |
| Event log with vector clocks (ShiViz/PObserve) | Done |
| Seeded RNG for deterministic replay | Done |
| Counterexample discovery (--runs N --show fail) | Done |
| parallel() concurrent execution | Done |
| Exhaustive exploration (--explore=all) | Done |
| Virtual time (--virtual-time) | Done |
| Network partition modeling (partition()) | Done |
| nondet() for nondeterministic services | Done |
| JSON output for agent consumption | Done |
| faultbox init scaffolding | Done |
| Demo: order-svc + inventory-svc (6 tests) | Done |
| Autonomous discovery (faultbox explore) | ADR written, next phase |

---

## What's Next

1. **Autonomous discovery** (`faultbox explore`) — automatically find failure modes
   by systematically trying fault combinations. ADR-022 is written.

2. **Code-to-spec extraction** — LLM reads Go source, generates `.star` files
   targeting identified failure points. ADR-018.

3. **CI/CD integration** — Dockerfile, GitHub Actions example, `faultbox` as a
   binary in CI pipelines.

4. **Protocol extensions** — gRPC, Redis, Kafka step drivers as Starlark modules.

5. **State machine hooks** — per-service state tracking with lifecycle-aware
   fault decisions (on_init, on_syscall).

---

## The Pitch

> Every distributed system has failure modes its tests don't cover.
> Faultbox finds them — by controlling what the kernel returns to your code.
>
> Write a test in Starlark. Run it. Faultbox exhaustively explores every
> interleaving of your concurrent requests under every fault you specify.
> Deterministic. Replayable. In seconds.
>
> No managed infrastructure. No code changes. No new language to learn.
> Just `faultbox test your-system.star`.
