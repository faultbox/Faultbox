# Choosing Fault Levels: Start from the Bug Class

You don't choose a mechanism - you choose a bug to close. Decide which
of the six bug classes you're testing for, and the fault level follows.
This guide gives the mapping first, then the mechanics of each level.

## Which bug are you closing?

| Bug class | Level | Typical fault |
|---|---|---|
| **Unhandled dependency failure** | Either - syscall for "the whole dependency is gone" (`connect=deny("ECONNREFUSED")`), protocol for "this operation is refused" (`error(query="INSERT*")`) | Start syscall-broad, refine protocol-precise |
| **Missing timeout / runaway retry** | Protocol - slowness must hit one operation while the service stays healthy | `delay(path="/search*", delay="2s")` |
| **Non-idempotent retry** | Syscall `hold()` + protocol `error()` - the failure must land *between* the side effect and the response | `write=hold("charge")`, then fail the reply |
| **Partial failure / torn writes** | Syscall - the only level that can split a transaction mid-flight | `write=deny("EIO", trigger="after=1")`, `fsync=deny("EIO")` |
| **Failed recovery** | Either level for the fault; the *assertion* is temporal | scoped `fault(...)`, then `eventually(recovered)` |
| **Bad responses, not outages** | Protocol only - response rewriting | `response(path="/quote", status=200, body="<garbage>")` |

The rest of this guide explains what each level can and cannot do, so
you can refine these defaults.

## Two levels, one API

```python
# Syscall level: affects ALL writes by the db process
fault(db, write=deny("EIO"), run=scenario)

# Protocol level: affects only INSERT queries to the orders table
fault(db.pg, error(query="INSERT INTO orders*"), run=scenario)
```

Same `fault()` builtin. The first argument determines the level:
- **Service** (`db`) → syscall level
- **Interface reference** (`db.pg`) → protocol level

## When to use syscall faults

Syscall faults simulate **infrastructure failures** - the kind that affect
everything a service does, not just specific operations.

| Scenario | Fault | What it simulates |
|---|---|---|
| Server disk dies | `write=deny("EIO")` | Every write fails |
| Disk fills up | `write=deny("ENOSPC")` | No space for any write |
| Network cable unplugged | `connect=deny("ECONNREFUSED")` | Can't reach anything |
| Network is slow | `connect=delay("2s")` | Every connection takes 2s |
| Total partition | `partition(svc_a, svc_b)` | Bidirectional network split |

**Strengths:**
- Works on ANY binary - no protocol support needed
- Catches unexpected write paths (logging, temp files, metrics)
- Simulates real infrastructure failures accurately
- Simple: one line tests a broad category

**Weaknesses:**
- Coarse: `write=deny("EIO")` blocks stdout, TCP, files - everything
- Can't target specific queries, paths, or commands
- May break service health (can't respond to healthchecks under write fault)

**Best for:** "is the infrastructure broken?" questions.

## When to use protocol faults

Protocol faults simulate **application-level failures** - one operation
fails while the rest of the service works normally.

| Scenario | Fault | What it simulates |
|---|---|---|
| One SQL query fails | `error(query="INSERT*")` | DB rejects a specific insert |
| HTTP upstream returns 429 | `response(path="/api/*", status=429)` | Rate limiting |
| Kafka message dropped | `drop(topic="orders")` | Message loss on one topic |
| Redis SET fails | `error(command="SET")` | Write to cache fails |
| Slow specific endpoint | `delay(path="/search*", delay="2s")` | One endpoint is slow |

**Strengths:**
- Precise: target specific queries, paths, commands, topics
- Realistic: real services fail at the query level, not the disk level
- Service stays healthy - healthchecks and other operations work normally
- Tests error handling for specific code paths

**Weaknesses:**
- Only works for supported protocols (HTTP, Postgres, Redis, Kafka, etc.)
- Proxy adds latency (usually <1ms, but measurable)
- Can't simulate low-level failures (disk corruption, kernel panics)

**Best for:** "does this specific operation handle errors correctly?" questions.

## Decision table

| Question | Level | Example |
|---|---|---|
| "What if the DB server is completely down?" | Syscall | `connect=deny("ECONNREFUSED")` |
| "What if this INSERT query fails?" | Protocol | `error(query="INSERT INTO orders*")` |
| "What if the disk is full?" | Syscall | `write=deny("ENOSPC")` |
| "What if this HTTP endpoint returns 500?" | Protocol | `response(path="/api/v1/orders", status=500)` |
| "What if the network is slow?" | Syscall | `connect=delay("2s")` |
| "What if this one Kafka topic drops messages?" | Protocol | `drop(topic="order-events")` |
| "What if Redis SET fails but GET works?" | Protocol | `error(command="SET")` |
| "What if two services can't talk to each other?" | Syscall | `partition(api, db)` |
| "What if the WAL fsync fails?" | Syscall | `fsync=deny("EIO")` |
| "What if the gRPC method returns UNAVAILABLE?" | Protocol | `error(method="/orders.OrderService/Create")` |

## Combining both levels

The most thorough tests use both:

```python
def test_degraded_system():
    """Upstream is rate-limited AND local disk is slow."""
    def scenario():
        # POST is blocked by the proxy (protocol fault).
        resp = api.post(path="/orders", body='...')
        assert_eq(resp.status, 429)

        # GET still works but DB is slow (syscall fault).
        resp = api.get(path="/orders/1")
        assert_true(resp.duration_ms > 400)
    
    def with_slow_db():
        fault(db, write=delay("500ms"), run=scenario)
    
    fault(api.http,
        response(method="POST", path="/orders*", status=429),
        run=with_slow_db,
    )
```

**When to combine:**
- Testing graceful degradation (some operations fail, others slow)
- Testing cascading failures (upstream error + local resource issue)
- Testing that partial failures don't corrupt state

## Progression for a new project

Work through the bug classes in order of incident frequency:

1. **Unhandled dependency failure first** - `connect=deny` +
   `error()` for every dependency of your critical flow. This is the
   class behind most production incidents, and one afternoon covers it.

2. **Timeouts and retries next** - `delay()`/`slow()` on the same
   dependencies. Assert latency bounds, not just status codes.

3. **Then the precision classes** for flows where correctness is money:
   non-idempotent retries (`hold()`), torn writes
   (`deny(trigger="after=N")`), recovery (`eventually()` after the
   fault clears), bad responses (`response()` rewriting).

Most projects get 80% of the value from step 1 alone - which is also
why "which bug class has no spec yet?" is the right review question,
not "which mechanism haven't we used?"
