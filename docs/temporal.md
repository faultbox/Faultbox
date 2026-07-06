# Temporal properties and monitors

This document covers Faultbox's temporal primitives — the vocabulary for
asserting on *what must be true* about a distributed system rather than
*how long to wait* before checking. Implemented per
[RFC-041](rfcs/0041-temporal-properties.md); foundation provided by
[RFC-040 (Determinism Levels)](rfcs/0040-determinism-levels.md).

## The five primitives

Faultbox ships five composable temporal primitives. They split cleanly
along two axes:

|                  | Passive (run alongside body) | Imperative (block body) |
|------------------|------------------------------|-------------------------|
| Local to a test  | `eventually(p)`, `always(p)` | `await_event(p)`, `await_stable(...)` |
| Spec-wide        | `monitor(name, on=, ...)`    | —                                       |

`test(name, body=, ...)` declares the lifecycle wrapper that turns body
return, `terminate_when=`, or `timeout=` into the three-valued verdict
table in [§Test completion](#test-completion).

## `eventually(predicate, anchor=)`

Asserts that `predicate` holds at some point before the test
terminates. No per-assertion deadline — the test's wall-clock
`timeout=` is the only bound.

```python
test("order_propagates_to_inventory",
    body = lambda: api.create_order(sku="abc-123", qty=5),
    expect = eventually(
        lambda t: t.event(type="inventory.stock", sku="abc-123").qty == -5,
    ),
    timeout = "30s",
)
```

Semantics:

- The predicate is evaluated after every event.
- The moment it returns true at any point during the test, the
  expectation is marked **satisfied**; subsequent evaluations are
  skipped.
- At Termination, Faultbox does a final check (an event might have
  arrived in the gap between the last evaluation and Termination).
- An `anchor=` matcher narrows the relevant event range: evaluation
  doesn't start until an event matching the anchor fires. Useful for
  "after request X, expect response Y".

Latency-bounded properties (must-respond-within) live *inside* the
predicate, keeping wall-clock comparisons explicit:

```python
expect = eventually(
    lambda t: t.event(type="api.response").duration_since(
        t.event(type="api.request")
    ) < duration("200ms")
)
```

`event.duration_since(other)` returns integer nanoseconds; `duration(s)`
parses a duration literal into nanoseconds so the two compose with `<`,
`<=`, etc. Comparing against bare strings would compare lexicographically
and silently misorder magnitudes ("1.5s" < "200ms" lexically).

## `always(predicate, between=)`

An invariant: the predicate must hold for every evaluation between two
anchor points.

```python
test("balance_never_negative",
    body = withdrawals_body,
    expect = always(
        lambda t: t.event(type="account.balance").amount >= 0,
        between = ("body_start", "stable"),
    ),
)
```

- A single false evaluation fails the test immediately (no waiting for
  Termination).
- `between=` accepts a tuple of two anchors. Each anchor can be a
  string lifecycle marker (`"body_start"`, `"body_end"`, `"stable"`)
  or a `MatcherVal` from `match.event(...)`. Matcher anchors are
  enforced today; string lifecycle anchors are wired through the
  Termination logic.
- **Vacuity warning (RFC-049).** If the `between=` *start* anchor
  never fires, the window never opens and the predicate is never
  evaluated. The verdict stays PASS — the window may be legitimately
  untriggered (e.g. `between=(error, recovery)` in a run with no
  error) — but the runtime emits a `vacuous_property` warning event
  into the trace so a typo'd or misnamed anchor surfaces instead of
  hiding as a silent green.

## `await_event(predicate_or_matcher)`

Blocks the test body until the event log contains a matching event.
Eager-checks the log on entry, so a match that's already there returns
immediately. Returns the matching event, enabling chaining.

```python
def body():
    api.start_workflow(workflow_id="wf-42")
    event = await_event(match.event(type="workflow.phase_1_complete", id="wf-42"))
    api.kick_off_phase_2(workflow_id=event.id)
```

- No per-call timeout — bounded by the test's `timeout=`.
- Accepts either a `MatcherVal` (preferred, uses the indexed event log
  for O(log N) checks) or a callable predicate over `trace`.
- Reserved kwarg `clock="virtual"` errors with a "requires gVisor"
  message so the syntax is reserved without being usable.

## `await_stable(quiescence_window=, ignore=)`

Blocks until the system has no non-ignored events for the full
quiescence window. Useful before asserting on settled state.

```python
def body():
    api.start_workflow()
    await_stable(quiescence_window="2s")  # block until async work drains
    api.check_status()
```

- Default `quiescence_window = "1s"`.
- `ignore=` is a matcher (or predicate) for events that should not
  reset the quiescence timer — typically heartbeats / metric flushes
  / telemetry pings:

```python
await_stable(
    quiescence_window="500ms",
    ignore=match.any(
        match.event(type="heartbeat"),
        match.event(type="metric.flush"),
    ),
)
```

- No own timeout — bounded by `test(timeout=)`. The body never
  proceeds past `await_stable` if quiescence is never reached; the
  test terminates INCONCLUSIVE via the timeout cause.

## `monitor(name, on=, state_init=, update=, check=)`

A spec-wide observer that fires on matching events throughout the test.
Maintains per-test memory via a small state machine:

```python
monitor("balance_invariant",
    on = match.event(type="account.balance"),
    state_init = {"last_balance": None},
    update = lambda event, state: {"last_balance": event.amount},
    check = lambda event, state:
        state["last_balance"] == None or state["last_balance"] >= 0,
)
```

For each event matching `on=`:

1. `new_state = update(event, state)` (Update is optional; default identity)
2. `verdict = check(event, new_state)` (Check is optional; default true)
3. If `verdict` is false → test FAILs, citing the event.
4. `state ← new_state` for the next iteration.

Scoping:

- A `monitor(...)` at spec top level auto-registers spec-wide — fires
  for every test in the spec.
- A `monitor(...)` passed via `fault_assumption(monitors=)` or
  `fault_scenario(monitors=)` is scenario-scoped instead. Faultbox
  reclaims it from the spec-wide list so it doesn't double-register.

Per-test memory:

- `state_init` is evaluated fresh at each test start.
- Memory is per-test and per-registration; two registrations of the
  same `MonitorDef` get independent state cells.

### Sandbox restrictions

`update` and `check` lambdas run in a restricted Starlark thread. They
cannot call any Faultbox builtin that would mutate runtime state or
recurse into the temporal machinery:

- Fault injection: `fault`, `fault_all`, `fault_start`, `fault_stop`
- Declarations: `service`, `interface`, `mock_service`, `determinism`,
  `scenario`, `fault_assumption/scenario/matrix`
- Assertions: `assert_*`
- Test runner integration: `parallel`, `partition`, `nondet`
- Temporal primitives themselves: `eventually`, `always`, `monitor`,
  `await_stable`, `await_event`
- Body-level trace plumbing: `trace`, `trace_start`, `trace_stop`,
  `events`. Monitors receive the matching `event` and a per-monitor
  `state` cell directly; they don't re-query the global trace.

This restriction is enforced at spec load. Calls to forbidden builtins
inside monitor lambdas fail with a clear error citing the line number
and the reason the builtin is denied.

## `test(name, body=, ...)`

Declarative test wrapper that binds a body to its temporal
configuration:

```python
test("order_two_phase",
    body            = body_callable,
    setup           = setup_callable,         # optional
    expect          = eventually(...),        # optional
    timeout         = "30s",                  # default 30s
    terminate_when  = eventually(...),        # optional
)
```

The test is registered as `test_<name>` so the test discovery layer
picks it up; `--test order_two_phase` and `--test test_order_two_phase`
both work from the CLI.

`def test_*()` functions remain supported. They run under the same
lifecycle with no per-test timeout (only the global infrastructure
timeout applies) and no `terminate_when=` watcher.

## Test completion

The body returning is not a verdict — it's a *trigger*. Faultbox
borrows the TLA+ pattern of terminating temporal properties: a test is
*complete* only when Termination fires and every temporal assertion has
been finally evaluated.

Termination fires on the first of:

| Cause | Description |
|-------|-------------|
| **(a) Natural completion** | Body returned **and** every registered `eventually`/`always` is in a positive terminal state. |
| **(b) `terminate_when=` fires** | User-declared predicate becomes true. |
| **(c) `timeout=` deadline** | Wall-clock budget elapsed. |
| **(d) Immediate failure** | Body raised, `always` violated mid-test, or a `monitor` fired. |

Final verdict per cause:

| Cause | All `eventually` satisfied | Some `eventually` unsatisfied |
|-------|----------------------------|-------------------------------|
| (a) Natural | PASS | **FAIL** (contradiction — natural completion requires every `eventually` already satisfied) |
| (b) terminate_when | PASS | **FAIL** (system reached its declared final state without `p` ever holding) |
| (c) timeout | **INCONCLUSIVE** | **INCONCLUSIVE** (more time might have helped) |
| (d) Immediate failure | n/a | **FAIL** |

`always` violations and `monitor` errors are always FAIL regardless of
cause. INCONCLUSIVE is a deliberate third state — CI integrations can
choose to gate on it via [exit codes](#cli-exit-codes).

**Timeout is always INCONCLUSIVE, never PASS (RFC-049).** A test that ends by
hitting its `timeout` is INCONCLUSIVE even if every `eventually` it declared
was already satisfied — the body did not reach a declared completion (natural
return or `terminate_when`), so the run is a *truncated prefix* and a green
verdict would over-claim. This is deliberate and conservative: a hung or
overrunning body is not a clean PASS just because its assertions happened to
hold before the deadline. To get a definitive PASS from a long-running or
`reuse=True` service, declare a `terminate_when=` so the test has a real
end-of-trace (cause b) rather than relying on the deadline.

This rule is also why per-property verdicts at timeout surface as pending:
a never-violated *unbounded* `always(p)` (no `between=` window) finalizes to
INCONCLUSIVE under timeout — a safety property merely *not yet violated* on a
truncated prefix is not established. At natural completion or `terminate_when`
the same `always` is a definitive PASS. Bounded `always(p, between=(a,b))` is
unaffected: once `b` is observed the window is decided, and an *unreached*
end-anchor at timeout was already INCONCLUSIVE.

### Why declare `terminate_when=`?

Without `terminate_when=`, an `eventually(p)` that will never hold
runs out the full `timeout=` budget (30s of wasted CI per failure).
With `terminate_when = eventually(lambda t: t.event(type="workflow.complete"))`,
the test terminates the moment the system declares "done" and
immediately FAILs the unsatisfied liveness predicate. Single-digit-
second feedback instead of 30s.

For tests where async convergence is expected and you can name the
"system done" event, always declare `terminate_when=`. For trivial
synchronous tests, omit it — natural completion handles the common
case.

## The predicate language

Predicates receive a `trace` object exposing:

### Lookup operators

| Operator | Returns | Notes |
|----------|---------|-------|
| `trace.event(type=..., **fields)` | Most recent (last) matching event, or `None` | Alias for `trace.last(...)`; use `trace.first(...)` for the earliest |
| `trace.events(matcher)` | All matching events | Ordered by emission |
| `trace.first(matcher)` | Earliest matching event | |
| `trace.last(matcher)` | Most recent matching event | Same as `event(...)` |
| `trace.count(matcher)` | Integer count | |
| `trace.events_between(start, end)` | Events between two anchors | Anchors are EventVal |
| `trace.events_within(matcher, window, of=event)` | Events within a duration of anchor | Convenience |
| `trace.causal_chain(event)` | Events causally preceding `event` | Vector-clock walk |

### Causal relations (methods on event objects)

| Operator | Meaning |
|----------|---------|
| `event_a.happens_before(event_b)` | A → B in vector-clock partial order |
| `event_a.happens_after(event_b)` | B → A |
| `event_a.concurrent_with(event_b)` | Neither A → B nor B → A |
| `event_a.same_service_as(event_b)` | Both from the same service |
| `event_a.same_correlation_as(event_b)` | Same correlation ID |
| `event.preceded_by(matcher)` | Some earlier event matches |
| `event.followed_by(matcher)` | Some later event matches |
| `event.preceded_by_within(matcher, window)` | Earlier event within a duration |
| `event.followed_by_within(matcher, window)` | Later event within a duration |
| `event.directly_caused_by(matcher)` | Immediate (non-transitive) causal predecessor matches |
| `event.duration_since(other)` | Elapsed time as integer nanoseconds (compare with `duration("...")`) |

### Aggregations on event lists

`trace.events(...)` returns a wrapped sequence with chainable
methods:

```python
trace.count(type="error") == 0
trace.events(type="postgres.query").reduce(
    lambda acc, e: acc + (1 if e.failed else 0),
    initial = 0,
) <= 3
```

Methods: `.map(fn)`, `.filter(fn)`, `.reduce(fn, initial)`, `.first`,
`.last`, `.count`.

### The `match` module

`match.*` builds reusable matcher values:

| Constructor | Meaning |
|-------------|---------|
| `match.event(type=..., **fields)` | Single-event matcher with field filters (`*` glob supported) |
| `match.any(*matchers)` | OR composition |
| `match.all(*matchers)` | AND composition (with no args, matches everything) |
| `match.never()` | Never matches |

Matchers are first-class values: store in variables, pass to monitor
`on=`, `await_event`, `await_stable(ignore=)`, etc.

## Determinism level interaction

The five primitives are level-aware (per RFC-040):

| Primitive | L1 (default engine) | L3 (gVisor + virtual clock, future) |
|-----------|---------------------|-------------------------------------|
| `eventually(p)` | Test `timeout=` is wall-clock | Virtual-clock timeout — reproducible across replays |
| `always(p, between=...)` | Checked on every mediated event | Checked on every total event |
| `await_stable()` | Wall-clock quiescence window | Virtual-clock convergence — deterministic |
| `monitor(...)` | Triggered on mediated events | Triggered on total events |
| `test(timeout=)` | Wall-clock | Virtual-clock — reproducible |

The `clock="virtual"` kwarg is reserved on `test()` and
`await_stable()` but errors at spec load with a "requires gVisor"
message. The syntax is locked in now so the L1 → L3 migration is a
substrate swap, not a spec rewrite.

## CLI exit codes

| Code | Meaning |
|------|---------|
| 0 | All tests passed |
| 2 | At least one test failed |
| 3 | INCONCLUSIVE-only (no failures, at least one timeout with pending temporal assertion) |

CI integrations can gate on 2, 3, or both. The INCONCLUSIVE signal
exists so a test that's frequently INCONCLUSIVE is visible in
dashboards as either misspecified (timeout too tight) or genuinely too
slow — not silently rolled into FAIL.

## What the event log can know

Predicates assert on the *event log*, not on external state. The log
captures whatever the configured event sources emit:

| Source | Captures | Property check feasible? |
|--------|----------|--------------------------|
| `topic()` (Kafka) | Message payloads, partition, offset, key | ✅ payload fields queryable directly |
| `wal_stream()` (Postgres logical replication) | Row-level INSERT/UPDATE/DELETE with old + new column values | ✅ derived DB state captured directly |
| MySQL/Redis proxy | Commands + responses | ⚠️ operations visible; values only when SUT reads back |
| `stdout()` / `stderr()` | Decoded log lines | ⚠️ whatever the SUT chose to log |

For state the log can't see today (write-only files, in-memory state
the SUT never reads back), use one of three patterns:

1. **CDC / WAL streaming** — strongest. Add `wal_stream()` for Postgres.
2. **Probe reads in the test body** — `api.get_balance()` issues a
   SELECT; the proxy captures the result.
3. **State projection via monitor memory** — fold events into derived
   state with `monitor(state_init=, update=, check=)`.

This is a design constraint of L1, not a bug. The L1 contract
(RFC-040) only promises mediated-event determinism; properties about
non-mediated state need to be made observable.

## See also

- [RFC-041 (Temporal Properties)](rfcs/0041-temporal-properties.md) — full design
- [RFC-040 (Determinism Levels)](rfcs/0040-determinism-levels.md) — level vocabulary
- [docs/determinism.md](determinism.md) — L1 manifest, unmediated_io categories
- [docs/spec-language.md](spec-language.md) — full builtin reference
