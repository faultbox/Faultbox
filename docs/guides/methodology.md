# Fault Testing Methodology

How to think about fault testing for distributed systems. This guide
gives you the mental model and process — not which API to call, but
how to approach your system systematically.

## The core question

Every distributed system makes assumptions about its environment:
- "The database is available"
- "Writes succeed"
- "The network is fast"
- "Messages are delivered exactly once"

**Fault testing asks:** what happens when each assumption breaks?

Not "does the system crash?" — that's the easy case. The hard question
is: does the system **behave correctly** when things go wrong? Does it
return useful errors? Does it preserve data integrity? Does it recover?

## The three layers

Fault testing has three layers, each building on the previous:

### Layer 1: Happy path

**What:** the system works correctly when everything is healthy.

```python
def test_create_order():
    resp = api.post(path="/orders", body='{"item":"widget","qty":1}')
    assert_eq(resp.status, 201)
    assert_true("order_id" in resp.body)
```

**Why this comes first:** if the happy path is broken, fault tests are
meaningless. You can't test error handling if the normal path doesn't work.

**How much:** one happy-path test per critical user flow. Not exhaustive
input testing — that's unit tests. Focus on: "does the complete flow work
end-to-end?"

### Layer 2: Fault response

**What:** the system responds correctly when a specific thing breaks.

```python
def test_db_write_failure():
    def scenario():
        resp = api.post(path="/orders", body='{"item":"widget","qty":1}')
        assert_eq(resp.status, 503)
        assert_true("database" in resp.body.lower())
    fault(db, write=deny("EIO"), run=scenario)
```

**Why this matters:** most services have error handling code. But is it
correct? Does `catch (e)` return 503 or does it return 200 with empty data?
Does the error message help an operator diagnose the problem?

**How to think about it:** for each dependency, ask three questions:
1. What if it's **down**? (connect refused)
2. What if it's **slow**? (delay)
3. What if it **errors**? (I/O error, disk full)

That gives you 3 tests per dependency — a good starting point.

### Layer 3: Invariants

**What:** properties that must hold regardless of what goes wrong.

```python
# This monitor runs on EVERY test, under EVERY fault:
def no_negative_stock(event):
    if event.data.get("stock") and int(event.data["stock"]) < 0:
        fail("stock went negative: " + event.data["stock"])

monitor(no_negative_stock, service="inventory")
```

**Why this is the hardest layer:** a test checks one scenario. An
invariant must hold across ALL scenarios — including ones you haven't
thought of. If you test "db write fails → API returns 503" that's a
test. If you assert "stock never goes negative regardless of failure
mode" — that's an invariant.

**Examples of invariants:**
- Money is never created or destroyed (financial systems)
- An order confirmed to the user is always persisted in the database
- No duplicate messages are published to the event bus
- A service that returns 200 has actually committed the data
- Failed operations leave no partial state (atomicity)

## The dependency matrix

The most practical technique for finding what to test.

### Step 1: List your services and dependencies

```
┌──────────┬────────────────┬──────────────┐
│ Service  │ Dependencies   │ Protocol     │
├──────────┼────────────────┼──────────────┤
│ api      │ db, cache, auth│ HTTP         │
│ worker   │ db, kafka, s3  │ HTTP (admin) │
│ auth     │ db             │ gRPC         │
│ db       │ disk           │ Postgres     │
│ cache    │ (memory only)  │ Redis        │
│ kafka    │ disk           │ Kafka        │
└──────────┴────────────────┴──────────────┘
```

### Step 2: For each dependency, enumerate failure modes

```
┌──────────┬────────────┬───────────────────────────────────┐
│ Service  │ Dependency │ Failure Modes                     │
├──────────┼────────────┼───────────────────────────────────┤
│ api      │ db         │ down, slow, query error, disk full│
│ api      │ cache      │ down, slow, stale data            │
│ api      │ auth       │ down, slow, rejects valid token   │
│ worker   │ kafka      │ down, slow, message loss          │
│ worker   │ s3         │ down, slow, permission denied     │
│ worker   │ db         │ same as api→db                    │
└──────────┴────────────┴───────────────────────────────────┘
```

### Step 3: Prioritize by impact

Not all failures are equal. Prioritize by:

1. **Data loss risk** — can this failure cause data to be lost or corrupted?
2. **User impact** — does the user see an error, or does the system silently fail?
3. **Recovery difficulty** — can the system recover automatically, or does it need manual intervention?
4. **Frequency in production** — how often does this actually happen?

Start with the top-left corner: high data-loss risk + high frequency.

### Step 4: Write specs

For each high-priority cell in the matrix, write a test:

```python
# api → db: down
def test_api_db_down():
    def scenario():
        resp = api.post(path="/orders", body='...')
        assert_eq(resp.status, 503)
    fault(api, connect=deny("ECONNREFUSED"), run=scenario)

# api → db: slow
def test_api_db_slow():
    def scenario():
        resp = api.post(path="/orders", body='...')
        assert_true(resp.status in [200, 504])
        assert_true(resp.duration_ms < 5000, "should timeout, not hang")
    fault(db, write=delay("3s"), run=scenario)

# api → db: disk full
def test_api_db_disk_full():
    def scenario():
        resp = api.post(path="/orders", body='...')
        assert_eq(resp.status, 503)
    fault(db, write=deny("ENOSPC"), run=scenario)
```

## Syscall vs protocol: decision framework

| I want to test... | Use | Why |
|---|---|---|
| "DB is completely down" | `fault(api, connect=deny("ECONNREFUSED"))` | Syscall: blocks all connections |
| "This specific SQL query fails" | `fault(db.pg, error(query="INSERT INTO orders*"))` | Protocol: targets one query |
| "Disk is full" | `fault(db, write=deny("ENOSPC"))` | Syscall: affects all writes |
| "Slow reads from one table" | `fault(db.pg, delay(query="SELECT * FROM orders*"))` | Protocol: targets one query |
| "Redis SET fails but GET works" | `fault(cache.redis, error(command="SET"))` | Protocol: targets one command |
| "Total network partition" | `partition(api, db)` | Syscall: blocks connect both ways |
| "HTTP 429 from upstream" | `fault(api.http, response(status=429))` | Protocol: specific HTTP response |
| "Kafka message loss" | `fault(kafka.broker, drop(topic="orders"))` | Protocol: drops specific messages |

**Rule of thumb:**
- **Start with syscall faults** for "is X down?" and "is the disk broken?" — they're simpler and catch broad categories
- **Add protocol faults** for "does this specific query/path/command fail correctly?" — they're more precise
- **Use both** when you need precision on some paths and broad coverage on others

## Process: from zero to covered

### Week 1: Foundation

1. Write a Faultbox spec with your topology (`service()`, `depends_on`, `healthcheck`)
2. Write happy-path tests for your 3-5 most critical user flows
3. Run them — fix any issues

### Week 2: Dependency failures

4. Build the dependency matrix (services × dependencies × failure modes)
5. Write fault tests for the top 10 highest-impact cells
6. Run them — discover missing error handling, fix it

### Week 3: Invariants

7. Identify 3-5 invariants your system must maintain
8. Write monitors that check them
9. Run your existing tests WITH monitors active — they might catch issues you missed

### Week 4: Concurrency & edge cases

10. Add `parallel()` tests for concurrent operations
11. Run `--explore=all` on critical tests to find timing-dependent bugs
12. Add protocol-level faults for precise targeting

### Ongoing

- When you add a new service: add it to the topology, write happy path + top 3 faults
- When you fix a production incident: write a fault test that reproduces it
- When you add a new dependency: add its failure modes to the matrix

## How to know you're done

You're never "done" — but you have good coverage when:

1. **Every dependency has at least "down" and "slow" tests** — the minimum
2. **Your top 3 invariants are monitored** — they run on every test
3. **Production incidents are reproducible** — every past outage has a corresponding fault test
4. **The team writes fault tests for new features** — it's part of the development process, not a separate activity

**The metric:** count of (tested failure modes) vs (possible failure modes from your dependency matrix). 50% is good. 80% is excellent. 100% is unnecessary — diminishing returns after the high-impact failures are covered.

## Anti-patterns

**"Test everything at once"** — don't inject 5 faults simultaneously on day 1. Start with one fault, one service. Understand the behavior. Then combine.

**"Only test the happy path under fault"** — if your test is `fault(db, ..., run=happy_path)` and you only check `resp.status >= 500`, you're testing that the system breaks — not that it breaks correctly. Assert on error messages, response bodies, and side effects.

**"100% fault coverage"** — not every failure mode matters. `fsync=deny("EIO")` on a service that never calls fsync is noise. Focus on failures that happen in production.

**"Faults in unit tests"** — Faultbox tests integration between services. If you're testing a single function's error handling, a Go `test` with a mock is simpler. Use Faultbox when you need to verify behavior across service boundaries.

**"Fix the test, not the code"** — when a fault test fails, the instinct is to adjust the assertion. Resist it. The failing test is showing you a real bug — the service doesn't handle this failure correctly. Fix the service, then the test passes.
