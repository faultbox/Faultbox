# Chapter 17: Mock Services — Stubs Without Containers

**Duration:** 25 minutes
**Prerequisites:** [Chapter 0 (Setup)](../00-prelude/00-setup.md), some familiarity with [Chapter 9 (Containers)](09-containers.md)

## Goals & Purpose

Your system under test (SUT) probably has a handful of dependencies
that aren't really the thing you're testing — they just need to be
*there* for the app to boot. An OIDC JWKS endpoint. A feature-flag
service. A metadata server that returns four lines of JSON. A Redis
that caches configuration values.

Running each one as a real container is overkill:

- **Slow** — pulling and booting real Kafka/MongoDB/Postgres takes 5-30
  seconds before your first test runs.
- **Heavy** — Docker images are 100s of MB each, and you probably only
  exercise 1% of their functionality.
- **Wrong abstraction** — the dependency's *behavior* is declarative
  data ("this endpoint returns this JSON") but you're modelling its
  *deployment* (Dockerfile, volume mounts, env vars).

Faultbox **mock services** (shipped in v0.8.0) let you stand up these
dependencies entirely in Starlark — no Dockerfile, no sidecar process.
This chapter teaches you to:

- **Use `mock_service()`** for HTTP, HTTP/2, TCP, UDP, and gRPC stubs.
- **Use `@faultbox/mocks/` stdlib** for Kafka, Redis, and MongoDB.
- **Compose mocks with real services** in the same topology.
- **Fault your mocks** — `fault_assumption()` works on mocks exactly as
  on real services.
- **Know what mocks are for and what they aren't** — when to reach for
  a real container instead.

After this chapter, you can test a microservice that talks to five
dependencies with zero containers and first-test latency under a
second.

## The JWKS problem

Your service validates JWTs at startup. The JWT library fetches
`/.well-known/openid-configuration/jwks` from the issuer URL in the
token. Without a reachable JWKS endpoint, the library panics and the
app crashes before it serves one request.

You don't care about OIDC discovery for the functional test — you care
about what happens *after* auth succeeds. But every test run has to
stand up something at that URL.

Before v0.8, options were all bad:

1. **Custom Docker image** with a tiny HTTP server — another Dockerfile
   to maintain, image to pull, container to wait for.
2. **`python:3-alpine` + `http.server` + volume mount** — works but
   imports Python into the dep graph, fights volume-mount semantics
   on macOS, still boots a container.
3. **Disable the code path in the SUT** — add a `--skip-auth` flag or
   similar conditional — changes production code to make tests pass.

With `mock_service()`:

```python
auth = mock_service("auth",
    interface("http", "http", 8090),
    routes = {
        "GET /.well-known/openid-configuration": json_response(200, {
            "issuer":   "http://auth:8090",
            "jwks_uri": "http://auth:8090/.well-known/openid-configuration/jwks",
        }),
        "GET /.well-known/openid-configuration/jwks": json_response(200, {
            "keys": [{
                "kty": "OKP", "crv": "Ed25519", "kid": "test-1",
                "x":   "11qYAYKxCrfVS_7TyWQHOg7hcvPapiMlrwIaaPcHURo",
            }],
        }),
    },
)
```

Eleven lines of spec, no container, no image, first test runs in
<100ms cold.

## Walk-through: auth stub + SUT + test

### 1. Declare the mock

```python
# faultbox.star

auth = mock_service("auth",
    interface("http", "http", 8090),
    routes = {
        "GET /.well-known/openid-configuration": json_response(200, {
            "issuer":   "http://auth:8090",
            "jwks_uri": "http://auth:8090/.well-known/openid-configuration/jwks",
        }),
        "GET /.well-known/openid-configuration/jwks": json_response(200, {
            "keys": [{
                "kty": "OKP", "crv": "Ed25519", "kid": "test-1",
                "x":   "11qYAYKxCrfVS_7TyWQHOg7hcvPapiMlrwIaaPcHURo",
            }],
        }),
    },
)
```

### 2. Point the SUT at it

```python
api = service("api",
    interface("public", "http", 8080),
    image = "myapp:latest",
    depends_on = [auth],
    env = {
        "OIDC_ISSUER": "http://auth:8090",
    },
)
```

`depends_on = [auth]` ensures the mock is listening before `api`
starts. Inside the container network, `api` reaches `auth` by its
service name — same as for real services.

### 3. Test the authenticated path

```python
def test_authenticated_request_succeeds():
    resp = api.public.get(path = "/me", headers = {
        "Authorization": "Bearer " + stub_jwt(kid = "test-1"),
    })
    assert_eq(resp.status, 200)

    assert_eventually(events()
        .where(service = "auth", op = "GET /.well-known/openid-configuration/jwks")
        .count() >= 1)
```

The `events()` assertion verifies the SUT actually fetched JWKS — not
just succeeded by luck or cached state. Mock services emit events into
the same event log as real services, so every existing assertion works
unchanged.

## Dynamic responses

The JWKS doc is static. The `/token` endpoint isn't — it has to mint a
JWT whose subject matches the `user=` query param so tests can vary
the logged-in identity.

```python
def mint_token(req):
    user = req["query"].get("user", "anonymous")
    return json_response(200, {
        "access_token": sign_jwt({"sub": user, "exp": now() + 3600}),
        "token_type":   "Bearer",
        "expires_in":   3600,
    })

auth = mock_service("auth",
    interface("http", "http", 8090),
    routes = {
        "POST /token": dynamic(mint_token),
        # ... static routes from before
    },
)
```

`dynamic(fn)` wraps a Starlark callable invoked per request. It
receives a dict with `method`, `path`, `headers`, `query`, and `body`
and returns a response.

### When to use `dynamic()` vs static

| Use case | Approach |
|---|---|
| JWKS, health, config — same response every call | Static (`json_response`) |
| JWT minting with variable subject | Dynamic |
| Flag evaluation based on request headers | Dynamic |
| Echo endpoints for integration-testing request shape | Dynamic |
| Dozens of operations on the same shape | Static (generate the dict in spec once) |

Dynamic handlers run Starlark on the hot path — fine for mock services
(designed for low RPS), but don't use them for throughput testing.

## Stdlib mocks: Kafka, Redis, MongoDB

Some protocols don't fit the route-table shape — Kafka has topics
(not paths), Redis has a key/value model, MongoDB has collections.
For these, Faultbox ships protocol-specific stdlib constructors at
`@faultbox/mocks/<name>.star`:

```python
load("@faultbox/mocks/kafka.star",   "kafka")
load("@faultbox/mocks/redis.star",   "redis")
load("@faultbox/mocks/mongodb.star", "mongo")

bus = kafka.broker(
    name      = "bus",
    interface = interface("main", "kafka", 9092),
    topics    = {"orders": [], "payments": []},
)

cache = redis.server(
    name      = "cache",
    interface = interface("main", "redis", 6379),
    state     = {"config:max_retries": "3", "flag:new_ui": "true"},
)

users_db = mongo.server(
    name      = "users-stub",
    interface = interface("main", "mongodb", 27017),
    collections = {
        "users": [
            {"_id": "1", "name": "alice", "role": "admin"},
            {"_id": "2", "name": "bob",   "role": "user"},
        ],
    },
)
```

Under the hood, each stdlib constructor is a thin Starlark wrapper
that calls the generic `mock_service()` with a protocol-specific
`config=` map. The Go machinery is the same; the split exists only so
call sites stay readable.

**Real clients work without modification.** Your `kafka-go`,
`go-redis`, `mongo-driver/v2` in the SUT container connect and speak
wire protocol just like they would against the real servers.

### What the stdlib mocks cover (and don't)

| Mock | Good for | Not good for |
|---|---|---|
| `kafka.broker` | Producer/consumer tests, consumer-group handshake, topic enumeration | High-throughput benchmarks; exotic broker configs |
| `redis.server` | String cache, counters, hashes, sorted sets, TTL | Streams, pub/sub, Lua scripting (miniredis has partial support but behavior drifts) |
| `mongo.server` | Handshake, `find`/`findOne` against seeded docs, write acknowledgement | Round-trip CRUD (writes are acknowledged but dropped), aggregation pipelines, transactions |

For anything in the right column, use a real container (Chapter 9).

## Composing mocks with real services

Mocks sit next to real services in the same topology:

```python
load("@faultbox/mocks/redis.star", "redis")

auth  = mock_service("auth",  interface("http", "http", 8090), routes = {...})
cache = redis.server("cache", interface = interface("main", "redis", 6379),
                              state = {"feature:new": "true"})

# Real containerized Postgres — the interesting dependency
db = service("db",
    interface("sql", "postgres", 5432),
    image = "postgres:16",
    env = {"POSTGRES_PASSWORD": "test"},
    healthcheck = tcp("localhost:5432"),
)

api = service("api",
    interface("public", "http", 8080),
    image      = "myapp:latest",
    depends_on = [auth, cache, db],
    env = {
        "JWKS_URL":     "http://auth:8090/.well-known/openid-configuration/jwks",
        "REDIS_URL":    "redis://cache:6379",
        "DATABASE_URL": "postgres://postgres:test@db:5432/postgres",
    },
)
```

Auth and cache are mocks — trivial dependencies, don't matter to the
test. Postgres is real — transactions, isolation, error codes are
exactly what you're testing. Best of both worlds: fast mocks for the
boring bits, real infrastructure where it matters.

## Faulting a mock

`fault_assumption()` works on mocks exactly as on real services:

```python
jwks_unavailable = fault_assumption("jwks_unavailable",
    target = auth.http,
    rules  = [error(path = "/.well-known/**", status = 503)],
)

fault_scenario("auth_fails_open",
    target  = jwks_unavailable,
    monitor = monitor(lambda e: e.service != "api" or e.status != 500),
)
```

The mock answers normally, then the proxy layer rewrites the response
to 503 — same machinery used for real services in Chapter 9. You can
combine mocks with recipes the same way:

```python
load("@faultbox/recipes/redis.star", "redis_recipes")

redis_oom = fault_assumption("cache_oom",
    target = cache.main,
    rules  = [redis_recipes.oom()],
)
```

## TLS

Real OIDC issuers serve HTTPS. Adding `tls = True` generates an
ECDSA cert signed by a per-runtime mock CA:

```python
auth = mock_service("auth",
    interface("https", "http", 8443),
    tls    = True,
    routes = {...},
)
```

The CA bundle is written to `${TMPDIR}/faultbox-ca-<timestamp>.pem`.
Your SUT (real service, real client) reads the CA from that path and
adds it to its `RootCAs` pool. No `InsecureSkipVerify=true` smell
leaking into production code.

TLS is supported on HTTP, HTTP/2, and gRPC mocks in v0.8. Other
protocols (Kafka, Redis, MongoDB) silently ignore `tls=True` — if you
need TLS on those, run the real service.

## What mocks deliberately don't do

- **Stateful CRUD with persistence.** MongoDB writes are dropped;
  Kafka topic messages aren't pre-populated. If your test cares about
  "I wrote X, then I read X back" for anything other than the simplest
  key-value case, use a real container.
- **Production-shaped throughput.** Mocks optimize for correctness,
  not performance. Benchmarks belong in real services.
- **Schema validation.** Routes match by pattern, not by request
  shape. Use `dynamic()` if you need to reject malformed requests.
- **Replace WireMock / Prism / mountebank.** Faultbox mocks stand up
  dependencies *of the system under test*, not act as general-purpose
  stub servers.

If you find yourself working around these limits, you want a real
service. Reach for Chapter 9.

## Exercises

1. **Write a minimal JWKS stub** for an SUT you actually work with. A
   single route, real Ed25519 keys, sign one token, verify the SUT's
   `Authorization: Bearer ...` path works.

2. **Add a feature-flag endpoint** that returns different flag
   values based on a header — `X-User-Bucket: canary` returns
   `{"new_ui": true}`, everyone else gets `false`. Use `dynamic()`.

3. **Compose** the auth stub with a real Postgres container and
   `mongo.server` seeded with 10 user records. Write a test that hits
   the SUT, triggers a DB query, and asserts both the mock events
   (auth call count) and the real DB (row count after write).

4. **Fault the mock.** Wrap your auth stub in a
   `fault_assumption()` that returns 503 for 50% of JWKS fetches.
   Assert the SUT gracefully falls back (cached keys, error page, retry
   — whatever your app does) rather than crashing.

## Reference

- [Mock Services reference](../../mock-services.md) — full API
- [Spec Language § Mock Services](../../spec-language.md#mock-services)
- [RFC-017](../../rfcs/0017-mock-services.md) — design rationale
- [`poc/mock-demo/`](https://github.com/faultbox/faultbox/tree/main/poc/mock-demo)
  — end-to-end example with every v0.8 mock type
