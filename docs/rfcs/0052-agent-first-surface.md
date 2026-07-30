# RFC-052: Agent-First Surface

> **Status: Accepted, in progress (v0.17.0).** 2026-07-30.
> Tracking issue: [#136](https://github.com/faultbox/Faultbox/issues/136).
> Plan: [`docs/implementation/v0.17.0-rfc-052-plan.md`](../implementation/v0.17.0-rfc-052-plan.md).
>
> **This document supersedes the issue body**, which was written when v0.14.0 was the
> next release and refers to it throughout. Two things changed in the interval:
> the release target moved to v0.17.0, and v0.16.0/v0.16.1 produced evidence that
> reframes the problem — see [Gap 8](#gap-8--detecting-specs-that-cannot-fail), which is new here and
> is the reason this RFC is worth doing now rather than later.

## Summary

Make the LLM agent a first-class author and operator of Faultbox specs.

The foundation already ships: an MCP server with 6 tools, `TraceOutput` v2 JSON, 6
diagnostic codes with suggestions, the `.fb` bundle with a machine-readable
manifest/trace/plan, and the `/fault-*` slash commands. This RFC closes the gaps that
still force an agent to shell out, guess, or blow its context window — and adds one gap
the original framing missed.

v0.17.0 takes the first slice: **Gaps 1, 2 and 8**, plus the deprecation removals that
were promised for v0.14.0 and never happened. Everything else in the RFC reports
*through* that slice, which is why it goes first.

## Motivation

The primary spec author is an LLM agent, not a human. An agent's loop is:

```
understand the DSL → write a spec → validate cheaply → run
    → read results without drowning → fix code or spec → replay
```

The original RFC identified the weak links as steps 1, 3 and 5: DSL ground truth lives
in prose docs (hallucination risk), validation requires launching services, and reading
results means parsing a full `TraceOutput` including the entire event log.

Those are all real. But v0.16.0 and v0.16.1 exposed a failure mode **earlier in the
loop**, and it is worse:

> A spec can be syntactically valid, cheap to validate, fast to run, and green — while
> being incapable of detecting that the thing it tests is completely broken.

Two credential bugs survived from the day their plugins were written until v0.16.0 and
v0.16.1. Postgres steps could not authenticate against any password-protected server;
MySQL steps could not authenticate at all, nor select a database. Neither was subtle.

**A CI spec exercised the broken Postgres path on every pull request and passed.** It is
worth reading, because it is not a careless test — it is a careful one, and the care is
what hid the bug:

```python
pg = service("pg", interface("main", "postgres", 5432),
    env = {"POSTGRES_HOST_AUTH_METHOD": "trust", ...},     # (1)
)

def test_proxy_fault_rewrites_select():
    def scenario():
        resp = pg.main.query(sql = "SELECT 1")
        assert_true(not resp.ok, "expected failed query under injected fault")   # (2)
        assert_true(resp.error != "", "expected non-empty error")
    fault(pg.main, error(query = "SELECT*", message = "injected: disk full"), run = scenario)
```

1. `trust` auth removes the credential path from the test entirely. The spec's own comment
   states the intent: *"so this test doesn't depend on authentication round-tripping."*
2. The assertion is that the query **fails**. A broken client satisfies it exactly as well
   as the injected fault does.

So the test asserted a true and relevant thing, on a real container, in CI — and could not
distinguish "the fault injection worked" from "the client cannot connect at all." **There
was no positive control**: no assertion anywhere that a Postgres step *succeeds*.

This generalises. Three distinct ways a spec fails to detect a broken subject, in
increasing order of subtlety:

| # | Failure mode | Measured at v0.16.0 |
|---|---|---|
| 1 | The test asserts nothing at all | 11 of 58 tests (18%) |
| 2 | A step's result is discarded | 1 true instance |
| 3 | **Only negative outcomes are asserted — no positive control** | the case above; hid both credential bugs |

Mode 3 is the dominant one and the one no existing tool catches. It is the classic
experimental-design flaw: without a positive control, a null result is uninterpretable.
The specs that *did* find these bugs — `poc/protocol-audit/*.star` — all pair a statement
that must succeed with one that must fail, and that pairing is the entire reason they
worked.

An agent will reproduce all three modes, and mode 3 most readily of all, because
"assert the fault caused a failure" is the obvious thing to write when the subject of the
test is a fault. Closing Gaps 1–7 without addressing this makes Faultbox excellent at
helping an agent produce green tests at scale that cannot fail — which is strictly worse
than being slow to use.

Market context: the category has moved to agent framing (LocalStack: "enables teams and
AI agents to validate code without ever touching the cloud"). Our differentiator is that
we are the *adversary* in the loop, not just the sandbox. An adversary that certifies
vacuous tests is not an adversary.

## The gaps

### Gap 1 — `faultbox check`: validation without execution

`star.Runtime.LoadFile()` already validates syntax, topology, service references and the
lambda AST denylists without launching anything — it is simply not exposed as a command.

Add `faultbox check <spec.star> [--format json]` and the MCP tool `check_spec`. Output is
a list of findings sharing the shape of run-time diagnostics, so an agent parses one
structure for both.

Per [open question 4](#open-questions), `check` also runs plan enumeration and reports
fan-out cost, so an agent learns at authoring time that its `choose()` axes multiply to
4,096 leaves.

### Gap 2 — machine-readable error taxonomy

Run-time diagnostics have codes; load-time and infrastructure errors are prose
`fmt.Errorf` strings. An agent cannot branch on prose.

The existing shape is already right:

```go
Diagnostic{Level, Code, Message, Suggestion, Service, Syscall}
```

Six codes exist: `FAULT_FIRED_BUT_SUCCESS`, `FAULT_NOT_FIRED`, `SERVICE_CRASHED`,
`TIMEOUT_DURING_FAULT`, `ASSERTION_MISMATCH`, `MULTIPLE_FAULTS_INTERACTION`. Extend the
same vocabulary to load-time and infrastructure failures — `SPEC_SYNTAX`,
`SPEC_UNKNOWN_SERVICE`, `SPEC_FORBIDDEN_LAMBDA`, `SPEC_UNKNOWN_KWARG`,
`HEALTHCHECK_TIMEOUT`, `LAUNCH_EXEC_NOT_FOUND`, `IMAGE_PULL_FAILED`, … — carried in
`--format json` and every MCP result.

**Every code carries `suggestion`: the agent's next move.** A code without a suggestion
is a code the agent has to guess about, which defeats the purpose. This is a requirement,
not a nicety, and the milestone plan gates on it.

### Gap 8 — detecting specs that cannot fail

**New in this document**, and reshaped by the M0 measurement above — the original sketch
of this gap assumed vacuity was mostly mode 1 (tests asserting nothing). Measuring found
mode 3 dominant, so the design leads with that.

Three diagnostics, one per failure mode, in descending order of value:

| Code | Fires when | Mode |
|---|---|---|
| `NO_POSITIVE_CONTROL` | An interface is stepped, but no test ever asserts that a step on it **succeeds** | 3 |
| `TEST_NO_ASSERTIONS` | A test ran to completion and evaluated zero assertions | 1 |
| `STEP_RESULT_DISCARDED` | A step's return value is never bound or asserted on | 2 |

#### `NO_POSITIVE_CONTROL` — the one that matters

Per interface, across the whole suite, ask: *does any test assert that a step on this
interface succeeded?* If every assertion about it is negative — `not resp.ok`,
`resp.status >= 500`, `assert_true(r.error != "")` — then the suite has no evidence the
interface works at all, and every one of those assertions is equally satisfied by a
totally broken client.

This is a **suite-level** property, not a per-test one, which is what makes it new. Any
single fault-injection test asserting failure is correct and normal. What is wrong is a
*suite* in which that is the only kind of assertion an interface ever receives. It has to
be computed across all tests, which is why no per-test lint would find it.

The suggestion writes itself, and is the shape `poc/protocol-audit/` uses:

```
Interface 'pg.main' is only ever asserted to FAIL. Add a test that asserts a
step succeeds without faults injected — otherwise a completely broken client
satisfies every assertion in this suite.
```

Ordering note: this is a run-time/suite-level diagnostic, since deciding whether an
assertion is "positive" or "negative" from the AST alone is unreliable. `check` reports
the other two; this one reports after a run. That asymmetry is accepted — see
[open question 7](#open-questions).

#### `TEST_NO_ASSERTIONS` — mode 1

A run-time count. The runtime already routes every `assert_*`/`expect_*`/`eventually`
call, so counting them per test is bookkeeping. Measured: 11 of 58 tests at v0.16.0, all
in one file (`poc/gvisor-rfc054/faultbox.star`, whose tests print observations rather than
assert on them).

Must treat `test(..., expect=...)` expectation evaluation as an assertion, or it fires on
correct specs — see [open question 6](#open-questions).

#### `STEP_RESULT_DISCARDED` — mode 2, and the weakest of the three

A load-time AST check, so it reaches the agent through `check` before a run. But M0 found
only **one** true instance in the entire repository, and a naive detector produced two
false positives out of three hits — `e.fields.get("event")` matches the same
`ident.ident.ident(...)` shape as `db.main.exec(...)`.

Kept because it is cheap and it catches a real mistake, but explicitly the least valuable
of the three, and it must resolve the receiver to a declared service interface rather than
pattern-matching on shape. **If that resolution proves unreliable, cut this diagnostic
rather than ship a noisy one** — the plan gates on the false-positive rate.

#### Why this is tractable

- **Vacuity as a concept already exists**: `internal/star/temporal.go` emits a
  `vacuous_property` warning when a temporal property is vacuously satisfied.
- **The nearest existing diagnostic**: `FAULT_FIRED_BUT_SUCCESS` already says "a fault
  fired and the test passed anyway — maybe the service isn't checking errors". That is this
  same instinct, applied to faults. `NO_POSITIVE_CONTROL` is its mirror image: *the test
  only ever asserts failure, so it proves nothing about success.*
- **Load-time AST scanning is routine**: `chooseaxes.go` scans for `choose()` axes;
  `validateMonitorLambdasInSource` enforces AST denylists at load.
- **The suite-level event log already carries what mode 3 needs**: step results and their
  assertions are both in the trace.

#### What it would have caught

`NO_POSITIVE_CONTROL` would have flagged `testops/corpus/postgres_fault_basic.star` — the
CI spec that exercised the broken path on every PR and passed — on the day it was written.
That is the whole case for this gap.

All three are **warnings, not errors.** Discarding a result is legitimate in `seed=`; a
suite may be legitimately negative-only mid-development. A tool that refuses to run such
specs is a tool people work around.

### Gaps 3–7 (deferred past v0.17.0)

Unchanged in substance from the issue body; restated here so this document is complete.

- **Gap 3 — MCP bundle ops with context discipline.** `list_bundles`,
  `bundle_summary` (no events), `query_trace` (paginated), `get_plan`. Unblocks
  `faultbox plan --suggest --strategy=llm`, which currently fails with an error naming
  v0.14.0.
- **Gap 4 — DSL ground truth for machines.** `faultbox schema --format json` generated
  from the same source as `typings/__builtins__.pyi`; `llms.txt` / `llms-full.txt`;
  `search_recipes(query)` over the embedded stdlib so agents copy real patterns instead
  of hallucinating API.
- **Gap 5 — agent-safe mode.** `faultbox mcp --safe`: plan cost cap, reject remote-service
  mutation, wall-clock budget, workspace-restricted spec paths.
- **Gap 6 — session tools.** `start_session` / `inject` / `release` / `events_since` /
  `stop_session` over the existing hold-queue scheduler. Turns a batch tool into something
  an agent can converse with. Cut first if the epic slips.
- **Gap 7 — CI surface for agent-driven PRs.** Job summary, `trace-summary.json`
  (bundle_summary shape), `.fb` bundle as artifact.

## Also in v0.17.0: the overdue deprecation removals

Promised for v0.14.0, still present at v0.16.1. Users currently see warnings naming a
version two behind the one they are running:

```
warning: `faultbox generate` is deprecated; use `faultbox plan --suggest` instead. Removal in v0.14.0.
warning: stdout() is deprecated (RFC-044); use observe.stdout instead. Removal in v0.14.0.
```

Remove `faultbox generate`, the top-level `stdout()` / `stderr()`, and the legacy decoder
constructors (`json_decoder()`, `logfmt_decoder()`, `regex_decoder()`). These are the only
breaking changes in v0.17.0, and they are pre-announced — twice over.

## Non-goals

- **"Intent → spec" generation inside Faultbox.** Authoring is the agent's job. Faultbox
  provides ground truth (schema, recipes, `check`) and verification. Another LLM wrapper
  inside the tool would be a worse copy of the agent already driving it.
- **A full LSP server.** `get_schema` + `check_spec` cover the agent need; LSP serves
  humans and can wait ([vscode-autocomplete](../design/vscode-autocomplete.md) Phase 3).
- **Making vacuity an error.** See Gap 8 — warnings only. A tool that refuses to run a
  spec because its `seed=` ignores a result is a tool people work around.
- **Judging assertion *strength*.** `assert_true(True)` is vacuous in spirit and stays out
  of scope — it appears in `poc/mysql-rfc022-repro`, and detecting it well means deciding
  what a "real" assertion is. `NO_POSITIVE_CONTROL` deliberately asks a **structural**
  question instead — *does any test assert success on this interface?* — which is decidable
  without judging any individual assertion.

## Impact

- **Breaking:** only the pre-announced deprecation removals.
- **New surface:** one CLI command (`check`), one MCP tool (`check_spec`), two diagnostic
  codes, and an error-code taxonomy over existing failure paths.
- **On existing specs:** the two new diagnostics are warnings, so no currently-passing
  spec starts failing. Expect them to fire widely on first run — `poc/` specs included,
  which is itself the evidence that the gap is real.
- **Docs:** a "Driving Faultbox as an agent" guide, and `docs/positioning.md` gains the
  agent-first premise this RFC cites, which was never actually recorded there.

## Alternatives considered

**Fix the docs instead.** v0.16.1 already added "Pattern 0: assert on every step" to the
patterns guide and the protocols reference. Necessary, insufficient: documentation is
advice, and the whole premise of an agent-first surface is that the agent needs
*machine-readable* ground truth. Prose that an agent may or may not have read is exactly
the hallucination surface Gap 4 exists to remove.

**Make step results raise on failure.** Would have caught both credential bugs
immediately and needs no new diagnostic. Rejected: in a fault-injection tool a failing
dependency is frequently the thing the spec is deliberately provoking, so raising makes
the common case unwritable and would break every existing spec. The `ok = False` contract
is right; the tooling around it was missing.

**Ship Gap 8 alone as a fast v0.17.0.** Considered and rejected on sequencing: without
`faultbox check` the diagnostics can only surface at run time and in bundles, so the agent
still burns a run to learn its spec asserts nothing. Gaps 1, 2 and 8 together are what
make the finding *cheap*.

**Defer Gap 8 to a later release.** Rejected because the ordering is load-bearing. An
agent-first surface that lands first and vacuity detection that lands second means a
window in which the tooling actively accelerates the production of vacuous specs.

## Open questions

1. **Telemetry.** Do MCP tool invocations count into (opt-in) usage metrics, and what is
   the privacy default? *Not blocking for v0.17.0 — no telemetry in this slice.*
2. **`bundle_summary` size budget.** Hard cap responses at N KB with drill-down, or trust
   `limit` params? *Deferred with Gap 3.*
3. **Session tools scope.** *Deferred; Gap 6 is explicitly last.*
4. **Should `check_spec` run plan enumeration by default?** Proposed answer: **yes**, and
   report cost without failing. Fan-out blowups are exactly the class of mistake an agent
   makes and cannot see, and enumeration launches nothing.
5. **How noisy is `STEP_RESULT_DISCARDED` in practice?** Unknown until measured against
   `poc/` and the recipes corpus. If the false-positive rate in `seed=`/`reset=` bodies is
   high, the check may need to exempt those contexts. **M1 measures before the default is
   fixed** — the plan gates on this rather than guessing.
6. **Does `TEST_NO_ASSERTIONS` need to understand `expect=`?** A `test(..., expect=...)`
   declaration asserts without an `assert_*` call in the body. The counter must treat
   expectation evaluation as an assertion or it will fire on correct specs.
7. **`NO_POSITIVE_CONTROL` cannot run in `check`.** Deciding whether an assertion is
   positive or negative from the AST alone is unreliable (`assert_true(not resp.ok)` vs
   `assert_true(resp.ok)` is easy; `assert_true(cond)` where `cond` was computed earlier is
   not). So the most valuable diagnostic is the one an agent cannot get without running —
   which cuts against this release's theme. Accepted for v0.17.0; a conservative AST
   approximation ("no test in this file contains a positive assertion on this interface")
   is possible future work if the run-time version proves its worth.

## Dependencies

- [RFC-042](0042-exploration-plan.md) — plan enumeration, for `check`'s cost reporting.
- [RFC-041](0041-temporal-properties.md) — the `vacuous_property` precedent, and the
  `expect=` surface open question 6 must handle.
- [RFC-043](0043-nondeterministic-operators.md) — `--strategy=llm` reservation (Gap 3).
- [RFC-044](0044-spec-language-simplification.md) — the deprecations being removed.
