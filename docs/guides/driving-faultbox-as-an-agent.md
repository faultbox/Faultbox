# Driving Faultbox as an agent

The agent-facing counterpart of the [tutorial](../tutorial/). The tutorial
teaches a human what to test; this page describes the loop a program runs and
the structured surfaces it reads.

Faultbox assumes [the primary spec author is an agent](../positioning.md#who-writes-the-specs).
That is a statement about volume, not capability: humans decide what is worth
testing and read the verdicts, agents write and iterate the specs encoding those
decisions.

## The loop

```
                 ┌──────────────────────────────────┐
                 ▼                                  │
 write spec → check (ms) → fix ─────────────────────┘
                 │
                 ▼
              run (s–min) → read summary → drill into bundle only if needed
                 │
                 ▼
              verdict + diagnostics
```

The rule that matters: **`check` before `test`.** Validation costs milliseconds
and needs no Docker; a run costs tens of seconds because it starts containers.
Most authoring mistakes are catchable without ever launching anything.

## 1. Validate without running

```bash
faultbox check spec.star --format json
```

```json
{
  "schema_version": 1,
  "spec": "spec.star",
  "ok": false,
  "tests": [],
  "plan_instances": 0,
  "findings": [
    {
      "level": "error",
      "code": "SPEC_LOAD_FAILED",
      "message": "load spec.star: service(\"db\"): unknown keyword argument \"img\"",
      "suggestion": "The spec parsed but failed while loading. …"
    }
  ]
}
```

Branch on `code`. Every code carries a `suggestion` — that is enforced by a
test, not by convention. The full list is in
[diagnostic-codes.md](../diagnostic-codes.md).

Exit codes: `0` clean or warnings only, `1` usage error, `2` error-level
findings. Warnings do not fail the command.

Pass `--max-instances N` to catch fan-out blowups at authoring time. A
`choose()` cross-product that multiplies to thousands of leaves is the mistake
that is invisible in the source and expensive at run time.

## 2. Run

```bash
faultbox test spec.star --format json
```

Human-readable output goes to stderr, JSON to stdout, so both can be consumed in
one invocation.

Per test the JSON carries `result`, `reason`, `assertions`, `diagnostics` and
the event log. **Read `assertions` before trusting `result`** — a passing test
that evaluated zero assertions has not verified anything, and Faultbox says so
with `TEST_NO_ASSERTIONS`.

## 3. Read results without drowning

The full event log is large. Start with what is small:

| Want | Read |
|---|---|
| Did it pass? | `pass` / `fail` / `inconclusive` counts |
| Why did it fail? | per-test `reason` and `diagnostics` |
| Does the suite prove anything? | run-level `diagnostics` |
| What actually happened? | the event log, last |

Run-level diagnostics are the ones no single test can produce. `NO_POSITIVE_CONTROL`
is the one to respect: it means every assertion about an interface is satisfied
by a client that cannot connect at all.

## 4. The trap to avoid

A protocol step that fails returns `ok = False`. It does **not** raise, because
in a fault-injection tool a failing dependency is frequently the thing the spec
is deliberately provoking.

The cost of that contract is that an ignored result is indistinguishable from a
successful one:

```python
db.main.exec(sql = "INSERT INTO t VALUES (1)")            # may have failed silently

r = db.main.exec(sql = "INSERT INTO t VALUES (1)")        # correct
assert_true(r.ok, "insert failed: %s" % r.error)
```

And the subtler form — assert that something **succeeds**, not only that it
fails:

```python
def test_write_fails_under_disk_full():
    def scenario():
        r = db.main.exec(sql = "INSERT INTO t VALUES (1)")
        assert_true(not r.ok, "expected failure")          # negative control
    fault(db.main, error(query = "INSERT*"), run = scenario)

def test_write_works_normally():
    r = db.main.exec(sql = "INSERT INTO t VALUES (1)")
    assert_true(r.ok, "insert failed: %s" % r.error)       # POSITIVE control
```

Without the second test, a completely broken client passes the first one. This
is not hypothetical: it is how two credential bugs survived three releases in
CI. See [Pattern 0](spec-patterns.md#pattern-0-assert-on-every-step).

## MCP tools

`faultbox mcp` speaks MCP over stdio.

| Tool | Cost | Use |
|---|---|---|
| `check_spec` | ms | Validate. Same code path as `faultbox check`. |
| `list_tests` | ms | Discover test names. |
| `init_spec` / `init_from_compose` | ms | Scaffold a starter spec. |
| `generate_faults` | ms | Topology analysis → suggested faults. |
| `run_test` / `run_single_test` | s–min | Execute. |

`check_spec` on a broken spec is a **successful** call returning a finding, not
a tool error — the question "is this spec valid?" was answered completely. Tool
errors are reserved for malformed calls.

## Ground truth

Prefer these over prose when generating a spec:

- [`typings/__builtins__.pyi`](../../typings/__builtins__.pyi) — every builtin's
  signature
- [`recipes/`](../../recipes/) — the embedded stdlib, real working patterns
- [`poc/protocol-audit/`](../../poc/protocol-audit/) — specs that assert
  properly, per protocol
- [diagnostic-codes.md](../diagnostic-codes.md) — every code and its meaning
- [starlark-dialect.md](../starlark-dialect.md) — where the dialect differs from
  Python

## Known limits

- **`check` cannot tell you whether a suite is meaningful.** `NO_POSITIVE_CONTROL`
  and `TEST_NO_ASSERTIONS` need a run. Deciding whether an assertion is positive
  or negative from source alone is unreliable, and a check that guessed would be
  worse than one that admits the boundary.
- **The error taxonomy is incrementally adopted.** Errors carry codes where the
  work has been done; elsewhere they report without one rather than being given a
  guess.
- **There is no JSON schema export yet.** Planned as RFC-052 Gap 4, along with
  `llms.txt` and a searchable recipe corpus.
