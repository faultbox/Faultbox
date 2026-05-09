# RFC-046: Beyond L1 — gVisor Runtime Roadmap & L5 Research

> **Status: Draft.** Roadmap RFC. Builds on RFC-040 (Determinism Levels) — uses the L0–L5 vocabulary defined there. **Non-binding for v0.13.0.** Captures the post-L1 implementation paths so the v0.14.0+ runtime/level decisions have a documented design baseline.

## Summary

RFC-040 defines the L0–L5 determinism taxonomy and commits v0.13.0 to making L1 a contract. This RFC sketches the implementation paths for *exceeding* L1: Path B (gVisor netstack extraction) and Path C (full Sentry fork) for L2/L3, the L4 Hermetic mode built on Path C, and the L5 instruction-boundary research direction (Hermit / Antithesis class).

None of this work is committed to a specific Faultbox release here. The split from RFC-040 is intentional: RFC-040 is the *contract* for v0.13.0; this RFC is the *map* for what comes next. Customers reading RFC-040 see the L1 promise; engineers reading this RFC see how L2 / L3 / L4 / L5 *could* be reached.

In-tree document: `docs/rfcs/0046-beyond-l1-roadmap.md`.

## Motivation

The motivation chain is the same as RFC-040 — InfoSec primary persona, Antithesis comparison framing, customer demand for stronger reproducibility — but the questions split cleanly:

- **What level do we promise today?** Answered in RFC-040 (L1).
- **How do we reach the levels we don't promise yet?** Answered here.

Without this RFC the post-L1 conversation either bloats RFC-040 (the original 511-line draft) or scatters across notion docs the engineering team can't link to from spec-load error messages. A single in-tree document is the lighter option for both audiences.

## Why the levels split where they do

The progression L3 → L4 → L5 captures three independent dimensions of "more deterministic":

- **L3 → L4 is about *I/O surface*.** L3 reproduces the bytes; L4 forces every I/O into the spec explicitly. Two SUTs at L3 can have wildly different background-I/O profiles; at L4 they cannot — every connection, file open, syscall is named. This is what makes L4 valuable for InfoSec and compliance use cases independent of replay.
- **L4 → L5 is about *thread scheduling*.** L4 still inherits L3's "goroutine scheduling inside the SUT is not controlled" caveat — two replays of an L4 bundle produce identical *external* behavior but the SUT's internal interleaving may differ. L5 closes that gap (Hermit-style PMU preemption). Plus VM snapshots, which Antithesis uses for branching state-space exploration.

The dimensions are independent enough that L4 without L3 is conceivable (deny-default at L1 — useful for surface audit even without replay). For now the level vocabulary in RFC-040 nests them strictly to keep the taxonomy linear; revisit if customer demand pulls in the cross-product.

## Path taxonomy

Three substrates can host Faultbox's mediation engine. Each caps at a different determinism level.

### Path A — seccomp-notify (today)

The current substrate. Userspace supervisor (Faultbox) subscribes to seccomp-notify events from the kernel; the SUT runs unmodified under a normal Linux kernel. RFC-022 (multi-process acquisition) and RFC-024 (proxy datapath) define the implementation.

**Caps at L1.** seccomp-notify cannot intercept VDSO calls (`clock_gettime`, `gettimeofday`), which means clock virtualization is impossible at this layer. The L1 vocabulary in RFC-040 — "mediated-event determinism" with explicit `unmediated_io` events for unobserved I/O — is precisely what Path A can honestly deliver.

### Path B — extract gVisor netstack

Pull `gvisor.dev/gvisor/pkg/tcpip` (the userspace TCP/IP stack) out of the gVisor monolith and run it as the data-path layer for protocol fault injection. The SUT still runs on the host kernel; only its network traffic crosses the netstack boundary.

**~2–4 weeks of work.** Promotes the *network* portion of L2 only — Faultbox can now drop, reorder, fragment, or corrupt packets at protocol layer 4, capabilities seccomp-notify-based proxying cannot reach. Does *not* unlock clock virtualization, RNG funnel, or filesystem mediation.

**Targeted v0.14.x or v0.15.0** as the InfoSec-primary-persona unlock — packet-level cheat investigation (custom TCP protocols, segment manipulation, RST storms mid-stream) is the canonical example that L1 cannot reach.

### Path C — fork gVisor as "Faultbox Sentry"

Fork the full gVisor Sentry (the userspace kernel) into a Faultbox-controlled binary. The SUT now runs *inside* the Sentry process; every syscall the SUT makes is interpreted by Faultbox-Sentry rather than the host kernel.

**~3–6 months of work.** Unlocks full L2 + L3-syscall-boundary: clock virtualization, RNG funnel, filesystem mediation, complete I/O reproducibility. This is the substrate the L4 Hermetic mode builds on.

**Customer-demand-driven; not on a committed roadmap line.** The commercial moat positioning — "Antithesis-class capabilities, open source" — applies if and when an enterprise customer signs to pay for the integration cost.

## Engine pluggability

When Path B and/or Path C land, the spec exposes the choice on the `determinism()` builtin:

```python
determinism(level = "L1", runtime = "default")        # default — seccomp-notify (today)

# When Path B lands:
# determinism(level = "L1", runtime = "gvisor")       # opt-in: L1 + protocol-level netfaults

# When Path C lands:
# determinism(level = "L2", runtime = "gvisor")       # required for L2+
# determinism(level = "L3", runtime = "gvisor")       # required for L3
```

### Spec-wide, not per-service

`runtime` is a **spec-level** concept. Three reasons (rationale lifted from RFC-040 §"Determinism is a spec-level setting"):

1. **The spec-level enforcement at higher levels is structural.** L4 forbids `remote=` and `reuse=True` *anywhere in the spec* — these are spec-wide invariants, not per-service flags. L2/L3 require gVisor on the data path, also spec-wide.
2. **Mixed engines produce hard-to-reason-about cross-service races.** A spec where service A runs under default seccomp-notify and service B runs under gVisor has different mediation surfaces on each side of an inter-service call. The interleavings between them are not reproducible at any level.
3. **The reproducibility promise is what the customer reads in the report.** "This test suite runs at L3 on gVisor" is one sentence with one truth value. Per-service mixing forces the report to qualify each event with which engine produced it — operationally confusing for no real win.

If a customer's stack has services that can't share an engine (e.g. Postgres breaking under gVisor), the choice is honest: lower the spec level so the default engine suffices, replace the incompatible service with a `mock_service()` / `service(remote=...)` (the latter only at L1, see L4 section below), or split into multiple specs.

### Capability matrix

| Runtime | Capable up to | Status |
|---------|---------------|--------|
| `default` (seccomp-notify) | L1 | shipping (Path A) |
| `gvisor` (Path B partial) | L1 + protocol-level network faults | reserved syntax; unimplemented |
| `gvisor` (Path C full) | L2, L3, L4 | reserved syntax; unimplemented |

A `determinism(level=X, runtime=Y)` combination where `Y` is not capable of `X` is a spec-load error: *"L2 requires runtime='gvisor'; runtime='default' caps at L1."*

RFC-040 §8.4 reserves the `runtime=` syntax in v0.13.0 (parses but errors with "available in a future Faultbox release") so the future migration is non-breaking.

## L4 — Hermetic mode

L4 layers a *policy* over an L3 substrate. Path C provides L3; L4 adds:

1. **Deny-default seccomp + network policy.** Every syscall and every connection must be on the allow-list; anything else returns `EPERM`/`ECONNREFUSED`.
2. **`reuse=False` enforced.** Container reuse (RFC-015) is rejected at spec load — every test is cold-start.
3. **`permission_denied` event emission.** Every denied operation emits a structured event with call site, target, and payload metadata.
4. **LLM rule-discovery loop.** Depends on RFC-043 (MCP bundle ops). The bundle goes to an LLM that analyzes denials and proposes additional allow rules with safety justifications.

**Targeted v0.15.x or v1.0.** InfoSec primary persona is the load-bearing use case — whitehat investigators running uncooperative software at L4 produce a full I/O surface report deterministically. Compliance environments demanding audit trails of every external dependency get exactly that as a side-effect.

### L4 is mutually exclusive with `remote=`

A `service(remote=...)` (RFC-036) target is a real running process — Faultbox can proxy its data path but cannot install seccomp filters, cannot enforce deny-default, cannot guarantee `permission_denied` event coverage for any I/O the remote performs internally. The "every I/O is explicitly declared" contract holds only for services Faultbox launches and supervises.

L4 spec load rejects `remote=` services with: *"L4 (Hermetic) requires Faultbox-supervised services; remove `remote=` or lower the level to L3."*

This constraint is deliberate and documented in RFC-040 §dependencies. RFC-037 (Determinism for Remote Services) explores how to restore reproducibility for `remote=` via record-and-replay or contract pinning. Whatever RFC-037 ships **caps at L3** — record-and-replay can produce byte-equivalent runs but cannot enforce deny-default I/O on a process Faultbox doesn't supervise.

### LLM rule-discovery — what makes L4 economically viable

Without an LLM-assisted allow-list discovery loop, L4 authoring is prohibitive for non-trivial services. The workflow:

1. Author writes initial spec with a minimal allow-list (or the LLM generates one from the SUT's docs).
2. `faultbox test --determinism=L4` runs; failures emit `permission_denied` events with full context.
3. The bundle goes to an LLM (via MCP, RFC-043, v0.14.0) that analyzes denials, proposes additional allow rules, and explains *why* each is safe.
4. Human reviews + merges; iterate to fixpoint.
5. Final spec = full audit trail of every I/O the SUT performs. The bundle is hermetic; the spec is the contract.

This is the v0.14.0 LLM-first epic compounding into v0.15.0+ determinism work. Without RFC-043 in place, L4 is a research project; with it, L4 becomes a shipped feature.

## L5 — Instruction-boundary determinism (research)

Instruction-boundary determinism is technically achievable open-source. Hermit (`facebookexperimental/hermit`, Rust, dormant) demonstrates the technique:

1. **PMU-based deterministic preemption.** Linux `perf_event_open` with `PERF_COUNT_HW_BRANCH_INSTRUCTIONS` (RCB — retired conditional branches) raises a signal at a programmable instruction count. The supervisor preempts the SUT thread at a deterministic logical-time tick rather than a wall-clock tick.
2. **Cooperative scheduler.** A single supervisor decides which thread runs next based on the deterministic tick stream. Combined with virtual clock and RNG funnel, the SUT becomes a deterministic state machine.
3. **`reverie` framework.** Hermit's underlying ptrace-based syscall-interception library, similar in spirit to gVisor's PTRACE platform but Rust + lighter-weight.

### Hermit-class vs Antithesis-class

Both reach L5 from different sides:

- **Hermit-class:** instruction-boundary scheduling, single-process-tree, no snapshots. Open-source-feasible.
- **Antithesis-class:** instruction-boundary + multi-VM + snapshots for branching exploration. Proprietary, $100K+/yr.

The distinction matters for *positioning* (Faultbox-at-L5 would more naturally resemble Hermit-class — the L5 vocabulary in RFC-040 collapses both for taxonomy simplicity).

### Why Faultbox does not adopt Hermit today

- **Language mismatch.** Faultbox is Go end-to-end; Hermit is Rust. Process-supervisor in Rust wrapping Go SUT services means two toolchains, two debug stories, cross-language operational complexity.
- **Dormant upstream.** Inheriting Hermit means inheriting its kernel-compat patches, perf-counter quirks across hardware (especially Apple Silicon / Lima where PMU access is restricted), and ARM64 corner cases. Reverie had more momentum than the determinism layer; both slowed.
- **ptrace ↔ seccomp-notify conflict.** Both want to be the process supervisor. Stacking is architecturally awkward; replacing seccomp-notify with ptrace gives up the surgical fault model that's our current strength.

### What Hermit gives us regardless

A canonical reference for any future Faultbox-native deterministic-scheduler work. If/when the demand and funding arrive to build instruction-boundary determinism in Go (~year-2 horizon), Hermit's design — PMU preemption, virtual clock, RNG funnel, cooperative scheduler — is the architecture to study, not reinvent.

**This RFC treats L5 as known-achievable but explicitly out of v1.0 / v2.0 scope.**

## Strict-mode semantics beyond L1

RFC-040 ships `strict` semantics for L0 and L1 only. The full per-level matrix is recorded here so v0.14.0+ implementation can land without re-litigating it.

| Level | What `strict=True` controls | What `strict=False` does |
|-------|----------------------------|--------------------------|
| L0 | Nothing — no violation events exist (plan determinism is binary) | n/a |
| L1 | Promotes `unmediated_io` events to test failures | Records them as warnings in the report |
| L2 | Same as L1 but broader coverage (gVisor sees more I/O surfaces) | Same as L1 |
| L3 | Promotes replay-byte-drift events (between two runs of the same bundle) to failures | Records as warnings; bundle is still produced |
| L4 | No effect — violations are *prevented* at runtime (denied syscall, denied connection), not detected after the fact | n/a |
| L5 | No effect — same structural enforcement as L4 plus scheduler determinism | n/a |

**Spec-load policy (under the post-L1 runtimes):**
- At L0, L4, L5: passing `strict=` is rejected with: *"`strict` has no effect at L<n>; remove it or change the level."* The kwarg signals an intent the level cannot honor.
- At L1, L2, L3: `strict` defaults to `True`. CLI escape hatch: `faultbox test --strict-determinism=false`.

**Why the asymmetry:**
- L0 has nothing detectable to flag — plan generation either is deterministic or it isn't, and the latter is a bug, not a drift.
- L4 / L5 enforce the contract structurally: at L4, an unauthorized syscall returns `EPERM` and the SUT either handles the failure or crashes; the test result reflects that directly. There's no "warning level" for a denied syscall — the SUT couldn't proceed regardless.
- L1 / L2 / L3 are the levels where Faultbox *observes* drift after the fact, so the warn-vs-fail choice is meaningful.

## Implementation horizon

| Path / Level | Target version | Status |
|--------------|----------------|--------|
| Path A (seccomp-notify) | v0.13.0 | Shipping; caps at L1 |
| Path B (gVisor netstack) | v0.14.x or v0.15.0 | Investigating |
| Path C (Faultbox Sentry) | v2.0 | Customer-demand-driven |
| L4 — Hermetic mode | v0.15.x or v1.0 | Depends on Path C + RFC-043 |
| L5 — instruction-boundary | post-v2.0 research | Long-arc; not on roadmap |

## Dependencies

- **RFC-040 (Determinism Levels):** source of the L0–L5 vocabulary. This RFC fills in the implementation paths but does not redefine the levels.
- **RFC-043 (MCP bundle ops, v0.14.0):** L4's LLM rule-discovery workflow depends on bundle MCP access.
- **RFC-036 (Remote Services):** L4 explicitly forbids `remote=`; rationale documented above.
- **RFC-037 (Determinism for Remote Services):** record-and-replay for `remote=` caps at L3, mutually exclusive with L4 regardless of how RFC-037 lands.
- **RFC-022 (Multi-Process Seccomp):** Path A substrate.
- **RFC-024 (Proxy Datapath):** Path A's data-path mediation.

## References

- [gVisor Strategy Notion doc (April 2026)](https://www.notion.so/gVisor-Strategy-Path-to-Antithesis-Class-Capabilities-April-2026-33a03cbd8e6181a2a2cce90891c41231) — defines Path A / Path B / Path C terminology used here.
- Hermit project: `facebookexperimental/hermit` (Rust, dormant).
- gVisor: `google/gvisor` (Go, Apache 2.0).
- Customer feedback: 2026-04-22 customer-feedback-analysis (Notion), Group A on determinism.
