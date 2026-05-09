# RFC-041: Temporal Properties & Monitors

> **Status: Draft.** Part of the v0.13.0 epic. Builds on RFC-040 (Determinism Levels) — temporal primitives' semantics depend on level. Pairs with RFC-042 (Exploration Plan) and RFC-043 (Non-deterministic Operators).

## Summary

Faultbox tests today assert on the trace *immediately* — what happened during the test body. But real distributed systems are *eventually consistent*: a test that completes when the SUT acknowledges a request often runs before the system has fully reconverged (replicas caught up, async work drained, retries settled). Asserting too early produces false negatives ("the test passed but the bug was still there"); waiting too long produces flaky tests ("worked locally, failed in CI"). Today users paper over this with `time.sleep()` calls, retry loops, or pre-test "wait for stable" probes — each of which is a flake source under any wall-clock-bound determinism level.

This RFC introduces a small set of temporal primitives that express *what* the assertion is waiting for, not *how long* to wait. **Wall-clock time appears in only one place** — the per-test `timeout=` (default 30s) — so `eventually` and `await_stable` describe properties without per-assertion deadlines:

1. **`eventually(predicate)`** — the predicate must hold at some point before test termination. No per-assertion duration.
2. **`always(predicate, between=...)`** — invariant; predicate must hold for every event between two anchors (anchors are events, not wall-clock times).
3. **`await_stable()`** — synchronous quiescence; block until the system has no in-flight work or the test timeout cuts in.
4. **`monitor(name, on=, ...)`** — long-running observer that fires on matching events throughout the test.

A test terminates when **any** of three conditions fires: (a) body returns, (b) the user-declared `terminate_when=` predicate becomes true, or (c) the wall-clock `timeout=` budget elapses. At termination, all temporal assertions do their final evaluation. This is the TLA+ pattern: liveness properties evaluated at terminating states.

The primitives are level-aware (RFC-040): their *meaning* sharpens as determinism increases. At L1, the test `timeout=` is wall-clock — flaky on slow CI. At L3 with virtual time, the timeout is virtual-clock — reproducible across replays. This level-awareness is the load-bearing reason RFC-040 had to land first.

In-tree document: `docs/rfcs/0041-temporal-properties.md`.

## Motivation

### What problem does this solve?

Two intertwined problems:

1. **Test-ends-before-system-converges.** A user writes `body = lambda: api.create_order(...)`; the call returns 201; the test ends; the assertion checks `inventory.stock_decremented == True`. But `inventory` is updated asynchronously via Kafka — the message hasn't been consumed yet. Assertion fails even though the SUT is correct. Users today work around this with hardcoded sleeps (flaky) or polling loops (also flaky, just slower).
2. **Wall-clock-bound assertions are flaky.** `time.sleep(2)` works on the user's laptop and fails on a loaded CI runner where the SUT takes 3 seconds to reconverge. Or vice versa — passes on slow CI, fails on fast laptop because the failure mode requires a specific timing. Either way, the test is testing "the system is fast enough" not "the system is correct."

The framing both problems share: **the user wants to express a property about *what* must be true, not a timing about *when* to check it.** The temporal primitives provide that vocabulary.

### Why is this important now?

- **RFC-040 just landed L1's contract.** Every primitive that bounds time has different semantics per determinism level. Without the level vocabulary, this RFC would have to invent its own framing inline; with it, the primitives become first-class level-aware constructs.
- **Customer flakes are temporal.** Per the 2026-04-22 customer-feedback Notion (Group B), the most common flake category is "test ends before system converges" — exactly what this RFC addresses.
- **RFC-042 needs `eventually` and `monitor` for assertions.** The Exploration Plan's `expect_*` family (RFC-027) currently has narrow temporal semantics; this RFC generalizes them.
- **L4 hermetic determinism (RFC-040) requires `await_stable()`.** A test that runs at L4 cannot finish until quiescence, otherwise the deny-default policy will fire on legitimate post-test work and produce false negatives.

### What happens if we don't do this?

- Users keep writing `time.sleep(2)` and accept the flake rate as a tax.
- "Test passed but bug still there" stays the dominant L1 failure mode.
- L4 hermetic mode (when it lands) cannot be used for any system that does *any* async work, which is most of them.
- LLM-first authoring (v0.14.0) generates specs full of `time.sleep(2)` calls because the primitives don't exist for the right answer.

## Current state

Today Faultbox provides:

- **Synchronous trace assertions.** `assert_eventually_in_trace(predicate)` exists informally — users can write Starlark loops over the event log. Not first-class; not level-aware; no `within=` bound.
- **`expect_success`, `expect_error_within`, `expect_hang`** (RFC-027 — Matrix Expectation Language). Narrow temporal semantics: `expect_error_within` accepts a duration and fails if no matching error appears in time. Useful but limited to error matching; doesn't compose with arbitrary predicates.
- **`fault_zero_traffic`** (RFC-027 OQ4, #75) — emits an event when a faulted operation never received traffic. Discrete event, not a temporal predicate.
- **No `await_stable()`, no `always()`, no `monitor()`.** Users improvise with sleeps and retry loops.
- **No virtual clock.** All durations are wall-clock; reproducibility is bounded by host scheduling, CI load, etc.

## Determinism level → temporal semantics

The RFC-040 levels determine what each primitive *means*:

| Primitive | L0 / L1 (default engine) | L2 (gVisor) | L3 (gVisor + clock funnel) | L4 (hermetic) | L5 (instruction-boundary) |
|-----------|--------------------------|-------------|---------------------------|----------------|---------------------------|
| `eventually(p)` | Test `timeout=` is wall-clock; final check at termination | Wall-clock `timeout=`; broader event coverage | **Virtual-clock `timeout=`; reproducible** | Same as L3; deny-default may fire if predicate never met | Same as L3 |
| `always(p, between=...)` | Predicate checked on every L1-mediated event in window | Predicate checked on every total event | Same; replay identical | Predicate checked between explicit start/end markers | Same |
| `await_stable()` | Best-effort; based on observed event-log quiescence; cut off by test `timeout=` | More accurate (broader event coverage) | **Deterministic; quiescence detected via virtual-clock convergence** | Required at test-end | Required |
| `monitor(n, on, ...)` | Triggered on matching L1-mediated events | On matching total events | Same; replay identical | Same | Same |
| `test(timeout=)` | Wall-clock; flaky on slow CI | Wall-clock | **Virtual-clock; reproducible** | Same as L3 | Same as L3 |

**The L1 → L3 jump is the practical sweet spot:** at L1 the test timeout is wall-clock-bound; at L3 it's virtual-clock-bound and reproducible. v0.13.0 ships L1-level semantics; L3 sharpening lands when Path C (gVisor fork) is implemented (post-v2.0 per RFC-040). With wall-clock concentrated in one place (`test(timeout=)`), the migration to virtual clock at L3 only needs to swap one substrate, not five.

## Proposed design

### 5.1 — `eventually(predicate)`

The most-used primitive. Asserts that `predicate` must hold at some point before test termination:

```python
test("order_propagates_to_inventory",
    setup = lambda: ...,
    body = lambda: api.create_order(sku="abc-123", qty=5),
    expect = eventually(
        lambda trace: trace.event("inventory.stock", sku="abc-123").qty == -5,
    ),
    timeout = "30s",       # wall-clock backstop; default if omitted
)
```

Semantics:
- Faultbox runs the test body and watches the event log throughout.
- The moment `predicate` returns true at any point during the test, the assertion is marked **satisfied**.
- At test termination (Section 5.5), Faultbox does a final check:
  - If satisfied → PASS for this assertion.
  - If not satisfied → final evaluation of the predicate against the full event log; if still false → FAIL. (The final check covers the case where the predicate became true exactly at termination but wasn't observed earlier.)
- **No per-assertion `within=`.** The only wall-clock bound is the test's `timeout=`; if a predicate hasn't held by termination, that's a deterministic FAIL regardless of how long was available.

For latency-bounded properties ("must respond within 200ms"), express the bound *inside* the predicate using `event.duration_since(other)`:

```python
expect = eventually(
    lambda t: t.event(type="api.response").duration_since(
        t.event(type="api.request")
    ) < "200ms"
)
```

This keeps wall-clock comparisons explicit and inside the predicate logic where they belong, instead of buried as a kwarg on the assertion.

`anchor=` kwarg lets the user specify when the predicate's window starts (still useful for narrowing the relevant event range):
- Default: beginning of test body
- `anchor=event_matcher` — start watching from the first event matching this matcher

<!--TODO: if between skiped last point should Termination event -->
### 5.2 — `always(predicate)`

Invariant assertion: `predicate` must hold for every event between two points.

```python
test("balance_never_negative",
    setup = lambda: account.deposit(100),
    body = lambda: wait_all(
        lambda: account.withdraw(50),
        lambda: account.withdraw(50),
        lambda: account.withdraw(30),    # one of these must fail
    ),
    expect = always(
        lambda trace: trace.event("account.balance").amount >= 0,
        between = ("body_start", "stable"),
    ),
)
```

Semantics:
- For every event matching the implicit filter (or `match=` kwarg), `predicate` must return true.
- Single failure fails the test.
- `between=("body_start", "stable")` means "from the start of the test body until system quiescence." Default is `("body_start", "body_end")`.

### 5.3 — Body-blocking primitives: `await_stable()` and `await_event()`

Two primitives that pause the test body until a condition holds. Distinct from `eventually` / `always` / `monitor` (which are passive observers running alongside the body); these are *imperative* — execution doesn't proceed past them until the condition is met.

#### 5.3.1 — `await_stable(quiescence_window=, ignore=)` — synchronous quiescence

Block until the system has no in-flight work:

```python
def body():
    api.start_workflow()
    await_stable()                          # block until async work drains
    api.check_status()                       # asserts on settled state

test("two_step_workflow",
    body = body,
    timeout = "30s",                         # backstop; cuts await_stable too
)
```

Semantics:
- Quiescence definition (v0.13.0): no **non-ignored** mediated events in the last `quiescence_window` (default 1s; tunable per call or spec-wide).
- Returns when quiescence holds for the full window.
- **No `timeout=` argument on `await_stable` itself.** If quiescence is never reached, the test's overall `timeout=` cuts in — the body never proceeds past `await_stable`, the test terminates INCONCLUSIVE.
- L1: wall-clock `quiescence_window`; the test `timeout=` cuts it. Flaky in tight cases.
- L3: virtual-clock window; deterministic.
- L4: required at test-end (auto-inserted unless the spec author calls it explicitly).

**The `ignore=` kwarg** — a predicate or matcher that marks events as "doesn't count as activity." Useful when the SUT has constant background chatter (heartbeats, metric flushes, polling loops) that would otherwise prevent quiescence from ever holding:

```python
# Ignore heartbeats — quiescence considers only non-heartbeat events.
await_stable(ignore=match.event(type="heartbeat"))

# Ignore multiple known-noise categories.
await_stable(ignore=match.any(
    match.event(type="heartbeat"),
    match.event(type="metric.flush"),
    match.event(type="telemetry.ping"),
))

# Predicate form for fine-grained ignore logic.
await_stable(ignore=lambda event:
    event.service == "telemetry" or event.type == "kafka.offset_commit"
)
```

When `ignore=` is set, an event is added to the log but does **not** reset the quiescence clock if `ignore(event)` returns true. This lets the user keep a tight 1s `quiescence_window` while filtering out predictable noise — a finer-grained alternative to widening the window past the noise period.

Default: `ignore=None` (no events ignored; every mediated event counts as activity).

**`ignore=` semantics interact with `quiescence_window=`:**
- A 500ms-heartbeat system + `quiescence_window="1s"` + no `ignore=` → quiescence never holds.
- Same system + `ignore=match.event(type="heartbeat")` → quiescence holds 1s after the last non-heartbeat event. The heartbeats are still emitted; they just don't break stability.
- Same system + `quiescence_window="3s"` + no `ignore=` → also works, but coarser; you wait 3s even when actual activity has stopped.

#### 5.3.2 — `await_event(predicate_or_matcher)` — block until a specific event

Block until the event log contains an event matching the predicate or matcher:

```python
def body():
    api.start_workflow(workflow_id="wf-42")

    # Matcher form — concise, for simple "type + fields" matches:
    await_event(match.event(type="workflow.phase_1_complete", id="wf-42"))

    api.kick_off_phase_2(workflow_id="wf-42")

    # Predicate form — full Starlark expressiveness:
    await_event(lambda t:
        t.event(type="kafka.message", topic="audit").payload.outcome == "approved"
    )

    api.finalize(workflow_id="wf-42")

test("workflow_two_phase",
    body = body,
    timeout = "30s",
)
```

Semantics:
- **Matcher form** — `await_event(match.event(type=..., **filters))` — equivalent to `await_event(lambda t: t.event(type=..., **filters) is not None)`. Most common case.
- **Predicate form** — `await_event(lambda trace: <bool>)` — full predicate access to the trace API (Section 5.7). Useful when the condition involves cross-event reasoning.
- Returns the matching event (matcher form) or `True` (predicate form) once the condition holds, allowing chaining: `event = await_event(match.event(type="X")); use(event.field)`.
- Evaluated **eagerly** — if the matching event already exists in the log when `await_event` is called, returns immediately. Otherwise blocks until a matching event is appended.
- **No own timeout.** Bounded by the test's `timeout=`; if the predicate never holds, the test terminates INCONCLUSIVE via (c).
- Same level-awareness as `await_stable`: wall-clock-bound at L1, virtual-clock-bound at L3.

**`await_event` vs `eventually`:**

| | `await_event(p)` | `eventually(p)` |
|--|------------------|-----------------|
| Body execution | **Blocks** the body | **Passive** — body proceeds independently |
| Used in | Test body (imperative flow) | `expect=` (declarative assertion) |
| If predicate never holds | INCONCLUSIVE via (c) | FAIL via (b) `terminate_when=` or INCONCLUSIVE via (c) |
| Returns | The matching event | N/A (assertion result) |

Most tests use both: `await_event` to drive the body's flow, `eventually` to declare end-state properties. Example:

```python
def body():
    api.create_order(...)
    order_event = await_event(match.event(type="order.created"))       # imperative — block for state
    api.charge(order_event.id)

test("order_propagates",
    body = body,
    expect = eventually(                                                # declarative — final assertion
        lambda t: t.event(type="inventory.decrement") is not None
    ),
)
```

(Multi-statement bodies use `def` rather than `lambda`; Starlark lambdas can only contain a single expression. The `await_event` return value is captured into a local variable, then used in subsequent calls.)

**`await_event` vs `terminate_when=`:**

Both watch the event log for a predicate. The difference:
- `await_event` **unblocks the body** when the predicate holds — test continues executing.
- `terminate_when=` **fires Termination** when the predicate holds — test ends.

A common pattern is to use both: `await_event` for intermediate sync points within the body, `terminate_when=` for the "system reached final state" signal that ends the test.

### 5.4 — `monitor(name, on=, state_init=, update=, check=)` — long-running observer

A monitor runs throughout the test. It maintains per-test memory that evolves with each matching event; if `check` returns false, the test fails with the violating event cited.

```python
monitor("balance_invariant",
    on = match.event(type="account.balance"),       # only trigger on these events
    state_init = {"last_balance": None},             # per-test memory; flushed at test end
    update = lambda event, state:                    # state transition (pure function)
        {"last_balance": event.amount},
    check = lambda event, state:                     # invariant predicate (pure function)
        state["last_balance"] is None or state["last_balance"] >= 0,
)

test("any_concurrent_workload",
    body = lambda: wait_all(...),
    # The monitor runs implicitly — no need to attach it to expect=.
)
```

**Per-event lifecycle:**
1. Event matching `on=` arrives. <!--TODO: add example with multiply event types-->
2. `new_state = update(event, state)`.
3. `verdict = check(event, new_state)`.
4. If verdict false → test fails, citing the event and the violated invariant.
5. `state ← new_state`, persist for next event.

**Memory lifecycle:**
- `state_init` is evaluated fresh at each test start.
- Memory is *per-test*; flushed at test completion. Monitor state does not leak between tests.
- Memory is *isolated* per monitor; two monitors don't share state.
- The simple invariant `state_init = {}` is fine for predicates that don't need history.

Monitors register at spec load and apply to every test in the spec (unless scoped). They're the equivalent of "always-on assertions" — useful for system-wide invariants that should hold across all tests. Same level-aware semantics as `always`.

**The `on=` kwarg is mandatory and exists for performance reasons.** Without filtering, a monitor's `check` runs on every mediated event in the event log — which can be tens of thousands of events per test for an active service. The `on=` matcher narrows this to events the monitor cares about (typically O(10–100) per test). Without `on=`, spec load fails with: *"monitor 'X' has no `on=` filter; specify the events that should trigger this monitor. To intentionally trigger on every event, use `on=match.all()`."*

<!--TODO: fully agreed, if eventlog doesnt contain needed information better way evolve plugins-->
**Monitors run in a restricted Starlark sandbox.** The `update` and `check` functions execute with read-only access to the event-log query API (`trace.event(...)`, `trace.events(...)`, vector-clock relations) and to the per-monitor memory. They **cannot** call any other Faultbox builtin — no `fault()`, no service-method calls, no `wait_all`, no plugin invocations. This is enforced at execution time: monitor predicates run in a Starlark `Thread` with a restricted globals map that excludes everything except the trace API. Reasons:

- A monitor that calls into the SUT (e.g. issuing a database query through a service plugin) makes the monitor non-deterministic and re-introduces wall-clock dependencies the determinism levels (RFC-040) explicitly removed.
- Monitor predicates must be pure functions over (event, state) for the verdict to be reproducible during replay (RFC-025).
- Plugin invocations from inside a monitor would race with the test body's invocations — undefined behavior.

The principle: **the event log is the single source of truth for what a monitor can know.** Section 8.5 covers the indexes that make this efficient.

### 5.5 — Test completion semantics

**The end of the test body is not a verdict — it's a trigger.** Today, Faultbox tests treat body-return as the moment to decide pass/fail. Temporal primitives change that: the body finishing is a transition into the *assertion phase*, where pending `eventually`, `always`, and `monitor`s settle.

Borrowing from TLA+'s terminating temporal properties: a test is *complete* only when Termination fires and every temporal assertion has been finally evaluated. The verdict depends both on assertion outcomes **and on which Termination condition fired**:

| Termination cause | All `eventually(p)` satisfied | Some `eventually(p)` never satisfied | Notes |
|------------------|--------------------------------|--------------------------------|-------|
| **(a) Natural completion** | PASS | (impossible — natural completion requires every `eventually` already satisfied) | The happy path |
| **(b) `terminate_when=` fires** | PASS | **FAIL** | System reached its declared final state without p ever holding — that's a real failure |
| **(c) `timeout=` deadline** | PASS | **INCONCLUSIVE** | Ran out of wall-clock; more time might have helped |
| **(d) Immediate failure** | n/a | **FAIL** | `always` violation, monitor violation, or body exception fired mid-test |

The key distinction: **(a) and (b) are *informed* terminations** — the system reached a state we either declared as "done" (b) or where every property already held (a). **(c) is an *uninformed* termination** — we cut the test off before a verdict could be reached. INCONCLUSIVE is the honest answer.

Without `terminate_when=`, an `eventually(p)` that's never satisfied has only one path: ride out the clock until (c). That's why declaring `terminate_when=` matters for tests where async convergence is the success criterion — it gives you (b) and turns 30s INCONCLUSIVEs into single-digit-second FAILs.

For `always(p, between=...)`:
- Any violation observed → FAIL (independent of termination cause).
- No violation, window closed → PASS.
- No violation, window's end-anchor never observed (e.g. `between=("body_start", "stable")` and `stable` never reached at deadline) → INCONCLUSIVE.

For monitors:
- Any violation seen during the test → FAIL.
- No violations → PASS, regardless of termination cause.

For `await_stable()` blocked at Termination:
- Body never completed → INCONCLUSIVE (we couldn't even start the assertion phase).

INCONCLUSIVE is a deliberate third state — not a hidden FAIL. It signals "we can't determine pass/fail from this run." CI integrations should:

- Show INCONCLUSIVE prominently in reports (different color from FAIL).
- Not block merges by default (treat as warning).
- Be track-able in dashboards over time — a test that's frequently INCONCLUSIVE is either misspecified (timeout too tight, `terminate_when=` predicate that never fires) or genuinely too slow.

**Test lifecycle:**

```
1. Spec load        — temporal assertions registered; spec-load checks run (Section 5.6).
2. Body runs        — may include `await_stable()` calls; emits mediated events; monitors evaluate inline.
                       eventually(p) predicates evaluated continuously throughout (not just post-body).
3. Body returns     — body-completion noted. Assertions continue evaluating against the live event log.
4. Termination      — fires when ANY of (a)-(d) above triggers. First to fire wins.
5. Verdict          — PASS / FAIL / INCONCLUSIVE per the table above.
```

Note that **body return is not Termination by itself** — it's just one *condition* that contributes to natural completion (a). If body returns at t=2s but an `eventually(p)` is still pending, the test continues until either p holds (→ a), `terminate_when=` fires (→ b), timeout elapses (→ c), or something fails immediately (→ d).

**Tests with no temporal assertions or monitors.** A test that uses only safety asserts (in-body `assert.equals()`, etc.) and no `eventually` / `always` / `monitor` has no pending temporal state at body return. Termination via (a) "natural completion" fires immediately — the empty set of temporal assertions is trivially "all satisfied." The lifecycle is identical to a classical test framework: body returns → verdict from safety asserts only. INCONCLUSIVE is structurally impossible in this case (no temporal assertion can be unsatisfied).

The general rule: **termination via (a) requires every temporal assertion to already be in a positive terminal state.** Tests with no temporal primitives have an empty set of assertions, so (a) fires at body-return for free. Tests *with* temporal primitives may need (b) `terminate_when=` to fire (a) early, or fall back to (c) timeout. Temporal assertions are opt-in cost.

**Termination conditions:**

A test reaches Termination when **any** of these fires first:

```python
test("eventual_propagation",
    body = lambda: api.start_workflow(workflow_id="wf-42"),
    timeout = "30s",                                          # (c) wall-clock backstop
    terminate_when = eventually(                               # (b) user-declared event-based termination
        lambda t: t.event(type="workflow.status", id="wf-42").status == "completed"
    ),
    expect = always(                                           # invariant during whole test
        lambda t: t.event(type="balance").amount >= 0,
    ),
)
```

- **(a) Natural completion.** Body has returned **and** every registered temporal assertion has reached a *positive* terminal state — every `eventually(p)` has been satisfied, every `always(p, between=...)` window has closed without violation. This is the happy path: the system did what was expected and the test concluded.
- **(b) `terminate_when=` predicate fires.** User-declared. Accepts an `eventually(...)` expression or any predicate over the trace. The moment it becomes true, Termination fires — even if some `eventually(p)` is still unsatisfied. This is the "system reached its declared final state" signal: if a liveness predicate hasn't held by then, it never will, so it's FAIL.
- **(c) `timeout=` wall-clock backstop.** Default 30s; configurable per-test via `test(timeout=...)` or spec-wide via `determinism(test_timeout="60s")`. If neither (a) nor (b) fires within the budget, Termination fires anyway — for any unsatisfied `eventually`, result is INCONCLUSIVE (we don't know if more time would have helped).
- **(d) Immediate failure.** Any `always(p, ...)` violation observed mid-test, any `monitor` violation, or body raising an exception → Termination fires immediately with FAIL. Fast feedback.

The first of these to fire wins.

**Default behavior — no `terminate_when=`:**

Most tests don't declare `terminate_when=`. In that case, Termination paths reduce to: (a) natural completion, (c) timeout, (d) immediate failure. Practical implications:

- **Synchronous test (no `eventually` / `always` / `monitor`).** Body returns → all temporal assertions are trivially "all satisfied" (because there are none). Termination via (a) is immediate. Result: PASS or FAIL based on body-side asserts only. INCONCLUSIVE structurally impossible.
- **Test with `eventually(p)` and async system work.** Body returns at t=2s; `eventually(p)` may still be pending. Faultbox keeps watching the event log:
  - If p becomes true at t=10s → Termination via (a) → PASS.
  - If p never becomes true → Termination via (c) at t=30s → INCONCLUSIVE (we couldn't confirm; without `terminate_when=`, we never know if more time would have helped).
- **Test with `always(p)` invariant.** Body returns at t=2s; `always(p)` window closed; no violation seen → contributes to (a) natural completion. Violation seen at any point → (d) immediate FAIL.

**Why declare `terminate_when=`?** Two reasons:

1. **Fast feedback for failing tests.** Without it, an `eventually(p)` that will never hold runs out the full `timeout=` budget — 30s of wasted CI per failure. With `terminate_when=eventually(t.event("workflow.complete"))`, the test terminates the moment the system declares "done" and immediately FAILs the unsatisfied liveness predicate. Single-digit-second feedback instead of 30s.
2. **Distinguishing "ran out of time" from "system finished but invariant didn't hold."** Without `terminate_when=`, both look like Termination via (c) — INCONCLUSIVE. With it, (b) makes the failure mode crisp.

For tests where async convergence is expected and you can name the "system done" event, always declare `terminate_when=`. For trivial synchronous tests, omit it — (a) handles the common case.

**Configuring Termination per test or spec-wide:**

```python
# Per-test:
test("foo",
    timeout = "60s",
    terminate_when = eventually(lambda t: t.count(type="error") > 0),
)

# Spec-wide defaults (extend RFC-040's determinism builtin):
determinism(level="L1",
    test_timeout = "45s",                                      # default for every test
)
```

Without `terminate_when=`, Termination is body-return + timeout backstop only — the classical lifecycle.

**At Termination — final evaluation:**

When Termination fires, Faultbox evaluates every temporal assertion one last time. The result of each per-assertion evaluation depends on which Termination cause fired (per the table above):
- `eventually(p)` — was p ever true during the test? Yes → PASS. No, and Termination via (b) `terminate_when=` → FAIL. No, and Termination via (c) `timeout=` → INCONCLUSIVE. No, and (a) natural completion is impossible because (a) requires p already satisfied.
- `always(p, between=...)` — within the bounded window, did p hold for every event? Yes → PASS, otherwise FAIL (independent of Termination cause).
- `monitor(...)` — final state collected. Any violation seen during the test → FAIL.
- `await_stable()` or `await_event()` calls in the body — if any are still blocked at Termination, the body did not complete; result is INCONCLUSIVE.

### 5.6 — Spec-load coherence checks

Cheap static checks at spec load catch misconfigured temporal expectations before they waste CI time:

1. **Monitor `on=` filter present.** Every `monitor(...)` must have an explicit `on=` (Section 5.4); spec load fails otherwise.
2. **`terminate_when=` references valid event types.** If `terminate_when=eventually(lambda t: t.event(type="X")...)` references an event type no declared service or interface emits, warn at spec load. Doesn't fail (event types can be dynamic) but flags likely typos.
3. **Sandbox restrictions on monitor `update`/`check`.** Static analysis of the lambda body verifies no disallowed builtins are referenced (Section 5.4).

(Section 5.1 removed the per-assertion `within=` argument, so the previous "every `eventually(within=X)` ≤ test.timeout" check is no longer needed — there is no `within=` to check.)

### 5.7 — Predicate language

**The `match` module.** Throughout this RFC, monitor `on=` filters and `await_event` matchers reference `match.event(...)`, `match.any(...)`, `match.all()`. `match` is a Starlark struct exposed in spec globals, analogous to the `observe.*` namespace from RFC-044. Surface:

- `match.event(type=..., **filters)` — matches a single event by type and field filters.
- `match.any(*matchers)` — composite OR — matches if any sub-matcher matches.
- `match.all(*matchers)` — composite AND — matches if all sub-matchers match.
- `match.never()` — never matches (useful for testing).
- `match.always()` (or just `match.all()` with no args) — always matches.

Implementation in `internal/star/match.go`. Matchers are first-class Starlark values; users can compose them, store them in variables, pass them around. The matcher form is the recommended way to express "what events I care about" because it's lighter-weight than predicate lambdas and uses the indexed event log directly (Section 8.5).

Predicates accept a `trace` object and `event` objects that expose a complete set of query operators. Coverage is split into four categories:

**Lookup operators (find events):**

| Operator | Returns | Notes |
|----------|---------|-------|
| `trace.event(type=..., **filters)` | First matching event (or None) | Latest by emission order |
| `trace.events(type=..., **filters)` | All matching events | Ordered by emission |
| `trace.first(matcher)` | Earliest matching event | Convenience for `events(...)[0]` |
| `trace.last(matcher)` | Most recent matching event | Same as `event(...)` |
| `trace.count(matcher)` | Integer count of matches | Aggregation |

**Pairwise causal relations (between two events):**

| Operator | Meaning | Implementation |
|----------|---------|----------------|
| `event_a.happens_before(event_b)` | A → B in vector-clock partial order | Vector-clock comparison |
| `event_a.happens_after(event_b)` | B → A | Inverse of above |
| `event_a.concurrent_with(event_b)` | Neither A → B nor B → A | Vector clocks not comparable |
| `event_a.same_service_as(event_b)` | Both from same service | Service-attribution check |
| `event_a.same_correlation_as(event_b)` | Same correlation/trace ID | Index lookup |

**Existential causal relations (one event vs a matcher):**

| Operator | Meaning |
|----------|---------|
| `event.preceded_by(matcher)` | Some earlier event in the causal past matches |
| `event.followed_by(matcher)` | Some later event in the causal future matches (only meaningful at end of evaluation window) |
| `event.preceded_by_within(matcher, window)` | Earlier event matching, within bounded duration |
| `event.followed_by_within(matcher, window)` | Later event matching, within bounded duration |
| `event.directly_caused_by(matcher)` | Direct (non-transitive) causal cause — immediate predecessor in vector-clock graph |

**Temporal / window operators:**

| Operator | Meaning | Level-awareness |
|----------|---------|-----------------|
| `event.timestamp` | Absolute timestamp (wall-clock at L1, virtual at L3) | Level-aware |
| `event_a.duration_since(event_b)` | Elapsed time A − B | Level-aware |
| `trace.events_between(start, end)` | Events between two anchors | Anchors can be events or time offsets |
| `trace.events_within(matcher, window, of=event)` | Events matching, within a window relative to anchor | Convenience for relative queries |
| `trace.causal_chain(event)` | All events causally preceding given event | Returns ordered list |

**Aggregations:**

```python
trace.count(type="error") == 0
trace.events(type="http.req").map(lambda e: e.duration_ms).sum() < 1000
trace.events(type="postgres.query").reduce(
    lambda acc, e: acc + (1 if e.failed else 0),
    initial=0,
) <= 3
```

The predicate is a Starlark lambda; Faultbox passes the trace (or event + memory for monitors) and evaluates. **Predicates are pure functions of their inputs** — given the same trace state and memory, they return the same verdict. This is what makes replay (RFC-025) reproducible at the predicate level.

**Composition for LTL-style queries:**

```python
# "If charge succeeds, inventory must eventually decrement."
eventually(
    lambda t: t.event(type="charge.success").followed_by_within(
        match.event(type="inventory.decrement"),
        window="5s",
    )
)

# "Balance never goes negative between credit and audit."
always(
    lambda t: t.event(type="balance.update").amount >= 0,
    between = ("credit_event", "audit_event"),
)

# "Every kafka publish is preceded by a successful authentication."
always(
    lambda t: t.event(type="kafka.produce").preceded_by(
        match.event(type="auth.success", actor=t.event(type="kafka.produce").actor),
    )
)
```

Verbose but expressive. RFC follow-up could add LTL syntax sugar (`G`, `F`, `U` operators) if customer demand emerges; v0.13.0 ships the building blocks only.

### 5.8 — What the event log can know

The "predicates query the event log, not external systems" principle (Section 5.4) is only useful if the event log actually carries the properties users need to assert about. This subsection sets honest expectations per event source.

| Source | Captures | Property check feasible? |
|--------|----------|-------------------------|
| `topic()` (Kafka) | Message payloads, partition, offset, key | ✅ Yes — payload fields directly queryable |
| `wal_stream()` (Postgres logical replication) | Row-level INSERT/UPDATE/DELETE with old + new column values | ✅ Yes — derived database state captured directly. Strongest substrate. |
| MySQL proxy | SQL statements (writes + reads), query results | ⚠️ Partial. Operations visible; resulting values only when SUT reads back |
| Redis proxy | RESP commands and responses | ⚠️ Same shape as MySQL — values from responses only |
| `stdout()` / `stderr()` | Decoded log lines | ⚠️ Whatever the SUT chose to log |

**Three patterns when the log doesn't naturally carry a property:**

1. **CDC / WAL streaming** — strongest. Add `wal_stream()` for Postgres or equivalent CDC source. The database emits its state changes; we capture them as events.
2. **Probe reads in the test body** — force state into the log via explicit reads (`api.get_balance(...)`). The SUT issues SELECT; result captured by the proxy.
3. **State projection via monitor memory** — fold events into derived state using `monitor(state_init=, update=, check=)`. Less efficient than CDC but works for accumulator-style invariants.

**What the event log structurally cannot know at L1:**

Write-only state with no CDC and no probe read is opaque. The SUT writes to local disk via `os.WriteFile` and never reads back; we see the syscall (write happened) but not the file contents. Mitigations require moving to L2/L3 (gVisor mediates content) or adding probe reads.

This is a **design constraint of L1**, not a bug. The L1 contract (RFC-040) only promises mediated-event determinism; properties about non-mediated state need to be made observable.

## v0.13.0 implementation scope

The committed v0.13.0 work for this RFC:

### 8.1 — `eventually(predicate, anchor=)` builtin

In `internal/star/builtins.go`. Returns an Expectation object. The test runner subscribes the predicate to the event log; on every event, evaluates predicate; on first true, marks satisfied. At Termination, does final evaluation. No per-assertion timeout.

### 8.2 — `always(predicate, between=)` builtin

Returns an Expectation. Iterates over the event log within the bounded window (anchored by event matchers, not durations); predicate must hold for every event.

### 8.3 — Body-blocking primitives: `await_stable()` and `await_event()`

**`await_stable(quiescence_window=, ignore=)`** — blocks the test body's execution thread until quiescence holds for the specified window. The `ignore=` predicate/matcher marks events that should not reset the quiescence clock (heartbeats, telemetry, polling). **No own timeout** — bounded by the test's `timeout=`. Implemented via observed event-log silence at L1 (each non-ignored event resets the quiescence timer); replaced with virtual-clock convergence at L3. The ignore check uses event-log indexes (Section 8.5) for O(1) matcher dispatch on common cases like `match.event(type="heartbeat")`.

**`await_event(predicate_or_matcher)`** — blocks the test body until the event log contains a matching event:
- Matcher form (`match.event(...)`) checked against the event-log indexes (Section 8.5) — O(log N) per check using the type/service indexes.
- Predicate form re-evaluated on each new event arrival; cost is O(predicate × new events).
- Eager check on entry: if the matching event already exists in the log, returns immediately without blocking.
- Returns the matching event (matcher form) or `True` (predicate form) for chaining.

Both implemented in `internal/star/builtins.go`; both block the test body's Starlark thread via a channel waiting on the event-log emitter.

If Termination fires while either primitive is blocked, the body never completes; verdict is INCONCLUSIVE.

### 8.4 — `monitor(name, on=, state_init=, update=, check=)` builtin

Top-level builtin. Registers a monitor at spec load with mandatory `on=` matcher and optional `state_init` / `update` / `check`. The engine:

1. At test start: evaluates `state_init` to produce per-test memory.
2. Subscribes the monitor to the indexed event stream (Section 8.5).
3. On each event matching `on=`: runs `update(event, state)` to get new state, then `check(event, new_state)` for the verdict.
4. On verdict false: fails the test with a clear error citing the violating event and the state at the time of violation.
5. At test end: discards memory (no leakage to next test).

**Restricted Starlark sandbox for `update` / `check`:** these run in a `starlark.Thread` with a globals map containing only the trace API (`trace.event`, `trace.events`, vector-clock relations, etc.) plus the `event` and `state` arguments. No other Faultbox builtins are available — no `fault()`, no service references, no `wait_all`, no plugin invocations. Implementation: separate Starlark globals dictionary in `internal/star/monitor_sandbox.go` distinct from the test-body globals.

Spec-load validation:
- `on=` required (per Section 5.6).
- `update` and `check` validated as compatible with the sandbox: any reference to a non-allowed builtin errors at spec load with a clear message naming the disallowed call.
- `state_init` evaluated once at spec load to verify it returns a valid Starlark value.

### 8.5 — Predicate `trace` API + causal operators + event log indexes

In `internal/star/trace.go`. Implements the four operator categories from Section 5.7:

**Lookup operators:** `trace.event(matcher)`, `trace.events(matcher)`, `trace.first(matcher)`, `trace.last(matcher)`, `trace.count(matcher)`.

**Pairwise causal relations** (methods on event objects): `event.happens_before(other)`, `event.happens_after(other)`, `event.concurrent_with(other)`, `event.same_service_as(other)`, `event.same_correlation_as(other)`. All implemented via vector-clock comparison and indexed metadata.

**Existential causal relations:** `event.preceded_by(matcher)`, `event.followed_by(matcher)`, `event.preceded_by_within(matcher, window)`, `event.followed_by_within(matcher, window)`, `event.directly_caused_by(matcher)`. Implemented via causal-chain traversal of the indexed event graph.

**Temporal / window operators:** `event.timestamp`, `event.duration_since(other)`, `trace.events_between(start, end)`, `trace.events_within(matcher, window, of=event)`, `trace.causal_chain(event)`.

**Aggregations:** the lists returned by `trace.events(...)` are Faultbox-wrapped sequences exposing `.map(...)`, `.reduce(...)`, `.sum()`, `.filter(...)` as custom methods. Standard Starlark lists do not include these; the trace API returns a wrapper type that adds them. Implementation in `internal/star/trace.go`.

**Event log indexes:** the event log gains secondary indexes for the queries the operators above rely on:

- By event type (string)
- By service name
- By time range (sorted by emission timestamp)
- By correlation/trace ID
- By causal-graph predecessors (per-event list of direct causes for `directly_caused_by` and `causal_chain`)

Indexes are built incrementally as events are emitted (small constant cost per event). `trace.event(type="X")` becomes O(log N) on the type index; `event.preceded_by(matcher)` walks the causal graph in O(causal-chain length) instead of O(N). This is what makes the "event log as single source of truth" principle (Section 5.4 / 5.8) viable for monitors firing on dense streams.

Implementation in `internal/engine/eventlog.go` — extend the existing append-only log with side indexes. No public API change beyond the trace operators; only the query layer benefits.

### 8.6 — Test lifecycle: Termination + PASS / FAIL / INCONCLUSIVE

In `internal/engine/session.go`. The test runner implements the three-condition Termination model and three-valued verdict:

**Termination conditions** (first to fire wins):
- (a) Body returns.
- (b) `terminate_when=` predicate becomes true. Implemented as a continuously-watched expectation registered alongside other temporal assertions.
- (c) Test `timeout=` elapsed.

**At Termination, final evaluation:**
- For each `eventually(p)` — verdict positive if marked satisfied during the test or final evaluation returns true; otherwise FAIL.
- For each `always(p, between=...)` — finalize the window check.
- For each `monitor` — collect violation state.
- For any blocked `await_stable()` in the body — body did not complete; INCONCLUSIVE.

**Verdict:**
- All terminal positive → PASS.
- Any terminal negative → FAIL.
- Termination via (c) with assertions still pending OR `await_stable` blocked → INCONCLUSIVE.

Bundle (`.fb`, RFC-025) records the verdict in `manifest.json` (`outcome` field gains `inconclusive` enum value). Report (RFC-029) renders INCONCLUSIVE with distinct color (yellow vs FAIL's red). CLI exit code: PASS = 0, FAIL = 1, INCONCLUSIVE = 2 (CI integrations can choose to gate on either or both).

**Configuration:**
- `test("foo", timeout="60s", terminate_when=eventually(...), ...)` — per-test.
- `determinism(test_timeout="60s", ...)` — spec-wide default for `timeout` (extension to RFC-040's builtin; coordinate with RFC-040 implementation).
- `terminate_when=` is per-test only (no spec-wide form — termination conditions are usually test-specific).

### 8.7 — Spec-load coherence checks

In `internal/star/builtins.go` validation:
- Every `monitor(...)` checked for `on=` presence. Missing → spec-load error with example fix.
- `terminate_when=` lambda statically checked: must return a boolean (or be an `eventually(...)` expression).
- (No `within=` ≤ `timeout=` check is needed — `within=` was removed in the simplification of Section 5.1; the only wall-clock concept is `test(timeout=)`.)

### 8.8 — Reserved syntax: virtual-clock kwargs

For v0.13.0:
- `test(timeout="30s", clock="wall")` — `clock="virtual"` reserved on `test()` and `determinism()`; errors with "virtual-clock semantics require gVisor (Path C); not available in this release."
- `await_stable(clock="wall")` — same reservation pattern.

(With the wall-clock surface concentrated in `test(timeout=)`, virtual-clock migration only needs to swap one substrate.)

Locks the syntax now so v0.14.0+/v2.0 migrations don't break specs written today.

### 8.9 — Tests

Per the #84 coverage gate. Goldens for representative scenarios:
- `eventually` succeeds, fails on timeout, fails on never-true predicate
- `always` invariant holds, breaks on violation
- `await_stable` returns on quiescence, errors on timeout
- `monitor` fires on violation, ignores non-matching events

### 8.10 — Documentation

- New file: `docs/temporal.md` — the four primitives, the predicate language, level-awareness, examples.
- Update `docs/spec-language.md` — temporal primitives section.
- Update `docs/feature-manifest.md` — temporal primitives row.
- Tutorial chapter — walk a concrete eventual-consistency test through the four primitives.

## Out of scope for v0.13.0

- Virtual-clock implementation. Reserved syntax only; full virtual-clock requires gVisor Path C (post-v2.0 per RFC-040).
- LTL-style operator sugar (`G`, `F`, `U`). Composable via existing primitives; sugar deferred until customer demand.
- Cross-test invariants (monitors that span multiple tests). v0.13.0 monitors are spec-scoped, not cross-spec.
- Statistical assertions ("predicate holds in 95% of cases"). Out of scope; `eventually` is hard-success only.

## Open questions

1. **Default per-test `timeout=`.** Strawman: 30 seconds. Configurable per-test via `test(timeout=...)` and spec-wide via `determinism(test_timeout=...)`. Worth a sanity check — too low and users hit INCONCLUSIVE often; too high and CI burns time on stuck tests.
2. **`await_stable` quiescence window default.** Strawman: 1 second. Configurable per call; configurable spec-wide via `determinism(quiescence_window=...)` (extension to RFC-040's builtin).
3. **Monitor scope.** Strawman: spec-wide by default. Reserved kwarg `scope="test"|"spec"` for future flexibility.
4. **CI semantics for INCONCLUSIVE.** Should INCONCLUSIVE block merges by default, warn, or be ignored? Strawman: warn (CI exit code 2; not failure). Different CI integrations can choose to gate on it. Track INCONCLUSIVE rate per test in dashboards; spec authors with high rates should investigate.
5. **Should `monitor` accept multiple matchers?** Strawman: `on=` is a single matcher; for "this monitor cares about A or B," users compose: `on=match.any(match.event(type="A"), match.event(type="B"))`. Keeps the API surface small.

**Resolved:**
- ~~Per-assertion `within=` semantics.~~ Resolved 2026-05-04: removed entirely. The only wall-clock concept is `test(timeout=)`; `eventually(p)` is satisfied if p was ever true during the test, evaluated finally at Termination. Latency-bounded properties go inside the predicate via `event.duration_since(other) < "200ms"`. Removes spec-load complexity (no more `within ≤ timeout` checks) and concentrates wall-clock in one place — making the L1 → L3 virtual-clock migration touch one substrate, not five.
- ~~Custom termination predicate.~~ Resolved 2026-05-04: `terminate_when=` kwarg on `test()` accepts an `eventually(...)` expression. Three termination conditions documented in Section 5.5 (body return, `terminate_when=`, `timeout=` backstop); first to fire wins.
- ~~Predicate evaluation cost.~~ Resolved 2026-05-04: `on=` matcher mandatory; predicates run in restricted Starlark sandbox (no plugin/service calls); event log gains secondary indexes (Section 8.5). Three-way mitigation makes monitor cost O(matching events × predicate cost) with cheap predicates.
- ~~Monitor state across events.~~ Resolved 2026-05-04: monitors get per-test memory via `state_init` / `update` / `check` callbacks; memory is flushed at test completion; isolated per monitor.
- ~~Test termination for tests without temporal assertions.~~ Resolved 2026-05-04: termination is "all safety asserts done." Tests without `eventually` / `always` / `monitor` have a trivially empty assertion phase; INCONCLUSIVE is structurally impossible. Documented in Section 5.5.
- ~~Causal operator inventory.~~ Resolved 2026-05-04: four categories of operators (lookup, pairwise causal, existential causal, temporal/window) documented in Section 5.7, all implemented in Section 8.5.

## Implementation plan

| Phase | Scope | Target |
|-------|-------|--------|
| 8.1 `eventually` | Builtin + post-body poll loop | v0.13.0-rc1 |
| 8.2 `always` | Builtin + window iteration | v0.13.0-rc1 |
| 8.3 Body-blocking primitives | `await_stable()` (quiescence) and `await_event()` (specific event); both bounded by test `timeout=` | v0.13.0-rc1 |
| 8.4 `monitor` | Builtin + `on=` matcher + event-log subscription | v0.13.0-rc1 |
| 8.5 Trace API + event log indexes | `trace.event`, `trace.events`, queries; secondary indexes for type/service/time/correlation | v0.13.0-rc1 |
| 8.6 Test lifecycle | PASS / FAIL / INCONCLUSIVE three-valued result; assertion phase; per-test `timeout=`; bundle/report integration | v0.13.0-rc2 |
| 8.7 Spec-load coherence | `monitor` requires `on=`; `terminate_when=` static checks; sandbox restrictions on monitor lambdas | v0.13.0-rc1 |
| 8.8 Reserved kwargs | Virtual-clock variants reserved | v0.13.0-rc1 |
| 8.9 Tests | Goldens + unit tests + #84 coverage | v0.13.0-rc2 |
| 8.10 Docs | `docs/temporal.md`, spec-language, manifest, tutorial | v0.13.0-rc2 |
| Virtual-clock implementation (out of this RFC) | Real virtual-clock for `test(timeout=)` and `await_stable` quiescence window | gVisor Path C / v2.0 |
| LTL sugar (out of this RFC) | `G`, `F`, `U` operators | Future, demand-driven |

## Dependencies

- **Depends on:** RFC-040 (#109, Determinism Levels) — temporal primitives are level-aware; the level vocabulary must exist first.
- **Builds on:** RFC-024 (Proxy Datapath) — provides the mediated event log that predicates query.
- **Builds on:** RFC-025 (`.fb` Bundle) — event log is captured in bundles, so post-hoc predicate evaluation works on replay.
- **Used by:** RFC-042 (Exploration Plan) — `expect = eventually(...)` is the natural assertion form for fan-out plan-tree leaves.
- **Used by:** RFC-043 (Non-deterministic Operators) — `assume(predicate)` is conceptually adjacent to `always` but for plan-tree pruning rather than test-time invariants.
- **Composes with:** RFC-027 (#67, Matrix Expectation Language) — `expect_success` / `expect_error_within` / `expect_hang` are specialized forms of `eventually`. RFC-044 simplification could collapse them under unified `eventually` syntax; flagged for review.

---

## References

- RFC-040 (#109, Determinism Levels) — provides level vocabulary.
- RFC-027 (#67, Matrix Expectation Language) — specialized expectations that compose with this RFC's primitives.
- RFC-042 (Exploration Plan — to be filed) — uses `eventually` for plan-tree leaf assertions.
- RFC-043 (Non-deterministic Operators — to be filed) — `assume(p)` is adjacent.
- RFC-044 (Spec Language Simplification — to be filed) — possible collapse of RFC-027 expectations into unified `eventually` syntax.
- Customer feedback: 2026-04-22 customer-feedback-analysis (Notion), Group B on flake patterns.
- LTL (Linear Temporal Logic) — academic background for the primitive set; not directly adopted but informs the design.
- TLA+ (Lamport) — terminating temporal properties pattern. The PASS / FAIL / INCONCLUSIVE three-valued result and "test ends when all temporal assertions terminate, not when the body returns" framing are direct lifts from the TLA+ model-checking convention.
