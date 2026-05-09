# Determinism in Faultbox

This document is the practical companion to [RFC-040 (Determinism Levels)](rfcs/0040-determinism-levels.md). RFC-040 defines the contract; this doc gives the per-level author manifests — *do this / avoid this* guidance for SUT and spec authors so the level you're operating at delivers the value it promises.

For the post-L1 implementation roadmap (Path B / Path C / L4 Hermetic / L5 instruction-boundary research), see [RFC-046 (Beyond L1)](rfcs/0046-beyond-l1-roadmap.md).

## Vocabulary at a glance

| Level | Name | Promise |
|-------|------|---------|
| **L0** | Plan determinism | Same spec + seed → same set of tests + same plan tree |
| **L1** | Mediated-event determinism | Same plan-node + seed → causal-precedence-equivalent log of *Faultbox-mediated* events |
| **L2** | Total-event determinism | Same plan-node + seed → same *full* event log, including non-mediated I/O |
| **L3** | Replay-equivalence | Same bundle, replayed, produces byte-identical SUT output artifacts |
| **L4** | Hermetic determinism | L3 + every I/O the SUT performs is explicitly declared |
| **L5** | Antithesis-class | L4 + instruction-boundary scheduling determinism + VM snapshots |

Faultbox v0.13.0 makes **L1 a contract**; L0 is shipping; L2–L5 are roadmap (see RFC-046).

## The manifests

The manifests are intentionally **non-binding**. None of these are enforced at spec load — they're idioms that maximize how much value Faultbox extracts at the level you're operating at. Following the L1 manifest under L0 still helps; following the L2 manifest under L1 prepares you for the gVisor migration when it lands.

---

### L0 — Plan determinism manifest (spec author)

**Do:**
- Use `seed = N` on every `fault_matrix()`, `random()`, and parameterized scenario. Seed is the single source of plan reproducibility.
- Keep spec-level computation pure: `param("retries", choices=[0, 1, 3])` instead of `param("retries", choices=compute_choices_from_env())`.
- Name tests deterministically — derive from parameters, not from `time.now()` or process IDs.

**Avoid:**
- Reading environment variables that vary across runs (`os.environ["BUILD_ID"]`) inside spec body. If you need them, capture them once at spec load and treat them as constants.
- Generating service names from random sources without a seeded RNG.
- Conditional spec branches based on host details (`if platform == "darwin"`) — fine for portability, but means the plan tree differs across machines, which is L0 violation in practice.

**Why it matters at L0:** plan determinism is what makes `faultbox plan` (RFC-042) useful as a coverage estimator and what makes CI-shared trace bundles meaningful. Without it, the same spec produces different test sets on different machines and the rest of the determinism stack collapses.

---

### L1 — Mediated-event determinism manifest (SUT and spec author)

This is the level most customers operate at today. The manifest is the longest because it's where investment pays off most.

**SUT author — do:**
- **Declare every external dependency** as a `service()` + `interface()` in the spec. If your code talks to it, Faultbox needs to know.
- **Funnel I/O through testable seams.** A single `httpClient` injected into your service is mediable; ten ad-hoc `http.Get` calls scattered across packages are a leak per call.
- **Make timeouts injectable.** A 30-second context deadline that comes from configuration is testable; one hardcoded `time.After(30 * time.Second)` is a flake source.
- **Make retry jitter disable-able.** Random backoff is great in production and a determinism leak in tests. A `Retrier` with a `Jitter func() time.Duration` field that tests can override is the pattern.
- **Use one DNS resolver path.** If your binary uses `net.Resolver` consistently, Faultbox's DNS mediation works. If half your code uses `cgo` resolution and half uses pure-Go, you have two unmediated leak surfaces.
- **Idempotent retries.** L1 replay reproduces *injected* faults, not necessarily SUT side-effects. Idempotent operations (PUT, INSERT ... ON CONFLICT, content-addressable writes) survive replay drift.

**SUT author — avoid:**
- Background goroutines that perform I/O at startup ("phone home for telemetry", "fetch dynamic config every 60s"). These race with the test body and aren't on the spec's mediation list. If unavoidable, gate them behind an env var the test sets to off, or declare `nondeterministic_ok=["network-unmediated"]` and accept the noise.
- Reading `/dev/urandom` or calling `crypto/rand.Read` outside well-defined boundaries (request ID generation is fine; using it in business logic that affects observable output is not).
- Logging wall-clock timestamps in a format the test asserts against. If your test does `assert.Contains(log, "2026-")`, that's coupling the test to wall-clock; use relative timestamps or duration-from-start instead.
- Goroutines that `time.Sleep()` for synchronization. Use channels, contexts, or explicit synchronization primitives. Sleep is a flake under any wall-clock-bound determinism level.

**Spec author — do:**
- Call `determinism("L1", strict=True)` at the top of CI specs. Documents the promise and turns warnings into failures. Local-dev specs can leave `strict=False` (or omit) for forgiving iteration.
- Use `nondeterministic_ok = [...]` per-service for known-drift services; use the spec-level `allow = [...]` only for drift that genuinely affects every service in the stack.
- Prefer `eventually(p)` (with the test's `timeout=`) over `time.sleep()` in tests. The latter is wall-clock; the former is a property check that's level-aware (and meaningful at L3).

**Spec author — avoid:**
- Asserting on absolute event timestamps (`event.timestamp == 1730000000`). Assert on ordering, durations, or relative properties.
- Using parallel composition (`wait_all`, `wait_first`) without considering whether the parallel branches mediate the same set of operations. Mismatched mediation across branches makes interleaving non-reproducible.

**Why it matters at L1:** L1 is what your CI gate runs against. Every leak — unmediated background I/O, wall-clock timeout, unseeded RNG — is a flake that future-you will spend a Friday afternoon debugging. The manifest is what makes "Faultbox tests are flaky" never a true sentence.

---

### L2 — Total-event determinism manifest (gVisor opt-in services)

> Roadmap level; see [RFC-046](rfcs/0046-beyond-l1-roadmap.md). Most of the L1 manifest still applies. L2 lifts a few specific constraints because the runtime mediates more, but it adds new ones because customers now expect *total* reproducibility.

**Newly relaxed under L2:**
- Background I/O is fine — Sentry sees it. Telemetry pings, periodic config refreshes, any network activity is mediated and ordered.
- DNS resolution path no longer matters — Sentry intercepts all of it.
- File ops are mediated — no need to funnel logging through a single sink.

**New L2 constraints:**
- **Make clocks injectable.** Even with virtual clock available, code that calls `time.Now()` directly is harder to test deterministically than code that takes a `Clock` interface. The latter lets the test inject a clock that advances on demand.
- **Avoid filesystem ordering assumptions.** `os.ReadDir` order is platform-dependent on the host but Sentry's order is its own. Sort explicitly.
- **Don't rely on goroutine startup order.** Even at L2, goroutine scheduling inside the SUT is *not* deterministic. Code that depends on "goroutine A starts before goroutine B" is broken at every level.
- **Avoid CGO calls that bypass Sentry.** Pure-Go is L2-friendly; cgo is a leak unless gVisor explicitly handles the syscalls the C code makes.
- **Cap log volume.** L2 captures everything; a service that logs 100MB/test will produce 100MB bundles. Use log levels.

**Why it matters at L2:** L2 is what enterprises pay for — "rerun this bundle and prove it reproduces." A SUT that follows the L2 manifest stays reproducible across machines, kernels, and Faultbox versions. A SUT that ignores it can be at L2 in theory and L1 in practice.

---

### L3 — Replay-equivalence manifest (syscall-boundary target)

> Roadmap level; see [RFC-046](rfcs/0046-beyond-l1-roadmap.md). Inherits L2. Adds the "byte-identical SUT outputs" requirements:

- **All time funneled through an injectable clock.** No `time.Now()` anywhere observable.
- **All randomness funneled through an injectable RNG.** No `crypto/rand.Read` outside a single seam the runtime can hook.
- **No environmental data in observable output** — don't log `os.Hostname()`, `os.Getpid()`, `runtime.NumCPU()` unless they're values the bundle captures and the runtime injects on replay.
- **Stable serialization.** JSON map iteration order is not stable in Go; use sorted-key encoders for any output the test inspects byte-wise.
- **No cgo.** Period.

**Why it matters at L3:** at this level, the bundle hash is the contract. A replay that produces the same syscall log but different SUT outputs is a customer-visible bug.

---

### L4 — Hermetic determinism manifest

> Roadmap level; see [RFC-046](rfcs/0046-beyond-l1-roadmap.md) for architecture. Inherits L3. Adds the "every I/O is named" contract.

**SUT author — do:**
- **Make every I/O target configurable from spec.** Hardcoded endpoints are L4-violations even if the test happens to allow them. Configuration injected via env or flags is the only sustainable pattern.
- **Provide a "test mode" that disables auto-update / telemetry / phone-home.** L4 cannot run on a binary that calls `https://updates.example.com/check` at startup. If the binary is yours, gate it behind `FAULTBOX_TEST=1`. If it's third-party, document the env vars to set or pick a different determinism level.
- **Document known I/O surfaces in the SUT repo.** A README section listing every external endpoint, file path, and syscall pattern gives the LLM (or human) a starting point for the allow-list.
- **Treat every new I/O call site as a public API change.** It's an addition to the spec's allow-list. Code review should call it out.

**SUT author — avoid:**
- Static-init code that performs I/O before `main()`. The spec hasn't run yet; deny-default will fail before the test starts.
- Goroutines that aren't bound to a `context.Context` you can cancel. L4 forbids `reuse=True`, so every test cleanly tears these down — only works if they're context-aware.
- Hidden caches (in-memory or on-disk) that mask non-determinism. With `reuse=False`, you can't rely on a cache being warm; every test is cold-start.

**Spec author — do:**
- Use `determinism = "L4"` on the `service()`. Forces deny-default + `reuse=False`.
- **Run discovery passes.** First spec ships with a permissive allow-list; iterate based on `permission_denied` events. The LLM-first workflow (RFC-043, v0.14.0) is what makes this fast; without it, L4 authoring is heavy.
- Use `permit_syscall(name=..., reason=...)` for system-level escapes (e.g. `clock_gettime` if the SUT genuinely needs it). The `reason=` is for the audit trail, not enforcement.
- Treat the final allow-list as a **deliverable**. It's the I/O surface documentation of the SUT, and it lives in version control.

**Spec author — avoid:**
- Catch-all permits (`permit_syscall("*")`, `permit_network("*")`). Defeats the purpose. If you find yourself needing one, drop to L3.
- Permitting things you haven't audited. The temptation is "make the test pass quickly." That converts L4 into L3-with-extra-friction.
- **`remote=` services (RFC-036).** External services Faultbox doesn't supervise are rejected at L4 spec load. Rationale: a `remote=` target is a real running process — Faultbox can proxy its data path but cannot install seccomp filters, cannot enforce deny-default, cannot guarantee `permission_denied` event coverage for any I/O the remote performs internally. The "every I/O is explicitly declared" contract holds only for services Faultbox launches and supervises.
- `reuse=True` (already rejected at L4 spec load — listed here for symmetry; same rationale applies).

**Why it matters at L4:**
- **InfoSec primary persona.** "What does this binary actually communicate with?" — the answer is the spec, not a hope. Whitehat investigators can run uncooperative software at L4 and produce a full I/O surface report deterministically.
- **Compliance.** Regulatory environments demanding audit trails of every external dependency get exactly that as a side-effect of L4 testing.
- **Reproducibility ceiling.** Short of L5 (instruction-boundary), L4 is the strongest possible reproducibility — and unlike L5, it's achievable without bespoke runtimes.

---

### L5 — Antithesis-class (out of scope)

> Research direction; see [RFC-046 §L5](rfcs/0046-beyond-l1-roadmap.md). If/when this lands (Hermit-class instruction-boundary determinism + snapshots, post-v2.0), the manifest extends with: minimize cross-thread shared mutable state, prefer single-threaded actor-style designs, avoid spinlocks (deterministic preemption interacts poorly with them). Documented when relevant; not actionable today.

---

## Worked examples

> *TODO (v0.13.0 doc work):* before/after Go snippets per L1 manifest item — the unmediated-I/O leak fix, the injectable clock pattern, the disable-able retry jitter, the idempotent-write idiom, the request-ID-vs-business-RNG split. The tutorial chapter (`docs/tutorial/`) walks one concrete SUT through these end-to-end with strict-mode catching the regressions.

## See also

- [RFC-040 — Determinism Levels (vocabulary + v0.13.0 contract)](rfcs/0040-determinism-levels.md)
- [RFC-046 — Beyond L1: gVisor roadmap & L5 research](rfcs/0046-beyond-l1-roadmap.md)
- [RFC-041 — Temporal Properties & Monitors](rfcs/0041-temporal-properties.md)
- [RFC-042 — Exploration Plan & Coverage Engine](rfcs/0042-exploration-plan.md)
- [Spec language reference](spec-language.md)
