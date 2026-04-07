# Design Document: Protocol-Level Fault Injection via Transparent Proxy

## Problem

Faultbox operates at the **syscall level** — it sees `write(fd, bytes, len)`
but can't inspect protocol content. Users need faults at the **protocol level**:

- Return HTTP 429 for `POST /orders` but not `GET /health`
- Reset a gRPC stream for a specific method
- Drop or delay a Kafka message on a specific topic
- Inject a Postgres query error without killing the connection
- Simulate Redis returning stale data from a replica
- Truncate a MySQL result set mid-response

These are invisible at the syscall level — a `write` is a `write` regardless
of whether it carries an HTTP 200 or a Kafka message.

## Approach: Transparent Proxy

Inject a proxy between services by rewriting address wiring. Each proxy
speaks the real protocol and can intercept, inspect, modify, delay, drop,
or fabricate responses.

```
Without proxy:     Service A ──────────────→ Service B
With proxy:        Service A ──→ Proxy ──→ Service B
                                  ↑
                           Faultbox rules
```

Services don't know the proxy exists — they connect to the same address
(rewritten by Faultbox env wiring).

## Starlark API

### `proxy_fault()` — protocol-level fault injection

```python
db = service("postgres",
    interface("main", "postgres", 5432),
    image="postgres:16",
)

api = service("api",
    interface("public", "http", 8080),
    build="./api",
    env={"DB_URL": "postgres://test@" + db.main.internal_addr + "/testdb"},
    depends_on=[db],
)

def test_query_timeout():
    """Specific query takes too long — API should timeout gracefully."""
    def scenario():
        resp = api.post(path="/users", body='{"name":"alice"}')
        assert_true(resp.duration_ms < 3000, "should timeout, not hang")
    proxy_fault(api, db,
        postgres_delay(query="INSERT INTO users*", delay="5s"),
        run=scenario,
    )

def test_http_rate_limit():
    """Payment gateway returns 429 — API should retry."""
    def scenario():
        resp = api.post(path="/checkout", body='{"amount":100}')
        assert_eq(resp.status, 200, "should succeed after retry")
    proxy_fault(api, gateway,
        http_response(method="POST", path="/charge", status=429,
                      body='{"error":"rate_limited"}'),
        run=scenario,
    )
```

### Fault types per protocol

**HTTP:**
```python
# Return specific status/body for matching requests:
http_response(method="POST", path="/orders", status=429, body='{"error":"rate_limited"}')
http_response(path="/health", status=503)

# Delay specific requests:
http_delay(method="GET", path="/slow*", delay="3s")

# Drop connection mid-response:
http_reset(method="POST", path="/upload")
```

**gRPC:**
```python
# Return gRPC error for specific method:
grpc_error(method="/pkg.OrderService/CreateOrder", status="UNAVAILABLE")
grpc_error(method="/pkg.OrderService/*", status="DEADLINE_EXCEEDED")

# Delay specific RPC:
grpc_delay(method="/pkg.OrderService/CreateOrder", delay="5s")
```

**Postgres / MySQL:**
```python
# Fail specific queries:
postgres_error(query="INSERT INTO orders*", error="relation does not exist")
postgres_delay(query="SELECT * FROM users*", delay="3s")

# MySQL equivalent:
mysql_error(query="INSERT INTO orders*", error="Table is read only")
```

**Redis:**
```python
# Return error for specific commands:
redis_error(command="SET", key="session:*", error="READONLY")

# Delay specific commands:
redis_delay(command="GET", key="cache:*", delay="2s")

# Return nil (simulate cache miss):
redis_nil(command="GET", key="cache:*")
```

**Kafka:**
```python
# Drop messages on a topic:
kafka_drop(topic="orders.events", probability="30%")

# Delay message delivery:
kafka_delay(topic="orders.events", delay="5s")

# Duplicate messages (test idempotency):
kafka_duplicate(topic="orders.events")
```

**NATS:**
```python
# Drop messages on subject:
nats_drop(subject="orders.*", probability="50%")
nats_delay(subject="orders.new", delay="2s")
```

### Combined with syscall faults

```python
# Both levels at once:
def test_cascade():
    def scenario():
        resp = api.post(path="/orders", body='...')
        assert_true(resp.status >= 500)

    # Protocol level: Postgres returns error for inserts
    # Syscall level: API disk is full (can't log the error)
    proxy_fault(api, db,
        postgres_error(query="INSERT*", error="disk full"),
        run=lambda: fault(api, write=deny("ENOSPC"), run=scenario),
    )
```

## Technical Architecture

```
┌─────────────────────────────────────────────────┐
│                  Starlark Runtime                 │
├──────────┬──────────────┬───────────────────────┤
│ fault()  │ proxy_fault()│ partition()            │
│ syscall  │ protocol     │ network               │
│ level    │ level        │ level                  │
├──────────┴──────┬───────┴───────────────────────┤
│                 │                                │
│    ┌────────────▼────────────┐                   │
│    │   Proxy Manager         │                   │
│    │   - Start proxy per edge│                   │
│    │   - Rewrite env/addr    │                   │
│    │   - Apply fault rules   │                   │
│    └────────────┬────────────┘                   │
│                 │                                │
│    ┌────────────▼────────────┐                   │
│    │   Protocol Proxies      │                   │
│    │   ┌──────────────────┐  │                   │
│    │   │ HTTP Proxy       │  │ httputil.ReverseProxy│
│    │   │ gRPC Proxy       │  │ grpc.UnaryInterceptor│
│    │   │ Postgres Proxy   │  │ TCP + wire protocol  │
│    │   │ MySQL Proxy      │  │ TCP + wire protocol  │
│    │   │ Redis Proxy      │  │ RESP parser          │
│    │   │ Kafka Proxy      │  │ Kafka wire protocol  │
│    │   │ NATS Proxy       │  │ NATS protocol        │
│    │   └──────────────────┘  │                   │
│    └─────────────────────────┘                   │
└─────────────────────────────────────────────────┘
```

### New Package: `internal/proxy/`

```
internal/proxy/
├── proxy.go          # Proxy interface, ProxyManager, rule types
├── http.go           # HTTP reverse proxy with request matching
├── grpc.go           # gRPC proxy with method interception
├── postgres.go       # Postgres wire protocol proxy
├── mysql.go          # MySQL wire protocol proxy
├── redis.go          # Redis RESP proxy
├── kafka.go          # Kafka wire protocol proxy
├── nats.go           # NATS protocol proxy
└── proxy_test.go     # Tests for each proxy
```

### Core Interface

```go
// internal/proxy/proxy.go

// Proxy intercepts traffic between two services at the protocol level.
type Proxy interface {
    // Protocol returns the protocol name this proxy handles.
    Protocol() string

    // Start begins listening. Returns the proxy's listen address.
    Start(ctx context.Context, target string) (listenAddr string, err error)

    // AddRule adds a fault injection rule to this proxy.
    AddRule(rule ProxyRule)

    // ClearRules removes all fault rules.
    ClearRules()

    // Stop shuts down the proxy.
    Stop() error
}

// ProxyRule describes a protocol-level fault to inject.
type ProxyRule struct {
    // Match criteria (protocol-specific):
    Method  string // HTTP method, gRPC method, Redis command
    Path    string // HTTP path (glob), gRPC service/method
    Query   string // SQL query pattern (glob)
    Key     string // Redis key pattern (glob)
    Topic   string // Kafka/NATS topic/subject (glob)

    // Action:
    Action  ProxyAction // respond, delay, drop, reset, duplicate

    // Action parameters:
    Status  int    // HTTP status code / gRPC status
    Body    string // Response body (JSON)
    Error   string // Error message
    Delay   time.Duration
    Prob    float64 // Probability [0,1]
}

type ProxyAction int
const (
    ProxyRespond  ProxyAction = iota // Return custom response
    ProxyDelay                        // Delay then forward
    ProxyDrop                         // Silently drop
    ProxyReset                        // TCP reset mid-stream
    ProxyDuplicate                    // Forward twice
)
```

### ProxyManager

```go
// ProxyManager manages proxy lifecycle per service pair.
type ProxyManager struct {
    proxies map[string]Proxy // "from:to:protocol" → running proxy
    mu      sync.Mutex
}

// EnsureProxy starts a proxy between two services if not already running.
// Returns the proxy's listen address (to rewrite in env).
func (pm *ProxyManager) EnsureProxy(ctx context.Context, from, to, protocol, targetAddr string) (string, error)

// AddRule adds a fault rule to the proxy between from and to.
func (pm *ProxyManager) AddRule(from, to string, rule ProxyRule)

// ClearRules removes all rules for a proxy.
func (pm *ProxyManager) ClearRules(from, to string)

// StopAll shuts down all proxies.
func (pm *ProxyManager) StopAll()
```

### How proxy_fault() Works

1. **Start proxy**: `ProxyManager.EnsureProxy(api, db, "postgres", "localhost:5432")`
   → starts a Postgres proxy on a random port, returns `localhost:54321`

2. **Rewrite address**: The service's env is already set. For **new** services
   (not yet started), rewrite before launch. For **running** services, the
   proxy must be started before the test and the address baked in.

3. **Add rules**: `pm.AddRule("api", "db", ProxyRule{Query: "INSERT*", Action: ProxyRespond, Error: "disk full"})`

4. **Run test**: Service A connects to proxy (thinks it's service B).
   Proxy inspects each request, matches against rules, injects fault or forwards.

5. **Clean up**: `pm.ClearRules("api", "db")` after test function returns.

### Address Rewriting

**Binary mode:**
- Before `startBinaryService()`, if any `proxy_fault()` targets this edge,
  rewrite the env var from `localhost:5432` to `localhost:PROXY_PORT`
- The proxy forwards to the real service on `localhost:5432`

**Container mode:**
- Start the proxy as a **sidecar container** on `faultbox-net`
- Rewrite `buildContainerEnv()` to point at the proxy's hostname
- Or: start proxy on the host and rewrite to `host.docker.internal:PROXY_PORT`

**Test driver calls:**
- In `executeStep()`, if a proxy exists for this service,
  rewrite `addr` to the proxy's listen address

### Injection Points in Existing Code

| What | Where | Change |
|------|-------|--------|
| Binary env rewrite | `buildEnv()` runtime.go:882 | Check ProxyManager, use proxy addr |
| Container env rewrite | `buildContainerEnv()` runtime.go:799 | Check ProxyManager, use proxy addr |
| Test step rewrite | `executeStep()` runtime.go:1239 | Check ProxyManager, use proxy addr |
| Proxy lifecycle | `startServices()` | Start proxies before services |
| Proxy cleanup | `stopServices()` | Stop proxies after services |

## Protocol Proxy Implementations

### HTTP Proxy (simplest)

```go
// Uses httputil.ReverseProxy with custom Transport.
type httpProxy struct {
    rules  []ProxyRule
    target string
    server *http.Server
}

func (p *httpProxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
    for _, rule := range p.rules {
        if matchHTTP(r, rule) {
            applyHTTPFault(w, r, rule)
            return
        }
    }
    // No match — forward to real service.
    httputil.NewSingleHostReverseProxy(target).ServeHTTP(w, r)
}
```

### Postgres Proxy

Parses the Postgres wire protocol (startup, query, parse, bind, execute).
Intercepts `Query` and `Execute` messages, matches against rule patterns.

Key messages to intercept:
- **Simple Query** (`Q` message): contains SQL text
- **Parse** (`P` message): prepared statement SQL
- **Execute** (`E` message): execute prepared statement

Response injection:
- **ErrorResponse** (`E` response): return Postgres error with SQLSTATE
- Delay: hold the connection before forwarding

Complexity: Medium — wire protocol is documented and sequential.

### MySQL Proxy

Similar to Postgres. Parses MySQL client/server protocol:
- **COM_QUERY**: contains SQL text
- **COM_STMT_PREPARE** / **COM_STMT_EXECUTE**: prepared statements

Response injection: ERR_Packet with error code and message.

Complexity: Medium — well-documented wire protocol.

### Redis Proxy

Parses RESP (Redis Serialization Protocol):
- Read command: `*3\r\n$3\r\nSET\r\n$3\r\nkey\r\n$5\r\nvalue\r\n`
- Match against command name and key pattern
- Inject error: `-ERR some error\r\n`
- Inject nil: `$-1\r\n`

Complexity: Low — RESP is simple (we already have a parser in protocol/redis.go).

### gRPC Proxy

Uses gRPC interceptors or raw HTTP/2 frame inspection:
- Intercept unary/streaming RPCs by method name
- Return gRPC status codes (UNAVAILABLE, DEADLINE_EXCEEDED, etc.)
- Delay specific RPCs

Complexity: Medium-High — HTTP/2 framing + gRPC encoding.

### Kafka Proxy

Parses Kafka wire protocol:
- **Produce**: match topic, inject error response
- **Fetch**: match topic, delay or return empty
- Message manipulation: drop, duplicate, reorder

Complexity: High — complex binary protocol with many API versions.

### NATS Proxy (low priority)

NATS protocol is text-based (like Redis):
- `PUB subject reply-to payload`
- `MSG subject sid reply-to payload`
- Easy to parse and intercept

Complexity: Low.

## Trace Integration

Proxy events are emitted into the EventLog as first-class events:

```go
// When proxy intercepts a request:
rt.events.Emit("proxy", svcName, map[string]string{
    "protocol": "postgres",
    "action":   "error",
    "query":    "INSERT INTO orders ...",
    "error":    "disk full",
    "from":     "api",
    "to":       "db",
})
```

Queryable by assertions:
```python
assert_eventually(where=lambda e: e.type == "proxy" and e.data["action"] == "error")
assert_never(where=lambda e: e.type == "proxy" and e.data["query"].startswith("DROP"))
```

Visible in ShiViz with causal arrows between services.

## Rollout Plan

### Phase 1: Core + HTTP + Redis (fastest value)

1. `internal/proxy/proxy.go` — Proxy interface, ProxyManager, rule types
2. `internal/proxy/http.go` — HTTP reverse proxy with request/path matching
3. `internal/proxy/redis.go` — Redis RESP proxy (reuse existing parser)
4. `internal/star/builtins.go` — `proxy_fault()` builtin + HTTP/Redis fault builtins
5. Address rewriting in `buildEnv()` and `executeStep()`
6. Trace event emission
7. Tests + docs

### Phase 2: Postgres + MySQL

8. `internal/proxy/postgres.go` — Postgres wire protocol proxy
9. `internal/proxy/mysql.go` — MySQL wire protocol proxy
10. SQL query pattern matching

### Phase 3: gRPC + Kafka + NATS

11. `internal/proxy/grpc.go` — gRPC interceptor proxy
12. `internal/proxy/kafka.go` — Kafka wire protocol proxy
13. `internal/proxy/nats.go` — NATS protocol proxy

### Phase 4: Advanced

14. Message reordering (Kafka consumer ordering)
15. Partial response corruption (TCP truncation)
16. Connection pool exhaustion simulation

## Key Files to Modify

| File | Change |
|------|--------|
| NEW `internal/proxy/` | Proxy package |
| `internal/star/builtins.go` | `proxy_fault()`, protocol fault builtins |
| `internal/star/runtime.go` | ProxyManager lifecycle, address rewriting |
| `internal/star/types.go` | ProxyFaultDef type |
| `cmd/faultbox/main.go` | (none — proxy is internal) |
