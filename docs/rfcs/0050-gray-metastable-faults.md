# RFC-050: Gray & Metastable Faults — Closing the Fault-Model Gaps

> **Status: Draft.** Feature RFC (Direction 3 of [RFC-047](https://github.com/faultbox/Faultbox/issues/132)). **Second in the agreed research-epic sequence (D4 → D3 → RFC-048 DPOR+LDFI).** Builds on RFC-041 (Temporal Properties — the recovery property), the fault lifecycle (`fault_start`/`fault_stop`), and the protocol proxy (`response`/`error`/`drop`/`delay`, path matching). Feeds RFC-048: the primitives defined here enter LDFI's modeled fault space.
>
> In-tree document: `docs/rfcs/0050-gray-metastable-faults.md`.

## Summary

Faultbox's fault primitives map cleanly onto the classic dependability taxonomy for crash, omission, and timing failures — but two failure *classes* that disproportionately hurt real production systems are expressible only awkwardly today:

- **Gray failures** — a component that *limps*: passes health checks while serving slow / partial / wrong responses to real traffic. The defining feature is **differential observability** — the failure detector's view (healthy) diverges from the user's view (degraded).
- **Metastable failures** — a system that stays broken *after the trigger is removed*, sustained by a feedback loop (retry storms, cache stampedes). The defining feature is **non-recovery**: a transient perturbation latches into a persistent bad state.

This RFC (1) maps Faultbox's primitives onto the dependability taxonomy to make the coverage explicit, and (2) adds the minimal vocabulary for the two gaps: a first-class **`degrade(...)`** fault and a **`transient(...)`-then-check-recovery** idiom. Both are demoable, low-risk, and squarely in Faultbox's spec-language wheelhouse; the field evaluation confirmed both gaps (a read endpoint that degraded under a slow database — a baby gray failure — and observed retry amplification on a mutating path).

## Motivation

Gray and metastable failures are *the* modes that survive positive-path QA and chaos engineering's "kill a node" repertoire, because neither is a clean crash: the gray one masks itself from the very health signal the platform trusts, and the metastable one only appears *after* the obvious fault is gone. Making Faultbox express and *verify* them is both a real capability gap and a sharp, marketable differentiator — "watch Faultbox catch a retry storm that survives the trigger being removed" is a demo no kill-a-node tool can match. As the user-facing epic in the D4→D3→RFC-048 sequence, D3 is the one that lands the next user.

## Technical Details

### The taxonomy mapping

| Fault class (Avizienis et al. 2004; Cristian 1991) | Faultbox primitive today | Status |
|---|---|---|
| Crash-stop | `deny(connect, ECONNREFUSED)` | ✅ clean |
| Omission | `drop` (protocol), `deny` (syscall) | ✅ clean |
| Timing (slow) | `delay` (syscall), `response(delay=)` (protocol) | ✅ clean |
| Byzantine (wrong value) | `response(...)` rewrite to a plausible-but-wrong body | ✅ expressible |
| **Gray (differential observability)** | `delay` + `response` + path matching, by hand | ⚠️ no first-class form |
| **Metastable (non-recovery)** | `fault_start` → `fault_stop` + `eventually`, by hand | ⚠️ no idiom |

The two gaps are not missing *mechanism* — the proxy can already slow, rewrite, and path-match, and the fault lifecycle can already trigger-then-withdraw. They are missing a **first-class name and the paired verification**, which is what turns "a fault you could hand-assemble" into "a failure class Faultbox tests and reports as such."

### `degrade(...)` — gray failure as a primitive

The essence of a gray failure is that the **health path stays green while the data path rots.** `degrade` is a protocol-interface fault that injects sub-total degradation on real traffic while exempting the health-check path:

```python
degrade(
    health_path = "/healthz",        # kept fast + 200 so the orchestrator never evicts
    latency     = "800ms",           # data-path slowdown
    error_rate  = 0.1,               # partial, not total — probability fan-out under the hood
    # optional: partial = lambda body: truncate(body),  # wrong/partial response
)
```

It composes from primitives that exist — `response(delay=)`, `error(...)`, the RFC-042 probability fan-out (`error_rate` → `max_fires`/`mode`), and proxy path matching for the `health_path` exemption — but ships as one named fault so the report can label it **"gray failure (differential observability)"** rather than a pile of unrelated proxy rules.

**The paired verification is the point.** A `degrade` fault is meant to be checked against the *divergence itself*:

```python
always(health_ok)                    # the platform keeps thinking it's healthy …
eventually(user_slo_violated)        # … while users see the SLO break
```

A test where both hold under `degrade` has reproduced a gray failure: the system masks a real degradation from its own failure detector. That divergence is the bug, and it is exactly what a kill-a-node tool cannot surface.

### `transient(...)` — metastable trigger + recovery check

The essence of a metastable failure is **non-recovery after the trigger clears.** Faultbox already has the lifecycle (`fault_start`/`fault_stop`); `transient` wraps it with an explicit withdrawal and a recovery anchor:

```python
transient(
    fault    = deny(connect, "ECONNREFUSED"),   # the trigger (e.g. upstream outage)
    duration = "5s",                            # … withdrawn after 5s (or until=<event>)
)
# emits a `fault_removed` anchor when the trigger is withdrawn
```

paired with a **bounded recovery property** (the MTL recovery shape noted in RFC-049):

```python
# after the trigger is gone, load/latency/queue-depth must return to baseline within N
eventually(recovered, after = "fault_removed", within = "10s")
# FAIL if the system stays latched in the bad state — that is metastability
```

What makes this observable on Faultbox's substrate: the feedback loop's signal (request/retry volume, queue depth) crosses the **mediated boundary** — the proxy sees retry traffic stay elevated after `fault_stop`. "Recovered" is defined relative to a **pre-trigger baseline** captured by `await_stable` before the transient fires. If the post-withdrawal state never re-quiesces to that baseline within the window, the test FAILs as metastable.

### Minimalism (RFC-044 ethos)

Both additions are deliberately thin: `degrade` is one named composite over existing proxy mechanism; `transient` is a lifecycle wrapper plus the recovery-property shape Faultbox already has via `eventually`/`await_stable`. No new engine subsystem. The new surface is two builtins and one anchor event (`fault_removed`).

## Impact

- **Breaking changes:** none — both are additive builtins.
- **Feeds RFC-048 (LDFI).** `degrade` and `transient` join the modeled fault space, so LDFI's hitting set ranges over "degrade" and "transient-trigger" faults, not just `deny`/`delay`/`drop` (RFC-048 OQ4). Note the open question there: a `transient` fault's "break" is a *recovery property failing*, not a single broken derivation, so LDFI needs a distinct success-predicate hook for it.
- **Report.** Gray and metastable get first-class labels and the paired-property view, so a degraded-but-green run reads as a gray failure rather than a confusing PASS.

## Open Questions

1. **Primitive vs composition surface.** Ship `degrade`/`transient` as builtins (proposed), or as documented recipes over `response`/`fault_start`? Builtins win on report-labeling and discoverability; recipes win on spec-language minimalism. Leaning builtins precisely because the *report semantics* (labeling the class) need a first-class type.
2. **"Recovered" baseline definition.** Is pre-trigger `await_stable` quiescence the right baseline, or should the user declare the recovery predicate explicitly (queue depth < X, p99 < Y)? Probably both: a default baseline from quiescence, overridable by an explicit predicate.
3. **Health-path mediation for `degrade`.** The `health_path` exemption assumes the health check traverses the proxy. For container healthchecks that hit the service directly (not through the FB proxy), how is the exemption enforced — does `degrade` require the healthcheck to be proxy-routed?
4. **Recovery-window clock.** Is `within=` wall-clock or virtual-time? Under `--virtual-time` the metastable feedback loop's timing must advance consistently; ties to the RFC-049 MTL recovery semantics.

## Implementation Plan

1. Write the taxonomy mapping into `docs/` (concepts + reference) — the table above, made complete.
2. `degrade(...)` builtin: parse + validate, lower to existing proxy `response`/`error` + path-exemption + probability fan-out; add the `gray_failure` report label.
3. `transient(...)` builtin: wrap `fault_start`/`fault_stop` with a duration/until withdrawal and emit the `fault_removed` anchor; add `eventually(..., after=, within=)` recovery-property support (or confirm it composes from existing temporal + the anchor).
4. Baseline capture via `await_stable` + the recovery verdict; `metastable` report label.
5. Hand the two new fault types to RFC-048's fault-space enumeration.

## References

A. Avizienis, J.-C. Laprie, B. Randell, C. Landwehr, "Basic Concepts and Taxonomy of Dependable and Secure Computing," *IEEE TDSC* 1(1), 2004. · F. Cristian, "Understanding Fault-Tolerant Distributed Systems," *CACM* 34(2), 1991. · F. Schneider, "Implementing Fault-Tolerant Services Using the State Machine Approach," *ACM CSUR* 22(4), 1990. · P. Huang et al., "Gray Failure: The Achilles' Heel of Cloud-Scale Systems," *HotOS* 2017. · N. Bronson et al., "Metastable Failures in Distributed Systems," *HotOS* 2021. · L. Huang et al., "Metastable Failures in the Wild," *OSDI* 2022.
