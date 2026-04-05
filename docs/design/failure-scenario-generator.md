# Design Document: Rule-Based Failure Scenario Generator

## Problem

Users write happy-path tests and manually add failure scenarios one by one.
This requires knowing which syscalls to target, which errnos to use, and
which dependencies can fail. Most engineers don't think in syscalls — they
miss failure modes that are obvious in hindsight.

**Goal:** Given a happy-path spec, automatically generate a comprehensive
set of failure scenarios covering every dependency × every failure mode.

## Overview

```
faultbox generate faultbox.star
```

Reads a `.star` file, analyzes the topology (services, dependencies,
interfaces, protocols), and outputs failure test functions to stdout.
The user reviews, edits, and saves the ones they want.

```
faultbox generate faultbox.star --output failures.star
faultbox generate faultbox.star --append           # append to same file
faultbox generate faultbox.star --format json      # machine-readable
```

## What the Generator Knows

From a loaded `.star` file, the generator can extract:

| Source | What it reveals |
|--------|----------------|
| `service()` declarations | Service names, binary/image, interfaces |
| `interface()` args | Protocol type, port — determines failure vocabulary |
| `depends_on` | Dependency graph — which services to fault |
| `env` wiring (`db.main.addr`) | Runtime dependencies not in depends_on |
| `healthcheck` | Readiness contract — what "healthy" means |
| Existing `fault()` calls | Already-tested failure modes (don't duplicate) |
| Existing `test_*` functions | Happy-path behavior to use as baseline |
| Protocol registry | Available step methods per protocol |

## Generation Strategy

### Phase 1: Dependency Analysis

Build a graph of which services depend on which:

```
api → db (depends_on + env: DB_ADDR=db.main.addr)
api → cache (env: CACHE_ADDR=cache.main.addr)
worker → kafka (env: BROKER=kafka.main.addr)
worker → db (depends_on)
```

Sources of dependency evidence:
- Explicit `depends_on = [db]`
- Environment variable referencing another service's `.addr` / `.internal_addr`
- `partition()` calls in existing tests

### Phase 2: Failure Matrix

For each dependency edge (A → B), generate scenarios based on B's protocol:

**Network failures (all protocols):**

| Scenario | Fault | What it tests |
|----------|-------|---------------|
| B is down | `fault(A, connect=deny("ECONNREFUSED"))` | Connection refused handling |
| B is slow | `fault(A, connect=delay("5s"))` | Timeout handling |
| B drops mid-request | `fault(A, read=deny("ECONNRESET"))` | Partial failure / retry |
| Network partition | `partition(A, B, run=scenario)` | Full isolation |

**Disk/storage failures (services with write paths):**

| Scenario | Fault | What it tests |
|----------|-------|---------------|
| Disk I/O error | `fault(B, write=deny("EIO"))` | Write error handling |
| Disk full | `fault(B, write=deny("ENOSPC"))` | Capacity handling |
| Sync failure | `fault(B, fsync=deny("EIO"))` | Durability guarantee |
| Read-only filesystem | `fault(B, write=deny("EROFS"))` | Graceful degradation |

**Protocol-specific failures:**

| Protocol | Extra scenarios |
|----------|----------------|
| postgres | Query timeout (delay on read), transaction failure |
| redis | Cache miss under fault, connection pool exhaustion |
| kafka | Publish failure, consumer lag under delay |
| http | 5xx from dependency, slow response cascade |

### Phase 3: Assertion Generation

For each failure scenario, generate appropriate assertions:

**Error response assertions:**
```python
# When B is down, A should return 5xx (not hang, not 200)
assert_true(resp.status >= 500, "expected error when B is down")
assert_true(resp.duration_ms < 5000, "should fail fast, not hang")
```

**Graceful degradation assertions:**
```python
# When cache is down, API should still work (from DB)
assert_eq(resp.status, 200, "should degrade gracefully without cache")
```

**No-data-loss assertions:**
```python
# After disk error, previously written data should survive
resp = api.get(path="/data/key1")
assert_eq(resp.status, 200, "old data should survive disk error")
```

### Phase 4: Deduplication

Don't generate scenarios already covered by existing tests:
- Scan existing `fault()` calls to find tested failure modes
- Mark generated scenarios as "new" or "already tested"
- In `--format json` output, include `"covered": true/false`

## API

### CLI

```bash
# Generate to stdout (review in terminal)
faultbox generate faultbox.star

# Write to file
faultbox generate faultbox.star --output failures.star

# Append to existing spec
faultbox generate faultbox.star --append

# Only generate for specific service
faultbox generate faultbox.star --service api

# Only specific failure category
faultbox generate faultbox.star --category network
faultbox generate faultbox.star --category disk
faultbox generate faultbox.star --category all     # default

# Machine-readable output (for LLM consumption or CI)
faultbox generate faultbox.star --format json

# Dry run — show what would be generated without code
faultbox generate faultbox.star --dry-run
```

### Output Format (Starlark)

```python
# ============================================================
# Auto-generated failure scenarios by: faultbox generate
# Source: faultbox.star
# Generated: 2026-04-05
#
# Review each test and keep the ones relevant to your system.
# Delete or comment out scenarios that don't apply.
# ============================================================

# --- Network failures: api → db ---

def test_gen_api_db_connection_refused():
    """When db is down, api should return 5xx."""
    def scenario():
        resp = api.post(path="/data/test", body="value")
        assert_true(resp.status >= 500, "expected 5xx when db is down")
        assert_true(resp.duration_ms < 5000, "should fail fast, not hang")
    fault(api, connect=deny("ECONNREFUSED", label="db down"), run=scenario)

def test_gen_api_db_slow():
    """When db is slow, api should timeout gracefully."""
    def scenario():
        resp = api.post(path="/data/test", body="value")
        assert_true(resp.status in [200, 504], "expected success or gateway timeout")
        assert_true(resp.duration_ms < 10000, "should not hang forever")
    fault(api, connect=delay("5s", label="db slow"), run=scenario)

def test_gen_api_db_connection_reset():
    """When db drops mid-request, api should handle partial failure."""
    def scenario():
        resp = api.post(path="/data/test", body="value")
        assert_true(resp.status >= 500, "expected error on connection reset")
    fault(api, read=deny("ECONNRESET", label="db dropped"), run=scenario)

def test_gen_api_db_partition():
    """Full network partition between api and db."""
    def scenario():
        resp = api.post(path="/data/test", body="value")
        assert_true(resp.status >= 500, "expected error during partition")
    partition(api, db, run=scenario)

# --- Disk failures: db ---

def test_gen_db_disk_io_error():
    """DB disk I/O error — does api handle it?"""
    def scenario():
        resp = api.post(path="/data/test", body="value")
        assert_true(resp.status >= 500, "expected 5xx on disk I/O error")
    fault(db, write=deny("EIO", label="disk I/O error"), run=scenario)

def test_gen_db_disk_full():
    """DB disk full — does api report meaningful error?"""
    def scenario():
        resp = api.post(path="/data/test", body="value")
        assert_true(resp.status >= 500, "expected 5xx on disk full")
    fault(db, write=deny("ENOSPC", label="disk full"), run=scenario)

def test_gen_db_fsync_failure():
    """DB fsync failure — data durability at risk."""
    def scenario():
        resp = api.post(path="/data/test", body="value")
        assert_true(resp.status >= 500 or resp.status == 200,
            "should either fail or succeed, not corrupt")
    fault(db, fsync=deny("EIO", label="fsync failure"), run=scenario)

# --- Recovery: db ---

def test_gen_db_recovery_after_error():
    """After disk error, previously written data should survive."""
    # Setup: write data while healthy.
    api.post(path="/data/safe-key", body="safe-value")

    # Break: disk error during new write.
    def break_writes():
        resp = api.post(path="/data/doomed-key", body="doomed")
        assert_true(resp.status >= 500, "new write should fail")
    fault(db, write=deny("EIO", label="transient disk error"), run=break_writes)

    # Verify: old data survived.
    resp = api.get(path="/data/safe-key")
    assert_eq(resp.status, 200, "old data should survive")
    assert_eq(resp.body, "safe-value")

# --- Concurrent failures ---

def test_gen_db_slow_concurrent():
    """Concurrent requests under slow db — no race conditions."""
    def scenario():
        results = parallel(
            lambda: api.post(path="/data/k1", body="v1"),
            lambda: api.post(path="/data/k2", body="v2"),
        )
        for r in results:
            assert_true(r.status in [200, 500, 503],
                "each request should succeed or fail cleanly")
    fault(db, write=delay("500ms", label="slow db"), run=scenario)
```

### Output Format (JSON)

```json
{
  "source": "faultbox.star",
  "generated": "2026-04-05T12:00:00Z",
  "scenarios": [
    {
      "name": "test_gen_api_db_connection_refused",
      "category": "network",
      "edge": "api → db",
      "fault_target": "api",
      "syscall": "connect",
      "errno": "ECONNREFUSED",
      "description": "When db is down, api should return 5xx",
      "covered_by": null,
      "severity": "critical",
      "code": "def test_gen_api_db_connection_refused():\n    ..."
    },
    {
      "name": "test_gen_db_disk_full",
      "category": "disk",
      "edge": "db (storage)",
      "fault_target": "db",
      "syscall": "write",
      "errno": "ENOSPC",
      "description": "DB disk full — does api report meaningful error?",
      "covered_by": "test_db_write_failure",
      "severity": "high",
      "code": "def test_gen_db_disk_full():\n    ..."
    }
  ],
  "coverage": {
    "total_scenarios": 15,
    "already_covered": 2,
    "new_scenarios": 13
  }
}
```

## Technical Implementation

### Architecture

```
cmd/faultbox/main.go
    └── generateCmd(args)
            │
            ├── Load .star file (star.New + LoadFile)
            │
            ├── internal/generate/analyzer.go
            │   └── AnalyzeTopology(rt) → TopologyGraph
            │       ├── services, interfaces, protocols
            │       ├── dependency edges (depends_on + env wiring)
            │       └── existing fault coverage (source scan)
            │
            ├── internal/generate/matrix.go
            │   └── BuildFailureMatrix(graph) → []Scenario
            │       ├── network failures per edge
            │       ├── disk failures per service
            │       ├── protocol-specific failures
            │       ├── recovery scenarios
            │       └── concurrent failure scenarios
            │
            ├── internal/generate/codegen.go
            │   └── GenerateStarlark(scenarios, opts) → string
            │       ├── test function generation
            │       ├── assertion generation
            │       └── label and comment generation
            │
            └── Output (stdout / file / json)
```

### New Package: `internal/generate/`

**analyzer.go:**

```go
type TopologyGraph struct {
    Services []ServiceInfo
    Edges    []DependencyEdge
    Covered  []CoveredScenario  // already tested
}

type ServiceInfo struct {
    Name       string
    Protocol   string            // primary interface protocol
    Interfaces []InterfaceInfo
    HasStorage bool              // true if binary/container writes to disk
}

type DependencyEdge struct {
    From     string  // service that depends
    To       string  // service depended on
    Via      string  // "depends_on", "env", "partition"
    Protocol string  // protocol of the target interface
}

type CoveredScenario struct {
    TestName string
    Service  string
    Syscall  string
    Action   string  // "deny", "delay", "partition"
    Errno    string
}

func AnalyzeTopology(rt *star.Runtime) (*TopologyGraph, error)
```

**matrix.go:**

```go
type Scenario struct {
    Name        string   // test function name
    Category    string   // "network", "disk", "recovery", "concurrent"
    Description string
    Edge        string   // "api → db" or "db (storage)"
    Severity    string   // "critical", "high", "medium"
    FaultTarget string   // service to fault
    Syscall     string   // "connect", "write", "fsync"
    Action      string   // "deny", "delay"
    Errno       string   // "ECONNREFUSED", "EIO"
    Label       string   // human-readable fault label
    CoveredBy   string   // existing test name, or empty
}

func BuildFailureMatrix(graph *TopologyGraph) []Scenario
```

**codegen.go:**

```go
type GenerateOpts struct {
    Format    string  // "starlark", "json"
    Service   string  // filter to one service
    Category  string  // filter to one category
    DryRun    bool
}

func GenerateStarlark(scenarios []Scenario, happy []HappyPath, opts GenerateOpts) string
func GenerateJSON(scenarios []Scenario, opts GenerateOpts) string
```

### Scenario Templates

Each scenario category has a template that generates Starlark code:

**Network template:**
```go
func networkDenyTemplate(edge DependencyEdge, errno, label string) string
func networkDelayTemplate(edge DependencyEdge, delay, label string) string
func partitionTemplate(edge DependencyEdge) string
```

**Disk template:**
```go
func diskDenyTemplate(svc ServiceInfo, errno, label string) string
func fsyncDenyTemplate(svc ServiceInfo) string
```

**Recovery template:**
```go
func recoveryTemplate(svc ServiceInfo, caller ServiceInfo) string
```

**Concurrent template:**
```go
func concurrentTemplate(svc ServiceInfo, caller ServiceInfo) string
```

### Happy Path Extraction

The generator needs to know what step calls the happy path makes so it
can reuse them in failure scenarios:

```go
type HappyPath struct {
    TestName string
    Steps    []StepCall  // e.g., api.post(path="/data/test", body="value")
}

type StepCall struct {
    Service   string
    Interface string
    Method    string
    Kwargs    map[string]string
}
```

Extracted by scanning test function bodies for step call patterns
(same substring approach as `requiredSyscallsForService`).

### Severity Classification

| Severity | Criteria |
|----------|----------|
| critical | Service completely down (ECONNREFUSED), data loss risk (fsync EIO) |
| high | Disk full (ENOSPC), connection reset (ECONNRESET), slow cascade |
| medium | Read-only filesystem, partial failure, concurrent race |
| low | File descriptor exhaustion, uncommon errnos |

### Integration with Existing Code

The generator reuses:
- `star.Runtime.LoadFile()` — parse .star
- `star.Runtime.Services()` — service registry
- `star.Runtime.DiscoverTests()` — existing test names
- `protocol.Get(name).Methods()` — available step methods
- `expandSyscallFamily()` — syscall expansion (for deduplication)
- Source text scanning (for covered scenario detection)

No changes to existing packages — `internal/generate/` is additive only.

## Use Cases

### 1. New Project Bootstrap

```bash
# Generate starter spec
faultbox init --name api --port 8080 ./api-svc --output faultbox.star

# Add db dependency manually, write happy path test

# Generate all failure scenarios
faultbox generate faultbox.star --output failures.star

# Review, keep relevant ones, run
faultbox test failures.star
```

### 2. Coverage Gap Discovery

```bash
# See what's tested vs what's missing
faultbox generate faultbox.star --format json | jq '.scenarios[] | select(.covered_by == null)'

# Output: 13 uncovered failure modes
```

### 3. CI Coverage Gate

```bash
# In CI: generate and check coverage
faultbox generate faultbox.star --format json > coverage.json
UNCOVERED=$(jq '.coverage.new_scenarios' coverage.json)
if [ "$UNCOVERED" -gt 5 ]; then
  echo "WARNING: $UNCOVERED failure modes not tested"
fi
```

### 4. Service-Specific Generation

```bash
# Just added a new Redis dependency — generate Redis-specific failures
faultbox generate faultbox.star --service cache --output cache-failures.star
```

### 5. Post-Incident Reproduction

After an incident where the payment gateway timed out:

```bash
# Generate shows it was already in the matrix but not tested
faultbox generate faultbox.star --format json | \
  jq '.scenarios[] | select(.description | contains("timeout"))'
```

### 6. LLM Workflow (future Phase 2)

```bash
# LLM reads the generated JSON, enriches with semantic understanding
faultbox generate faultbox.star --format json | \
  claude "Review these failure scenarios for a payment system. \
          Add any business-logic failures I'm missing."
```

## Naming Convention

Generated test names use the `test_gen_` prefix to distinguish from
hand-written tests:

```
test_gen_<caller>_<target>_<failure_mode>
test_gen_api_db_connection_refused
test_gen_api_db_slow
test_gen_db_disk_full
test_gen_db_recovery_after_error
```

## Rollout Plan

1. **`internal/generate/analyzer.go`** — topology analysis
2. **`internal/generate/matrix.go`** — failure matrix generation
3. **`internal/generate/codegen.go`** — Starlark code generation
4. **`cmd/faultbox/main.go`** — `generate` subcommand
5. **Tests** — unit tests for analyzer, matrix, codegen
6. **Docs** — CLI reference, tutorial chapter
