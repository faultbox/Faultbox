# Diagnostic and error codes

Every machine-readable code Faultbox emits, what it means, and what to do about
it. Codes appear in `--format json`, in `.fb` bundles, in MCP tool results, and
in terminal output as `[CODE]`.

**Codes are an API.** Agents branch on them, so they are treated as stable:
additions are cheap, changes are breaking.

## Run-level diagnostics

Findings about the suite as a whole — questions no single test can answer.

### `NO_POSITIVE_CONTROL`

> An interface is stepped, but no test ever asserts that a step on it succeeds.

Every assertion about that interface is satisfied by a client that cannot
connect at all, so the suite would not fail if it broke.

This is the diagnostic with a real story behind it. A CI spec exercised a broken
Postgres client on every pull request for three releases and passed:

```python
env = {"POSTGRES_HOST_AUTH_METHOD": "trust", ...}   # removes the credential path

resp = pg.main.query(sql = "SELECT 1")
assert_true(not resp.ok, "expected failed query under injected fault")
```

It asserts the query **fails**. A client that cannot authenticate satisfies that
exactly as well as the injected fault does. Two credential bugs lived behind
that green badge until a spec was written that asserted success.

**Fix:** add one test that runs a step with no fault injected and asserts
`r.ok`. The `poc/protocol-audit/` specs are the pattern — each pairs a statement
that must succeed with one that must fail.

Note this is a *suite-level* property. A single fault-injection test asserting
failure is correct and normal; what is wrong is a suite where that is the only
kind of assertion an interface ever receives.

## Per-test diagnostics

### `TEST_NO_ASSERTIONS`

> The test passed without evaluating any assertion.

It cannot fail. Add an `assert_*` call, an `expect=` predicate, or a temporal
property — or, if the test exists to exercise a path rather than check one, say
so in its docstring.

Counted assertions include `assert_*`, `expect_*`, `eventually()`, `always()`,
and a declarative `test(..., expect=...)`.

Only reported for tests that **passed**: a failed or timed-out test may have
stopped before reaching its assertions, and saying "you asserted nothing" on top
of a failure is noise.

### `FAULT_FIRED_BUT_SUCCESS`

> A fault fired but the test passed anyway.

The service may not be checking errors on that syscall. Verify its error
handling, or add an assertion that the response reflects the failure.

### `FAULT_NOT_FIRED`

> A fault was installed but never fired.

Usually a different syscall variant than expected (`pwrite64` rather than
`write`) or a path filter that does not match. `--debug` shows the actual
syscalls.

### `SERVICE_CRASHED`

> The service exited non-zero during the test.

### `TIMEOUT_DURING_FAULT`

> The test timed out while faults were active.

Look for infinite retry loops, missing timeouts on network calls, or deadlocks.

Fires only when a fault actually **fired**. Before v0.18.0 it fired on every
timeout, so a run with no faults at all was reported as timing out "while faults
were active" — naming a cause that did not exist and sending readers to look for
retry loops in a service that had never started. The two codes below cover what
it used to absorb.

### `TIMEOUT_NO_FAULT_FIRED`

> The test timed out; faults were declared but none fired.

The timeout is not explained by an injected fault. Check service startup and
healthchecks first, then whether the fault targets the syscall or operation the
service actually uses — see [`FAULT_NOT_FIRED`](#fault_not_fired).

### `TIMEOUT_NO_FAULTS`

> The test timed out and the run injected no faults.

A plain timeout. Check that every service started and passed its healthcheck: a
missing image or a failing readiness check reaches the deadline the same way a
hung service does.

### `ASSERTION_MISMATCH`

> An assertion failed, with the specific values.

### `MULTIPLE_FAULTS_INTERACTION`

> More than one fault was active and firing when the test failed.

Test each in isolation first to find which one causes the failure.

## Spec-load errors

Everything [`faultbox check`](cli-reference.md#faultbox-check-v0170) can find
without launching anything.

| Code | Meaning |
|---|---|
| `SPEC_SYNTAX` | The file is not valid Starlark, or the resolver rejected it. Nothing executed. See [starlark-dialect.md](starlark-dialect.md) — the dialect differs from Python. |
| `SPEC_LOAD_FAILED` | The spec parsed and resolved, then failed while loading: an unknown keyword argument, a rejected value, a removed builtin. |
| `SPEC_FORBIDDEN_LAMBDA` | A `monitor()` or `assume()` predicate called something the sandbox forbids. Signal failure through the return value instead. |
| `SPEC_RECIPE_NOT_FOUND` | No such recipe in the embedded stdlib. `faultbox recipes list` shows what ships. |
| `NO_TESTS_DISCOVERED` | The spec loads and declares no tests. A warning, not an error. |
| `PLAN_COST_EXCEEDED` | The plan expands beyond `--max-instances`. |

## Infrastructure errors

| Code | Meaning |
|---|---|
| `HEALTHCHECK_TIMEOUT` | The service never became ready. If the healthcheck is `tcp()`, it only proves a port is bound — prefer [`ready()`](spec-language.md#readytimeout-v0160). |
| `LAUNCH_FAILED` | The service could not be started: a missing binary, or an image that cannot be pulled. |
| `DOCKER_UNAVAILABLE` | Docker is not reachable. On macOS, `make env-start`. |
| `TRACE_HOST_NOT_REGISTERED` | `watch()` needs `sudo faultbox setup-trace` once, then a Docker restart. |

## Coverage of the taxonomy

Codes are attached where a failure is *raised*, by a typed error — never by
matching message text, which would mean a reworded error silently changes its
code.

The consequence is that **adoption is incremental, and the tool says so**. An
error without a code reports without one rather than being given a guess. A gap
in this list is discoverable and can be filled; a wrong code is something an
agent acts on.
