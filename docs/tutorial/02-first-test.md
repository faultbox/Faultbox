# Chapter 2: Writing Your First Test

In chapter 1 you injected faults manually. Now you'll write automated tests
in Starlark — Faultbox's configuration language.

## Starlark

Starlark is a Python dialect designed for configuration. If you know Python,
you know Starlark. The key difference: no imports, no classes, no exceptions.
Just functions, variables, and data.

Faultbox uses a single `.star` file to declare your system topology AND your
test cases. Configuration is code.

## The mock database

`poc/mock-db/` is a simple TCP key-value store. Build it:

```bash
go build -o /tmp/mock-db ./poc/mock-db/
```

It accepts text commands over TCP:
```
PING       → PONG
SET k v    → OK
GET k      → value or NOT_FOUND
QUIT       → closes connection
```

## Your first spec file

Create `my-first-test.star`:

```python
db = service("db", "/tmp/mock-db",
    interface("main", "tcp", 5432),
    healthcheck = tcp("localhost:5432"),
)

def test_ping():
    resp = db.main.send(data="PING")
    assert_eq(resp, "PONG")
```

This declares:
- A **service** named "db" that runs `/tmp/mock-db`
- It has one **interface** called "main" on TCP port 5432
- A **healthcheck** that verifies it's accepting connections
- A **test** that sends PING and expects PONG

## Run it

```bash
faultbox test my-first-test.star
```

Output:
```
--- PASS: test_ping (150ms, seed=0) ---
  syscall trace (42 events):

1 passed, 0 failed
```

## What just happened?

For each test, Faultbox:

```
1. Start all services (in dependency order)
2. Wait for healthchecks to pass
3. Run the test function
4. Stop all services
5. Report result + syscall trace
```

The database was started, health-checked, tested, and stopped — automatically.

## Service lifecycle

```python
db = service("db", "/tmp/mock-db",
    interface("main", "tcp", 5432),
    env = {"PORT": "5432"},            # environment variables
    healthcheck = tcp("localhost:5432"),
)
```

| Parameter | Purpose |
|-----------|---------|
| `"db"` | Service name (used in logs and traces) |
| `"/tmp/mock-db"` | Path to the binary |
| `interface(...)` | Network interface declaration |
| `env = {...}` | Environment variables for the process |
| `healthcheck` | How to verify the service is ready |

## Interface addressing

Once declared, access the interface:

```python
db.main.addr        # "localhost:5432"
db.main.host        # "localhost"
db.main.port        # 5432
```

For TCP interfaces, call `.send()` to exchange data:
```python
resp = db.main.send(data="PING")
```

## Assertions

Faultbox provides two basic assertions:

```python
assert_eq(actual, expected)           # equality check
assert_true(condition, "message")     # boolean check
```

They fail the test immediately with a clear error message.

## Adding more tests

Extend your spec file:

```python
db = service("db", "/tmp/mock-db",
    interface("main", "tcp", 5432),
    healthcheck = tcp("localhost:5432"),
)

def test_ping():
    resp = db.main.send(data="PING")
    assert_eq(resp, "PONG")

def test_set_and_get():
    db.main.send(data="SET greeting hello")
    resp = db.main.send(data="GET greeting")
    assert_eq(resp, "hello")
```

Run all tests:
```bash
faultbox test my-first-test.star
```

Run one test:
```bash
faultbox test my-first-test.star --test set_and_get
```

## What you learned

- `.star` files declare topology (services) and tests (functions)
- `service()` defines a binary, its interfaces, env vars, and healthcheck
- `interface()` declares how services communicate (TCP, HTTP)
- `test_*` functions are discovered and run automatically
- `assert_eq` and `assert_true` validate behavior
- Each test gets fresh service instances (restarted between tests)

## Exercises

1. **Write and read**: Add a test that does `SET mykey myvalue`, then
   `GET mykey`, and asserts the result is `"myvalue"`.

2. **Missing key**: Add a test that does `GET nonexistent` and asserts
   the result is `"NOT_FOUND"`.

3. **Overwrite**: Add a test that sets the same key twice with different
   values, then reads it back. Which value do you get?

4. **Multiple interfaces**: The mock-db could theoretically listen on
   two ports. Declare a second interface:
   ```python
   db = service("db", "/tmp/mock-db",
       interface("main", "tcp", 5432),
       interface("admin", "tcp", 5433),
   )
   ```
   What happens when you run this? (Hint: the binary only listens on PORT.)
