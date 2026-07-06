# RFC-050: Gray & Metastable Faults — Closing the Fault-Model Gaps

> **Status: Draft (rev. 2).** Feature RFC (Direction 3 of [RFC-047](https://github.com/faultbox/Faultbox/issues/132)). **Second in the agreed research-epic sequence (D4 → D3 → RFC-048 DPOR+LDFI).** Builds on **RFC-049** (accepted — the verdict framework the bounded-recovery operator plugs into, and whose MTL deferral this RFC narrowly reopens), the fault lifecycle (`fault_start`/`fault_stop`), and the protocol proxy (`response`/`error`/`drop`/`delay`, path matching). Feeds RFC-048: **`degrade` only** enters LDFI's modeled fault space (`transient`/metastability is a different verification shape LDFI does not subsume — see Impact).
>
> Rev. 2 (2026-07-05) reworks the first draft after review: adds the `load(...)` traffic driver (metastability is load-induced; the original under-addressed this), scopes the gray-failure claim honestly (proxy-routed healthcheck only; no modeled failure-detector consequence), makes the bounded-recovery operator an explicit new-surface item rather than "composes for free," and scopes "recovered" to boundary-observable signals.
>
> In-tree document: `docs/rfcs/0050-gray-metastable-faults.md`.

## Summary

Faultbox's fault primitives map cleanly onto the classic dependability taxonomy for crash, omission, and timing failures — but two failure *classes* that disproportionately hurt real production systems are expressible only awkwardly today:

- **Gray failures** — a component that *limps*: passes health checks while serving slow / partial / wrong responses to real traffic. The defining feature is **differential observability** — the failure detector's view (healthy) diverges from the user's view (degraded).
- **Metastable failures** — a system that stays broken *after the trigger is removed*, sustained by a feedback loop (retry storms, cache stampedes). The defining feature is **non-recovery**: a transient perturbation latches into a persistent bad state.

This RFC (1) maps Faultbox's primitives onto the dependability taxonomy to make the coverage explicit, and (2) adds the vocabulary for the two gaps: a first-class **`degrade(...)`** fault, a **`transient(...)`** trigger-then-withdraw fault, a **`load(...)`** background traffic driver, and a narrow **bounded-recovery** temporal operator (`eventually(p, after=, within=)`). The field evaluation confirmed both gaps (a read endpoint that degraded under a slow database — a baby gray failure — and observed retry amplification on a mutating path).

**Two honesty caveats, made load-bearing in the design below rather than buried:** (a) *gray failure* — Faultbox can reproduce the degradation and let you assert the health/data-path *divergence*, but it does **not** model the orchestrator/failure-detector that would *act* on the healthcheck; the exemption also only works when the healthcheck is proxy-routed. (b) *metastable failure* — a metastable latch is **load-induced**: it only appears when sustained traffic pushes the system past a tipping point. Faultbox drives the SUT with scripted `step()` calls, not production load, so `transient` alone tests only "does the system recover from a brief fault" (the *easy* direction — most do). Catching real metastability requires the new `load(...)` driver to supply the sustaining pressure. This RFC treats load generation as the *other half* of the feature, not an afterthought.

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

The two gaps differ in how much is missing. **Gray** is mostly a *naming + paired-verification* gap: the proxy can already slow, rewrite, and path-match, so `degrade` is largely a first-class name (plus the health-path exemption) over existing mechanism. **Metastable** is a genuine *mechanism* gap: reproducing a load-induced latch needs sustained traffic Faultbox cannot generate today (`load(...)`) and a bounded-recovery deadline the temporal layer does not yet have (`eventually(after=, within=)`). Calling both "just missing a name" would be wrong — and is the trap the first draft fell into.

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

A test where both hold under `degrade` has reproduced the *observable signature* of a gray failure: the health path stays green while users see the SLO break. That divergence is the bug, and it is exactly what a kill-a-node tool cannot surface.

**Scope — what `degrade` does and does not model.** Two honest limits:

- **The healthcheck must be proxy-routed** for the `health_path` exemption to mean anything. A container-native `HEALTHCHECK` or a k8s liveness probe that hits the pod directly does not traverse the Faultbox proxy, so `degrade` cannot hold it green while degrading other paths. `degrade` validates the exemption target at spec-load and errors if the service's healthcheck is not routed through an interface the proxy owns.
- **There is no failure detector in the loop.** Faultbox does not model an orchestrator/load-balancer that *acts* on the healthcheck (evicts, reroutes, opens a circuit). So `always(health_ok)` asserts *the test author observed the health path stay 200*, not *a real detector was fooled into a wrong decision*. `degrade` reproduces the differential-observability *condition*; verifying the *consequence* (does the platform mis-route because of it) is out of scope until a modeled detector exists (a possible future RFC). Stated plainly so the demo claim stays honest.

### `load(...)` — the other half of metastability

A metastable latch is **load-induced**: the sustaining feedback loop (retry storm, connection-pool exhaustion, cache stampede) only engages when traffic holds the system past its tipping point. Scripted `step()` calls in the test body do not supply that. So `transient` needs a companion that generates **sustained background traffic** through the proxy:

```python
load(
    target   = api.http,                 # drive requests at this interface
    request  = lambda: api.http.get("/feed"),
    rate     = "200rps",                 # constant offered load …
    duration = "30s",                    # … for the window that spans the transient
)
```

`load` runs concurrently with the test body, firing `request` at `rate`. Because every request crosses the proxy, the response of the loop to the transient — retries piling up, latency climbing, throughput collapsing and *staying* collapsed — is **observable at the mediated boundary**. Without `load`, `transient` only exercises recover-from-a-brief-fault (the easy direction); with it, the test can actually reach and detect a latched state. `load` is a minimal constant-rate generator (no ramps/arrival-distribution modelling in v1); those are a later refinement.

### `transient(...)` + bounded recovery — the metastability check

`transient` wraps the existing `fault_start`/`fault_stop` lifecycle with an explicit withdrawal, emitting a `fault_removed` anchor:

```python
transient(
    fault    = deny(connect, "ECONNREFUSED"),   # the trigger (e.g. upstream outage)
    duration = "5s",                            # … withdrawn after 5s (or until=<event>)
)
```

Under sustained `load`, the metastability question is: *after the trigger is gone, does the system re-quiesce, or stay latched?* That is a **bounded-recovery property** — this RFC owns the narrow temporal operator for it:

```python
eventually(recovered, after = "fault_removed", within = "10s")
# recovered within the window → PASS; deadline passes still latched → FAIL (metastable)
```

**This is a new temporal operator, not free.** RFC-049 (accepted) deliberately deferred general MTL/STL and kept `always(between=)` as the only metric form. `eventually(p, after=anchor, within=duration)` is the *minimal* slice needed here: a single anchor plus a hard deadline — not full MTL. Its verdict plugs into the RFC-049 framework cleanly: **the `within` deadline is a declared end-of-trace for that property** (LTL_f-style), so an unsatisfied recovery *at the deadline* is a definitive **FAIL** — not INCONCLUSIVE — because the bound, not a timeout, closed it. (If the enclosing test times out *before* the deadline, the property is still `?` → INCONCLUSIVE, per RFC-049.)

**"Recovered" is boundary-scoped — the same discipline as RFC-048.** The recovery signal must be something Faultbox observes at the mediated boundary: request/retry rate, error rate, or latency *through the proxy*. Internal signals the SUT never exposes across the boundary — queue depth, in-process pool saturation — are **not** usable as `recovered` predicates, exactly as RFC-048's guarantees are scoped to mediated events. The default `recovered` baseline is a pre-trigger `await_stable` quiescence of the boundary signal; a spec may override with an explicit predicate over boundary-observable quantities (e.g. `p99_latency < baseline * 1.2`). If the loop never engaged (the transient did not perturb the boundary signal at all), the recovery check is vacuous → it emits a `vacuous_property` warning (reusing the RFC-049 vacuity mechanism) so a scenario that never actually stressed the system does not read as a green PASS.

### Worked example — a metastable retry storm

Putting the three pieces together on the eval's mutating path (the one that showed retry amplification):

```python
db  = service("db", image = "postgres:16", interface("pg", "postgres", 5432), healthcheck = tcp("localhost:5432"))
api = service("api", image = "shop-api:latest", interface("http", "http", 8080),
    depends_on = [db], healthcheck = http("localhost:8080/health"))

def test_retry_storm_is_transient():
    # 1. hold steady offered load across the whole scenario
    load(target = api.http, request = lambda: api.http.post("/orders", body = ORDER),
         rate = "200rps", duration = "30s")

    # 2. capture the pre-trigger boundary baseline (request/latency quiesced)
    await_stable(quiescence_window = "2s")

    # 3. a brief DB outage — then withdraw it
    transient(fault = deny(db.pg, connect = "ECONNREFUSED"), duration = "5s")

    # 4. the metastability check: once the DB is back, the boundary signal must
    #    re-quiesce within 10s. If retries keep the system pinned, this FAILs.
    eventually(recovered, after = "fault_removed", within = "10s")

test("retry_storm_is_transient", body = test_retry_storm_is_transient, timeout = "60s")
```

The bug this catches: the DB recovers at t=5s, but client/queue retries built up during the outage keep offered load above the tipping point, so the system never re-quiesces — `recovered` never holds within the window → **FAIL (metastable)**. Remove the `load(...)` and the same test passes trivially (a 5s outage with no sustained pressure recovers on its own) — which is exactly why `load` is load-bearing, not optional.

### Minimalism vs. what this actually costs (RFC-044 ethos)

`degrade` is a thin named composite over existing proxy mechanism. `transient` is a thin lifecycle wrapper. But the feature is **not** free: reproducing real metastability requires the new `load(...)` traffic driver (a genuine new subsystem, kept minimal), and the bounded-recovery check requires the new `eventually(after=, within=)` operator (a narrow MTL slice RFC-049 deferred). The honest surface is **three builtins** (`degrade`, `transient`, `load`), **one temporal-operator extension**, and **one anchor event** (`fault_removed`) — larger than the original "two thin builtins" framing, because the metastability half needs load and a deadline to mean anything.

## Impact

- **Breaking changes:** none — all additive builtins.
- **Feeds RFC-048 (LDFI) — but only `degrade` fits cleanly.** `degrade` is a degradation that can break a success *derivation*, so it joins LDFI's modeled fault space and its hitting set directly (alongside `deny`/`delay`/`drop`). **`transient`/metastability does not fit LDFI's model:** its "break" is a *recovery property failing over time under load*, not a single fault breaking a derivation of a success event. LDFI's hitting-set formulation has no natural place for it. So the honest statement is: `degrade` extends LDFI; `transient` is a *different verification shape* that LDFI does not subsume (RFC-048 OQ4 should be narrowed to `degrade` only). Do not claim both enter LDFI's fault space.
- **Report.** Gray and metastable get first-class labels and the paired-property view, so a degraded-but-green run reads as a gray failure rather than a confusing PASS, and a non-recovering transient reads as metastable rather than a bare temporal FAIL.
- **New subsystem cost.** `load(...)` is a real (if minimal) traffic generator — the first time Faultbox drives *sustained* offered load rather than discrete `step()` calls. Worth calling out as the one non-trivial engine addition in this RFC.

## Open Questions

1. **`load(...)` fidelity in v1.** Constant-rate offered load is the minimum that can induce a latch. Is that enough to reproduce real metastability, or does the tipping point need closed-loop behaviour (client-side retry/backoff modelling, arrival-distribution, ramps)? v1 ships constant-rate + client retries observed at the boundary; ramps/distributions are a follow-up. How is `load`'s own traffic distinguished in the trace from the body's `step()` calls (a `source=load` tag)?
2. **Recovery-window clock (`within=`).** Wall-clock or virtual-time? Under `--virtual-time` the feedback loop's timing must advance consistently, and `load`'s rate must be virtual-time-aware, else the metastable window is non-reproducible. Ties to the bounded-recovery operator's semantics.
3. **`degrade` without a proxy-routed healthcheck.** The scope section requires the healthcheck to traverse the proxy for the exemption to hold; `degrade` errors at spec-load otherwise. Is a hard error the right call, or should it warn-and-degrade-everything (losing the differential but still injecting the degradation)?
4. **Primitive vs recipe surface (minor).** `degrade`/`transient` could ship as documented recipes over `response`/`fault_start`, but the *report-labeling* (gray/metastable as first-class classes) needs a real type — hence builtins. `load` must be a builtin regardless (new mechanism).

*Resolved in this revision:* the "recovered" baseline is a boundary-observable signal (default: pre-trigger `await_stable` quiescence, overridable by a boundary-scoped predicate — internal queue depth is explicitly excluded); the recovery operator is a narrow bounded-`eventually` whose deadline is an LTL_f end-of-trace for that property (FAIL at deadline, per RFC-049); a never-engaged loop emits a `vacuous_property` warning.

## Implementation Plan

1. Write the taxonomy mapping into `docs/` (concepts + reference) — the table above, made complete, with the two scope caveats stated.
2. **`load(...)` builtin first** — the constant-rate background traffic driver through the proxy, virtual-time-aware, tagging its requests `source=load`. It is the prerequisite for any real metastability test, so it leads.
3. `degrade(...)` builtin: parse + validate (including the proxy-routed-healthcheck check), lower to existing proxy `response`/`error` + path-exemption + probability fan-out; add the `gray_failure` report label.
4. Bounded-recovery operator: `eventually(p, after=anchor, within=duration)` with the RFC-049-consistent verdict (FAIL at the deadline). Boundary-scoped `recovered` default via `await_stable`; vacuity warning when the loop never engages.
5. `transient(...)` builtin: wrap `fault_start`/`fault_stop` with duration/until withdrawal, emit the `fault_removed` anchor; `metastable` report label.
6. Hand **`degrade` only** to RFC-048's LDFI fault-space enumeration (not `transient` — see Impact).

## References

A. Avizienis, J.-C. Laprie, B. Randell, C. Landwehr, "Basic Concepts and Taxonomy of Dependable and Secure Computing," *IEEE TDSC* 1(1), 2004. · F. Cristian, "Understanding Fault-Tolerant Distributed Systems," *CACM* 34(2), 1991. · F. Schneider, "Implementing Fault-Tolerant Services Using the State Machine Approach," *ACM CSUR* 22(4), 1990. · P. Huang et al., "Gray Failure: The Achilles' Heel of Cloud-Scale Systems," *HotOS* 2017. · N. Bronson et al., "Metastable Failures in Distributed Systems," *HotOS* 2021. · L. Huang et al., "Metastable Failures in the Wild," *OSDI* 2022.
