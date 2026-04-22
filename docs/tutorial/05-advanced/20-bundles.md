# Chapter 20: `.fb` Bundles — Reproducibility by Default

**Duration:** 10 minutes
**Prerequisites:** [Chapter 1 (First Fault)](../01-first-taste/01-first-fault.md) — enough Faultbox to have produced a green and a red test

## Goals & purpose

Six weeks into a fault-injection engagement the customer realised
every bug they had found was "folklore" — the specs were in git, but
nobody had saved the trace, the seed, the host versions, or the
app stdout at the moment of failure. When a colleague tried to
reproduce a bug two months later, they had to reconstruct the
environment from Slack history.

**`.fb` bundles** (shipped in v0.9.7) close that gap. Every
`faultbox test` run emits a single archive file — trace, env
fingerprint, per-service logs, the `.star` source tree, and a
one-liner reproduction script — with zero flags required. Bundles
are the input contract for `faultbox replay` (v0.10.0) and
`faultbox report` (v0.11.0), so everything downstream operates on
the same portable object.

This chapter teaches you to:

- **Produce a bundle** on every test run (you're already producing one).
- **Inspect a bundle** to answer "what happened here?"
- **Share a bundle** by email, Slack, or git commit.
- **Understand the reproducibility guarantees** and their limits.

## 1 · Produce a bundle

Nothing to change. Run any test:

```bash
$ faultbox test faultbox.star

--- PASS: test_happy_path (12ms, seed=42) ---
--- FAIL: test_fault_scenario (204ms, seed=42) ---
  reason: expected status 200, got 503

Bundle: run-2026-04-22T15-03-11-42.fb

1 passed, 1 failed

Replay: faultbox replay run-2026-04-22T15-03-11-42.fb --test test_fault_scenario --seed 42
```

Three new lines compared to pre-v0.9.7 output:

- **`Bundle: …`** — path of the archive emitted alongside the test.
- **`Replay: …`** — one-liner that reproduces the failure. Copy-paste
  into a bug report and you're done.
- **(not shown above) `Zero-traffic faults: …`** — only appears if
  a fault rule was installed but matched no syscalls during the
  fault window. See [§4 below](#4--debugging-with-zero-traffic-hints).

**Opt-out:** `--no-bundle` skips emission. Rare — used when a run
is purely diagnostic.

**Override location:** `--bundle=<path>` or the
`$FAULTBOX_BUNDLE_DIR` env var for CI workdirs.

## 2 · Inspect a bundle

```bash
$ faultbox inspect run-2026-04-22T15-03-11-42.fb

Bundle:          run-2026-04-22T15-03-11-42.fb
Produced by:     faultbox 0.9.7
Schema version:  1
Run ID:          8ebbad2351b0c7accb6aa1ee8ce1aca8
Created:         2026-04-22T15:03:11Z
Seed:            42
Spec root:       faultbox.star
Host:            linux/arm64 (kernel 6.8.0-49-generic)
Go toolchain:    go1.26.1
Docker:          27.3.1

Tests: 2 total, 1 passed, 1 failed, 0 errored
  ✓ test_happy_path (12ms)
  ✗ test_fault_scenario (204ms)

Files:
  manifest.json
  env.json
  trace.json
  replay.sh
  spec/faultbox.star
  spec/helpers/jwt.star
```

The header answers the first three questions any debugger asks:
**what ran, where did it run, when did it run.** Click through to
specific files:

```bash
# Get the pass/fail summary as JSON.
faultbox inspect run-*.fb manifest.json | jq '.summary'

# See what the SUT logged during the failed test.
faultbox inspect run-*.fb trace.json | jq '.tests[] | select(.result=="fail") | .events'

# Extract the whole archive into a working directory.
faultbox inspect run-*.fb --extract ./unpacked/
```

## 3 · Share a bundle

Bundles are **self-contained single files**. Three workflows cover
most use cases:

- **Bug report.** Attach the `.fb` to the ticket. The reader has
  everything they need to run `faultbox replay <bundle>` and
  reproduce your run.
- **Regression evidence.** Commit a golden bundle under
  `regressions/<issue-id>.fb`. CI can re-run it to prove the fix
  holds. A typical `.gitignore` lets everyday bundles slide but
  allows curated ones:

    ```
    *.fb
    !regressions/*.fb
    ```

- **CI artifact upload.** GitHub Actions, BuildKite, and similar
  CIs publish arbitrary files per job. Upload the `.fb`; the PR
  author can download and inspect without rebuilding the env.

## 4 · Debugging with zero-traffic hints

The hardest fault-injection bug to notice is **the one that didn't
fire**. You wrote `fault(db, connect=deny(...))` but the test
executes a cached client that never reopens the connection.
Pre-v0.9.4 Faultbox would happily pass the test; the rule matched
zero syscalls and nothing told you.

Since v0.9.4 the runtime emits a `fault_zero_traffic` event per
orphaned rule; since v0.9.7 you see it in the terminal:

```
Zero-traffic faults (1): rule installed, matched no syscalls
  test_fault_scenario — db.connect (deny)
  Hint: the scenario may not be exercising the upstream during the fault window.
```

Three common causes:

1. **The scenario's happy path doesn't touch this upstream.** Add
   an operation that does, or move the fault into a different
   `fault_matrix` row.
2. **A client-side cache is holding an old connection.** Reset
   between tests with a `reset()` hook.
3. **You're denying the wrong syscall.** Named operations help —
   `ops = {"net_write": op(syscalls=["sendto", "sendmsg"])}`
   then `fault(..., net_write=deny(...))`.

## 5 · Reproducibility guarantees (and limits)

A bundle captures:

- ✓ The `.star` source tree (root + local `load()`s).
- ✓ The seed used for probabilistic faults.
- ✓ Host OS / arch / kernel / Go toolchain / Docker version.
- ✓ Full event log with vector clocks.
- ✓ Pass/fail outcomes and durations.
- ✓ Per-service stdout/stderr (Phase 1.5).

A bundle does **not** capture (today):

- ✗ Container image digests by `sha256:` (planned for v0.10.0 —
  `faultbox.lock` concept).
- ✗ `@faultbox/*` stdlib bytes — they're in the binary. A replay
  uses the consumer's stdlib, not the producer's.
- ✗ Your app binary (`/tmp/truck-api` or wherever). Include
  `service(build=...)` in the spec and Faultbox will rebuild from
  source; for pre-built binaries, pin them separately.

Version compatibility for consumers (`replay`, `report`):

- Same version → silent.
- Same major, minor/patch differs → **warn, proceed.**
- Major differs (`0.x` ↔ `1.x`) → **refuse** (replay only;
  inspect and report still read the bundle).

See [`docs/bundles.md`](../../bundles.md) for the full format spec.

## Takeaways

- Every run produces a `.fb` — shareable, auditable, replayable.
- `faultbox inspect <bundle>` is the first tool a debugger reaches
  for after a failed CI run.
- The failure-time `Replay:` hint is one copy-paste from a usable
  reproduction command.
- Zero-traffic warnings catch the entire class of silently-
  ineffective fault rules that used to pass tests dishonestly.

Next: [Chapter 21 — Replay, coming in v0.10.0 →](21-replay.md)
