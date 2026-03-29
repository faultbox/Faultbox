# Chapter 2: Writing Your First Test

**Duration:** 20 minutes
**Prerequisites:** [Chapter 0 (Setup)](00-setup.md) completed

## Goals & Purpose

In chapter 1 you injected faults manually. That's useful for exploration,
but not for building confidence. You need **repeatable specifications** —
files that describe your system and what "working" means.

The key insight: **a distributed system's behavior is defined by its topology
(who talks to whom) and its expectations (what should happen)**. If you can
write both down, you can test them automatically.

This chapter teaches you to think in terms of:
- **Services** — the components of your system
- **Interfaces** — how they communicate
- **Healthchecks** — what "ready" means
- **Test functions** — what "correct" means

After this chapter, you'll have a mental framework for specifying any
system: "these services exist, they talk over these protocols, and when I
do X, I expect Y."

## Starlark

Faultbox uses Starlark — a Python dialect designed for configuration.
If you know Python, you know Starlark. No imports, no classes, no exceptions.
Just functions, variables, and data. Configuration is code.

## The mock database

The mock-db binary was built in Chapter 0 (`make build` or `make demo-build`).

It's a simple TCP key-value store:
```
PING       -> PONG
SET k v    -> OK
GET k      -> value or NOT_FOUND
QUIT       -> closes connection
```

## Your first spec file

Create `my-first-test.star`. The binary path depends on your platform:

**Linux:**
```python
db = service("db", "bin/mock-db",
    interface("main", "tcp", 5432),
    healthcheck = tcp("localhost:5432"),
)

def test_ping():
    resp = db.main.send(data="PING")
    assert_eq(resp, "PONG")
```

**macOS (Lima):** use the cross-compiled path:
```python
db = service("db", "bin/linux-arm64/mock-db",
    ...
```

Let's break this down:

**The service declaration** says: "there is a component called 'db' that runs
`/tmp/mock-db`, listens on TCP port 5432, and is ready when it accepts TCP
connections."

**The test function** says: "when I send PING, I expect PONG." That's your
specification — a concrete, executable claim about system behavior.

## Run it

**Linux:**
```bash
bin/faultbox test my-first-test.star
```

**macOS (Lima):**
```bash
vm bin/linux-arm64/faultbox test my-first-test.star
```

```
--- PASS: test_ping (150ms, seed=0) ---
  syscall trace (42 events):

1 passed, 0 failed
```

## What just happened?

For each `test_*` function, Faultbox:

```
1. Start all services (in dependency order)
2. Wait for healthchecks to pass
3. Run the test function
4. Stop all services (SIGTERM -> SIGKILL after 2s)
5. Report result + syscall trace
```

Every test gets **fresh instances** — services are restarted between tests.
No state leaks. This is critical: if test A's data affected test B's result,
your specs would be unreliable.

## Anatomy of a service declaration

```python
db = service("db", "/tmp/mock-db",
    interface("main", "tcp", 5432),
    env = {"PORT": "5432"},
    healthcheck = tcp("localhost:5432"),
)
```

| Parameter | What it means | Why it matters |
|-----------|---------------|----------------|
| `"db"` | Service name | Appears in logs and traces — you'll use it to filter events |
| `"/tmp/mock-db"` | Binary path | The actual program to run |
| `interface("main", "tcp", 5432)` | Communication endpoint | Declares how other services connect |
| `env = {...}` | Environment variables | Configure the process without changing code |
| `healthcheck = tcp(...)` | Readiness probe | Faultbox won't run tests until this passes |

**The mental model:** a service declaration is a contract: "this binary, on
this port, with this protocol, is ready when this check passes."

## Interface addressing

Once declared, access the interface programmatically:

```python
db.main.addr        # "localhost:5432"
db.main.host        # "localhost"
db.main.port        # 5432
```

For TCP interfaces, `.send()` exchanges data:
```python
resp = db.main.send(data="PING")
```

For HTTP interfaces (chapter 3), you get `.get()`, `.post()`, etc.

## Assertions

Two basic assertions — simple but powerful:

```python
assert_eq(actual, expected)           # equality check
assert_true(condition, "message")     # boolean check
```

They fail the test immediately with a clear error message. No fuzzy matching,
no eventual consistency — did the value match or not?

## Adding more tests

```python
def test_ping():
    resp = db.main.send(data="PING")
    assert_eq(resp, "PONG")

def test_set_and_get():
    db.main.send(data="SET greeting hello")
    resp = db.main.send(data="GET greeting")
    assert_eq(resp, "hello")
```

Run all (on Linux; on macOS prefix with `vm bin/linux-arm64/`):
```bash
bin/faultbox test my-first-test.star
```

Run one:
```bash
bin/faultbox test my-first-test.star --test set_and_get
```

**Each test is independent.** The database restarts between tests, so
`test_set_and_get` doesn't depend on `test_ping` having run first. This is
by design — independent tests are parallelizable and debuggable.

## What you learned

- `.star` files are executable specifications: topology + expectations
- `service()` declares what a component is and how it communicates
- `interface()` defines the protocol boundary
- `healthcheck` ensures the service is ready before testing
- `test_*` functions are discovered and run automatically with fresh instances
- `assert_eq` and `assert_true` validate concrete expectations

**The mental framework:** for any system you want to test, ask:
1. What are the services?
2. What protocols do they speak?
3. How do I know they're ready?
4. What should happen when I interact with them?

## What's next

You can now write tests for the happy path — "when everything works, here's
what I expect." But the interesting questions are about failure:

- What happens when the database can't write to disk?
- What happens when the API can't reach the database?
- What happens when writes are slow?

Chapter 3 introduces `fault()` — a way to inject specific failures into
specific services during specific test scenarios.

## Exercises

1. **Write and read**: Add a test that does `SET mykey myvalue`, then
   `GET mykey`, and asserts the result is `"myvalue"`.

2. **Missing key**: Add a test that does `GET nonexistent` and asserts
   the result is `"NOT_FOUND"`. (This tests the error path — just as
   important as the happy path.)

3. **Overwrite**: Add a test that sets the same key twice with different
   values, then reads it back. Which value do you get? Is this the right
   behavior?

4. **Healthcheck timeout**: Change the healthcheck to use a wrong port:
   `healthcheck = tcp("localhost:9999")`. What happens? How long does it
   wait? (This builds intuition for healthcheck configuration.)
