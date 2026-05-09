# RFC-040: Determinism Levels

> **Status: Draft.** Foundational RFC for the v0.13.0 epic. Defines the determinism vocabulary that RFC-041 (Temporal Properties) and RFC-042 (Exploration Plan) build on. Implementation scope for v0.13.0 is limited to Section 8 — *L1 tightening*. The post-L1 roadmap (gVisor Path B/C, L4 Hermetic, L5 instruction-boundary research) is split out to **[RFC-046](0046-beyond-l1-roadmap.md)**; per-level author manifests live in **[docs/determinism.md](../determinism.md)**.

## Summary

Faultbox makes claims about reproducibility ("same spec + seed → same outcome"), but the current product has no formal definition of *what* is reproducible, *under which conditions*, and *what the user is allowed to assume*. This RFC introduces a six-level taxonomy of determinism (L0–L5), places Faultbox v0.12.x on that taxonomy honestly, declares the v1.0 promise (L1 with explicit escape hatches), and commits v0.13.0 to making L1 a contract. The longer-arc roadmap (Path B / Path C / L4 hermetic / post-v2.0 instruction-boundary work) is documented in [RFC-046](0046-beyond-l1-roadmap.md) so this RFC stays focused on the v0.13.0 commitment.

The framing is the load-bearing artifact. RFC-041 (`eventually(p)` + `test(timeout=)`) and RFC-042 (`faultbox plan`) only make sense once we can name the level a given test is operating at. Customer-facing docs need the same vocabulary so we don't promise reproducibility we can't deliver.

In-tree document: `docs/rfcs/0040-determinism-levels.md`.

## Motivation

### What problem does this solve?

Three problems compound today:

1. **The product silently misses non-determinism.** When a service-under-test does I/O outside what Faultbox proxies (background HTTP for telemetry, raw `getrandom`, `gettimeofday`, DNS resolution against an unmediated resolver), Faultbox sees nothing. Tests pass deterministically *as far as the event log knows*, but the SUT's actual behavior may have varied run-to-run. Customers reporting "this test is flaky" sometimes have a real bug, sometimes have an unmediated I/O path; we have no vocabulary to distinguish them.

2. **Replay claims are stronger than implementation supports.** The `.fb` bundle and `faultbox replay` are presented as a deterministic-rerun mechanism (RFC-025). For mediated operations, that holds. For everything else — clock-dependent timeouts, RNG-driven retry jitter, goroutine scheduling inside the SUT — replay is best-effort and customers learn the gap by being burned.

3. **Downstream RFCs cannot land cleanly without it.** RFC-041's `test(timeout=duration)` means "wall-clock duration" at L1, "virtual-clock duration" at L3. The same primitive expresses different guarantees depending on level. Without a determinism vocabulary in place first, the temporal RFC has to invent its own framing inline, and the exploration RFC has to hand-wave about whether interleavings are reproducible.

### Why is this important now?

- **v0.13.0 is the determinism release.** Customer feedback (Notion: 2026-04-22 customer-feedback-analysis, Group A: "Determinism is the difference between PoC and production quality gate") put determinism at the top of the post-bundle backlog.
- **InfoSec primary persona pivot** (planned for v0.14.0, see roadmap memory) needs determinism vocabulary in place: when a whitehat investigator asks "did my fault reproduce the bug, or did the SUT race differently this time?", the answer has to be a level, not a shrug.
- **Antithesis comparison framing.** Customers ask "are you Antithesis?" The honest answer is a determinism-level comparison ("Antithesis at L3-instruction; Faultbox at L1, with L2 on the gVisor roadmap"), not marketing.

### What happens if we don't do this?

- The temporal-properties RFC ships with implicit, unstated determinism assumptions and inherits the same flakiness debt.
- "Faultbox tests are flaky sometimes" stays an unstructured complaint with no path to triage.
- The gVisor adoption decision (Path B / Path C, see [reference: gVisor Strategy Notion doc](#references)) has no level vocabulary to anchor against, making the cost-benefit case ad-hoc.

## Current state

Today Faultbox provides:

- **Deterministic plan generation.** `fault_matrix()` and friends produce the same set of test cases for a given spec + seed. Verified by tests in `internal/star/`.
- **Mediated-event interception.** Operations declared in the spec (HTTP/gRPC/Kafka/Redis/TCP/raw syscalls listed in seccomp filter) are intercepted, ordered through the engine, and recorded to the event log. Within that mediated set, ordering is deterministic for a given seed (RFC-024 proxy datapath, RFC-022 seccomp acquisition).
- **No first-class clock or RNG control.** SUT calls to `clock_gettime`, `getrandom`, `time.Now()` go through to the kernel. We don't intercept VDSO calls; seccomp-notify fundamentally cannot.
- **No first-class detection of unmediated I/O.** If the SUT opens a raw socket to a resolver Faultbox doesn't proxy, we never hear about it.
- **Bundle-driven replay** (RFC-025) reproduces the *plan* and the *injected faults*, but inherits whatever non-determinism the SUT itself contributed.

In short: we are at **L1 partial** in the taxonomy below, with no vocabulary to communicate which level we're claiming.

## Determinism levels

Six levels, each strictly stronger than the last:

| Level | Name | Promise | Mechanism |
|-------|------|---------|-----------|
| **L0** | Plan determinism | Same spec + seed → same set of tests + same plan tree | Pure-functional plan generation in `internal/star/` |
| **L1** | Mediated-event determinism | Same plan-node + seed → causal-precedence-equivalent log of *Faultbox-mediated* events (concurrent events may interleave; happens-before pairs hold their order) | seccomp-notify + protocol proxies + engine ordering + vector clocks |
| **L2** | Total-event determinism | Same plan-node + seed → same *full* event log, including non-mediated I/O (stdout, FS, time, DNS, signals) | Full syscall mediation (gVisor Sentry, Path C) |
| **L3** | Replay-equivalence | Same bundle, replayed, produces byte-identical SUT output artifacts | L2 + virtual clock + RNG funnel + hermetic FS |
| **L4** | Hermetic determinism | L3 + every I/O the SUT performs is explicitly declared; no implicit network/FS/syscall access; no container state reuse; no externally-managed services on the data path | Deny-by-default seccomp + network policy + `reuse=True` forbidden + `remote=` forbidden + iterative LLM-assisted allow-list discovery |
| **L5** | Antithesis-class | L4 + instruction-boundary scheduling determinism + VM snapshots for branching exploration | Hermit-style PMU preemption (instruction-boundary) + checkpoint/restore (snapshots); requires bespoke runtime |

L2–L5 design rationale (why the levels split where they do, the L3→L4 I/O-surface vs L4→L5 thread-scheduling distinctions, Hermit reference architecture) is documented in **[RFC-046](0046-beyond-l1-roadmap.md)**.

## Where Faultbox is today

| Level | Status | Notes |
|-------|--------|-------|
| L0 | ✅ Solid | `fault_matrix` + seed-driven generators |
| L1 | ⚠️ Partial | Mediated set is honest, but *unmediated I/O is silently missed* — no detection, no warning, no failure |
| L2 | ❌ | No full syscall mediation; `stderr()` event source is a small step in this direction |
| L3 | ❌ | No clock virtualization, no RNG funnel |
| L4 | ❌ | `reuse=True` is supported (RFC-015); no deny-default policy; no LLM rule-discovery loop |
| L5 | ❌ | No instruction-boundary scheduling; Hermit reference (Appendix A) |

The honest framing for customer docs: **"Faultbox v0.12.x provides L0 + best-effort L1; v0.13.0 makes L1 a contract."**

## v1.0 promise

**L1 with declarative escape hatches and strict-mode enforcement.**

### Determinism is a spec-level setting

Both the **level** and the **engine** are spec-level: one promise, one substrate, one choice. Three reasons:

1. **The spec-level enforcement at higher levels is structural.** L4 forbids `remote=` and `reuse=True` *anywhere in the spec* — these are spec-wide invariants, not per-service flags. L2/L3 require gVisor on the data path, which is also spec-wide.
2. **Mixed engines produce hard-to-reason-about cross-service races.** A spec where service A runs under default seccomp-notify and service B runs under gVisor has different mediation surfaces on each side of an inter-service call. The interleavings between them are not reproducible at any level. Better to refuse the configuration than to ship a determinism promise that quietly degrades.
3. **The reproducibility promise is what the customer reads in the report.** "This test suite runs at L3 on gVisor" is one sentence with one truth value. Per-service mixing would force the report to qualify each event with which engine produced it — operationally confusing for no real win.

The model is therefore: **one `determinism(...)` declaration at the top of the spec sets level + engine + strict + global allow-list. Per-service is only for escape hatches.**

If a customer has services that can't run under the engine the spec needs (e.g. Postgres breaking under gVisor), the choice is honest: lower the spec level so the default engine suffices, or replace the incompatible service with a `mock_service()` / `service(remote=...)` (the latter only at L1, since `remote=` is forbidden at L4 and gVisor doesn't supervise remote pods).

### Spec syntax

```python
# Spec-level declaration (top of file). Drives global enforcement.
determinism(
    level = "L1",
    runtime = "default",                          # implied; can be omitted at L0/L1
    strict = True,                                # strict promotes warnings → failures
    allow = [],                                   # global drift exceptions (none here)
)

service("api",
    image = "company/api:1.4.2",
    nondeterministic_ok = ["clock", "dns"],       # this service is known to drift here
)

service("worker",
    image = "company/worker:2.1.0",
    # no escape hatches — strict L1 contract holds
)

test("smoke",
    setup = ...,
    body = ...,
    # tests inherit spec-level determinism; no per-test override
)
```

**`determinism(level, runtime="default", strict=True, allow=None)` — top-level builtin:**
- `level` — one of `"L0"`, `"L1"`, `"L2"`, `"L3"`, `"L4"`. Defaults to `"L1"` if `determinism()` is not called.
- `runtime` — `"default"` (seccomp-notify) or `"gvisor"`. Must be capable of the chosen level. Defaults to `"default"`.
- `strict = True/False` — if true (the default), `unmediated_io` events fail the test instead of just warning. CLI flag `--strict-determinism=false` overrides the spec for debug runs (lets a developer iterate locally on a strict CI spec without editing it).
- `allow = [...]` — global drift exceptions applied to *every* service. Per-service `nondeterministic_ok` adds to this.

A level/runtime mismatch (e.g. `level="L2", runtime="default"`) errors at spec load with a clear message: *"L2 requires runtime='gvisor'; runtime='default' caps at L1."*

### When and how to use `nondeterministic_ok` / `allow`

These kwargs are **escape hatches for known drift the spec author has investigated and accepted** — not a way to make warnings go away.

The categories shipped in v0.13.0 are `clock`, `dns`, `rand`. Each maps to a class of unmediated I/O Faultbox can detect:

| Category | What it covers | When to acknowledge |
|----------|----------------|---------------------|
| `clock` | `clock_gettime`, `gettimeofday`, VDSO time reads | SUT logs wall-clock timestamps; SUT uses time only for monotonic durations the test doesn't assert on; SUT exposes a metrics endpoint that includes uptime |
| `dns` | DNS resolution outside Faultbox's resolver path | SUT uses cgo-based resolution Faultbox doesn't intercept; SUT talks to a host outside any declared `interface()` (e.g. `localhost` for self-checks) |
| `rand` | `getrandom`, `/dev/urandom`, `crypto/rand.Read` | SUT generates request IDs / UUIDs / nonces that don't affect business logic the test asserts on; SUT seeds an internal PRNG for retry jitter the test doesn't care about |

**Workflow for using these:**

1. Run the test with `strict=True` (the default).
2. Strict mode fails the test on the first `unmediated_io` event. The error names the category and the call site.
3. Investigate. Decide whether the drift is acceptable:
   - **Yes, it's harmless** (e.g. a request-ID UUID the test never asserts on) → add the category to `nondeterministic_ok` on that service. Now strict mode tolerates it for that service only.
   - **Yes, but it affects every service** (e.g. all your Go services use `time.Now()` for monotonic timings) → add to spec-level `allow=[...]` instead of repeating per-service.
   - **No, this drift can change test outcomes** → fix the SUT (inject a clock, funnel RNG, mock the dependency) rather than suppressing.

**Anti-patterns:**

- *"Add to `nondeterministic_ok` to make CI green"* without investigation. This is debt — the next test that's actually flaky for the same reason will be silenced too.
- *"Add `allow=['clock', 'dns', 'rand']` at spec-level to skip strict mode"*. Equivalent to `strict=False` with extra steps; just turn strict off if that's what you want.
- *"Add the category every time strict mode complains"*. Strict mode is supposed to make you decide. The decision should be in code review, not in autopilot.

**Composition:**

`determinism(allow=["clock"])` + `service("api", nondeterministic_ok=["dns"])` → service "api" tolerates `clock` and `dns`. Other services tolerate only `clock`. The lists union; they don't replace.

### What `strict` means at L0 / L1

`strict` controls the failure mode of *level-violation events*. The events that exist — and whether they're detected vs structurally prevented — differ per level. v0.13.0 ships the semantics for L0 and L1 only:

| Level | What `strict=True` controls | What `strict=False` does |
|-------|----------------------------|--------------------------|
| L0 | Nothing — no violation events exist (plan determinism is binary) | n/a |
| L1 | Promotes `unmediated_io` events to test failures | Records them as warnings in the report |

**Spec-load policy:**
- At L0: passing `strict=` is rejected with: *"`strict` has no effect at L0; remove it or change the level."* The kwarg signals an intent the level cannot honor.
- At L1: `strict` defaults to `True`. Local-dev override via CLI: `faultbox test --strict-determinism=false`. Argument: strict-by-default makes silent drift impossible to ignore; the CLI escape hatch keeps debug iteration cheap.

L2 / L3 strict-mode semantics, and the structural enforcement at L4 / L5 that makes `strict` inert, are documented in [RFC-046 §"strict-mode semantics beyond L1"](0046-beyond-l1-roadmap.md#strict-mode-semantics-beyond-l1).

### Behavior under L1

1. Faultbox attempts to mediate every operation declared in the spec (`interface()` lines, `fault()` targets, `observe()` sources).
2. When the SUT performs I/O Faultbox can detect but did not mediate (e.g. a `connect()` syscall to an unmediated address), an `unmediated_io` event is emitted to the event log.
3. With `determinism("L1", strict=True)` (CI default recommendation), `unmediated_io` events become test failures unless the offending category appears in the service's `nondeterministic_ok = [...]` list or the spec-level `allow = [...]` list.
4. Without strict mode, `unmediated_io` events are warnings — visible in the report (RFC-029) but non-fatal. Default for local development.

### What L1 promises about replay

- Same plan-node + seed → mediated events with the same **causal-precedence structure**. Vector clocks for any two mediated events `A` and `B` where `A → B` (happens-before) hold the same ordering on every replay.
- Concurrent mediated events (no happens-before relationship) **may interleave differently** across replays. This is honest: Faultbox does not control SUT-internal goroutine scheduling at L1.
- The set of mediated events is the same on every replay.

### What L1 does *not* promise

- That two runs produce byte-identical event logs or SUT logs (clocks differ → log timestamps differ; concurrent events may reorder).
- That goroutine schedules are identical (Go runtime is non-deterministic).
- That wall-clock durations are reproducible (`test(timeout=)` is wall-clock at L1, virtual-clock at L3; see RFC-041).
- That replay reproduces stdout/stderr exactly (file ops not mediated).
- That `unmediated_io` event counts are identical between runs (background activity Faultbox doesn't control).

These are honest limitations of L1 and should appear in `docs/determinism.md` adjacent to the contract.

### Runtime / level compatibility

The `runtime` kwarg on `determinism(...)` selects the substrate. v0.13.0 ships only the `default` substrate (seccomp-notify), which caps at L1. `runtime="gvisor"` is reserved syntax (parses but errors at spec load) so future migration is non-breaking. Capability matrix and Path B / Path C design live in [RFC-046](0046-beyond-l1-roadmap.md).

A `determinism(level=X, runtime=Y)` combination where `Y` is not capable of `X` is a spec-load error: *"L2 requires runtime='gvisor'; runtime='default' caps at L1."*

## L1 author manifest

Determinism levels constrain the *runtime*; they don't dictate code style. But the value a customer gets from L1 depends on how their SUT and spec are written. The "do this / avoid this" manifest for SUT and spec authors lives in **[docs/determinism.md](../determinism.md)** — written as a customer-facing doc, not an RFC, because it's idioms and worked examples rather than design.

The L1 manifest is the load-bearing artifact for v0.13.0. It is what makes "Faultbox tests are flaky" never a true sentence: every leak — unmediated background I/O, wall-clock timeout, unseeded RNG — is a flake that strict-mode will surface. The manifest catalogs the patterns; strict-mode enforces them.

L0, L2, L3, L4, L5 manifests live alongside the L1 one in `docs/determinism.md` as roadmap documentation; only the L1 one is enforced by v0.13.0 tooling.

## Beyond L1 — pointer to the roadmap

This RFC does **not** commit to L2, L3, L4, or L5 implementation. It defines the levels so the roadmap conversation has vocabulary. The implementation paths and architectural constraints (Path A/B/C, L4 Hermetic, L5 research, why `runtime` is spec-wide, why L4 is mutually exclusive with `remote=`) are documented in **[RFC-046: Beyond L1](0046-beyond-l1-roadmap.md)**.

## v0.13.0 implementation scope

The committed v0.13.0 work for this RFC:

### 8.1 — Detection: `unmediated_io` events

Audit `internal/star/` and `internal/engine/` for the I/O paths Faultbox can *observe* but does not currently *mediate*. Add an emitter that fires an `unmediated_io` event with category metadata when the SUT performs:

- Network I/O to an address not matching any declared `interface()` (category: `network-unmediated`)
- File I/O outside container-mediated paths (category: `fs-unmediated`)
- DNS resolution outside Faultbox's resolver (category: `dns`)
- `getrandom` / `/dev/urandom` reads (category: `rand`)
- `clock_gettime` / `gettimeofday` calls (category: `clock`) — best effort; VDSO unreachable

### 8.2 — Declarative escape hatches

Two layers, both implemented in `internal/star/builtins.go`:

- **Spec-level:** `determinism(level, strict=False, allow=[...])` top-level builtin. The `allow` list applies globally — drift categories listed here are tolerated for every service in the spec.
- **Per-service:** `service()` accepts `nondeterministic_ok = [...]` kwarg. Adds to (does not replace) the spec-level `allow` list for this service.

Both validate against the known category list at spec-load time. The effective allow-set for a service is the union of the spec-level `allow` and the service's `nondeterministic_ok`.

### 8.3 — Strict mode

`determinism(level, strict=True)` at the top of the spec is the source of truth. The CLI flags `faultbox test --strict-determinism[=true|false]` and `--no-strict-determinism` override the spec for the duration of one run — bidirectional, per the open-questions resolution below. Use case: iterate locally with strict off on a strict CI spec without editing it. When strict is in effect, an `unmediated_io` event whose category is not in the effective allow-set fails the test with a clear error pointing to the event in the trace.

### 8.4 — Reserved syntax: `determinism(...)` top-level builtin

For v0.13.0:

- `determinism(...)` is the single new top-level builtin. Implemented kwargs and values:
  - `level` accepts `"L0"` or `"L1"` only. `"L2"`, `"L3"`, `"L4"`, `"L5"` parse but error at spec load: *"determinism level X is reserved for a future Faultbox release; currently only L0 and L1 are supported."*
  - `runtime` accepts `"default"` only. `"gvisor"` parses but errors: *"runtime='gvisor' is reserved for a future Faultbox release."*
  - `strict`, `allow` fully implemented.
- `service()` does **not** gain a `runtime=` kwarg in v0.13.0 (or ever — runtime is a spec-level concept). It does gain `nondeterministic_ok = [...]`.

This locks the surface so v0.14.0+ migrations adding gVisor support don't break specs written today.

### 8.5 — Documentation

- New file: `docs/determinism.md` — the L0–L3 taxonomy, Faultbox's current level, the L1 contract, the escape-hatch list, examples, **and the per-level author manifests** with worked code snippets (the RFC summarizes the manifests; the doc carries the full version with before/after Go examples for each "do/avoid" item).
- Update `docs/spec-language.md` to document `determinism =`, `nondeterministic_ok =`, `runtime =`.
- Update `docs/feature-manifest.md` with the determinism rows.
- README bullet pointing to `docs/determinism.md`.
- Tutorial chapter addition (`docs/tutorial/`) — one chapter walking a concrete SUT through the L1 manifest: an unmediated-I/O leak, the warning event, the fix, and how strict-mode catches regressions. The manifest is only as useful as the worked example.

### 8.6 — Tests

Per the #84 coverage gate: every new file in `internal/proxy/` or `internal/engine/` carrying detection logic gets a sibling `*_test.go`. Goldens for `unmediated_io` events under representative scenarios (DNS leak, raw socket, clock read) in `testops/`.

## Out of scope for v0.13.0

- L2 or L3 implementation (deferred to Path B / Path C epics).
- Clock virtualization (requires gVisor or LD_PRELOAD; deferred).
- RNG funnel (cheap to add but only meaningful at L2; deferred for cohesion).
- Goroutine scheduler determinism (Hermit-class; post-v2.0 research).
- Cross-test determinism (same suite run twice → same outcomes); covered by L1 already.

## Open questions

(All open framing questions resolved on 2026-05-03; section retained as the resolution log.)

**Resolved:**
- ~~Per-test override semantics.~~ Resolved 2026-05-03: determinism is spec-level only; tests inherit, no per-test override. If different tests need different levels, split into different specs.
- ~~Per-service runtime / mixed-engine specs.~~ Resolved 2026-05-03: runtime is spec-level (declared inside `determinism(runtime=...)`), not per-service. Mixed engines are not expressible. If the customer's stack has services that can't share an engine, the choice is to lower the level so the default engine suffices, mock the incompatible services, or split into multiple specs. The honest constraint is preferable to silent degradation.
- ~~Default for strict mode at L1/L2/L3.~~ Resolved 2026-05-03: `strict=True` is the default. CLI flag `--strict-determinism=false` is the debug-iteration escape hatch. Argument: strict-by-default makes silent drift impossible to ignore; the CLI override keeps local debugging cheap without needing to edit the spec.
- ~~Granularity of `nondeterministic_ok` / `allow`.~~ Resolved 2026-05-03: categories (`clock`, `dns`, `rand`) for v0.13.0. Path-level granularity (`clock:CLOCK_MONOTONIC`, `dns:resolv.conf`) deferred until customer feedback demands it. Usage workflow documented in Section 7 ("When and how to use `nondeterministic_ok` / `allow`").
- ~~Does L1 imply event-log replay determinism?~~ Resolved 2026-05-03: **L1 promises causal-precedence equivalence for mediated events, not byte-identical event logs.** Two replays of the same bundle at L1 produce event logs where: (a) every mediated event present in run 1 is present in run 2; (b) for any pair of mediated events `(A, B)` where vector clocks show `A → B` (A happens-before B), the same ordering holds in both runs; (c) concurrent mediated events (no happens-before) may interleave differently — this is honest because Faultbox does not control SUT-internal goroutine scheduling at L1. `unmediated_io` event counts may also diverge between runs and are not part of the L1 replay contract.
- ~~Naming.~~ Resolved 2026-05-03: use both. Numeric labels (`L0`–`L5`) remain the internal vocabulary in API, error messages, and engineering docs. Descriptive names (`Mediated-event determinism`, `Total-event determinism`, `Replay-equivalence`, `Hermetic determinism`, `Antithesis-class`) appear in customer-facing docs, report headers, and marketing material. Always paired: e.g. "L1 — Mediated-event determinism" in `docs/determinism.md`, "L4 (Hermetic)" in report banners.

## Implementation plan

Single-rc cadence — no real users yet (2026-05-08), so we don't ramp through a "warnings now, enforced later" cycle. PR sequence on `epic/v0.13.0-determinism`:

| PR | Scope | Status |
|----|-------|--------|
| 1 | `determinism()` top-level builtin + `service(nondeterministic_ok=...)` kwarg + reserved-syntax gating | ✅ landed |
| 2 | `unmediated_io` detection (clock / rand / dns / network-unmediated) gated on already-faulted services | ✅ landed |
| 3 | strict-mode failure in `RunTest` + `strict_determinism_violation` outcome label | ✅ landed |
| 4 | `--strict-determinism[=true|false]` / `--no-strict-determinism` CLI flag | ✅ landed |
| 5 | Docs (`docs/determinism.md`, spec-language, feature-manifest, tutorial chapter) + commit RFC drafts + CHANGELOG | this PR |

End-to-end goldens (DNS leak, raw socket, clock read) are deferred to a Linux-side follow-up — they require seccomp + a real binary, not seedable from macOS host.

Beyond v0.13.0 (Path B, Path C, L4 Hermetic mode, L5 instruction-boundary research) — see [RFC-046](0046-beyond-l1-roadmap.md).

## Dependencies

- **Unlocks:** RFC-041 (Temporal Properties — `eventually(p)` semantics + `test(timeout=)` clock interpretation depend on level), RFC-042 (Exploration Plan — interleaving reproducibility claims depend on level).
- **No incoming RFC dependencies.** Builds on existing `internal/star/`, `internal/engine/`, RFC-024 (proxy datapath), RFC-025 (`.fb` bundle).
- **Roadmap continuation:** [RFC-046 (Beyond L1)](0046-beyond-l1-roadmap.md) carries the post-v0.13.0 roadmap — gVisor Path B/C, L4 Hermetic mode, L5 instruction-boundary research. RFC-040 keeps the vocabulary; RFC-046 fills in the substrate.
- **Related:** RFC-037 (#94, Determinism for Remote Services) explores how to restore reproducibility for `remote=` services via record-and-replay or contract pinning. Whatever RFC-037 ships caps at **L3** — record-and-replay can produce byte-equivalent runs but cannot enforce deny-default I/O on a process Faultbox doesn't supervise. L4 (Hermetic) is mutually exclusive with `remote=` regardless of how RFC-037 lands; rationale documented in RFC-046.
- **Related:** RFC-036 (#91, Remote Services) introduces `remote=`; L4's rejection of `remote=` is captured in RFC-046 §L4.

## References

- RFC-024 (proxy datapath) — provides the L1 mediated-event substrate.
- RFC-025 (`.fb` bundle) — replay tooling that depends on this taxonomy to make accurate claims.
- RFC-041 (Temporal Properties — to be filed) — depends on this RFC.
- RFC-042 (Exploration Plan — to be filed) — depends on this RFC.
- [RFC-046 (Beyond L1: gVisor Roadmap & L5 Research)](0046-beyond-l1-roadmap.md) — post-v0.13.0 roadmap split out of this RFC.
- Customer feedback: 2026-04-22 customer-feedback-analysis (Notion), Group A on determinism.
