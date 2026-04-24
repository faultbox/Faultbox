# Reading a Faultbox report

Every `faultbox test` run produces a `.fb` bundle (RFC-025). The
bundle is the source of truth — machine-readable, byte-identical for
the same inputs. The report is the **human** view of it: one HTML
file you can share, screenshot, or walk a colleague through.

This chapter is a guided tour. Open
[faultbox.io/reports/sample.html](https://faultbox.io/reports/sample.html)
in another tab and follow along.

## Generate a report locally

```sh
faultbox test faultbox.star
# → Bundle: run-2026-04-22T15-03-11-42.fb

faultbox report run-2026-04-22T15-03-11-42.fb
# → wrote report.html
```

Open `report.html` in any browser. No server, no network.

## The story the report tells

The page reads top-to-bottom as a three-act narrative:

### 1. What we tested

The header and hero answer the first question: _"how much did this
run cover?"_

- **3 × 4 matrix cells** — three scenarios × four fault assumptions.
- **45 faults delivered** — how many times Faultbox actually
  intercepted a syscall and injected a failure.
- **4 services observed** — how many of the topology's services
  actually executed a syscall we captured.
- **4.33 s duration** — wall clock.

One sentence under the title states the thesis: _"10 of 12 checks
held; 2 regressed under fault."_ If every check held, the sentence
becomes _"All 12 checks held up under the injected faults."_ — the
kind of line you paste into a release PR.

### 2. How it went

Two views show outcomes.

**Attention** lists failed tests and warning-level diagnostics at
the top, not buried. Each failed test shows the assertion that
tripped, the diagnostic code (e.g.
`ASSERTION_MISMATCH`, `FAULT_FIRED_BUT_SUCCESS`), and a
one-line replay command you can paste into your terminal.

**The fault matrix** is the iconic visual. Scenarios are rows,
fault assumptions are columns, cells carry a ✓ / ✗ / · state and
are coloured for accessibility. Click any cell to drill down.

![Matrix screenshot](/reports/sample.html)

### 3. What to do next

**Observed coverage** groups activity by service: how many tests
touched it, how many syscalls were captured, how many were
faulted, and which syscalls dominated. This is the honest
picture — what your tests actually exercised, measured from the
run itself, not from a coverage DSL you have to maintain.

**Reproducibility** gives you everything you need to rerun:
Faultbox version, Go toolchain, host kernel, Docker version,
every container image pinned by digest, and the seed.

## The drill-down

Clicking a cell in the matrix — or a row in the tests table, or a
card in Attention — opens a drill-down panel for that test.

The drill-down answers _"why did this test go this way?"_:

- **Reason** — the assertion or timeout that tripped.
- **Faults applied** — every fault rule that was active for this
  test, with hit counts. A fault with `hits=0` is flagged amber:
  it was declared but never matched a syscall, which usually
  means your match path is wrong or the service uses a different
  syscall variant.
- **Diagnostics** — pattern-based hints Faultbox generates
  automatically. `FAULT_FIRED_BUT_SUCCESS` is a classic: the
  fault hit, the test still passed — the service likely swallowed
  the error instead of propagating it.
- **Replay** — single command, ready to paste.
- **Event trace** — a swim-lane view. Each service is a lane;
  markers show syscalls, faults, lifecycle, violations. Hover a
  marker for its syscall and decision.

## Sharing a report

Because the report is one file, sharing it is trivial:

- **Slack / Teams** — drag the `.html` into the channel. The
  recipient clicks and it opens — no mystery stack.
- **Git** — commit `report.html` next to your test spec as a
  baseline. Diffing future runs against it catches regressions.
- **Email** — attach. It's a static file.
- **Artifact in CI** — upload as a GitHub Actions / GitLab /
  BuildKite artifact.

Direct-link drill-down: the URL hash `#test=<name>` auto-opens
a specific drill-down panel. Handy for PR comments:

```markdown
Looks like the cache-latency regression traces to an unbounded
retry loop: see
[sample.html#test=test_order_flow__cache_latency](sample.html#test=test_order_flow__cache_latency).
```

## What's next

The v0.11.0 report is the single-run baseline. Multi-run
comparisons, causal-arrow overlays on the trace viewer, and a
"publish to hosted bundle viewer" path are on the v0.11.x and
v0.12.x roadmaps.

For the full reference, see [docs/reports.md](../../reports.md).
