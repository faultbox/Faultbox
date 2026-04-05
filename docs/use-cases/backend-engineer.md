# Use Case: Backend Engineer

**Persona:** Anna, Go backend engineer at a fintech startup

## The Problem

Anna owns an order service that talks to Postgres, Redis, and a payment
gateway. Last month, a 30-second Postgres failover caused double-charges —
the retry logic didn't check idempotency keys.

The team added a fix, but nobody could prove it worked. The only way to test
it was to wait for the next failover.

## Day 1 with Faultbox

Anna installs Faultbox and writes her first spec in 10 minutes:

```python
db = service("postgres",
    interface("main", "postgres", 5432),
    image="postgres:16",
    env={"POSTGRES_PASSWORD": "test"},
    healthcheck=tcp("localhost:5432"),
)

api = service("api",
    interface("public", "http", 8080),
    build="./api",
    env={"DATABASE_URL": "postgres://test@" + db.main.internal_addr + "/testdb"},
    depends_on=[db],
    healthcheck=http("localhost:8080/health"),
)

def test_payment_retry_on_db_failure():
    """Retry after DB failure should not double-charge."""
    # First payment succeeds.
    api.post(path="/payments", body='{"amount": 100, "idempotency_key": "abc"}')

    # DB fails — retry with same idempotency key.
    def db_dies():
        resp = api.post(path="/payments", body='{"amount": 100, "idempotency_key": "abc"}')
        assert_eq(resp.status, 200)  # retry should succeed
    fault(db, write=deny("EIO", label="db failover"), run=db_dies)

    # Verify: only ONE charge, not two.
    resp = db.main.query(sql="SELECT count(*) as n FROM payments WHERE idempotency_key='abc'")
    assert_eq(resp.data[0]["n"], 1)
```

## What She Gets

Proof that the double-charge bug is fixed. She adds this to CI — it runs on
every PR. The next Postgres failover is a non-event.

## Growth Path

- **Week 2:** Adds `--explore=all` for concurrent payment tests — two
  payments for the same order arriving simultaneously.
- **Month 2:** Uses `observe=[wal_stream(...)]` to monitor Postgres WAL
  events and verify transaction isolation.
- **Month 3:** Builds a full failure matrix: Postgres down, Redis down,
  payment gateway timeout, disk full — each is a test case in CI.

## Key Value

Anna gets **proof, not hope**. The assertion either passes or shows her
exactly what went wrong — with syscall traces, ShiViz diagrams, and
replay seeds for deterministic reproduction.
