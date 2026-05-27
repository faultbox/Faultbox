# RFC-043: Non-deterministic Spec Operators

> **Status: Implemented (rc2, v0.13.0).** rc1 shipped the language surface (§8.1–§8.4, §8.6 partial, §8.8). **rc2** wires the operators into plan-tree fan-out via RFC-042's body-re-execution engine: named `choose("name", [...])` axes produce one execution per option; per-test `assume=` predicates evaluate at body entry against the current leaf's axis assignment (body-time `choose()` calls included), with predicate Starlark errors mapping to `Result="error"`; §8.7 AST denylist is enforced at spec load for lambda predicates (named `def`s slip past the static walk — same limitation as monitor predicates). **Deferred:** §8.5 plan-walker-time `assume=` pruning (rc2 halts at body entry; lifting pruning into the enumerator to shrink the plan tree up front is a follow-up) and §8.6 cost-guard for fan-out budgets. User guide: [docs/nondeterministic-operators.md](../nondeterministic-operators.md).

## Summary

Faultbox's existing fan-out primitives (`fault_matrix`, `param`, `wait_all` interleavings, `probability=` faults) are each specialized expressions of a single underlying concept: **the spec describes non-determinism, and the plan engine enumerates all possible worlds.** This RFC names the concept directly and adds four small Starlark builtins inspired by P-lang and TLA+:

- **`choose([options])`** — non-deterministic finite choice. Plan engine produces one world per option.
- **`nondet()`** — sugar for `choose([True, False])`. Non-deterministic boolean.
- **`halt()`** — terminate the current plan-tree branch. The branch contributes no test execution and no verdict; useful for pruning unreachable or uninteresting paths.
- **`assume(predicate)`** — filter the plan tree. Branches where the predicate is false are pruned.

**Note on naming:** P-lang uses bare `$` as the non-deterministic-boolean operator. Starlark's lexer cannot parse `$` as an identifier or operator — the grammar restricts identifiers to `[A-Za-z_][A-Za-z0-9_]*`. To avoid forking the upstream `go.starlark.net` parser indefinitely, we use `nondet()` as the canonical Starlark-compatible spelling. Functionally identical to P-lang's `$`.

The operators sit alongside existing fan-out primitives, not in replacement of them. `fault_matrix`, `wait_all` interleavings, and `probability=` are syntactic sugar over the same underlying machinery. RFC-044's unified-fan-out refactor consolidates the implementation; this RFC adds the new user-facing primitives.

The conceptual frame: **a Faultbox spec is a partial model of the system's space of behaviors. The plan engine explores that space exhaustively (subject to user-controlled cost knobs from RFC-042). Non-deterministic operators give the spec author the vocabulary P-lang and TLA+ have used for decades to describe model-able systems concisely.**

In-tree document: `docs/rfcs/0043-nondeterministic-operators.md`.

## Motivation

### What problem does this solve?

Today, customers expressing non-determinism in Faultbox specs reach for whichever specialized primitive happens to fit:

- *"Test both fault and no-fault paths"* → `fault_matrix([fault_assumption("X", deny=...), None])` — abuses fault_matrix to encode optional faults.
- *"Test multiple retry counts"* → `param("retries", choices=[0, 1, 3])` (RFC-013, never shipped) or hand-written `for retries in [0, 1, 3]: test(...)` loops.
- *"Test what happens if A and B race in either order"* → `wait_all(A, B, interleavings="all")` (RFC-042, new).
- *"Test the rare case where this fault fires"* → `fault(..., probability=0.1)` reframed as exhaustive in RFC-042.
- *"Test where the user's input is one of three values"* → no good answer today; users hand-roll a loop.
- *"Test where some flag is true OR false depending on env"* → again, hand-rolled.

Each pattern is a special case of "spec describes choice; engine explores both." The lack of a unified vocabulary forces:

- New users to memorize five primitives instead of one concept.
- LLM-first authoring (v0.14.0) to pick from inconsistent primitives without a unifying framing.
- Future RFCs (RFC-009 independence relation, RFC-010 DPOR) to special-case each fan-out source.

P-lang solved this with `$` and `choose`. TLA+ solved this with `\E` (exists) and `IF... THEN... ELSE`. Both languages have decades of experience modelling distributed systems. This RFC adopts their vocabulary directly — adapted for Starlark (which uses `nondet()` instead of P-lang's bare `$` due to lexer limitations) and the Faultbox runtime.

### Why is this important now?

- **The unifying frame just landed.** RFC-040 (determinism levels) and RFC-042 (plan tree) provide the substrate; without them, "non-determinism in specs" had no formal meaning. Now it does — non-determinism in the spec produces fan-out in the plan tree.
- **RFC-044 simplification gates on this.** RFC-044 withdraws RFC-013 (`param`) and unifies the fan-out machinery. Both depend on the new operators landing here so there's something to consolidate around.
- **LLM-first authoring (v0.14.0) needs a small uniform vocabulary.** An LLM generating specs will pick `choose()` over `param()` more reliably because the former is a general primitive with one form of usage. Inconsistent primitives produce inconsistent generations.
- **Spec-as-model framing aligns with the InfoSec persona pivot (v0.14.0).** Whitehat investigators thinking in terms of "what could the system do?" are well-served by P-lang-style nondet vocabulary; "fault_matrix with retries kwarg" is operationally clearer but conceptually narrower.

### What happens if we don't do this?

- Specs continue to encode non-determinism via specialized, overlapping primitives. Five names for one concept.
- LLM-generated specs are harder to validate — every fan-out source has different syntax.
- RFC-044's unified fan-out machinery has no clean abstraction to consolidate around (every fan-out source remains a one-off).
- Future RFCs (RFC-009 independence relation, DPOR) special-case each existing primitive instead of operating on a single `NonDeterministicChoice` interface.

## Current state

Today Faultbox provides:

- **`fault_matrix(...)`, `fault_assumption(...)`, `fault_scenario(...)`** (RFC-001, implemented) — produce cartesian products of fault combinations. Specialized to faults.
- **`param(name, choices=...)`** (RFC-013, **never shipped**) — parameterized scenarios. To be withdrawn by RFC-044.
- **`wait_all(..., interleavings=)`** (RFC-042, new in this epic) — fan-out over orderings of parallel branches.
- **`fault(..., probability=p)`** (existing, reframed in RFC-042) — fan-out over fired/not-fired per firing point.
- **No `nondet` / `choose` / `halt` / `assume`** — the unifying primitives don't exist.

What's missing: a small, uniform set of operators that express non-determinism directly, decoupled from specific fan-out domains (faults, params, parallel composition, probability). With them, the existing primitives become syntactic sugar.

## Proposed design

### 5.1 — `nondet()` — non-deterministic boolean

Returns a boolean whose value is non-deterministic. The plan engine fans out into two worlds — one where it's `True`, one where it's `False`:

```python
def body():
    if nondet():                                   # plan tree forks here
        api.login(user="alice")
    api.list_orders()                              # both branches reach this

test("user_logged_in_or_not", body=body)
```

Plan-tree expansion: 2 leaves per `nondet()` call.

**Spec syntax:** `nondet()` is a top-level builtin returning a `Choice` value with branches `[True, False]`. At plan generation, each occurrence in a code path produces a 2-way branch.

**Sugar form:** `nondet()` is exactly equivalent to `choose([True, False])`. Both compile to the same plan-tree fan-out internally. `nondet()` is shorter and matches P-lang readers' expectations; `choose([True, False])` is explicit if you prefer the unified vocabulary.

**Counting:** N occurrences of `nondet()` in a single code path → 2^N plan-tree leaves for that path. Cost-gated by RFC-042's `--check-cost`.

### 5.2 — `choose([options])` — non-deterministic finite choice

Pick one element from a finite, statically-known set non-deterministically. Plan engine produces one world per option:

```python
def body():
    retries = choose([0, 1, 3])                     # plan tree forks 3 ways
    api.create_order_with_retries(retries)

test("retries_with_various_counts", body=body)

# With a name (for the report; equivalent to RFC-013's `param(name, choices=...)`):
def body():
    retries = choose("retries", [0, 1, 3])
    api.create_order_with_retries(retries)

# Choice over service references (top-level — runs at spec load):
target = choose([primary_db, replica_db])
fault(target.main, deny=on(connect=True))

# Choice over fault definitions:
err = choose([
    fault_assumption("503", http_status=503),
    fault_assumption("504", http_status=504),
    fault_assumption("timeout", http_timeout=True),
])
```

**Spec syntax:** `choose(options)` or `choose(name, options)`. `options` must be a list of finite, statically-evaluable values (numbers, strings, fault definitions, service refs). Lists with runtime-computed elements are rejected at spec load with: *"`choose` requires a statically-evaluable list; for runtime-computed choices, refactor to express the dependency at spec load."*

**Plan-tree expansion:** N options → N plan-tree leaves per occurrence.

**Subsumes `param(name, choices=...)` from RFC-013.** RFC-044 withdraws `param`; `choose("name", [values])` is the replacement.

### 5.3 — `halt()` — terminate the current branch

Mark the current plan-tree branch as ignorable. The branch contributes no test execution, no bundle, no verdict. Useful for pruning unreachable or uninteresting paths:

```python
def body():
    retries = choose([0, 1, 3])
    if retries == 0 and nondet():                   # rare invalid combo we don't care about
        halt()                                       # this leaf is pruned
    api.run_workflow(retries=retries)

test("flow_with_invalid_states_pruned", body=body)
```

**Plan-tree semantics:** when execution reaches `halt()`, the current plan-tree leaf is marked **halted**. Halted leaves are:
- Counted in `faultbox plan` output as "halted" (separate from PASS/FAIL/INCONCLUSIVE counts).
- Not executed.
- Not present in the bundle's plan.json as executable test instances; they're recorded in a separate "halted_branches" list with the path of `choose`/`nondet` decisions that led to them, for audit.

**Why have `halt()` if you can express the same with `assume`?** Symmetry with P-lang and TLA+ where `halt` and `assume` are both first-class. `halt()` is *imperative* (used inside the body); `assume()` is *declarative* (a condition checked at spec load / plan-time). They overlap in expressiveness but read differently:

```python
# halt() — imperative pruning
def body():
    retries = choose([0, 1, 3])
    if retries == 0 and nondet():
        halt()
    api.run(retries)

# assume() — declarative condition (Section 5.4)
assume(lambda choices: not (choices.retries == 0 and choices.fault_fired))
def body():
    retries = choose([0, 1, 3])
    api.run(retries)
```

Both prune the same branch; the second is cleaner when the condition is across multiple choices.

### 5.4 — `assume(predicate)` — filter the plan tree

Declarative pruning: the plan engine evaluates the predicate against the choices made in each leaf and prunes leaves where the predicate returns false:

```python
test("only_realistic_combinations",
    setup = setup,
    assume = [
        # Don't test retries=0 with the long timeout — meaningless combo.
        lambda choices: not (choices.retries == 0 and choices.timeout == "30s"),
        # Don't test fast retries with short timeout — known to be a flake source.
        lambda choices: not (choices.retries == 3 and choices.timeout == "1s"),
    ],
    body = lambda: ...,
)
```

```python
def setup():
    retries = choose("retries", [0, 1, 3])
    timeout = choose("timeout", ["1s", "5s", "30s"])
```

**On the optional `name=` argument:** providing a name is **only required when an `assume` predicate references the choice by name**. For unnamed `choose([options])`, the report labels choices by position (`choice@<line>:<col>`) and `assume` predicates can't reference them by attribute. Named form is the right pattern when constraints between choices matter; unnamed is fine for choices used purely inside the body.

**Spec syntax:** `assume=[predicates]` on `test()` (or top-level `assume(predicate)` for spec-wide constraints). Each predicate accepts a `choices` object — a struct with one field per `choose(name, ...)` call, holding the value selected for that leaf.

**Plan-tree semantics:** at plan generation, for each leaf, evaluate every assume predicate. If any returns false, prune the leaf. Pruned leaves are surfaced in `faultbox plan` output as "pruned by assume" with the violated predicate cited.

**Predicate context:** assume predicates are pure functions of the choices made (and possibly fault assumptions, params, etc. — anything from the plan-tree leaf's selection vector). They're evaluated **at plan time, not test time** — they don't see runtime events.

For runtime-event-based pruning, use `halt()` inside the body (Section 5.3). For combination filtering at plan time, use `assume`.

### 5.5 — Composition with existing fan-out sources

The new operators compose seamlessly with existing fan-out:

```python
def setup():
    # Three of the five fan-out sources composing — three named choices for assume access:
    return struct(
        retries    = choose("retries", [0, 1, 3]),
        cache_warm = choose("cache_warm", [True, False]),   # equivalent to nondet() with a name
        target     = choose("target", [primary, replica]),
    )

def body(s):
    api.complex_workflow(retries=s.retries, cache_warm=s.cache_warm, target=s.target)

test("complex",
    setup = setup,
    fault_matrix = [http_5xx_errors, db_errors],            # RFC-001: 4 fault combinations
    assume = [
        lambda c: not (c.retries == 0 and c.cache_warm),    # silly combo pruned
    ],
    body = body,
    expect = eventually(...),
)
```

Plan-tree leaves: `3 (retries) × 2 (cache_warm) × 2 (target) × 4 (fault_matrix) = 48`, minus the leaves pruned by `assume`. The composition is cartesian product (the unified fan-out machinery from RFC-044 makes this clean).

**Order of evaluation:**
1. Plan generation enumerates the cartesian product of all fan-out sources.
2. For each leaf, `assume` predicates are evaluated; failing leaves are pruned.
3. For each surviving leaf, the body runs with the selected choices.
4. `halt()` calls inside the body prune the leaf at runtime (after some events may have been emitted; the leaf result is "halted," distinct from PASS/FAIL/INCONCLUSIVE).

### 5.6 — Implementation walkthrough: how plan-tree fan-out actually happens

The non-deterministic operators are not magic — they're implemented via **classical model-checking traversal** of the spec body, executed in two phases.

#### Phase 1: Plan-discovery (no services launched, no real I/O)

```
1. Faultbox executes the spec body in *discovery mode*.
2. Service-method calls (api.X(), db.Y(), etc.) are stubbed — recorded but not invoked.
3. nondet() / choose() calls are intercepted; each call site is assigned a position index.
4. When a position is reached without a pre-set value, fork: pick one outcome, enqueue the
   alternatives for later exploration.
5. After body returns, record the selection vector as a plan-tree leaf.
6. Dequeue the next alternative selection vector and re-execute the body from scratch.
7. Continue until no alternatives remain.
```

This is the same algorithm SPIN, TLC, and P-lang use — on-the-fly choice-tree enumeration.

#### Worked example

```python
def body():
    if nondet():
        api.http_get_1()
    else:
        api.http_get_2()
```

| Run | Selection vector at start | Path taken | Selection vector at end | Action |
|-----|---------------------------|------------|-------------------------|--------|
| 1 | `[]` | `nondet()` at position 0 → defaults to `True`; alternative `[False]` enqueued | `[True]` | Recorded as leaf #1 |
| 2 | `[False]` (dequeued) | `nondet()` at position 0 → returns `False` | `[False]` | Recorded as leaf #2 |
| — | (queue empty) | — | — | Plan-discovery done |

Plan tree: 2 leaves. Each leaf carries `(call_site, selected_value)` pairs.

For `choose([1, 2, 3])`: same shape — 3 runs with vectors `[1]`, `[2]`, `[3]`, producing 3 leaves.

#### Asymmetric trees — choices revealed by other choices

```python
def body():
    if nondet():               # call #0
        if nondet():            # call #1 — only reachable when call #0 was True
            pass               # (placeholder for body work)
```

Plan-discovery iterates as choices reveal new choice sites:

| Run | Vector | Reaches | Records |
|-----|--------|---------|---------|
| 1 | `[]` → `[True, True]` | calls #0 (True, alt `[False]` enqueued), #1 (True, alt `[True, False]` enqueued) | Leaf `[True, True]` |
| 2 | `[True, False]` | call #0 (True), #1 (False per vector) | Leaf `[True, False]` |
| 3 | `[False]` | call #0 (False); #1 unreachable on this branch | Leaf `[False]` |

Plan tree: 3 leaves, asymmetric. Some branches have more choice points than others — discovered as paths are taken.

#### Phase 2: Test execution

For each plan-tree leaf, Faultbox launches services and re-executes the body with `nondet()`/`choose()` returning the leaf's pre-fixed values. *Now* `api.http_get_1()` etc. are real calls; events are captured; bundles are written.

```
Leaf [True]:  services started → body run → http_get_1 fires → events captured → bundle written
Leaf [False]: services started → body run → http_get_2 fires → events captured → bundle written
```

Each leaf is its own bundle, its own trace, its own PASS / FAIL / INCONCLUSIVE verdict — exactly the model `fault_matrix` already uses.

#### Why this works (and required spec discipline)

- **Starlark is pure** — spec body has no host I/O during evaluation. Re-running with a different selection vector produces identical control flow given identical inputs. This is what makes plan-discovery via re-execution valid.
- **Faultbox builtins behave differently** in discovery mode (stubbed) vs execution mode (real). The runtime tracks which mode it's in.
- **Spec body must be re-executable.** No mutable host state captured between runs. Starlark's value semantics give this for free; users don't have to do anything special.
- **Call-site positional.** "The 3rd `nondet()` reached during this run" is the canonical key. Two `nondet()` calls in the same place across runs are the same call site; in different code paths they're different sites.

#### Cost characteristics

- Plan-discovery is fast: each run is Starlark interpretation with stubbed builtins. ~milliseconds for typical specs.
- The exploration queue's max size = total plan-tree size, bounded by the cartesian product of all reachable choice points.
- RFC-042's `--check-cost --max-instances=N` runs *after* plan-discovery completes; if the count exceeds budget, spec load warns/errors **before** Phase 2 launches any services. No CI time wasted on accidental explosions.

### 5.7 — Determinism level interactions

The operators add fan-out *axes* to the plan tree (RFC-042). At every level:

- **L0 (plan determinism):** the plan tree is reproducible — same spec + seed → same enumeration. Each fan-out branch has a deterministic identifier in `plan.json`.
- **L1+:** within a single branch, the test executes deterministically per the level's contract. Replay reproduces the same branch identically.

Importantly, **the operators don't alter the level** the spec is operating at — they're orthogonal. A spec with `nondet()` and `choose` running at L1 still gets L1's mediated-event determinism per branch. The branches differ in what choices were made; the determinism contract within each branch is unchanged.

### 5.8 — Spec-load coherence checks

Static checks at spec load:

1. **`choose` options must be statically evaluable.** Reject lists with runtime-computed elements (e.g. `choose(api.list_targets())` — runtime call).
2. **`assume` predicates must be pure.** Reject predicates that reference services, faults, or any non-choice runtime state. Static analysis at spec load: walk the lambda's free variables, allow only `choices` parameter and statically-evaluable globals.
3. **`halt()` only inside test body.** Reject `halt()` at module top-level or inside `setup=`.
4. **Cost guard.** If the cartesian product (before `assume` pruning) exceeds `--max-instances` (RFC-042's `--check-cost` flag), spec-load warn (not error) with the count and a recommendation to add `assume` constraints.

### 5.9 — Future direction: independence relations and DPOR

The four operators all produce branches in the plan tree. Two future RFCs operate on this:

- **RFC-009 (Independence Relation)** — uses Mazurkiewicz-trace theory to identify branches that are observationally equivalent (different choices but same final state). Equivalent branches collapse into one execution. This dramatically reduces cost for high-fan-out specs.
- **RFC-010 (DPOR — Dynamic Partial-Order Reduction)** — uses runtime dependency information to prune branches that can't differ from already-explored branches. Sharper than independence relations because it uses observed data.

Both depend on the unified `NonDeterministicChoice` abstraction RFC-044 introduces — which is exactly the abstraction this RFC's operators slot into. The RFC doesn't commit to RFC-009 / RFC-010 implementation; it positions the operators so those refinements land naturally when they ship.

## v0.13.0 implementation scope

The committed v0.13.0 work for this RFC:

### 8.1 — `nondet()` builtin

In `internal/star/builtins.go`. Implemented as sugar that returns `choose([True, False])` — i.e. a 2-branch `Choice` value. No special parser handling needed; it's a regular Starlark function call. The plan-generation pass treats it identically to `choose([True, False])`.

### 8.2 — `choose(options)` and `choose(name, options)` builtins

Two-arity overload. Returns a `Choice` value carrying the selected option for the current plan-tree leaf. At plan generation, fan out over options.

Subsumes `param(name, choices=...)` from RFC-013 (RFC-044 handles the deprecation/migration).

### 8.3 — `halt()` builtin

Marks the current plan-tree leaf as halted. Body execution stops at the `halt()` call site; the leaf is recorded as halted in `plan.json` (RFC-042) with the choice path that led to it. Halted leaves don't contribute to PASS/FAIL/INCONCLUSIVE counts.

### 8.4 — `assume(predicate)` and `test(assume=[...])`

Two forms:
- Top-level `assume(predicate)` for spec-wide constraints.
- `test(assume=[predicates])` kwarg for per-test constraints.

Plan-time evaluation: for each leaf, run all applicable assume predicates against the choice vector; prune failing leaves.

### 8.5 — Plan-tree integration

Hook the four operators into RFC-042's plan-tree expansion. The unified `NonDeterministicChoice` abstraction (RFC-044) is the interface; this RFC's operators are four implementations:

| Operator | `NonDeterministicChoice` instance |
|----------|----------------------------------|
| `nondet()` | Sugar for `choose([True, False])` — same as below |
| `choose([options])` | N-branch choice with values from options |
| `halt()` | Pseudo-choice with one branch (halted); special-cased in expansion |
| `assume(p)` | Plan-tree filter, not a choice; evaluated post-cartesian-product |

### 8.6 — Spec-load coherence checks

Per Section 5.8. Implemented in `internal/star/builtins.go` validation passes.

### 8.7 — Restricted Starlark sandbox for `assume` predicates

Like monitor `update`/`check` predicates (RFC-041 §5.4), `assume` predicates run in a restricted Starlark sandbox with no service / plugin access — only the `choices` parameter and statically-evaluable Starlark builtins. Implementation in `internal/star/assume_sandbox.go`.

### 8.8 — Documentation

- New file: `docs/nondeterministic-operators.md` — the four operators, their semantics, composition with existing primitives, examples.
- Update `docs/spec-language.md` — operator section.
- Update `docs/feature-manifest.md` — operators row.
- Tutorial chapter — walk a small spec through `nondet()` and `choose` with the resulting plan tree visualized.
- Migration note in v0.13.0 release notes for the `param` → `choose` rename (per RFC-044).

### 8.9 — Tests

Per the #84 coverage gate. Goldens for representative scenarios:
- Single `nondet()` produces 2-leaf plan
- `choose([1,2,3])` produces 3-leaf plan
- Composition: `nondet() × choose × fault_matrix` produces correct cartesian count
- `assume` prunes expected leaves
- `halt()` records correct halt reason in plan.json

## Out of scope for v0.13.0

- **Independence-relation refinement** (Mazurkiewicz traces collapsing equivalent branches) — RFC-009.
- **DPOR — Dynamic Partial-Order Reduction** — RFC-010.
- **Probabilistic `choose([options], weights=...)`** — weighted non-determinism. The current `choose` is uniform; weighted choice is potential future. Easy to add additively.
- **Symbolic / parametric ranges** — `choose(range(0, 100))` for small ranges works (literally 100 branches), but real symbolic ranges (e.g. integers up to MAX_INT) require SAT/SMT-style reasoning, deferred indefinitely.
- **Auto-`assume` from type constraints** — inferring obvious incompatibilities (e.g. `retries < 0` doesn't make sense). Doable later; not in scope.

## Open questions

1. **`assume` with cross-leaf state.** Some constraints want to express "no two leaves have the same combination of (choice_a, choice_b)" — i.e. dedup. Strawman: out of scope; users can dedup post-hoc or use distinct `choose` values. Revisit if customer demand emerges.
2. **`halt()` accounting in `plan.json`.** Should halted leaves be invisible in the count, surfaced as a separate category, or counted as "skipped"? Strawman: separate category — `halted` count alongside PASS/FAIL/INCONCLUSIVE in the report. Distinct from "pruned by assume" (which never executes) because `halt()` runs partway through the body and may have side effects.
3. **Naming `assume` vs `where` vs `filter`.** TLA+ uses `assume`; some testing frameworks use `where`. Strawman: `assume` for fidelity to the model-checking literature (which the InfoSec persona, our v0.14.0 primary target, will recognize).
4. **Should `choose` accept `weights=` for probabilistic exploration sampling?** Strawman: no, in v0.13.0. Exhaustive coverage by default; sampling is a separate concern (RFC-042 handles probability fan-out as its own axis). If demand emerges, add as optional kwarg.

**Resolved:**
- ~~`$` syntax acceptability.~~ Resolved 2026-05-04: Starlark's lexer cannot parse `$` as an identifier or operator (grammar restricts to `[A-Za-z_][A-Za-z0-9_]*`). Forking the upstream `go.starlark.net` parser would impose ongoing maintenance cost for a single-character savings. Decision: ship `nondet()` as the canonical builtin, implemented as sugar that returns `choose([True, False])`. P-lang flavor preserved without the parser fork.

## Implementation plan

| Phase | Scope | Target |
|-------|-------|--------|
| 8.1 `nondet()` | Builtin sugar over `choose([True, False])` (overloaded against existing `nondet(svc)` form) | ✅ landed (v0.13.0-rc1) |
| 8.2 `choose` | Builtin with one- and two-arity forms; ChoiceVal value type; rc1 returns first option | ✅ landed (v0.13.0-rc1) |
| 8.3 `halt` | Body builtin marking current leaf halted; SuiteResult.Halted + bundle outcome | ✅ landed (v0.13.0-rc1) |
| 8.4 `assume` | Top-level builtin + `test(assume=)` kwarg; sandboxed evaluation thread | ✅ landed (v0.13.0-rc1) |
| 8.5 Plan-tree integration | Hook into RFC-042's NonDeterministicChoice | 🟡 deferred to rc2 (single-leaf in rc1) |
| 8.6 Spec-load checks | Static validation per Section 5.8 | 🟡 partial in rc1 (halt() outside body rejected); rest defer to rc2 |
| 8.7 Restricted sandbox for `assume` | AST walk like monitor predicates (RFC-041) | 🟡 deferred to rc2 (predicates run in tagged thread today; AST walk later) |
| 8.8 Docs | `docs/nondeterministic-operators.md`, spec-language, manifest | ✅ landed (v0.13.0-rc1; tutorial chapter with rc2) |
| 8.9 Tests | Unit tests under #84 coverage | ✅ landed (v0.13.0-rc1; testops goldens with rc2 alongside fan-out) |
| Independence-relation refinement (out of this RFC) | Collapse equivalent branches | RFC-009 |
| DPOR (out of this RFC) | Runtime branch pruning | RFC-010 |
| Weighted `choose` (out of this RFC) | `weights=` kwarg | Future, demand-driven |

## Dependencies

- **Depends on:** RFC-040 (#109, Determinism Levels) — provides the level vocabulary; non-determinism only makes sense inside a level contract.
- **Depends on:** RFC-042 (Exploration Plan — to be filed) — provides plan-tree machinery the operators feed into.
- **Subsumes:** RFC-013 (Parameterized Scenarios — Draft) — `param(name, choices=...)` becomes `choose(name, options)`. RFC-044 handles the formal withdrawal.
- **Drives:** RFC-044 (Spec Language Simplification — to be filed) — the `NonDeterministicChoice` abstraction RFC-044 introduces is what these operators implement; without them, RFC-044 has no clean abstraction to consolidate around.
- **Unblocks:** RFC-009 (Independence Relation), RFC-010 (DPOR) — both operate on the unified `NonDeterministicChoice` abstraction.

---

## References

- RFC-040 (#109, Determinism Levels) — provides level vocabulary.
- RFC-042 (Exploration Plan — to be filed) — provides plan-tree machinery.
- RFC-044 (Spec Language Simplification — to be filed) — consolidates fan-out machinery this RFC's operators feed into.
- RFC-013 (Parameterized Scenarios) — to be Withdrawn by RFC-044; `param()` subsumed by `choose()`.
- RFC-009 / RFC-010 — future; benefit from the unified abstraction.
- P-lang manual: https://p-org.github.io/P/manualoutline/expressions/ — `$` (rendered here as `nondet()`), `choose`, `halt`, `assume` directly inspire this RFC's operators.
- TLA+ — `\E` (exists), `IF...THEN...ELSE`, model-checking convention. Inspired the framing "spec describes non-determinism, engine explores all worlds."
- Customer feedback: 2026-04-22 customer-feedback-analysis (Notion), Group D on test-count predictability and spec-language ergonomics.
