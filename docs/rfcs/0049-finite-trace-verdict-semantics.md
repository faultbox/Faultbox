# RFC-049: Finite-Trace Verdict Semantics — Pinning the Monitor Table to LTL₃ / LTL_f

> **Status: Draft.** Specification RFC (Direction 4 of [RFC-047](https://github.com/faultbox/Faultbox/issues/132)). Refines RFC-041 (Temporal Properties) — does not add surface, it *grounds* the existing verdict logic. **First in the agreed research-epic sequence (D4 → D3 → RFC-048 DPOR+LDFI):** despite the higher RFC number, this lands first because it gates the trustworthiness of every verdict the later epics emit.
>
> In-tree document: `docs/rfcs/0049-finite-trace-verdict-semantics.md`.

## Summary

RFC-041 ships a three-valued verdict — PASS / FAIL / **INCONCLUSIVE** — whose finite-prefix resolution is described by a **hand-written table** (`docs/temporal.md` §"Test completion"). The INCONCLUSIVE verdict means Faultbox is already, implicitly, doing three-valued runtime-verification LTL. This RFC makes that explicit: map the verdict to **LTL₃ / RV-LTL** (Bauer, Leucker & Schinz 2011) and the terminating-test evaluation to **finite-trace LTL_f** (De Giacomo & Vardi 2013), then **re-derive the table from the semantics and diff against the shipped behavior.** Every disagreement is either a documentation clarification or a latent bug to fix.

This is a ~2-week specification task with no engine rebuild. Its value is twofold: (1) it reconciles the verdict semantics across docs, per-property `Finalize`, and the runtime aggregation — which, on inspection, disagreed in *opposite* directions (the docs table over-claimed PASS at timeout; the per-property `Finalize` over-claimed PASS for unbounded `always`; the runtime aggregation was already conservative-correct — see the findings); and (2) it gives users (and the RFC-048 coverage statements that sit on top of these verdicts) a citable, well-understood mental model instead of a bespoke table.

## Motivation

Two forces:

1. **Two reduction algorithms are about to depend on this layer.** RFC-048's DPOR reports "violation found" and LDFI reasons over a "success outcome" — both are PASS/FAIL/INCONCLUSIVE verdicts. Building coverage *guarantees* on top of a hand-rolled verdict table is unsound by construction. The verdict semantics must be pinned to a named logic *before* the moat epic claims anything.
2. **Hand-written finite-prefix tables get the boundary wrong.** The subtle cases — `eventually` that never fires, `always` truncated by timeout, an open `between=` interval, vacuous intervals — are exactly where infinite-trace intuition and finite-trace reality diverge. A table written by hand tends to encode the common case correctly and the boundary by accident.

## Technical Details

### The verdict ↔ logic mapping

| Faultbox verdict | LTL₃ / RV-LTL value | Meaning |
|---|---|---|
| **PASS** (for a property) | ⊤ | definitively satisfied — no extension of the trace can change it |
| **FAIL** | ⊥ | definitively violated — no extension can repair it |
| **INCONCLUSIVE** | ? | the finite prefix does not decide it; a longer trace could go either way |

A test's overall verdict is the conjunction over its properties, with FAIL dominating ? dominating ⊤ (any ⊥ → FAIL; else any ? → INCONCLUSIVE; else PASS).

### Which logic applies depends on *why* the trace ended

This is the crux the current table encodes informally and this RFC makes explicit. A finite trace is interpreted under **LTL_f** (the trace is *logically complete* — there is no "rest of the trace") **only** when termination signals genuine completion; otherwise it is an LTL₃ *truncated prefix* (the system was still running; "?" verdicts survive).

| Termination cause (RFC-041) | Interpretation | Consequence |
|---|---|---|
| (a) Body returns | **LTL_f** end-of-trace | `eventually`/`always` get *definite* verdicts |
| (b) `terminate_when=` fires | **LTL_f** end-of-trace | same — the user declared "final state reached" |
| (c) `timeout=` elapses | **LTL₃** truncated prefix | unresolved `?` stays INCONCLUSIVE — we stopped watching, the system did not stop |
| (d) Immediate failure | ⊥ directly | FAIL |

### Per-operator semantics

- **`eventually(p)` = F p — co-safety, monotone.** Once `p` holds, the verdict latches ⊤ for the rest of the trace. Before that it is `?`. On an LTL_f end-of-trace (a/b) with `p` never held → ⊥ (FAIL). On an LTL₃ truncation (c) with `p` never held → `?` (INCONCLUSIVE). *This matches the shipped table.*
- **`always(p)` = G p — safety.** Violated the instant `p` is false → ⊥ (FAIL), any cause. Never violated: ⊤ only on an LTL_f end-of-trace (a/b); on truncation (c) it is **`?`, not ⊤** — a longer trace could still violate it. The shipped per-property `Finalize` did not distinguish this for the *unbounded* case (Finding B below).
- **`always(p, between=(a,b))` — bounded safety / MTL.** Once the closing anchor `b` is observed, G p over [a,b] is *definitively* decided even mid-run → can be ⊤ before termination. If `b` is never observed before timeout, the interval is **open** → `?` (INCONCLUSIVE). **Already correct in the engine** — `AlwaysExpectation.Finalize` returns `Pending` for an unreached end-anchor under `TerminationTimeout`, and PASS under `terminate_when` (the window spans the whole test). This is the *precedent* the unbounded-`always` fix below should follow. If the opening anchor `a` never occurs, the interval is **empty** → classically vacuously ⊤ (the one remaining open question — vacuity is a known monitor footgun and should be a stated decision, ideally with a warning).
- **`monitor(check=)` — per-event safety.** Equivalent to G over the check predicate. Same truncation rule as unbounded `always`: a never-violated monitor is ⊤ at LTL_f completion, `?` at timeout.
- **`await_event` / `await_stable` — liveness-flavored, block the body.** Not reached before timeout → INCONCLUSIVE via the timeout cause. This is already correct LTL₃ (quiescence/await is a `?` until observed).

### Long-living and `reuse=True` services (decided)

A `reuse=True` service — or any service that never naturally returns — has no LTL_f *body-returns* terminal (cause a). For these, **the termination signal is the user's to define.** An explicit `terminate_when=` marks the logical end of the unit of work under verification, and Faultbox interprets the trace up to that point under LTL_f: if a declared `eventually(p)` has not held by the user's `terminate_when` signal, the test **FAILS** — "the service reached its declared completion and `p` never happened." Absent any `terminate_when`, the only terminal cause is `timeout` → **INCONCLUSIVE**: Faultbox will not invent an end-of-trace the user did not declare. `terminate_when=` is therefore strongly recommended (effectively required for a non-INCONCLUSIVE verdict) on long-living / `reuse=True` specs.

The asymmetry is deliberate and matches how these systems are actually tested:

- The **reused dependency** (a long-living Postgres, Redis, broker) is assumed *stable background infrastructure*, not the verification target. Its events appear in the trace, but its non-termination does **not** poison the verdict — we do not wait for the database to "finish."
- The **service-under-test** is the target. The user-declared termination is about *its* unit of work, and the verdict is rendered against *its* behavior at that point.
- Soundness of the long-living dependency's use across tests — state leakage, a missing `seed`/`reset` — is the **spec author's responsibility**, not something the verdict semantics can recover. Faultbox already warns on `reuse=True` without `seed`/`reset`; that warning is the contract boundary here.

### The findings (after tracing the *aggregation*, not just the table)

The initial hypothesis — "the engine over-PASSes at timeout" — was **half right, and the interesting half is the correction.** Tracing the actual test-level aggregation (`runtime.go`, not just `AlwaysExpectation.Finalize`) shows two distinct things:

**Finding A — test-level timeout was already conservative-correct (the docs were wrong, not the engine).** The runtime termination path returns **INCONCLUSIVE for *any* timed-out test that did not FAIL** — including one whose every `eventually` was satisfied — because the body never reached a *declared* completion (natural return or `terminate_when`), so the run is a truncated prefix. The shipped docs table claimed `(c) timeout / all satisfied → PASS`; that cell is contradicted by the engine's catch-all. **Decision (kept): INCONCLUSIVE.** A hung or overrunning body is not a clean PASS just because its assertions happened to hold before the deadline; long-running / `reuse=True` specs that want a definite PASS must declare `terminate_when=` (cause b). The fix here is to the **docs table** and to making the catch-all's intent explicit in code — not to test outcomes.

**Finding B — per-property over-claim (real, but dominated by A at the test level).** `AlwaysExpectation.Finalize` returned `VerdictPass` for a never-violated **unbounded** `always(p)` under *every* cause, including timeout: the `endPending` guard fires only for an unreached `between=` end-anchor, so the unbounded case fell through. Per LTL₃ that per-property verdict should be `?` at timeout. We fix it — the engine already did exactly this for *bounded* windows, so the change extends that truncation logic to the unbounded case — but note it does **not** change test outcomes (Finding A already made them INCONCLUSIVE); it corrects per-property *reporting* and aligns the engine with the oracle.

**Non-finding — the `monitor` case.** Monitors aren't in the expectation set, so a never-violated `monitor` at timeout falls through to the same catch-all (Finding A) → INCONCLUSIVE. No separate change needed.

The lesson is exactly why step 3 (diff the oracle against the *engine*, not the prose table) is in the plan: the docs table and the per-property `Finalize` both disagreed with the engine's already-conservative aggregation, in opposite directions.

### The re-derived verdict table (proposed)

Overall test verdict = worst property verdict, with safety/liveness resolved per cause:

| Cause | `eventually` unsatisfied | unbounded `always`/`monitor` not violated | bounded `always` (interval closed) | bounded `always` (interval open) |
|---|---|---|---|---|
| (a) Natural / (b) terminate_when | **FAIL** (LTL_f ⊥) | **PASS** (LTL_f ⊤) | PASS/FAIL as decided | **INCONCLUSIVE** (open at end) |
| (c) timeout | **INCONCLUSIVE** | **INCONCLUSIVE** (per-property fix, Finding B) | PASS/FAIL as decided | **INCONCLUSIVE** |
| (d) immediate failure | **FAIL** | **FAIL** (the violation) | — | — |

(`eventually` satisfied → that property is ⊤ everywhere; the table shows the *binding* property.) **At the test level, the entire `(c) timeout` row is INCONCLUSIVE regardless of per-property verdicts** — the runtime catch-all (Finding A) maps any non-FAIL timeout to INCONCLUSIVE, because the body never reached a declared completion. The per-property column for (c) matters for *reporting* (what the report shows for each property), not for flipping the test outcome.

### Out of scope (noted)

- **MTL/STL for timing predicates over `slow` faults** (Koymans 1990; Maler & Nickovic 2004). `always(between=)` is already metric; full MTL/STL for latency assertions is a later RFC.
- **The recovery property** ("healthy within N seconds after a fault is removed") is a bounded-MTL property; D4 should confirm the verdict semantics match intent at the timeout boundary, but the `transient`/recovery *vocabulary* is RFC-047 Direction 3 (RFC-050).

## Impact

- **Breaking changes: none at the test-outcome level.** The conservative "any timeout → INCONCLUSIVE" behavior (Finding A) was already shipping; this RFC only corrects the docs table to match it and makes the catch-all's intent explicit. The per-property `always` fix (Finding B) changes a *reported* per-property verdict, not a test verdict. So no test that passed before now fails or flips outcome. Bounded-window and `monitor` cases needed no change. Vacuity is a doc/warning decision (the one open question), not a verdict change. The oracle (step 3) is now in the tree (`TestVerdictOracle_FinalizeMatrix`) and found no further per-property divergence.
- **CLI exit codes:** unchanged (0/2/3); D4 only changes *which* verdict a borderline test produces, which then flows through the existing exit-code mapping.
- **Downstream:** RFC-048's coverage statements become semantically grounded — DPOR's "violation" and LDFI's "success outcome" are now LTL₃/LTL_f verdicts with a citation.

## Decisions (resolved)

- **3-valued, not 4-valued (was OQ1).** Faultbox exposes exactly PASS / FAIL / INCONCLUSIVE. A never-violated safety property at `timeout` is **INCONCLUSIVE**, not a qualified PASS — we do not adopt RV-LTL's "presumably true." This is the honest, CI-actionable choice and aligns with the conservatism principle the RFC-048 guarantees rely on.
- **Timeout is always INCONCLUSIVE, never PASS (Finding A).** A test that ends on its `timeout` deadline is INCONCLUSIVE even if every declared property was satisfied — the body did not reach a declared completion, so the run is a truncated prefix and a green verdict would over-claim. This was already the engine's behavior (a conservative catch-all); D4 corrects the docs table to match and documents the intent. It is the test-level corollary of the OQ2 decision: an un-terminated service run is not a clean PASS.
- **User-defined termination for long-living / `reuse=True` services (was OQ2).** Faultbox does not auto-detect end-of-trace for a service that never naturally ends; the user declares it via `terminate_when=`, at which point LTL_f applies (unsatisfied `eventually` before the declared completion → FAIL). Absent `terminate_when=`, the only terminal is `timeout` → INCONCLUSIVE. The reused dependency is stable background infrastructure, not the verification target; soundness of its cross-test use is the spec author's responsibility. See "Long-living and `reuse=True` services" above.
- **Vacuity: PASS + warning, not a verdict change (was the last OQ).** An `always(p, between=(a,b))` whose start anchor `a` never fires has a never-opened window → vacuously ⊤ → **PASS preserved.** Forcing INCONCLUSIVE was rejected: a window can be *legitimately* untriggered (e.g. `between=(error, recovery)` in a clean run), and demoting every such case to INCONCLUSIVE would flood normal runs with false alarms. Instead the runtime emits a **`vacuous_property` warning event** when a window never opened, so an accidentally-broken/typo'd anchor surfaces in the trace rather than hiding as a silent green — without changing the verdict. Implemented: `AlwaysExpectation.VacuousWindow()` + the emit in the Finalize loop, guarded so an unbounded always (opens immediately) and a violated always are never flagged.

## Open Questions

None remaining for the verdict semantics. Possible follow-up: a report-renderer treatment for `vacuous_property` (surface it as a soft warning chip), and extending the oracle to the test-outcome level once a no-service way to drive a *satisfied* `eventually` exists.

## Implementation Plan

This is a specification + test task, not an engine change:

1. Write `docs/concepts/` + `docs/temporal.md` semantics section stating the LTL₃/LTL_f mapping and the per-cause interpretation (the tables above).
2. Build a **verdict-derivation oracle** — a small reference function from (property kind, prefix, termination cause) → {⊤,?,⊥} — and a golden test matrix covering each operator × each cause × each boundary case.
3. **Diff the oracle against shipped behavior.** Each disagreement is triaged: doc clarification (oracle matches intent, docs were vague) or bug (engine deviates) → fix with the regression test from step 2.
4. Land the docs-table correction (Finding A), the per-property `always` fix (Finding B), and the vacuity warning behind the same release; update the RFC-041 verdict table in the docs and on the site.

**Status:** steps 1–4 implemented on `epic/d4-verdict-semantics` (`TestVerdictOracle_FinalizeMatrix` is the step-2/3 oracle; it found no divergence beyond Finding B). The docs/site verdict-table sync (the site mirror) is the remaining piece.

## References

A. Bauer, M. Leucker, C. Schinz, "Runtime Verification for LTL and TLTL," *ACM TOSEM* 20(4), 2011 — LTL₃ / RV-LTL three-valued semantics. · G. De Giacomo, M. Vardi, "Linear Temporal Logic and Linear Dynamic Logic on Finite Traces," *IJCAI* 2013 — LTL_f. · A. Pnueli, "The Temporal Logic of Programs," *FOCS* 1977 — LTL. · R. Koymans, "Specifying Real-Time Properties with Metric Temporal Logic," *Real-Time Systems* 2(4), 1990 — MTL. · O. Maler, D. Nickovic, "Monitoring Temporal Properties of Continuous Signals," *FORMATS* 2004 — STL. · M. Leucker, C. Schallhart, "A Brief Account of Runtime Verification," *JLAP* 78(5), 2009 — survey.
