# RFC-044: Spec Language Simplification

> **Status: Implemented (v0.13.0).** All eight sub-sections shipped: §8.1 (RFC-013 withdrawal) + §8.3 (`faultbox generate` deprecation) + §8.4 (RFC-002 withdrawal) + §8.5 (L1-cap documentation) in C1 (#126); §8.2 (unified fan-out machinery — `NonDeterministicChoice` interface in `internal/star/nondet.go`) in C2 (#127); §8.6 (`observe.*` namespace) + §8.7 (`decoder("name", ...)` dispatcher) in C3, with deprecated aliases for the pre-rc2 surface that emit a one-time stderr warning and route through the canonical implementation. Removal of the deprecated aliases is scheduled for v0.14.0.

## Summary

The four preceding v0.13.0 RFCs (Determinism Levels, Temporal Properties, Exploration Plan, Non-deterministic Operators) introduce a unified conceptual frame: *spec describes non-determinism, engine enumerates all worlds, levels formalize the contract*. With this frame in place, several pre-existing primitives in the spec language become redundant, oversized, or split-where-they-should-be-unified. This RFC consolidates seven cleanups into a single coordinated change so that v0.13.0 ships not just new features but a smaller, clearer language.

The seven items, grouped by impact:

**Major (engine + API):**
1. Withdraw RFC-013 (`param`); fold parameterized scenarios into RFC-043's `choose()`.
2. Refactor the engine's fan-out machinery into one mechanism that handles `fault_matrix`, `choose`, `nondet()`, `interleavings=`, and `probability=` uniformly.
3. Merge `faultbox generate` into `faultbox plan --suggest`.

**Medium (status decision + docs):**
4. Withdraw RFC-002 (`domain()`).
5. Document `remote=` and `reuse=True` as structurally L1-capped features.

**Polish (cosmetic API):**
6. Unify event-source builtins (`stdout`, `stderr`, `topic`, `tail`, `wal_stream`, `poll`) under one `observe.*` namespace.
7. Unify decoder builtins (`json_decoder`, `logfmt_decoder`, `regex_decoder`) under one `decoder("name", ...)` form.

This RFC is **implemented last** in v0.13.0 because items #1–#3 require their replacements (RFC-043's `choose`, RFC-042's plan machinery and `--suggest`) to be shipped first, item #2 is cleanest when all fan-out producers are stable, and the rest naturally land at the end of an epic. The *design* in this RFC, however, ships immediately so RFC-042 and RFC-043 implementers reference it and avoid baking in code that would have to be ripped out.

In-tree document: `docs/rfcs/0044-spec-language-simplification.md`.

## Motivation

### What problem does this solve?

Faultbox accreted primitives over many releases. Each was added in response to a specific need; collectively they overlap, duplicate, and confuse:

- `param("retries", choices=[0, 1, 3])` (RFC-013, never shipped) and `choose([0, 1, 3])` (RFC-043, new) are the same primitive with two names.
- Fan-out happens in five separate code paths: `fault_matrix`, `param`, `interleavings=`, `probability=`, `choose` / `nondet()`. Each grew its own expansion logic in `internal/star/`. Five mechanisms for one concept.
- `faultbox generate` (failure-scenario synthesis) and `faultbox plan --suggest` (RFC-042 — coverage-driven suggestions) overlap. Two commands, both producing test stubs from topology analysis.
- `domain()` (RFC-002) has been Draft for the entire project lifetime with zero implementation. Every new RFC has to reason about whether it interacts with the unshipped abstraction; the indecision is a tax on every design conversation.
- `remote=` and `reuse=True` are silently incompatible with L4 hermetic determinism (RFC-040). Today's docs don't say this; users discover it by trying.
- Six event-source builtins (`stdout`, `stderr`, `topic`, `tail`, `wal_stream`, `poll`) and three+ decoder builtins (`json_decoder`, `logfmt_decoder`, `regex_decoder`) clutter the top-level Starlark namespace where one `observe.*` and one `decoder("name", ...)` would do.

The accretion isn't a bug — it's how language features get added in a startup. But v0.13.0's conceptual unification under non-determinism is a natural moment to spend down the debt.

### Why is this important now?

- **The unifying frame just landed.** Without RFC-040 (determinism levels) and RFC-043 (non-deterministic operators), there's no principled way to argue "these five primitives are the same." With them, the cleanup is obvious.
- **Major-version threshold.** v0.13.0 is positioned as a major release; v0.13.x and v0.14.x are tuning. Breaking changes are cheaper to ship now than to propagate through subsequent minors.
- **Engine code is about to grow.** RFC-042 (plan-tree machinery, interleaving fan-out, probability fan-out) and RFC-043 (`nondet()`/`choose`/`halt`/`assume`) add significant code. Doing the unification refactor *after* they land — but in the same epic — keeps the grown surface manageable.

### What happens if we don't do this?

- Spec authors keep tracking two-or-more-names-for-the-same-thing forever. The LLM-first user (v0.14.0) sees an inconsistent surface and asks the wrong primitive for what they want.
- Engine code accumulates parallel expansion paths. Future RFCs (RFC-009 Mazurkiewicz refinement; RFC-010 DPOR) have to fork each path separately.
- `domain()` (RFC-002) keeps appearing as a "what about this?" footnote in every design conversation.
- Customers learning Faultbox have a steeper hill to climb because the language has more surface area than concepts.

## Current state — what exists today

| Primitive | Source | Status | Issue |
|-----------|--------|--------|-------|
| `param(name, choices=...)` | RFC-013 | Draft (never shipped) | Duplicates `choose()` (RFC-043) |
| `fault_matrix(...)` | RFC-001 | Implemented | Bespoke fan-out path |
| `choose([...])` | RFC-043 | Drafted (this epic) | New |
| `nondet()` (nondet bool) | RFC-043 | Drafted (this epic) | New |
| `wait_all(..., interleavings=)` | RFC-042 | Drafted (this epic) | Bespoke fan-out path |
| `fault(..., probability=)` | Pre-RFC + RFC-042 reframe | Implemented (stochastic); reframed exhaustive in RFC-042 | Bespoke fan-out path |
| `faultbox generate` | Existing | Implemented | Overlaps with `plan --suggest` |
| `domain()` | RFC-002 | Draft (never shipped) | Indecision tax |
| `service(remote=...)` | RFC-036 | Implemented | L1-only, undocumented |
| `service(image=..., reuse=True)` | RFC-015 | Implemented | L1/L2/L3-only, undocumented |
| `stdout()`, `stderr()`, `topic()`, `tail()`, `wal_stream()`, `poll()` | Various | Implemented | 6 top-level builtins for one concept |
| `json_decoder()`, `logfmt_decoder()`, `regex_decoder()` (+ future `proto_decoder()` from RFC-045) | Various | Implemented | 3+ top-level builtins for one concept |

## Proposed design

### 5.1 — Withdraw RFC-013; `choose()` replaces `param()`

RFC-013's parameterized scenarios are RFC-043's `choose()` with a `name=` annotation for the report. Keep the name semantics; drop the separate primitive:

```python
# Before (RFC-013, never shipped):
retries = param("retries", choices=[0, 1, 3])

# After (RFC-043 with optional name):
retries = choose("retries", [0, 1, 3])    # name positional, optional
# or
retries = choose([0, 1, 3], name="retries")
```

**Action:** RFC-013's status changes from Draft to Withdrawn. RFC-043's `choose()` gains the optional `name=` argument (and accepts it positionally for ergonomics). One less primitive in the spec language.

### 5.2 — Unified fan-out machinery

Five code paths today (`fault_matrix`, `param`, `interleavings=`, `probability=`, `choose` / `nondet()`) collapse into one mechanism in `internal/star/`. The unified abstraction:

```go
// internal/star/nondet.go
type NonDeterministicChoice interface {
    Source() string                    // "fault_matrix:http_errors", "interleaving:wait_all_3", etc.
    Branches() []Branch                // ordered list of value/config alternatives
    Cardinality() int                  // len(Branches)
    Independence() []string            // labels for future Mazurkiewicz collapse (RFC-009)
}

type Branch struct {
    Label    string                    // human-readable identifier
    Value    interface{}               // payload — fault config, ordering, nondet-bool, etc.
    Metadata map[string]interface{}    // probability, etc.
}
```

Today's specialized expansions become thin wrappers:

| Today | Becomes |
|-------|---------|
| `fault_matrix([a, b, c])` | Returns `[]NonDeterministicChoice` with one Choice per axis |
| `choose([0, 1, 3])` | Returns `NonDeterministicChoice` with 3 branches |
| `nondet()` | Returns `NonDeterministicChoice` with 2 branches (`True`, `False`) — sugar for `choose([True, False])` |
| `wait_all(..., interleavings="all")` | Returns `NonDeterministicChoice` with one branch per ordering |
| `fault(..., probability=p)` per firing point | Returns 2-branch `NonDeterministicChoice` (fired, not fired) |

**Plan-tree expansion** (in `internal/engine/`) becomes a single recursive cartesian product over the list of `NonDeterministicChoice`. Plan-tree leaves carry the full `[]Branch` selection vector.

**Why this matters beyond elegance:**

- RFC-009 (independence relation) operates on `Independence()` labels — adding it touches one place, not five.
- RFC-010 (DPOR) operates on the same abstraction.
- Every future fan-out source (e.g. environment fault injection, time perturbation) gets the existing machinery for free.
- Engine code shrinks from N×M expansion logic to N+M.

This is a pure refactor at the user-facing API level. `fault_matrix`, `choose`, `nondet()`, `interleavings=`, `probability=` keep their current spelling; their *implementations* unify.

### 5.3 — Merge `faultbox generate` into `faultbox plan --suggest`

Today: `faultbox generate` produces failure-scenario mutations; user pastes patches manually. RFC-042: `faultbox plan --suggest` produces test stubs from coverage gaps; user pastes manually.

Both walk the topology, identify gaps or mutations, emit Starlark stubs. Two paths into one suggestion engine.

**Action:**
- v0.13.0: `faultbox generate` continues to work but emits a deprecation warning pointing to `faultbox plan --suggest`. Internally `generate` becomes a thin alias (calls `plan --suggest --auto-write`).
- v0.14.0: remove `faultbox generate` entirely.

### 5.4 — Withdraw RFC-002 (`domain()`)

RFC-002's `domain()` was meant to be a multi-service grouping abstraction; the project pivoted to `service()`-first and never came back. Decision: withdraw, not ship.

**Action:** RFC-002 status changes from `—` (open) to `Withdrawn` with rationale: *"The service-first model proved sufficient for the customer use cases that motivated v0.3–v0.12. Multi-service grouping can be expressed in user code via Starlark functions. If a future use case demonstrates need (e.g. persona plugins for EM/Staff engineers thinking in domains), revisit then."*

**Implication:** every future RFC stops needing to consider `domain()` interaction. Indecision tax paid down.

### 5.5 — Document L1-cap features

`remote=` (RFC-036) and `reuse=True` (RFC-015) are structurally incompatible with higher determinism levels:

- `remote=` is forbidden at L4 (RFC-040). Achievable up to L3 only via record-and-replay (RFC-037, deferred).
- `reuse=True` is forbidden at L4. Composable with L1/L2/L3.

**Action (pure docs):**
- `docs/spec-language.md` — add a "Determinism interactions" subsection per feature. State the cap explicitly.
- `docs/determinism.md` — feature-compatibility table by level.
- `service()` builtin error messages: when a spec uses `determinism="L4"` and `remote=` or `reuse=True`, the spec-load error names the offending kwarg and points to this RFC.

No code changes beyond the error-message wiring.

### 5.6 — Unify event-source builtins under `observe.*`

Today: six top-level builtins for event sources (`stdout`, `stderr`, `topic`, `tail`, `wal_stream`, `poll`).

**After:**

```python
observe = [
    observe.stdout(decoder=decoder("json")),
    observe.stderr(decoder=decoder("logfmt")),
    observe.topic(broker="...", topic="orders", decoder=decoder("proto", schema="...")),
    observe.tail(path="/var/log/app.log"),
    observe.wal_stream(slot="repl_slot"),
    observe.poll(url="...", interval="5s"),
]
```

Implementation: `observe` becomes a Starlark module (struct-with-attrs) holding the existing factory functions. The plugin registry (`internal/eventsource/.RegisterSource`) is unchanged; only the surfaced API is.

**Migration:** v0.13.0 keeps the bare names (`stdout()`, `topic()`, etc.) as deprecated aliases that emit a one-line warning at spec load. v0.14.0 removes them.

### 5.7 — Unify decoder builtins under `decoder("name", ...)`

Today: separate top-level builtins per decoder (`json_decoder()`, `logfmt_decoder()`, `regex_decoder()`, future `proto_decoder()` from RFC-045).

**After:**

```python
decoder("json")
decoder("logfmt")
decoder("regex", pattern=r"...")
decoder("proto", schema="...", message="orders.OrderCreated")
decoder("avro", schema_registry="...")    # future
```

Implementation: `decoder()` is a single Starlark builtin that dispatches to the registered `DecoderFactory` (`internal/eventsource/.RegisterDecoder`) by name. The plugin registry is unchanged; the surfaced API collapses from N builtins to one.

**Migration:** same as 5.6 — v0.13.0 keeps the per-decoder builtins as deprecated aliases; v0.14.0 removes them. RFC-045 (Protobuf decoder) ships under the new form directly.

## v0.13.0 implementation scope

The committed v0.13.0 work for this RFC, **scheduled last in the epic**:

### 8.1 — `param()` withdrawal

- Update `docs/rfcs/0013-parameterized-scenarios.md` status: Draft → Withdrawn. Add rationale paragraph.
- Update `docs/rfcs/README.md` table.
- `internal/star/builtins.go`: if `param()` was ever stubbed (it wasn't — RFC-013 never shipped), remove. Otherwise no code change.
- RFC-043's `choose()` gains `name=` (positional or keyword).

### 8.2 — Unified fan-out machinery

> **Status: Implemented (C2, v0.13.0).** `NonDeterministicChoice` interface lives in `internal/star/nondet.go` with `Cardinality()` and `Apply(leaf, digit)` methods. `ChoiceVal`, `ProbFaultSite`, and `ParallelSite` implement the interface; `enumerateLeaves` is now a thin wrapper that flattens its three source slices into a homogeneous `[]NonDeterministicChoice` via `collectChoices` and forwards to a generic mixed-radix walker `expandLeaves`. All pre-existing testops goldens unchanged (the user-facing plan-tree shape is byte-identical to the rc2 implementation). **Scope note:** `fault_matrix` is intentionally NOT a NonDeterministicChoice — it produces discrete *test* entries at spec load time via `internal/plan/`, not *leaves* of one test at body time. The five-code-paths claim in the original RFC overstated the situation post-rc2 (only one path needed unifying — the others already went through `enumerateLeaves`).

Original sequence (kept for historical context):

1. Define `NonDeterministicChoice` and `Branch` types in `internal/star/nondet.go`.
2. Refactor `fault_matrix` expansion to produce `[]NonDeterministicChoice` (was: list of dicts). — *NOT done; fault_matrix is a test fan-out, not a leaf fan-out.*
3. Refactor RFC-043's `choose` and `nondet()` to produce same. — *done.*
4. Refactor RFC-042's `interleavings=` expansion to produce same. — *done.*
5. Refactor RFC-042's `probability=` expansion to produce same. — *done.*
6. Replace specialized cartesian product code in `internal/engine/` with a single recursive expansion over `[]NonDeterministicChoice`. — *done in `internal/star/nondet.go::expandLeaves`; engine never had its own expansion code.*
7. Verify all pre-existing goldens unchanged (the user-facing plan tree must be byte-identical to before refactor). — *done.*

### 8.3 — `faultbox generate` deprecation

- Add deprecation warning to `cmd/faultbox/generate.go` pointing to `plan --suggest`.
- Reroute `generate` to `plan --suggest --auto-write` internally (thin alias).
- Update `docs/cli-reference.md`.
- Mark for removal in v0.14.0 release notes.

### 8.4 — RFC-002 (`domain`) withdrawal

- Update `docs/rfcs/README.md`: RFC-002 status to Withdrawn.
- Add a Withdrawal rationale section to RFC-002 in tree (file doesn't exist; create a stub `docs/rfcs/0002-domain.md` with status and rationale only).
- No code changes; nothing to remove.

### 8.5 — L1-cap feature documentation

- `docs/spec-language.md` — Determinism interactions subsection per `remote=` and `reuse=True`.
- `docs/determinism.md` — feature-compatibility table.
- `internal/star/builtins.go` — error-message wiring for L4 + `remote=` / L4 + `reuse=True`.

### 8.6 — `observe.*` unification

> **Status: Implemented (C3, v0.13.0).** New Starlark module `observe` (a `starlarkstruct.Struct`) exposes `observe.stdout` and `observe.stderr` as attributes (`internal/star/observe_decoder.go::makeObserveModule`). Top-level `stdout()` / `stderr()` remain registered as deprecated aliases (`builtinStdoutDeprecated` / `builtinStderrDeprecated`) that emit a one-time stderr warning per process and delegate to the canonical implementation. **Scope correction:** the RFC originally listed `topic()`, `tail()`, `wal_stream()`, `poll()` as additional factories to unify, but those were never top-level builtins — only the event source plugins ship under those names (loaded via `observe=` lists on `service()`). Future Starlark-level factories plug into `makeObserveModule()` as additional attributes.

### 8.7 — `decoder("name", ...)` unification

> **Status: Implemented (C3, v0.13.0).** Unified `decoder(name, ...)` dispatcher in `internal/star/observe_decoder.go::builtinDecoder` handles `"json"`, `"logfmt"`, and `"regex"`. Legacy `json_decoder()` / `logfmt_decoder()` / `regex_decoder()` remain registered as deprecated aliases that emit a one-time stderr warning and route through the new dispatcher — DecoderVal construction has a single source of truth. RFC-045 (Protobuf decoder) will plug in as `case "proto":` in the dispatcher.

### 8.8 — Tests

Per the #84 coverage gate: every refactored file gets coverage. Goldens for the unified plan-tree expansion against representative specs verify byte-identical output across the refactor.

## Out of scope for v0.13.0

- Any *new* primitives. RFC-044 only consolidates existing ones.
- Breaking changes that don't have a deprecated-alias migration path.
- Removing the deprecated aliases (deferred to v0.14.0).
- Renaming `service()`, `interface()`, `fault()`, `fault_assumption()`, `fault_scenario()`, `mock_service()`, or other core primitives. These are well-tuned and customer-facing; don't touch.

## Open questions

1. **Should `observe.*` use module syntax or namespaced strings?** Two options: (a) `observe.stdout(...)` — module attribute access; (b) `observe("stdout", ...)` — string dispatch like `decoder()`. Strawman: (a) for `observe.*` because each source has different kwargs that are easier to discover with attribute syntax; (b) for `decoder()` because decoders share a common interface and string dispatch fits. Inconsistent, but each form fits its domain. Worth confirming.
2. **Deprecation warning frequency.** Once-per-spec-load? Once-per-process? Strawman: once-per-spec-load, with file:line if available. Annoying enough to motivate migration without being noise.
3. **Should `faultbox generate` be removed in v0.13.0 or just deprecated?** Strawman: deprecate in v0.13.0, remove in v0.14.0. Same migration window as the other deprecations. Consistent.
4. **Plan-tree golden stability.** The unified-fan-out refactor must produce byte-identical plan trees to today's specialized paths. How thorough should the golden corpus be? Strawman: every existing example spec in `poc/` plus 5–10 hand-written stress tests. Run them all in CI as part of the refactor PR.

## Implementation plan

| Phase | Scope | Target |
|-------|-------|--------|
| Design (no code) | This RFC, referenced by RFC-042/043 implementers | v0.13.0-rc1 (drafted alongside other RFCs) |
| 8.1 `param` withdrawal | Status change, RFC-043 `choose(name=)` | v0.13.0-rc3 |
| 8.2 Unified fan-out | Largest item — refactor of `fault_matrix`, `choose`, `nondet()`, `interleavings`, `probability` expansion | v0.13.0-rc3 |
| 8.3 `generate` deprecation | Alias to `plan --suggest`; warning | v0.13.0-rc3 |
| 8.4 `domain` withdrawal | Pure docs / status change | v0.13.0-rc3 |
| 8.5 L1-cap docs | `spec-language.md`, `determinism.md`, error messages | v0.13.0-rc3 |
| 8.6 `observe.*` unification | New module, deprecated aliases | v0.13.0-rc3 |
| 8.7 `decoder()` unification | New builtin, deprecated aliases | v0.13.0-rc3 |
| 8.8 Tests | Coverage + plan-tree goldens | v0.13.0-rc3 |
| Remove deprecated aliases | `param`, `generate`, per-source/per-decoder builtins | v0.14.0 |

**Why all of 8.1–8.8 in rc3** (after rc1 = engine work for 040/041/042/043 enumeration, rc2 = engine work for 042/043 execution): items #1, #2, #3 require RFC-043's `choose` and RFC-042's plan machinery to be shipped first. Items #4–#7 are docs and cosmetic API changes that naturally batch at the end.

## Dependencies

- **Depends on:** RFC-040 (#109, Determinism Levels) — provides L4 contract that drives item #5 documentation.
- **Depends on:** RFC-042 (Exploration Plan) — provides `plan --suggest` for item #3 and the fan-out producers (`interleavings=`, `probability=`) for item #2.
- **Depends on:** RFC-043 (Non-deterministic Operators) — provides `choose` / `nondet()` for item #1 and item #2.
- **Withdraws:** RFC-013 (Parameterized Scenarios — Draft).
- **Withdraws:** RFC-002 (`domain()` — Draft).
- **Affects:** RFC-045 (#108, Protobuf decoder) — ships under the new `decoder("proto", ...)` form rather than `proto_decoder()`. Coordinate with RFC-045 implementer.
- **Unblocks:** RFC-009 (Independence Relation) and RFC-010 (DPOR) — both operate on the unified `NonDeterministicChoice` abstraction landing here.

---

## References

- RFC-040 (#109, Determinism Levels) — provides the L4 contract.
- RFC-041 (Temporal Properties — to be filed) — co-shipped in v0.13.0.
- RFC-042 (Exploration Plan — to be filed) — provides plan machinery and `--suggest`.
- RFC-043 (Non-deterministic Operators — to be filed) — provides `choose` / `nondet()` / `halt` / `assume`.
- RFC-013 (Parameterized Scenarios) — to be Withdrawn by this RFC.
- RFC-002 (`domain()`) — to be Withdrawn by this RFC.
- RFC-009 (Independence Relation) — future; benefits from unified `NonDeterministicChoice`.
- RFC-010 (DPOR) — future; benefits from unified `NonDeterministicChoice`.
- RFC-045 (#108, Protobuf decoder) — ships under unified `decoder()` form.
