# RFC-042: Exploration Plan & Coverage Engine

> **Status: Implemented (rc1, v0.13.0).** Analysis surface (§8.1–§8.7, §8.10, §8.11) shipped; execution surface (§8.8 spec-level interleaving fan-out, §8.9 probability fan-out) deferred to rc2. The rc2 work changes runtime semantics — see the per-section status in §8 below. User guide: [docs/exploration.md](../exploration.md).

## Summary

Faultbox specs declare test scenarios via `fault_matrix()`, `fault_assumption()`, parameterized scenarios (RFC-013), and parallel composition (`wait_all`, `wait_first`, `wait_n`). The cross-product of these can produce surprising test counts — a `fault_matrix` with three categories of three options each, parameterized over two retries policies, run inside `wait_all(...)` with three branches, generates 3³ × 2 = 54 plan instances. Today there is no way to see the count *or the structure that produces it* — where forks happen, which compositions multiply with which, where parallel actions diverge — before launching the run. CI cost surprises, coverage gaps, "did fault_matrix do what I expected?" debugging, and "what interleavings could this `wait_all` actually produce?" all bottleneck on the same missing primitive.

This RFC introduces three things:

1. **`faultbox plan <spec>`** — a static analysis that enumerates the plan tree (with forks, parallel actions, and interleaving spaces explicit) without executing anything.
2. **Plan as a first-class bundle artifact.** `faultbox test` runs plan generation as its first phase and writes the resulting tree into the `.fb` bundle. Every test run now ships with its plan attached, so post-hoc analysis (in the report, in MCP queries, in CI dashboards) has the same source of truth as the runtime trace.
3. **A coverage engine** that suggests faults the spec author has not written but probably should — rule-based in v0.13.0, LLM-driven later.

Together they make the test-set visible (the plan tree), the structure visible (forks and parallel actions), the interleaving space visible (what could race with what), and the gaps visible (uncovered dependency edges).

The plan command, bundle integration, and the **report's Plan tab** are v0.13.0 deliverables. The coverage engine is sketched here as direction; the v0.13.0 commitment is foundational rule-based analysis (Section 8). The full LLM-driven coverage engine compounds with RFC-043 (MCP bundle ops, v0.14.0) and lands later. Runtime *interleaving exploration* (DPOR — actually driving the SUT through each possible interleaving) stays out of scope; static *interleaving enumeration* (showing the space) is in scope.

In-tree document: `docs/rfcs/0042-exploration-plan.md`.

## Motivation

### What problem does this solve?

Three problems, all rooted in "the spec author cannot see what the spec will do":

1. **CI cost surprise.** A spec with `fault_matrix([http_errors, db_errors, redis_errors])` plus `choose("retries", [0, 1, 3])` plus a `wait_all` block produces a non-obvious test count. When CI takes 90 minutes instead of the expected 10, the cause is buried in the plan-generation logic and the only way to diagnose it is to run the full suite and count.
2. **Coverage gaps.** A team adds a new dependency edge (service `api` now calls `inventory`) and forgets to add a fault test for it. The spec passes; the gap is invisible until production fails. Today there is no tool that says "you have an undeclared dependency edge with zero fault tests."
3. **Manual fault-matrix authoring is brittle.** Writing all interesting failure modes by hand is high-effort. The team drops to "test the most likely failures" — leaving long-tail bugs to production. The existing `faultbox generate` command (for failure-scenario synthesis) is a step toward automation but sits separately from `fault_matrix` authoring; users don't always know to run it, and it produces patches the user must accept manually.

### Why is this important now?

- **v0.13.0 is the determinism release.** L0 plan determinism (RFC-040) is the substrate that makes plan visualization coherent — `faultbox plan` produces the same tree every time because the plan tree is a pure function of `(spec, seed)`. Without RFC-040's guarantee, `plan` would itself be flaky.
- **`fault_matrix` is reaching adoption pressure.** Customers are writing larger matrices and hitting the cost-surprise problem. The customer-feedback Notion (2026-04-22, Group D) flagged "test count predictability" as a top-three usability ask.
- **LLM-first authoring (v0.14.0) needs a planning surface.** When an LLM generates a spec, it should be able to ask "show me the plan for this spec — does it match the user's intent?" before committing. `faultbox plan` is that surface.

### What happens if we don't do this?

- Spec authors run-then-discover the test count; CI cost surprises persist.
- Coverage gaps stay invisible until production failures expose them.
- LLM-generated specs cannot be validated for intent without executing them — slow, expensive, and risky in CI integrations.
- `faultbox generate` stays a separate command users may or may not run; coverage stays opportunistic.

## Current state

Today Faultbox provides:

- **`fault_matrix(...)`, `fault_assumption(...)`, `fault_scenario(...)`** in `internal/star/` — produce the cartesian product of fault combinations at runtime.
- **`choose(name, options)`** (RFC-043, this epic) — non-deterministic finite choice; subsumes the never-shipped `param(name, choices=...)` from RFC-013 (per RFC-044).
- **Parallel composition** — `wait_all`, `wait_first`, `wait_n` — multiplies the runtime branches but not the plan tree directly.
- **`internal/generate/.Analyze(rt)`** — extracts topology (services, interfaces), dependency edges, and scenarios from a loaded runtime. This is the substrate for plan analysis.
- **`faultbox generate`** — produces failure-scenario mutations from happy-path scenarios. Existing pre-RFC; codegen-driven, not interactive.

What's missing:

- No "show me the plan" command. Plan enumeration happens at runtime; it's not exposed as a separate phase.
- No plan artifact in the `.fb` bundle. Bundles capture the *trace* (what happened) but not the *plan* (what was supposed to happen) — so the report cannot show the plan, and post-hoc tooling has nothing to query.
- No plan visualization in the HTML report (RFC-029). The report renders the trace; there is no Plan tab.
- No coverage report against the dependency graph (which edges are faulted? which aren't?).
- No estimation of plan size / wall-clock cost before launch.
- No JSON/structured output of the plan tree for tools (LLMs, CI dashboards) to consume.
- No static enumeration of interleavings produced by `wait_all` / `wait_n` — users cannot see "this `wait_all(A, B)` produces N possible event orderings."

## Determinism prerequisite

This RFC depends on RFC-040 L0 (plan determinism): same spec + seed → same plan tree. Without that guarantee, `faultbox plan` would produce different trees on different machines and the whole exercise collapses. RFC-040 makes L0 a first-class promise; this RFC makes that promise visible to users.

### Plan tree vs interleaving — what's in scope here

Two kinds of "tree" matter, and the distinction between **spec-level** and **SUT-internal** interleavings is load-bearing for what's tractable:

- **Plan tree** (configurations) — the set of `(test, fault_combination, parameters)` instances the spec produces. Deterministic at L0 for a fixed seed.
- **Spec-level interleaving** (executions of a single configuration via `wait_all` / `wait_n` / `wait_first`) — when the spec author writes `wait_all(branch_A, branch_B)`, *Faultbox is the orchestrator*. We initiate the branches; we observe their mediated events; we can hold and release them via the engine. **This is fully controllable at L1.**
- **SUT-internal interleaving** — when a service spawns two goroutines that race on a shared mutex, we cannot reproduce a specific scheduling. This requires L5 / Hermit-class instruction-boundary determinism (RFC-040 Appendix A).

This RFC addresses both spec-level and SUT-internal at the *visibility* level (showing the count, the participating branches, the causal-link annotations) but commits to **spec-level interleaving execution** as in-scope:

- **Plan tree: full enumeration.** Every instance, every fork, every fault combination shown explicitly.
- **Spec-level interleaving: full enumeration AND deterministic execution.** For each `wait_all` / `wait_n` / `wait_first` block, the engine can drive each interleaving by holding-and-releasing the participating branches in a chosen order. This is L1-compatible because spec-level events are mediated (Faultbox sees them and can sequence them).
- **SUT-internal interleaving: visibility only, no execution.** Show the upper-bound count and statically-inferable causal links. We cannot deterministically visit each ordering at L1; that's L5 territory.

The framing is: **a `wait_all` block becomes a fan-out in the plan tree** rather than a single instance with N possible runtime orderings. The plan author opted into parallelism by writing `wait_all` — they presumably want the cross-product tested, at least as an option. RFC-009 (Independence Relation) reduces the cross-product by collapsing equivalent orderings; this RFC ships the unreduced fan-out with controls (Section 5.8 — `interleavings=` kwarg).

What stays out of scope: SUT-internal goroutine scheduling (Hermit/L5 territory) and the independence-relation refinement (RFC-009).


## Proposed design

### 5.1 — `faultbox plan <spec>` CLI command

Static analysis. No services launched. No tests executed. No bundle produced.

```
$ faultbox plan ./tests/integration.star

Spec: tests/integration.star
Seed: 42 (from determinism())
Determinism: L1 (strict)

Plan tree:
└── 2 tests
    ├── test "smoke"
    │   └── 6 instances
    │       ├── fault_matrix(http_errors=[503, 504, timeout])      [3 branches]
    │       └── choose("retries", [0, 3])                          [2 branches]
    │       (each instance: wait_all(api_call, db_call, kafka_publish) — 3 concurrent branches)
    │
    └── test "shutdown"
        └── 1 instance
            └── fault_scenario("graceful_shutdown")

Total: 7 plan instances
Estimated wall-clock: ~3m 30s (50s/instance × 7, sequential) or ~1m (parallel suite, 7 concurrent)

Coverage:
- 4 services declared (api, inventory, db, kafka)
- 5 dependency edges
  ✓ api → inventory (faulted in: smoke)
  ✓ api → db        (faulted in: smoke)
  ✓ api → kafka     (faulted in: smoke)
  ⚠ inventory → db  (no fault test)
  ⚠ inventory → kafka (no fault test)

2 edges without fault coverage — see `faultbox plan --coverage --suggest` for proposed tests.
```

### 5.2 — Output formats

```
faultbox plan <spec>                    # human-readable text (above)
faultbox plan <spec> --format=json      # structured for tooling / LLM consumption
faultbox plan <spec> --format=dot       # graphviz for rendering
faultbox plan <spec> --coverage         # adds coverage table
faultbox plan <spec> --suggest          # adds suggested fault tests for uncovered edges
```

JSON shape (lossless representation of the plan tree):

```json
{
  "spec_path": "tests/integration.star",
  "seed": 42,
  "determinism": {"level": "L1", "strict": true, "runtime": "default"},
  "tests": [
    {
      "name": "smoke",
      "instances": 6,
      "compositions": [
        {"kind": "fault_matrix", "axes": [
          {"name": "http_errors", "values": ["503", "504", "timeout"]}
        ]},
        {"kind": "choose", "name": "retries", "values": [0, 3]}
      ],
      "runtime_branches": [
        {"kind": "wait_all", "branches": ["api_call", "db_call", "kafka_publish"]}
      ]
    }
  ],
  "topology": {
    "services": [...],
    "edges": [
      {"from": "api", "to": "inventory", "protocol": "http", "fault_tests": ["smoke"]},
      {"from": "inventory", "to": "db",  "protocol": "postgres", "fault_tests": []}
    ]
  },
  "totals": {
    "instances": 7,
    "estimated_wall_clock_sec": 210
  }
}
```

### 5.3 — Coverage analysis

Three coverage signals computed against the dependency graph from `internal/generate/.Analyze`:

| Signal | Definition | Severity |
|--------|------------|----------|
| **Edge coverage** | Every dependency edge has ≥1 test that injects a fault on it | Warning if missing |
| **Protocol coverage** | Every distinct protocol (http, grpc, postgres, etc.) appears in ≥1 fault test | Info if missing |
| **Service coverage** | Every service is the *target* of ≥1 fault test (not just an upstream) | Info if missing |

`--strict-coverage` flag promotes coverage warnings to spec-load errors. CI knob.

### 5.4 — Suggestion engine (foundation for v0.13.0)

`faultbox plan --suggest` proposes fault tests for uncovered edges. v0.13.0 ships **rule-based suggestions only** — same heuristics already encoded in `internal/generate/`:

```
Uncovered edge: inventory → db (postgres)

Suggested test:
    test("inventory_db_unavailable",
        setup = lambda: fault(db.main, deny=on(connect=True, errno="ECONNREFUSED")),
        body  = lambda: inventory.api.get_stock("sku-123"),
        expect = expect_error_within("inventory.api", duration_ms=5000),
    )
```

The suggestion is text the user copy-pastes. v0.13.0 does not auto-write to the spec.

LLM-driven suggestions (which read the topology, the SUT's actual call patterns, and the existing test corpus to propose semantically-aware faults) are deferred to v0.14.0 / RFC-043 (MCP bundle ops). The CLI seam — `faultbox plan --suggest --strategy=llm` — is reserved in v0.13.0 with an explicit "available in a future release" error.

### 5.5 — Plan as a bundle artifact

`faultbox test` runs plan generation as its first phase, **before** launching any service. The resulting `PlanTree` is serialized into the `.fb` bundle as a new top-level file `plan.json` (alongside `manifest.json`, `trace.json`, `env.json`).

```
run-2026-05-03T14-22-08-42.fb
├── manifest.json
├── trace.json         # what happened (existing)
├── plan.json          # NEW — what was supposed to happen
├── env.json
├── replay.sh
├── spec/
└── services/
```

`plan.json` schema is the same JSON shape as `faultbox plan --format=json` produces (Section 5.2). Versioned via the same `schema_version` field as the bundle.

Every test run now ships with both the trace (execution) and the plan (intent). This unlocks:

- **Report integration** (Section 5.6) reading from one source of truth.
- **MCP queries** (RFC-043, v0.14.0) — LLMs can read `plan.json` from any bundle without re-parsing the spec.
- **Replay validation** — `faultbox replay` can compare the new run's plan tree against the bundle's `plan.json` and warn if the spec changed in a way that altered the plan.
- **Drift detection across CI runs** — the same plan today vs yesterday is meaningful diff data.

**Always-on, with opt-out.** `faultbox test --no-plan` skips plan generation (rare; mostly for debug). Plan generation adds <100ms even for large specs (it's pure static analysis on already-loaded Starlark) so the cost is negligible.

### 5.6 — Report integration: the Plan tab

The HTML report (RFC-029) gains a **Plan tab** alongside the existing trace tab. The user can switch between:

- **Trace view** — what actually happened (existing). Swim-lane visualization of intercepted events.
- **Plan view** (new) — what was supposed to happen. The plan tree rendered as collapsible nodes, with:
  - Test instances listed at the top
  - Each instance expandable to show its fault combination, parameters, and runtime branches
  - Coverage table embedded inline (uncovered edges highlighted)
  - Interleaving space annotation per parallel block ("24 possible orderings")

A toggle at the top of each test row switches between trace and plan view for that instance — so the user can see the planned configuration side-by-side with what actually executed.

This is **v0.13.0 committed scope**, not stretch. The Plan tab reads from `plan.json` in the bundle; the trace tab reads from `trace.json`; both tabs are independently rendered from the same underlying React-style component tree in `internal/report/template.html`.

### 5.7 — Spec-level interleaving enumeration and execution

For each `wait_all` / `wait_n` / `wait_first` block in the plan tree, Faultbox enumerates spec-level interleavings as a **fan-out in the plan tree** — each interleaving is a separate plan-tree leaf with its own deterministic execution. This is L1-compatible because Faultbox is the orchestrator: we initiate the branches, observe their mediated events, and use the existing hold-and-release engine substrate (event log architecture, RFC-014) to drive a specific ordering.

```
test "checkout"
  └── instance #3 (fault: db_timeout, retries=1)
      └── wait_all(charge_card, write_db, send_kafka_event)
          ├── charge_card  → 4 mediated events: [http.req, http.resp, http.req(retry), http.resp]
          ├── write_db     → 2 mediated events: [postgres.exec, postgres.commit]
          └── send_kafka_event → 1 mediated event: [kafka.produce]

      Spec-level interleaving fan-out: 6 orderings (default heuristic — see 5.9)
      Upper bound: 1,260 orderings (7! / (4! × 2! × 1!))

      Statically-inferred causal links (reduce real distinct executions):
      - http.resp → http.req(retry)  (program order, same branch)
      - postgres.commit → kafka.produce (cross-branch, inferred from `depends_on`)

      Plan-tree leaves for this instance:
        instance #3.a — interleaving: charge → write → send
        instance #3.b — interleaving: charge → send → write
        instance #3.c — interleaving: write → charge → send
        instance #3.d — interleaving: write → send → charge
        instance #3.e — interleaving: send → charge → write
        instance #3.f — interleaving: send → write → charge

      Each leaf executes deterministically — the engine drives the chosen
      ordering via hold-and-release on mediated events.

      SUT-internal interleavings (e.g. goroutines inside charge_card):
      visibility only — cannot be deterministically reproduced at L1.
      See RFC-040 Appendix A (Hermit / L5).
```

**Key claim:** spec-level interleavings are *not* "things that might happen at runtime" — they are deterministic plan-tree leaves. `faultbox replay` reproduces each leaf identically. CI cost grows because each interleaving is a real test execution; this is the user's choice to make via `interleavings=` (Section 5.9).

The independence-relation refinement (Mazurkiewicz traces, collapsing orderings that produce equivalent results) is RFC-009 territory; v0.13.0 ships the unreduced fan-out with user-controlled exploration depth.

### 5.8 — Controlling interleaving fan-out: `interleavings=` kwarg

Each `wait_all` / `wait_n` / `wait_first` accepts an `interleavings=` kwarg that controls how many spec-level orderings become plan-tree leaves:

```python
# Default: one interleaving per parallel block (causal-natural order).
# Plan-tree leaves: 1 per fault_matrix combination.
wait_all(charge_card, write_db, send_kafka_event)

# Test all spec-level orderings (full fan-out).
# Plan-tree leaves: N! per fault_matrix combination, where N is the
# number of mediated events across branches with no causal links.
wait_all(charge_card, write_db, send_kafka_event, interleavings="all")

# Sample N orderings deterministically (seed-driven).
wait_all(charge_card, write_db, send_kafka_event, interleavings=10)

# Heuristic: only orderings that exercise distinct fault paths
# (e.g. fault before write_db vs after; one ordering per causal slice).
wait_all(charge_card, write_db, send_kafka_event, interleavings="critical")
```

Defaults:
- **`interleavings=1` (default)** — single causal-natural order. Backward-compatible with existing `wait_all` semantics. CI cost unchanged.
- **`"critical"`** — recommended starting point when adopting interleaving testing. ~5–10 orderings per parallel block, picked by the engine to exercise distinct fault interaction paths.
- **`"all"`** — exhaustive. Use carefully — combined with a 6-instance `fault_matrix`, a 1,260-interleaving `wait_all` produces 7,560 test instances. `faultbox plan --check-cost` will catch it.
- **integer `N`** — sample N orderings deterministically from the upper-bound space using the spec-level seed.

`faultbox plan` shows the resolved fan-out per `wait_all` block and the total instance count, so accidental explosions are visible before launching.

### 5.9 — Probability and exhaustive fan-out

Faultbox supports `probability=` on faults today: `fault(svc.api, deny=on(connect=True), probability=0.3)`. The current runtime semantics are *stochastic* — at each trigger point, the engine consults the seeded RNG and decides whether this specific occurrence fires. A single test run produces one realization of the probability distribution.

This RFC reframes probability for the plan tree: **`probability=p` creates a 2-way fan-out at each firing point.** The probability value becomes metadata for cost analysis and reporting, not a sampling parameter.

**Why exhaustive by default:**

- A fault with `probability=0.01` represents *"a rare failure mode."* Skipping it because it's rare defeats the point — that's exactly the bug a user wanted to catch by writing the probability annotation.
- Customers think of probability as a *frequency in production*, not a *coverage knob*. The plan engine treats coverage (visit every leaf) and frequency (probability metadata) as orthogonal concerns.
- The cost gate (`--check-cost`) is the safety net: probabilistic faults that create huge fan-outs are visible *before* CI runs them. Probability should be used carefully precisely because it can multiply the plan dramatically; the goal is exhaustive coverage even when individual probabilities are tiny.

**Concrete example:**

```python
fault(svc.api, deny=on(connect=True), probability=0.3)
# Test body makes 3 connects to api.

# Plan-tree expansion:
# 2^3 = 8 plan-tree leaves
#   Leaf #1: connect 1 fired, 2 fired, 3 fired       (combined probability: 0.027)
#   Leaf #2: connect 1 fired, 2 fired, 3 not fired   (combined probability: 0.063)
#   ...
#   Leaf #8: none fired                              (combined probability: 0.343)
```

Each leaf is a deterministic test execution. The probability is shown in the plan and report as metadata, useful for prioritizing investigation but *not* affecting whether to run.

**Static expansion requirements:**

For exhaustive fan-out, the plan-tree expander needs to know the trigger count. Three sources, in order of preference:

1. **Static count from spec.** If the spec body uses `client.connect()` exactly 3 times against the faulted interface, the trigger count is 3. Possible when the spec is straight-line code; not possible when the spec has loops or conditionals depending on runtime state.
2. **`max_fires=N` annotation on the fault.** User caps the trigger count explicitly: `fault(..., probability=0.3, max_fires=5)`. The plan tree fans out `2^5`. Trigger occurrences beyond 5 follow stochastic semantics at runtime (best-effort coverage).
3. **Fallback: stochastic mode with warning.** If neither static count nor `max_fires` is available, the fault falls back to today's stochastic semantics (single realization per run) and the plan tree records `probability_mode="stochastic"`. An `unmodeled_fanout` warning is emitted at plan time so the user knows coverage is incomplete.

**Plan output for probability faults:**

```
Plan tree:
└── test "checkout"
    └── 6 instances (fault_matrix × params)
        └── per instance:
            ├── 8 probability fan-outs (3 connects × probability=0.3 fault)
            └── 6 spec-level interleavings (wait_all)

Total: 6 × 8 × 6 = 288 plan-tree leaves
Probability spread:
  - 16 leaves with combined probability ≥ 0.05 (high)
  - 192 leaves with combined probability ∈ [0.001, 0.05] (medium)
  - 80 leaves with combined probability < 0.001 (long tail)
```

`faultbox plan --check-cost --max-instances=288` confirms the user opted into the count.

**Spec syntax additions:**

```python
fault(svc.api, deny=on(connect=True),
    probability = 0.3,                    # existing
    max_fires = 5,                        # NEW — cap trigger count for plan expansion
    mode = "exhaustive",                  # NEW — default; alternative: "stochastic"
)
```

Defaults:
- `mode="exhaustive"` (new default) — plan-tree fan-out as described above.
- `mode="stochastic"` — old behavior; one realization per run. Useful when the trigger count is genuinely unbounded and the user wants probabilistic sampling.
- `max_fires` defaults to "static count if determinable; else fall back to stochastic with warning."

**Backward compatibility:**

Existing `probability=` annotations transition to exhaustive mode by default, which **changes test count for existing specs**. The migration is documented in `docs/exploration.md` with a clear note: when adopting v0.13.0, run `faultbox plan` first to see the new fan-out and decide per-fault whether to keep exhaustive (default) or switch to `mode="stochastic"`.

**Composition with other fan-out axes:**

```
Total plan-tree leaves =
    tests × fault_matrix × params × probability_fanout × interleavings
```

A spec with all five axes active can produce huge counts. The `--check-cost` gate, the `interleavings="critical"` heuristic, and `max_fires=N` are the three knobs the user has to bound the cost.

### 5.10 — Other integration points

- **CLI:** new `faultbox plan` subcommand under `cmd/faultbox/`.
- **CI hooks:** `faultbox plan --check-cost --max-instances=N` exits non-zero if instance count exceeds budget. Useful as a pre-commit hook.
- **MCP (v0.14.0):** an MCP tool exposes `plan` to LLMs so they can validate generated specs before committing. Out of scope for this RFC; tracked under RFC-043. The bundle's `plan.json` is the surface MCP queries against.

## Coverage engine philosophy — beyond manual `fault_matrix`

The longer-arc question: how does Faultbox stop relying on humans to write every interesting failure mode by hand?

Three generations of generation:

1. **v0.13.0 — rule-based.** Static analysis of topology + scenarios produces fault suggestions from a fixed rule set: every edge gets a `connect=deny`, every gRPC call gets a `ResourceExhausted`, every Kafka publish gets a `BrokerNotAvailable`, etc. Coverage-driven; the rules are declared in `internal/generate/`. This is mostly already implemented as `faultbox generate`; this RFC exposes it through the planning surface.
2. **v0.14.0 — LLM-driven.** Feed the LLM (via MCP) the topology, the existing test corpus, and the SUT's call patterns from past bundles. Ask: "what failure modes are missing?" The LLM proposes semantically-aware tests — knowing, for example, that the `payment` service has retry logic so a transient failure should test that the retry actually succeeds, not just that the call fails. Tracked separately under RFC-043.
3. **v0.15.0+ — execution-trace-driven.** Use prior bundles to find code paths the existing tests don't exercise. Mutate scenarios to drive execution into uncovered branches. This is the "fuzzing for distributed systems" direction; long-arc, depends on coverage instrumentation in the SUT.

The v0.13.0 commitment is generation 1 (rule-based) exposed through `faultbox plan --suggest`. Generations 2 and 3 are sketched here so the architecture leaves room.

## v0.13.0 implementation scope

The committed v0.13.0 work for this RFC. **Sequencing call-out:** this RFC's implementation footprint is materially larger than RFC-040 — it touches `cmd/faultbox/`, `internal/engine/`, `internal/bundle/`, `internal/report/`, `internal/star/`, `internal/generate/`, adds a new bundle artifact (`plan.json`), introduces three new spec primitives (`interleavings=`, `max_fires=`, `mode=` on faults), and reframes the runtime semantics of probability faults. Realistic v0.13.0 scope alongside RFC-040 (mostly docs + small detection code) but leaves limited room for RFC-041 (Temporal Properties — to be filed) to also be large.

**Suggested sequencing if all three v0.13.0 RFCs are heavy:**
- v0.13.0-rc1: Plan command + bundle integration + interleaving *enumeration* + probability *enumeration* (analysis-only, no engine work).
- v0.13.0-rc2: Coverage + suggestion + report Plan tab + interleaving *execution* + probability *execution* (engine work).
- v0.13.1: Anything that didn't fit (likely the report Plan tab if it grows, or the probability `max_fires` static analyzer if static count proves harder than expected).

Worth a sequencing decision before all three RFCs land. Flag for review.

The committed v0.13.0 work for this RFC:

### 8.1 — `faultbox plan` command skeleton

New subcommand in `cmd/faultbox/`. Loads the spec via existing `internal/star/.Runtime`. Walks the plan tree without launching services. Outputs the human-readable text format from Section 5.1.

### 8.2 — Plan-tree enumeration

In `internal/star/`, factor the plan-generation logic into a pure function: `(spec, seed) → PlanTree`. Today this happens implicitly during `faultbox test`; we expose it as a first-class API.

`PlanTree` shape:
- Tests (top-level)
- Compositions per test (`fault_matrix`, `param`, `fault_scenario`)
- Runtime branches per instance (`wait_all`, `wait_first`, `wait_n`) — recorded but not enumerated, since interleavings are runtime concerns
- Cross-product instance count

### 8.3 — JSON output

`--format=json` flag on `faultbox plan`. Lossless dump of the `PlanTree` plus topology and coverage data. Schema versioned (`schema_version: 1`) per the RFC-025 `.fb` bundle convention.

### 8.4 — Coverage analysis

In `internal/generate/.Analyze`, add cross-reference between dependency edges and tests that fault them. Surface as the coverage table in `faultbox plan --coverage`.

### 8.5 — Rule-based suggestion output

`faultbox plan --suggest`. For each uncovered edge, emit a copy-pasteable test stub using existing `internal/generate/codegen` heuristics. v0.13.0 ships with the rules already in `internal/generate/`; no new rules needed.

### 8.6 — Cost gate

`faultbox plan --check-cost --max-instances=N` exits non-zero if instance count > N. CI integration knob. Implementation: trivial — read the count, compare, exit code.

### 8.7 — Bundle integration: `plan.json`

`faultbox test` runs plan generation as its first phase (before launching services), serializes the `PlanTree` to `plan.json`, and writes it into the bundle alongside `trace.json`. Implementation:

- New phase in the `internal/engine/` test orchestrator: `Plan(spec, seed) → PlanTree`, called before service launch.
- Update `internal/bundle/writer.go` to emit `plan.json`.
- Update `internal/bundle/reader.go` to expose `Bundle.Plan() (*PlanTree, error)`.
- `--no-plan` flag on `faultbox test` skips this phase (rare; debug only).
- Schema version bumped on the bundle's `manifest.json` to signal `plan.json` is present (compatibility with older `faultbox replay`).

### 8.8 — Spec-level interleaving enumeration AND execution

For each `wait_all` / `wait_n` / `wait_first` block in the plan tree:

**Enumeration (in plan-tree expansion):**
- Identify participating branches and their mediated events (using existing interface declarations).
- Compute the upper-bound interleaving count.
- Identify statically-inferable causal links (program order within a branch; cross-branch via `depends_on` and request/response inference from interfaces).
- Apply the `interleavings=` kwarg policy (1 / "critical" / N / "all") to determine which orderings become plan-tree leaves.

**Execution (engine-driven):**
- New `interleaving_id` field on each plan-tree leaf — a deterministic ordering descriptor `(branch_id, event_index)[]`.
- The engine's existing hold-and-release substrate is extended: when a `wait_all` block runs, mediated events from all branches are held; the engine releases them in the order specified by `interleaving_id`.
- Each interleaving is a separate test execution — produces its own bundle, own trace, own PASS/FAIL/INCONCLUSIVE verdict (per RFC-041's three-valued result).

Implementation:
- `internal/star/`: plan-tree enumeration logic (existing) extended to fan out per `interleavings=` policy.
- `internal/engine/`: hold-and-release sequencing per `interleaving_id` (extends RFC-014's hold queue).
- `internal/star/builtins.go`: `wait_all`, `wait_n`, `wait_first` accept the `interleavings=` kwarg.

Output appears in CLI text format, JSON, and the report's Plan tab.

**Scope note:** This is the heaviest engine work in this RFC. Estimated 2–3 weeks of focused work. If RFC-041 (Temporal) turns out heavy, sequencing could land enumeration in v0.13.0 (cheap — just plan-tree expansion and the `interleavings=` kwarg) and execution in v0.13.1 (engine work). Worth a sequencing decision; see scope note at top of Section 8.

### 8.9 — Probability fan-out

For each `fault(..., probability=p)` declaration:

**Plan-tree expansion:**
- Determine the trigger count: (1) static analysis of spec body; (2) `max_fires=N` annotation; (3) fallback to stochastic mode with warning.
- For each trigger occurrence, fan out the plan tree into 2 leaves (fired / not fired).
- Record the per-leaf combined probability (product of individual occurrences) as plan metadata.
- Apply user controls: `mode="exhaustive"` (default) vs `mode="stochastic"`.

**Engine execution:**
- New leaf descriptor field: `probability_outcomes: [bool, bool, bool]` indicating fired/not-fired per occurrence.
- The engine's existing fault-firing logic (per `internal/engine/fault.go`) is extended to consult the leaf's probability_outcomes vector instead of the seeded RNG when `mode="exhaustive"`.
- Stochastic mode preserved: when `mode="stochastic"`, existing RNG-driven firing logic runs unchanged.

**Spec changes:**
- `fault()` accepts `max_fires=N` (integer, optional) — caps trigger count for plan expansion.
- `fault()` accepts `mode="exhaustive"|"stochastic"` (default `"exhaustive"`).
- Spec-load validation: `max_fires` only meaningful with `probability` set; error if used without.

**Implementation locations:**
- `internal/star/builtins.go`: new kwargs on `fault()`.
- `internal/star/`: trigger-count static analysis (best-effort; falls back to stochastic).
- `internal/engine/fault.go`: extend fault-firing decision logic.
- `internal/engine/`: plan-tree expansion adds probability axis.

**Migration note (must ship in 8.11 docs):** existing `probability=` annotations gain exhaustive fan-out by default in v0.13.0. Specs that depended on stochastic semantics need to add `mode="stochastic"` explicitly. The release notes call this out as a behavior change.

### 8.10 — Report integration: Plan tab

Update `internal/report/template.html` and `app.js` to add a Plan tab alongside the existing trace tab. The Plan tab:

- Reads `plan.json` from the bundle (already a single-file HTML report per RFC-029, so the data is inlined at report-build time).
- Renders the plan tree as collapsible nodes.
- Embeds the coverage table inline.
- Annotates each parallel block with its static interleaving count.
- Provides a per-instance toggle to switch between Plan and Trace views side-by-side.

Implementation: new template section + new JS component. No new external dependencies (matches RFC-029's "self-contained HTML" constraint).

### 8.11 — Reserved syntax / flags

- `--strategy=llm` flag on `--suggest` — accepted but errors with "LLM-driven suggestion is reserved for a future Faultbox release (v0.14.0+, RFC-043)."
- `interleavings="dpor"` value on parallel composition kwargs — accepted but errors with "DPOR exploration with independence-relation refinement is reserved for a future Faultbox release; see RFC-009."
- `interleavings="sut-internal"` value — accepted but errors with "SUT-internal goroutine interleaving exploration requires L5 (instruction-boundary determinism); not available in this release. See RFC-040 Appendix A."

Locks the syntax now so v0.14.0 / v0.15.0 / RFC-009 migrations don't break existing CI integrations.

### 8.12 — Documentation

- New file: `docs/exploration.md` — the planning model, the `faultbox plan` command, output formats, coverage signals, suggestion engine, generations (rule-based / LLM / execution-driven).
- Update `docs/cli-reference.md` — add `faultbox plan` section.
- Update `docs/spec-language.md` — note that `fault_matrix`, `param`, etc. are subject to plan-tree visualization.
- README bullet pointing to `docs/exploration.md`.
- Tutorial chapter — walk through writing a small spec, running `faultbox plan` to see the tree, observing the coverage gap, applying a suggestion.

### 8.13 — Tests

Per the #84 coverage gate: every new file in `cmd/faultbox/` and `internal/star/` carrying plan-extraction logic gets a sibling `*_test.go`. Goldens for `faultbox plan --format=json` against representative specs (single test, fault_matrix, param + fault_matrix, parallel composition) in `testops/`.

## Out of scope for v0.13.0

- **SUT-internal interleaving exploration.** Goroutines inside a service that race on shared state cannot be deterministically reproduced at L1. This is L5 / Hermit-class territory (RFC-040 Appendix A). We can show *that* SUT-internal races exist (vector clocks indicate concurrent events) but not deterministically visit each ordering.
- **Independence-relation refinement** (collapsing equivalent spec-level interleavings via Mazurkiewicz traces). v0.13.0 ships unreduced fan-out with the `interleavings="critical"` heuristic for sampling; RFC-009 ships the principled refinement.
- **Probability-mass sampling** of plan-tree leaves (skipping low-probability branches to reduce cost). v0.13.0 ships exhaustive coverage by default; users opt out via `mode="stochastic"` per fault. Future RFC could add `--sample-by-probability` for cost-reduced runs that still cover high-mass branches.
- LLM-driven suggestion engine (RFC-043, v0.14.0).
- Execution-trace-driven generation (v0.15.0+).
- Auto-application of suggestions (writing back to the spec). v0.13.0 ships text output; the user pastes.
- Multi-spec planning (planning across an entire spec directory). v0.13.0 plans one spec at a time.

**Moved into scope for v0.13.0** (vs the original draft): bundle integration (`plan.json`), report integration (Plan tab), **spec-level interleaving enumeration AND execution**, the `interleavings=` kwarg on parallel composition primitives, and **probability-driven exhaustive plan-tree fan-out** (with `max_fires=` and `mode=` controls). The interleaving and probability work together deliver "interleaving is the most interesting feature" and "we cover all scenarios even when probability is small" without needing L5.

<!-- Open: Execution engine should record which plan-tree node each test executed and surface that in the report's plan visualization. Track in GitHub issue before implementation. -->

## Open questions

1. **Where does plan-tree enumeration live?** Strawman: pure function in `internal/star/`, called by both `faultbox test` (which then executes) and `faultbox plan` (which just prints). Avoids divergence between "what plan said would happen" and "what test actually ran."
2. **How are runtime branches (`wait_all`, etc.) represented in the plan tree?** Options: (a) flatten them into the count (3-branch wait_all = 3× the cost) — overestimates because they run concurrently; (b) annotate them as "concurrent" without expanding — accurate but opaque to cost estimation. Strawman: (b). Annotate with concurrency hint; cost estimator uses the *max* branch duration, not the sum.
3. **Cost estimation source.** Where does the "50s/instance" estimate come from? Options: (a) hardcoded average; (b) read from prior bundle history if available; (c) per-test annotation in the spec (`test("smoke", estimated_duration="30s")`). Strawman: (c) primary, (b) fallback, (a) last resort with a "no estimate available" badge.
4. **Coverage scope.** Is "edge coverage" enough, or do we also want "interface coverage" (every named operation on every interface)? Strawman: edges in v0.13.0; interface-level deferred until customers ask.
5. **Suggestion noise.** A topology with 50 edges and 0 tests will produce 50 suggestions. Useful or overwhelming? Strawman: surface the top-N most "interesting" (by some heuristic — high-fanout services, edges to external systems) with `--suggest --top=10`; full list with `--suggest --all`.

## Implementation plan

| Phase | Scope | Target |
|-------|-------|--------|
| 8.1 CLI skeleton | `faultbox plan` subcommand wiring | ✅ landed (v0.13.0-rc1) |
| 8.2 Plan-tree enumeration | Pure function in `internal/plan/Enumerate(rt)`; reused by `test` | ✅ landed (v0.13.0-rc1) |
| 8.3 JSON output | `--format=json` + `--format=dot` with versioned schema | ✅ landed (v0.13.0-rc1) |
| 8.4 Coverage analysis | Edge × test cross-reference in `internal/plan/WithCoverage` | ✅ landed (v0.13.0-rc1) |
| 8.5 Suggestion output | `--suggest` rule-based stub emitter | ✅ landed (v0.13.0-rc1) |
| 8.6 Cost gate | `--check-cost --max-instances=N` | ✅ landed (v0.13.0-rc1) |
| 8.7 Bundle integration | `plan.json` written by every `faultbox test` run; `Reader.PlanJSON()` accessor | ✅ landed (v0.13.0-rc1) |
| 8.8 Spec-level interleaving | Enumeration + execution via hold-and-release sequencing | 🟡 deferred to rc2 (kwarg surface reserved) |
| 8.9 Probability fan-out | Static trigger-count analysis + `max_fires=` / `mode=` kwargs + engine integration | 🟡 deferred to rc2 (changes runtime semantics) |
| 8.10 Report integration | Plan tab in HTML report (RFC-029 follow-up) | ✅ landed (v0.13.0-rc1) |
| 8.11 Reserved syntax | `--strategy=llm` (LLM suggestions); interleavings="dpor"/"sut-internal" | ✅ landed (v0.13.0-rc1) |
| 8.12 Docs | `docs/exploration.md`, CLI reference, manifest, RFC status | ✅ landed (v0.13.0-rc1) |
| 8.13 Tests | Unit + CLI tests under the #84 coverage gate; rc2 will add testops goldens | ✅ landed (v0.13.0-rc1; goldens deferred) |
| LLM-driven suggestions (out of this RFC) | `--strategy=llm` via MCP | v0.14.0 / RFC-043 |
| Independence-relation refinement (out of this RFC) | Collapse equivalent interleavings via Mazurkiewicz traces | RFC-009 |
| Runtime DPOR interleaving exploration (out of this RFC) | Deterministically drive each interleaving | RFC-010, post-v2.0 |

## Dependencies

- **Depends on:** RFC-040 (Determinism Levels, #109) — L0 plan determinism is the substrate that makes `faultbox plan` produce the same tree on every run. Without it, plan visualization is itself flaky.
- **Subsumes:** RFC-013 (Parameterized Scenarios — Draft, never shipped) — its `param()` becomes RFC-043's `choose()`. RFC-044 handles the formal withdrawal.
- **Builds on:** RFC-001 (Scenarios as Probes & Fault Composition — Implemented) — the `fault_matrix`/`fault_scenario` substrate.
- **Reuses:** existing `internal/generate/.Analyze` for topology / dependency-edge extraction.
- **Unlocks:** RFC-043 (MCP bundle ops, v0.14.0) — the LLM-driven suggestion engine plugs into the `--strategy=llm` seam reserved here. RFC-009/010 (DPOR) — interleaving-tree visualization plugs into `--format=interleaving-tree` reserved here.

---

## References

- RFC-040 (Determinism Levels, #109) — provides L0 plan determinism guarantee.
- RFC-001 (Scenarios as Probes, #26) — fault composition primitives.
- RFC-013 (Parameterized Scenarios, #27) — parameterized scenarios.
- RFC-009 / RFC-010 (Independence + DPOR — not yet filed) — runtime interleaving exploration.
- RFC-029 (Interactive HTML Reports, #60) — potential Plan tab integration.
- RFC-043 (MCP bundle ops — to be filed, v0.14.0) — LLM-driven suggestion strategy.
- Existing `internal/generate/` package — rule-based mutation engine reused here.
- Customer feedback: 2026-04-22 customer-feedback-analysis (Notion), Group D on test-count predictability.
