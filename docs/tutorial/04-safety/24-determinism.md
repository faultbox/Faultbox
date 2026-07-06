# Chapter 24: Determinism & the L1 Contract

**Duration:** 25 minutes
**Prerequisites:** [Chapter 4 (Traces & Assertions)](../02-syscall-level/04-traces.md) and [Chapter 14 (Invariants)](14-invariants.md) completed

## Goals & Purpose

When a Faultbox test fails the same way twice but passes the third time, you have one of two problems: a real race condition, or your service-under-test is doing I/O that Faultbox isn't watching. The latter is what RFC-040 calls **unmediated I/O** - and until v0.13.0, Faultbox couldn't tell you which one it was.

This chapter teaches you to:
- **Read the L0–L5 determinism taxonomy** and place your test on it
- **Use `determinism()`** to make the spec's reproducibility promise explicit
- **Use `nondeterministic_ok=`** as the documented escape hatch - and recognise the anti-pattern of using it to silence flakes
- **Debug strict-mode failures** with the runtime override flag

## The taxonomy in one paragraph

Faultbox v0.13.0 ships **L1: mediated-event determinism**. That means: same plan-node + seed → causal-precedence-equivalent log of the events Faultbox mediated (proxy traffic, faulted syscalls). Concurrent events may interleave differently across replays, and any I/O Faultbox doesn't see (a background `getrandom` call, a DNS lookup outside the proxy, `clock_gettime` for a request-ID timestamp) is invisible to the contract.

Higher levels (L2–L5) are roadmap. Lower (L0) is plan-only determinism - useful for `--dry-run` but not for actual test outcomes. The full taxonomy lives in [docs/determinism.md](../../determinism.md); for this chapter we focus on L1 because that's what runs today.

## Strict mode is on by default

The default spec - no `determinism()` call - runs at L1 with `strict=True`:

```python
service("api", "/usr/local/bin/api",
    interface("main", "http", 8080),
)

def test_smoke():
    api.main.get(path = "/healthz")
```

Run this. If `api` calls `getrandom` to mint a request ID, you'll see:

```
test_smoke FAIL: strict determinism: unmediated_io[rand] from service "api"
  (syscall=getrandom, pid=4242)
  - add "rand" to determinism(allow=...) or service("api",
    nondeterministic_ok=[...]) to tolerate, or fix the underlying I/O leak
```

The error names the category, the service, the syscall, and the two escape hatches - in that order. The third option (fix the SUT) is intentional: most untolerated `unmediated_io` events are real flakes-in-waiting, and silencing them is debt.

## When to tolerate, when to fix

The rule of thumb: **does this drift change what your test asserts?**

A request-ID UUID the test never inspects: tolerate.

A clock read used for monotonic timing the test ignores: tolerate.

DNS resolution against a host outside any declared `interface()` (e.g. a metrics endpoint): tolerate.

A clock read that drives a timeout the test asserts on: fix. Mock the clock, inject the timeout as a config value, or move it to a `delay()` fault you control.

A `getrandom` that seeds retry jitter the test does care about (because the test counts retries): fix. Inject a deterministic seed.

A `connect()` to a real DNS server when the SUT *should* be talking to a Faultbox proxy: that's a misconfigured spec, not a tolerance question. Add the missing `interface()`.

## The two escape hatches

### Per-service: `nondeterministic_ok=`

```python
service("api", "/usr/local/bin/api",
    interface("main", "http", 8080),
    nondeterministic_ok = ["rand"],   # api uses getrandom for request IDs
)
```

The list is service-scoped: only `api`'s `unmediated_io[rand]` events are tolerated. A different service emitting `unmediated_io[rand]` will still fail strict mode.

### Spec-wide: `determinism(allow=[...])`

```python
determinism(allow = ["clock"])    # every Go service uses time.Now() for logging

service("api", "/usr/local/bin/api",
    interface("main", "http", 8080),
)

service("worker", "/usr/local/bin/worker",
    interface("main", "http", 8081),
    nondeterministic_ok = ["rand"],
)
```

Effective allow set:
- `api` tolerates `clock` (from spec).
- `worker` tolerates `clock` (from spec) **and** `rand` (per-service).

The two lists union; they don't replace.

### Anti-pattern: making CI green

```python
# DON'T DO THIS without investigating each category
determinism(allow = ["clock", "rand", "dns", "network-unmediated"])
```

If you find yourself adding every category to the spec-level `allow=`, you're effectively running with `strict=False`. Just write that - the intent is honest, and a future reader knows you opted out deliberately. `allow=` is for documented exceptions, not for chasing green builds.

## Local debug iteration without editing the spec

Sometimes you want to see what *else* is going wrong with a test, after the strict failure on the first event. The CLI override:

```bash
faultbox test smoke.star --no-strict-determinism
```

This flips strict off for the duration of one run. The `unmediated_io` events still appear in the trace and bundle (now as warnings, visible in `--format json` and the HTML report); they just don't fail the test. The spec is unchanged, your CI is unchanged.

The override is bidirectional - `--strict-determinism` also forces strict on if a spec turned it off. Useful for one-off compliance checks before merging.

## What L1 detection actually catches

Five categories. Three caveats worth knowing.

| Category | Caught | Not caught |
|----------|--------|------------|
| `clock` | `clock_gettime` syscall path | **Go's `time.Now()`** - uses VDSO, which seccomp can't intercept. Best-effort. |
| `rand` | `getrandom` + `/dev/urandom` reads | An in-process Mersenne twister seeded once at startup |
| `dns` | `connect()` to port 53 outside declared interfaces | DoH / DoT - encrypted DNS over HTTPS or TLS |
| `network-unmediated` | `connect()` to any address/port not matching an `interface()` and not a Faultbox proxy listener | Nothing - this is the cleanest signal |
| `fs-unmediated` | reserved category in v0.13.0 - accepted in `allow=` lists but not detected yet | (everything for now) |

The Go VDSO caveat is load-bearing for most users: if every service in your stack is Go and you don't tolerate `clock`, strict mode will *under-report* clock drift, not over-report. That's the right side of the trade - false negatives are debt; false positives are noise. Document the limit in your spec comments.

This VDSO blindness is resolved only at **Path C (Faultbox Sentry fork)** - see [RFC-046](../../rfcs/0046-beyond-l1-roadmap.md). Path B gVisor (v0.14.x) replaces the TCP proxy with gVisor netstack but does *not* run SUT code inside gVisor Sentry, so VDSO calls remain outside seccomp reach at v0.14.x.

## Worked example: a real flake

Service `api` mints request IDs with `crypto/rand.Read`. The test passes most of the time:

```python
service("api", "/usr/local/bin/api",
    interface("main", "http", 8080),
)

def test_request_id_in_response():
    resp = api.main.post(path = "/echo", body = "hello")
    assert_true("X-Request-ID" in resp.headers)
```

Pre-v0.13.0 you'd see this fail intermittently in CI with no useful signal - the request ID is non-empty, but the test that *also* asserts it matches a regex pattern flakes when the random bytes contain unprintable characters.

With v0.13.0, the first run shows:

```
test_request_id_in_response FAIL: strict determinism: unmediated_io[rand]
  from service "api" (syscall=getrandom, pid=4242) - ...
```

You investigate. The request ID is fine for the test, but the unprintable-character check is brittle. You decide to:

1. Fix the brittleness (assert on length, not pattern), and
2. Accept the underlying randomness - it doesn't change behavior the test asserts on.

The final spec:

```python
service("api", "/usr/local/bin/api",
    interface("main", "http", 8080),
    nondeterministic_ok = ["rand"],
)

def test_request_id_in_response():
    resp = api.main.post(path = "/echo", body = "hello")
    rid = resp.headers["X-Request-ID"]
    assert_true(len(rid) >= 16, "request ID should be at least 16 chars")
```

Strict mode is happy. The brittleness is gone. The PR review now has an explicit signal - `nondeterministic_ok = ["rand"]` - that says "we considered randomness here and decided it's fine." Future readers can challenge the decision; future tests can't accidentally inherit silent drift.

## Exercises

1. **Force a clock leak.** Write a tiny Go service that calls `syscall.Syscall(syscall.SYS_clock_gettime, ...)` directly (bypassing VDSO) on every request. Observe the strict-mode failure. Add `clock` to the service's `nondeterministic_ok`. Verify the test now passes and the event appears as a warning in the bundle.

2. **Test a spec-level allow.** Set `determinism(allow=["clock"])` and remove the per-service `nondeterministic_ok`. Verify both services tolerate clock. Add a third service that *doesn't* tolerate it (say, by emitting from a partition test) and prove the spec-level allow propagates.

3. **Read a strict-determinism bundle.** Run a strict-mode failure to completion, then `faultbox report <bundle.fb>`. Find the strict-determinism row in the manifest summary; click into the drill-down; identify the exact `unmediated_io` event in the swim-lane viewer.

4. **Run with the CLI override.** Take a strict-spec test you know fails, run it with `--no-strict-determinism`, and observe the change in outcome. Convince yourself that the spec-level configuration didn't change - only the runtime decision did.
