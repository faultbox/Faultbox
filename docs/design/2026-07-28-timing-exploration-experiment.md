# Can the fault-timing space be searched with what v0.14.0 shipped?

**Date:** 2026-07-28
**Status:** Experiment complete. Answer: **half of it.**
**Branch:** `epic/v0.14.1`
**Follows:** [Raft mesh results §4.2](2026-07-28-raft-mesh-results.md)
**Artifact:** [`poc/raft-cluster/explore.star`](../../poc/raft-cluster/explore.star)

---

## Why

v0.14.0's Raft write-up closed on a specific claim: the harness could express
the faults but not *search* the timing space, and `choose()` + `--explore`
looked like it would close that gap with no new code. RFC-054's future-work
list was reordered on the strength of that claim.

It was a claim, not a measurement. This is the measurement.

## Result

| Axis | Verdict |
|---|---|
| **Work volume** — `submit(choose("behind", [25, 120, 400]))` | **Works.** 6/6 leaves, each with distinct cluster state. |
| **Wall clock** — `await_stable(quiescence_window = choose(...))` | **Does not work.** 12/18 leaves hung 3 minutes each and reported INCONCLUSIVE. |

The mechanism is not a bug in `await_stable`. It is a category error in using it
as a delay, and it fails in the one situation the delay is for.

## The measurement

`await_stable` returns when no event has arrived for the full quiescence window.
During an active partition, in a 3-node Raft cluster:

```
lines in one 3-minute leaf:     6681
max quiet gap between events:   0.338 s
```

Every dropped packet emits a `packet` event; every failed heartbeat, every
`requestVote` timeout emits a `stdout` event. The stream never goes quiet for
longer than ~340 ms. So:

| window | outcome |
|---|---|
| `50ms`, `100ms` | returns |
| `400ms` | marginal — near the 338 ms boundary, timing-flaky |
| `900ms`, `1200ms` | **never returns**; blocks until the 3-minute test deadline |

**The fault generates the events that prevent quiescence.** The longer the delay
asked for, the more certain it is never to arrive — exactly inverted from what a
timing knob needs. `ignore=` does not rescue it: the noise is the SUT's own
stdout, which is also what a spec legitimately waits on.

12 leaves × 3 min = 36 minutes of wall clock, producing no signal about
`hashicorp/raft` and one line of output:

```
6 passed, 0 failed, 12 inconclusive
```

## What this costs, stated plainly

The wall-clock axis of the Raft transfer scenario (Antithesis bug 2) **has still
never been exercised.** 12 of 18 leaves never reached their `/transfer` call.
The snapshot scenario's work-volume axis (bug 3) did get a real search — 6
leaves, follower lag from 25 to 400 entries, snapshot before and after rejoin,
all converged.

So the v0.14.0 report's §6 stands, with one correction: the missing capability is
narrower and more specific than "fault-timing exploration". Fan-out works. What
is missing is a way to say *wait 400 ms*.

## Defects found

Four, all reachable only because v0.14.0 shipped a TUN-backed gateway.

**1. The TUN device leaks on SIGTERM.** `cmd/faultbox/main.go` installs
`signal.NotifyContext(ctx, os.Interrupt)` — SIGINT only. A `timeout`-killed run,
a CI job that overruns, or any `kill` leaves `faultbox0` behind. Every later
packet-fault run on that host then dies with:

```
start packet gateway: TUNSETIFF faultbox0: device or resource busy
```

Recovery is `sudo ip link delete faultbox0`, documented nowhere. The teardown
comment at `runtime.go:3045` anticipates precisely this leak; it only covers the
normal path.

**2. The device name is a constant.** `faultbox0` means two concurrent runs on
one host collide, and the collision is indistinguishable from defect 1.

**3. `faultbox plan` cannot see `choose()` axes.** It reported `Total: 2 plan
instances` for a spec that runs 24 leaves. Axes are discovered by *executing* a
discovery leaf (`runtime.go:1206`); `plan` is static and executes nothing. So
`--check-cost --max-instances N`, whose entire purpose is catching fan-out
blowups before they run, is blind to the construct most likely to cause one.

**4. Timeout verdicts do not say why.** The summary reports `12 inconclusive`
after 36 minutes with no indication that every one was an `await_stable` that
could not quiesce. `TestResult.Reason` carries the text; the summary drops it.

## What the guard caught

Worth recording, because it is the counterfactual for this whole document.

An earlier run of the same 18 leaves reported every leaf reaching
`accepted=10` — the liveness assertion passing 18 times. It was meaningless: a
leaked `faultbox0` from defect 1 meant no gateway attached, so the partitions
dropped nothing. Instead of 18 green leaves, v0.14.0's fail-loudly check
produced:

```
reason: packet faults were installed 8 time(s) but no netstack gateway was
        attached, so no packet was affected; the result below would be meaningless
```

`accepted=10` everywhere is the exact signature of a partition that did nothing.
Without that check this document would report "bug 2 does not reproduce across
18 timing configurations", which would have been the third false conclusion in
this line of work.

## Recommendation

- **`sleep(duration)` / `hold(duration)`** — a real wall-clock wait, independent
  of event traffic. Small, and it is the only thing standing between the current
  harness and a genuine search over fault timing. This is now measured rather
  than assumed.
- Fix defects 1 and 2 together: signal-safe teardown plus a unique device name.
  Both are user-facing and both brick a machine until manually cleaned.
- Defects 3 and 4 are honesty gaps of the same family as §4.2 — a number that
  reads like coverage but is not.
