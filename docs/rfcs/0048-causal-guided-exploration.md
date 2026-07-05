# RFC-048: Causal-Guided Exploration — DPOR + LDFI on the Mediated Boundary

> **Status: Draft.** Epic RFC. Commits Directions 1 (DPOR) and 2 (LDFI) from [RFC-047](https://github.com/faultbox/Faultbox/issues/132) (Theoretical Foundations) as a single coupled epic. Builds on RFC-041 (Temporal Properties — the causal/`happens_before` API), RFC-042 (Exploration Plan — the leaf/plan-tree engine and the reserved `interleavings="dpor"` stub), RFC-014 (hold/release scheduler — the actuator), and RFC-034 (traffic observability — the lineage source). Relates to RFC-046 (Beyond L1): the soundness boundary defined here is exactly the line a Path-C runtime would push downward.
>
> In-tree document: `docs/rfcs/0048-causal-guided-exploration.md`.

## Summary

Faultbox's exploration space has two axes: **which order** mediated events occur in (interleavings) and **which faults** are injected (the matrix). Today both are explored badly — `parallel(interleavings=)` ships launch-ordering only, and `fault_matrix` is a blind Cartesian product whose only coverage metric is dependency-edges-faulted. This epic reduces *both* axes on a *shared causal substrate*, and reports each with the *same explicitly-scoped guarantee*:

- **Direction 1 — DPOR** reduces the interleaving axis: explore one representative per Mazurkiewicz-equivalence class of mediated events, using the `happens_before` order Faultbox already emits as the independence relation, and the RFC-014 hold/release scheduler as the actuator.
- **Direction 2 — LDFI** reduces the fault axis: derive the causal lineage of a *successful* outcome, compute the minimal fault sets that could break every observed derivation (hitting set), and inject only those — carrying a coverage statement instead of a cell count.

The two directions are coupled, not bolted together: they consume one causal-graph extraction library (Phase 0), and they emit one **coverage statement** vocabulary. DPOR is "the order axis," LDFI is "the fault axis"; together they turn the matrix into a state-space search with a stated — and honestly bounded — guarantee.

**The load-bearing design decision in this RFC is the soundness boundary** (next section). Faultbox does not control goroutine/thread scheduling at L1, so neither direction may claim absolute soundness. Both make completeness claims *relative to the mediated boundary*, using *conservative* relations so the claim is never "covered when it wasn't." That honesty is the guarantee story, not a hole in it.

## The soundness boundary (read this first)

Every claim in this RFC is scoped to **the mediated boundary**: the set of events Faultbox observes and can control — intercepted syscalls (seccomp user-notify) and proxied protocol messages. Between two consecutive mediated events on a thread, the SUT runs arbitrary **uncontrolled interior**: goroutine creation, channel ops, shared-memory access, lock acquisition, and Go-runtime / OS scheduling. Faultbox neither observes nor controls that interior at L1.

This has a precise consequence for each direction, and a single governing principle.

**Governing principle — conservative relations.** A reduction is only sound if the relation it prunes on is *correct*. Faultbox derives its relations (independence for DPOR, lineage for LDFI) from mediated events only, so it cannot see in-interior dependencies. We therefore make the relations **conservative**: declare two events independent / a fault irrelevant *only with positive evidence* (different processes → true isolation; provably disjoint external resources → different DB rows by key, different files/keys). Absent evidence, treat as dependent / relevant. Conservatism costs executions (less pruning) and **buys soundness at the boundary**: the failure mode is "explored more than necessary," never "claimed covered when it wasn't."

**What Faultbox claims, and what it explicitly does not:**

| | Claimed (scoped) | **Not** claimed |
|---|---|---|
| **DPOR** | Complete w.r.t. orderings of *mediated* events under a conservative independence relation. | Complete w.r.t. intra-process goroutine/thread interleavings. A race whose two accesses have **no mediated event between them** is invisible and unexplored. |
| **LDFI** | No fault set of size ≤ k *from the modeled fault space*, over the *observed* lineage, breaks the success outcome. | No fault outside the model; no unobserved derivation. |

**Why the scoped claim is still the one customers want.** The races Faultbox is bought to find are overwhelmingly **external-state** races — check-then-act on a DB row, lost update on a cache key, double-process of a queue message, an `UPDATE` racing a concurrent `SELECT`. The shared state lives *behind a mediated interface*, so the racing accesses *are* mediated events and **do** cross the boundary. The customer's confirmed accept-race (in-memory status check + unconditional `UPDATE`, mediated DB events between) is exactly this class. Pure in-memory data races are `go test -race`'s job; Faultbox should not claim them — and saying so plainly is more credible to a sophisticated buyer than an absolute claim they can falsify in five minutes.

**The boundary is also the roadmap.** The line "below which Faultbox cannot see" is precisely what a Path-C deterministic-scheduling runtime (RFC-046) would push downward. The L1 scoped guarantee is honest *today* and *motivates* the L5 north star: to extend completeness below the mediated boundary, you buy the deterministic runtime. The boundary is a product axis, not an embarrassment.

## Motivation

Three forces make this the right next epic:

1. **It closes the two gaps the field evaluation surfaced.** The truck-api eval confirmed both a request-handling race that requires a specific interleaving (DPOR's target) and a fault-matrix that explodes with a coverage metric that means almost nothing (LDFI's target). This epic is the direct, principled answer to both.
2. **The substrate already exists; the algorithms don't.** DPOR's hardest input — the independence relation — is the `happens_before` / `concurrent_with` order FB emits (RFC-041). LDFI's input — the causal derivation of a success — is the same DAG plus RFC-034 traffic observability. The actuator DPOR needs to *drive* a schedule is the RFC-014 hold/release scheduler. We are implementing algorithms over data we already produce, not building new runtime.
3. **The guarantee is the moat.** "No fault combination of size ≤ k breaks this, and every mediated interleaving was explored" is a different *category* of claim than "we ran 137 cells." It is the defensible differentiation versus blind chaos engineering — and, scoped honestly, it is one Faultbox can actually stand behind.

## Direction 1 — DPOR on the mediated-event engine

**Entry point.** RFC-042 rc2 already reserved `parallel(interleavings="dpor")` as an explicit "future release" error. This epic implements it.

**Independence relation.** Two mediated events are independent iff conservative evidence says they cannot interfere: distinct processes/services (address-space isolation), or operations on disjoint external resources (different DB row keys, cache keys, file paths, queue partitions) as resolved from the proxied protocol payload. All other same-process pairs are dependent. This is derived from the RFC-041 vector clocks + correlation IDs, intersected with the resource-key extraction the protocol plugins already do for fault matching.

**Actuator.** The hold/release scheduler (RFC-014) pauses a thread at the seccomp-notify stop point of a mediated event and releases in a chosen order. DPOR decides the schedule; hold/release enforces it. Control exists *only* at mediated points — which is exactly why the completeness claim is boundary-scoped.

**Algorithm.** Optimal DPOR (Abdulla et al., POPL 2014 — source sets / wakeup trees) over the conservative independence relation, paired with **iterative context bounding** (CHESS; Musuvathi & Qadeer, PLDI 2007) so the search prioritizes few-preemption schedules (empirically where bugs cluster) and degrades gracefully under a cost bound rather than exploding.

**Prototype & validation (customer-driven).** The evaluation team has offered to co-drive this on their confirmed accept-race with a running environment. Success = DPOR-driven search finds the violating interleaving within a bounded, conservative exploration and reports the leaf — where today's launch-ordering misses it. **This prototype also empirically answers the central open question:** does sound boundary-scoped DPOR hold for real external-state races at L1 (expected yes), and how much does conservatism inflate the search (the practicality question).

## Direction 2 — Lineage-driven fault injection (LDFI)

**Lineage.** Take the `happens_before` DAG of mediated events leading to an asserted **success** event (a 2xx, a committed write, a confirmed message). That DAG — which upstream calls, DB writes, and cache reads the success depended on — is the lineage. It comes from the RFC-041 causal API + RFC-034 observability; no new capture.

**Hitting set.** Compute, via MaxSAT / minimal-hitting-set, the minimal sets of injectable faults that break *every* observed derivation of the success. Inject only those. New component: the solver (well-understood) plus the boundary-conservative fault-relevance relation (when unsure whether a fault could affect a derivation, include it — explore more).

**Iterative loop (the honest version).** LDFI's power comes from reasoning over *alternative* derivations (redundancy = fault tolerance), but Faultbox observes one run at a time. So LDFI here is a fixpoint loop: derive lineage → hitting set → inject the minimal sets → observe whether success still derives (possibly via a redundant path FB hadn't seen) → augment the lineage → recompute → repeat until no new derivation appears. The coverage statement is over the *accumulated, observed* lineage space — scoped, not absolute, per the boundary section.

**Surface.** A `plan` mode (e.g. `faultbox plan --ldfi --success <probe> --max-k N`) that replaces the Cartesian matrix with the hitting-set-derived suite, emitting the coverage statement. The resulting suite should be *smaller* than the current matrix while carrying the "no k-set breaks it" statement — and still surface genuine degradations (the slow-DB read endpoint).

## Shared substrate & unified report

**Phase 0 — causal-graph extraction (landed first).** One library that turns the event log into a causal DAG with the conservative independence/relevance relations, plus a formal definition of the mediated boundary and the disclaimed-nondeterminism set. Both directions consume it. This answers RFC-047 OQ5 (share it) and is the dependency for everything below.

**Unified coverage statement.** Both directions emit one report artifact in the bundle, with identical scoping language:
- DPOR → "explored *M* interleaving classes of *N* mediated events; complete at the mediated boundary under conservative independence."
- LDFI → "no fault set of size ≤ *k* from the modeled space breaks *success*, over the observed lineage."

Each statement names what it does **not** cover (goroutine/thread interleavings; unmodeled faults / unobserved derivations) — the disclaimer is part of the artifact, not buried in docs.

## Phasing

**Predecessors — this epic is third in line.** The agreed sequencing is **Direction 4 → Direction 3 → this epic (1 & 2)**. Both predecessors are load-bearing here, for different reasons:

- **Direction 4 (verdict semantics) — gates the *claims*.** Both directions emit a PASS/FAIL/**INCONCLUSIVE** verdict: DPOR reports "violation found," LDFI reasons over a "success outcome." That verdict logic is hand-rolled today (RFC-041). Two reduction algorithms claiming guarantees on top of an ad-hoc verdict table is unsound by construction, so Direction 4 — pin the verdict semantics to LTL₃ / LTL_f and re-derive the table — must land first. It is a ~2-week specification task, cheap and off the algorithm critical path, but no coverage statement is trustworthy until it is done.
- **Direction 3 (gray/metastable vocabulary) — enriches LDFI's fault space.** Because the gray/metastable primitives (`degrade(...)`, `transient(...)`-then-check-recovery) land *before* this epic, LDFI's modeled fault space includes them from the start — the hitting set ranges over "degrade" and "transient-trigger" faults, not just `deny`/`delay`/`drop`. This turns what would have been a later extension into a day-one design input (see Open Questions).

- **Phase 0 — shared causal-graph extraction + boundary definition.** Conservative independence/relevance from VCs + correlation IDs + protocol resource keys. Co-requisite: the Direction-4 verdict-semantics fix above. (RFC-047 OQ5.)
- **Phase 1 — DPOR** on the customer's accept-race, co-developed. Implements `interleavings="dpor"` + context bounding. Empirically validates boundary-scoped soundness and conservatism cost. (RFC-047 OQ1.)
- **Phase 2 — LDFI** on a write-path success lineage: solver + iterative loop + `--ldfi` surface + coverage statement. (RFC-047 OQ2.)
- **Phase 3 — unified coverage report** + scoping language across both, wired into the HTML report and `plan.json`.

Each phase ships independently behind its own flag; Phase 0 unblocks 1 and 2 in parallel.

## Impact

- **Breaking changes:** none. `interleavings="dpor"` activates a currently-reserved error value; `--ldfi` is additive; the Cartesian `fault_matrix` stays the default.
- **Performance:** both directions *reduce* executions versus naïve exhaustive/Cartesian — that is the point. Conservatism trades some of that reduction back for a sound boundary-scoped claim; the solver and independence bookkeeping are small relative to the SUT runs they eliminate.
- **Positioning:** delivers the "stated guarantees" half of the Antithesis-class-but-open-source framing, on the current L1 substrate, with claims scoped so they survive contact with a skeptical buyer.

## Open Questions

1. **Conservatism cost (DPOR).** How much does the conservative independence relation inflate the search on a real service, and is iterative context bounding enough to keep it tractable? (Phase 1 measures this.)
2. **Lineage fidelity (LDFI).** How meaningful is the coverage statement when lineage is observed at the mediated boundary rather than derived from program semantics? Is the iterative loop's fixpoint reached in practice, or does it chase derivations indefinitely on a redundant system? (Phase 2 measures this.)
3. **Resource-key precision.** Independence/relevance both lean on extracting the external resource a mediated event touches (DB row, cache key) from the protocol payload. How precise is this per plugin, and what is the failure mode when it's coarse (it must fail *toward* dependence/relevance — explore more)?
4. **Gray/metastable fault space (LDFI).** Direction 3 lands before this epic, so its `degrade`/`transient` primitives are in LDFI's modeled fault space from the start. Open: does the hitting-set formulation treat a `transient`-trigger fault (whose effect is a temporal recovery property, not a single broken derivation) the same as a point fault, or does it need a distinct "did the system fail to recover" success predicate?

## References

Condensed from RFC-047 (full citations there). DPOR: Flanagan & Godefroid (POPL 2005); Abdulla et al. (POPL 2014); Musuvathi & Qadeer (PLDI 2007, CHESS); Mazurkiewicz (1986). LDFI: Alvaro, Rosen & Hellerstein (SIGMOD 2015); Alvaro et al. (SoCC 2016). Causal foundations: Lamport (1978); Mattern (1989).
