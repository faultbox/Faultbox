# Plan & Coverage

> RFC-042's `faultbox plan` subcommand, the `plan.json` bundle artifact, and the report's Plan tab. v0.13.0-rc1 shipped the **analysis** surface (enumerate, render, attach to bundles). **rc2** ships the body-re-execution engine: named `choose()` axes (RFC-043) and syscall-level probability fan-out (`max_fires=`/`mode=`) now produce multi-leaf test execution end-to-end. Spec-level interleaving execution (§8.8) and protocol-level probability fan-out are still deferred; see "What's still reserved" below.

## Why a plan command

Three problems Faultbox specs hit as they grow:

1. **CI cost surprise.** A `fault_matrix(scenarios=[a,b,c], faults=[x,y])` plus a parameterized retries axis can balloon to dozens of instances. With nothing surfacing the count, the first sign is a 90-minute CI run.
2. **Coverage gaps.** Add a new dependency edge (`api → inventory`) and forget the fault test. Spec passes; the gap is invisible until production breaks.
3. **Manual fault-matrix authoring is brittle.** Writing every interesting failure mode by hand is high-effort. Teams ship the obvious ones and the long tail blows up on prod.

`faultbox plan` makes the test-set visible, the structure visible, and the gaps visible — without running anything.

## `faultbox plan <spec.star>`

Static analysis. No services launched, no tests executed, no bundle written. Loads the Starlark spec, walks the registered tests + scenarios + matrices, and prints the plan tree.

```
$ faultbox plan tests/integration.star

Spec: tests/integration.star
Seed: (unseeded)
Determinism: L1 (strict) — default; spec did not call determinism()

Plan tree:
└── 4 tests
    ├── test "test_matrix_scenario_browse"  [fault_matrix]
    │   ├── 2 instances
    │   ├── fault_matrix
    │   │   ├── scenarios: [scenario_browse]
    │   │   └── faults: [db_down, db_slow]
    │   └── expect: expect_ok
    ├── test "test_matrix_scenario_checkout"  [fault_matrix]
    │   ├── 2 instances
    │   ├── fault_matrix
    │   │   ├── scenarios: [scenario_checkout]
    │   │   └── faults: [db_down, db_slow]
    │   └── expect: expect_ok
    ├── test "test_smoke"  [def]
    └── test "test_standalone_check"  [fault_scenario]
        └── faults: [db_down]

Total: 6 plan instances
Services: 1 (svc)
```

### Flags

| Flag                                      | What it does |
|-------------------------------------------|--------------|
| `--format=text`                           | Human-readable (default). |
| `--format=json`                           | Structured JSON. Same schema as the bundle's `plan.json` (Section "JSON shape" below). |
| `--format=dot`                            | Graphviz DOT. Pipe into `dot -Tsvg` for slides / docs. |
| `--coverage`                              | Append the coverage table — every dependency edge, the tests faulting it, the gaps. |
| `--suggest`                               | Print copy-pasteable Starlark stubs for uncovered edges. Implies `--coverage`. |
| `--strategy=rules` (default)              | Suggestion strategy. Rule-based today; `--strategy=llm` is reserved for v0.14.0 (RFC-043) and errors clearly in this release. |
| `--check-cost --max-instances N`          | Exit code 2 if the plan tree exceeds N instances. Pre-commit / CI cost gate. |

### Coverage

`--coverage` adds a table to the text output (and a `coverage` object to JSON):

```
Coverage:
- 3 services declared
- 2 dependency edges
  ✓ api → db  (faulted in: test_api_db_down)
  ⚠ api → redis  (no fault test)

1 edge without fault coverage — see `faultbox plan --suggest` for proposed tests.
```

The rule is simple: an edge `A → B` is covered iff some `fault_scenario`'s `fault_assumption(target=B)` lands in the plan. Today only syscall-level + proxy-level rules count; v0.14.0 will broaden this once new fault primitives ship.

### Suggestions

`--suggest` emits a copy-pasteable stub per uncovered edge:

```python
# Uncovered edge: api → redis (redis)
redis_unavailable = fault_assumption("redis_unavailable",
    target = redis,
    connect = deny("ECONNREFUSED"),
)
def scenario_api_calls_redis():
    # TODO: call api functionality that exercises redis.
    pass
fault_scenario("api_when_redis_down",
    scenario = scenario_api_calls_redis,
    faults   = redis_unavailable,
)
```

The stub is conservative — `connect=deny("ECONNREFUSED")` works for every protocol and is the most common starting point. Adjust the syscall, errno, or add latency/partial-write before pasting.

The smarter, intent-aware variant (`--strategy=llm`, reading topology + call patterns + existing tests via MCP) lands in v0.14.0 alongside RFC-043. The flag surface is locked in now: `faultbox plan --suggest --strategy=llm` returns `error: --strategy=llm requires Faultbox v0.14.0`, so CI integrations can probe the feature without churning later.

### Cost gates

`--check-cost --max-instances=100` exits with code 2 when the plan tree is bigger than the budget. Useful as a pre-commit hook or CI step before launching:

```
$ faultbox plan tests/integration.star --check-cost --max-instances=10
...
Total: 54 plan instances
Services: 5 (api, db, redis, kafka, inventory)

cost gate: plan has 54 instances; --max-instances=10 exceeded
$ echo $?
2
```

The plan output still prints — so the engineer who triggered the gate sees what they're paying for.

## Plan as a bundle artifact

Every `faultbox test` run writes the same plan tree into the `.fb` archive as `plan.json`, alongside `trace.json`. Pre-RFC-042 bundles stay valid: the report and CLI both treat a missing `plan.json` as "no plan data" rather than an error.

```
run-2026-05-13T14-22-08-42.fb
├── manifest.json
├── env.json
├── trace.json          # what happened
├── plan.json           # what was supposed to happen (new)
├── replay.sh
├── spec/
└── services/
```

Why: the bundle becomes self-describing. The report's Plan tab reads from `plan.json` directly; future MCP queries (RFC-043) can ask "show me the plan for this run" without re-parsing the spec; `faultbox replay` will eventually validate that the new run's plan matches the old one and warn on drift.

`faultbox test --no-plan` skips plan generation for debug. Enumeration adds <100ms even for large specs (it's pure static analysis on already-loaded Starlark) so the cost is otherwise negligible.

## Report Plan tab

The HTML report (`faultbox report bundle.fb`) gains a Plan section showing:

- Header: spec path, determinism contract, total instance count.
- The test tree — one box per `PlanTest` with the composition axes and the per-test metadata (faults, expect, timeout) inline.
- An embedded coverage table — ✓ for covered edges, ⚠ for gaps. `faultbox test` calls `WithCoverage` before serialising so every bundle ships with the coverage data; legacy bundles without it render the section as empty.

The Plan tab is read-only and analysis-only. Per-instance toggle between Plan and Trace views layers on after rc2 when interleaving execution makes "this leaf actually ran" meaningful.

## JSON shape

`faultbox plan --format=json` and the bundle's `plan.json` share one schema, versioned via `schema_version`:

```json
{
  "schema_version": 1,
  "spec_path": "tests/integration.star",
  "determinism": {"level": "L1", "runtime": "default", "strict": true},
  "tests": [
    {
      "name": "test_matrix_checkout",
      "kind": "fault_matrix",
      "instances": 4,
      "compositions": [
        {
          "kind": "fault_matrix",
          "axes": [
            {"name": "scenarios", "values": ["checkout"]},
            {"name": "faults", "values": ["db_down", "db_slow"]}
          ]
        }
      ],
      "matrix_cells": ["test_matrix_checkout_db_down", "test_matrix_checkout_db_slow"],
      "expect": "expect_ok"
    }
  ],
  "totals": {"instances": 5},
  "topology": {"services": [{"name": "api", "interfaces": ["public"]}]},
  "coverage": {
    "edges": [
      {"from": "api", "to": "db", "protocol": "postgres", "via": "depends_on",
       "fault_tests": ["test_matrix_checkout_db_down"]}
    ],
    "uncovered_edges": 0
  }
}
```

Top-level fields are stable from rc1; new fields land in additive places (e.g. `tests[].probability_fanout` arrives in rc2). The `schema_version` bumps when a backwards-incompatible change ships.

## What landed in rc2

The body-re-execution engine is live. Two new fan-out axes drive multi-leaf test execution from one spec declaration:

- **Named `choose("name", [opts])`** (RFC-043 §5.2) — each option becomes one test execution. Anonymous `choose([opts])` remains single-leaf because there's no name to address the axis by. See [docs/nondeterministic-operators.md](nondeterministic-operators.md) for the operator reference.
- **`fault(..., probability=p, max_fires=N, mode="exhaustive")`** (RFC-042 §8.9, syscall-level) — exhaustive coverage of every fired/not-fired combination across N occurrences. 2^N leaves per rule. Each leaf carries a deterministic per-occurrence vector consulted by the engine instead of the seeded RNG.
- **`parallel(fn1, fn2, ..., interleavings=)`** (RFC-042 §8.8) — each interleaving becomes one test execution. Policies: `1` (default, single execution), `"all"` (full factorial of branch orderings), `"n"` (capped subset), `"critical"` (heuristic 2N-1 boundary orderings). **Scope limit:** today's "interleaving" is **launch ordering** — branches launch sequentially in the per-leaf order. Mediated-event-level ordering (concurrent branches with the engine releasing syscalls in a specific sequence) is a follow-up; the kwarg surface + leaf descriptors are the substrate that work plugs into.

Migration: existing `probability=p` declarations are **unaffected** unless the spec author explicitly adds `max_fires=N`. Bare `probability=p` keeps stochastic semantics; you have to opt into exhaustive fan-out. `mode="exhaustive"` requires `max_fires=N` (spec-load error otherwise — the runtime would otherwise silently fall back to stochastic). Existing `parallel(...)` calls without `interleavings=` keep the rc1 single-execution behavior.

Multi-leaf bundle attribution: each leaf gets a stable `LeafID` in `TestResult` → `bundle.TestRow.LeafID` → the HTML report's tests table (display name suffixed with `[leaf N]`). Single-leaf executions stay byte-identical to rc1 manifests.

## What's still reserved (post-rc2 follow-ups)

- **Mediated-event-level interleaving execution.** Today's `parallel(interleavings=)` ships **launch ordering** — branches launch sequentially in the per-leaf order. True mediated-event interleaving (concurrent branches with the engine releasing syscalls in a specific sequence via RFC-014's hold queue) is a follow-up. The PlanLeaf/site/decider substrate is in place; the follow-up extends the hold queue.
- **`wait_all` / `wait_n` / `wait_first` builtins.** Not in the language today; `parallel()` is the rc2 surface. Likely lands with RFC-044 spec language simplification.
- **Protocol-level probability fan-out.** `response()` / `error()` / `drop()` still use the legacy stochastic path. The same `max_fires=` / `mode=` surface threads through `ProxyFaultDef` and `internal/proxy` in a follow-up.
- **Static trigger-count analysis.** rc2 requires `max_fires=N`; rules without it fall back to stochastic without a plan warning. The `unmodeled_fanout` diagnostic surfaces in `faultbox plan` once static analysis lands.

## Coverage philosophy — beyond manual `fault_matrix`

Three generations of generation, the v0.13.0 commitment is the first:

1. **v0.13.0 (rule-based).** Static analysis of topology produces stubs from a fixed rule set (`connect=deny`, per-protocol syscall). What `--suggest` ships today.
2. **v0.14.0 (LLM-driven).** Feed the LLM the topology, the existing test corpus, and prior bundles via MCP (RFC-043). Ask it to propose semantically-aware failure modes — "the payment service has retry logic; test that the retry actually succeeds, not just that the call fails."
3. **v0.15.0+ (execution-trace-driven).** Use prior bundles to find code paths the existing tests don't exercise; mutate scenarios to drive execution into uncovered branches. Long-arc work — depends on coverage instrumentation in the SUT.

The architecture leaves the seams open. Generation 2 plugs into `--strategy=llm`; generation 3 plugs into the bundle reader's prior-run data.

## References

- RFC-042 — Exploration Plan & Coverage Engine (`docs/rfcs/0042-exploration-plan.md`).
- RFC-040 — Determinism Levels. L0 plan determinism is the substrate that makes `faultbox plan` produce the same tree every time.
- RFC-029 — Interactive HTML Reports. The Plan tab follows the same self-contained pattern.
- RFC-043 — MCP bundle ops (filed). LLM-driven suggestions will plug into `--strategy=llm`.
