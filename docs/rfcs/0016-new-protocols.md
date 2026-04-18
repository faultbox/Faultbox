# RFC-016: New Protocol Support — UDP, QUIC, HTTP/2, MongoDB, ClickHouse, Cassandra

- **Status:** Partially Implemented (v0.7.0) — MongoDB, HTTP/2, UDP, Cassandra, ClickHouse shipped. QUIC deferred to its own RFC (complex TLS + connection migration story).
- **Author:** Boris Glebov, Claude Opus 4.6
- **Created:** 2026-04-16
- **Branch:** `rfc/016-new-protocols`

## Summary

Add five new protocols to the Faultbox protocol plugin system: UDP, QUIC,
HTTP/2, MongoDB, and ClickHouse. Each protocol adds step methods for
spec-level interaction, proxy fault rules for protocol-level injection,
and integration with the existing `fault_assumption()` / `fault_matrix()`
composition model.

## Motivation

The current 8 protocols (HTTP, TCP, Postgres, MySQL, Redis, Kafka, NATS,
gRPC) cover the majority of web service architectures. But real-world
systems increasingly use:

- **UDP** — DNS resolution, metrics (StatsD, Datadog), game servers,
  real-time streaming, IoT telemetry
- **QUIC** — HTTP/3 transport, modern CDN connections, Google Cloud
  services, increasingly adopted by load balancers
- **HTTP/2** — gRPC transport (already partially supported), REST APIs
  behind modern proxies, multiplexed connections
- **MongoDB** — document databases, widely used in Node.js/Python
  ecosystems, common in microservice architectures
- **ClickHouse** — analytics databases, event/log storage, increasingly
  used for observability backends

Without these, users can only use syscall-level faults on services that
speak these protocols — losing the precision of protocol-level injection.

## Design per Protocol

### UDP

**Interface declaration:**

```python
dns = service("dns",
    interface("main", "udp", 53),
    image = "coredns/coredns:1.11",
)

statsd = service("statsd",
    interface("main", "udp", 8125),
    image = "statsd/statsd:latest",
)
```

**Step methods:**

| Method | Description |
|--------|-------------|
| `send(data="", hex="")` | Send a UDP datagram, receive response (if any) |
| `send_no_reply(data="")` | Send a UDP datagram, don't wait for response |

```python
# DNS query
resp = dns.main.send(hex="...")  # raw DNS packet
# resp.data = {"raw": "...", "size": 64}

# StatsD metric
statsd.main.send_no_reply(data="api.requests:1|c")
```

**Fault rules:**

| Rule | Match | Action |
|------|-------|--------|
| `drop()` | — | Drop datagram silently |
| `delay(delay=)` | — | Hold datagram for duration |
| `corrupt(probability=)` | — | Flip random bits in datagram |
| `reorder()` | — | Deliver out of order (buffer + swap) |

```python
dns_loss = fault_assumption("dns_loss",
    target = dns.main,
    rules = [drop()],  # 100% packet loss
)

dns_flaky = fault_assumption("dns_flaky",
    target = dns.main,
    rules = [drop(probability="20%")],  # 20% packet loss
)

metrics_delayed = fault_assumption("metrics_delayed",
    target = statsd.main,
    rules = [delay(delay="5s")],
)
```

**Implementation notes:**
- Proxy is a UDP relay (listen on random port, forward to real service)
- No connection state — each datagram is independent
- `corrupt()` and `reorder()` are new fault actions unique to UDP
- Response matching: by source port (ephemeral) or content pattern

**Complexity:** Medium. UDP proxy is simpler than TCP (no connection
state, no stream reassembly). New fault actions (corrupt, reorder) need
new proxy logic.

---

### QUIC

**Interface declaration:**

```python
cdn = service("cdn",
    interface("main", "quic", 443),
    image = "caddy:2",
)
```

**Step methods:**

| Method | Description |
|--------|-------------|
| `get(path="", headers={})` | HTTP/3 GET over QUIC |
| `post(path="", body="", headers={})` | HTTP/3 POST over QUIC |

Same HTTP methods as HTTP/1.1 — QUIC is the transport layer.

```python
resp = cdn.main.get(path="/assets/logo.png")
assert_eq(resp.status, 200)
```

**Fault rules:**

Same as HTTP (response, error, delay, drop) plus transport-level:

| Rule | Match | Action |
|------|-------|--------|
| `response(path=, status=, body=)` | HTTP/3 request | Custom response |
| `delay(path=, delay=)` | HTTP/3 request | Delay specific request |
| `drop(path=)` | HTTP/3 request | Close QUIC stream |
| `connection_migration_fail()` | — | Reject connection migration |
| `handshake_timeout()` | — | TLS handshake never completes |

```python
quic_slow = fault_assumption("quic_slow",
    target = cdn.main,
    rules = [delay(path="/api/*", delay="2s")],
)

migration_fail = fault_assumption("migration_fail",
    target = cdn.main,
    rules = [connection_migration_fail()],
)
```

**Implementation notes:**
- Requires QUIC proxy library (e.g., `quic-go`)
- HTTP/3 framing on top of QUIC streams
- Connection migration faults are QUIC-specific (not possible with TCP)
- TLS is mandatory in QUIC — proxy needs certificate management

**Complexity:** High. QUIC has complex connection state, mandatory TLS,
multiplexed streams, and connection migration. Proxy implementation is
significantly more work than TCP-based protocols.

---

### HTTP/2

**Interface declaration:**

```python
api = service("api",
    interface("public", "http2", 8080),
    image = "myapp:latest",
)
```

**Step methods:**

Same as HTTP/1.1 — the wire protocol changes but the API is identical:

| Method | Description |
|--------|-------------|
| `get(path="", headers={})` | HTTP/2 GET |
| `post(path="", body="", headers={})` | HTTP/2 POST |
| `put(path="", body="", headers={})` | HTTP/2 PUT |
| `delete(path="", headers={})` | HTTP/2 DELETE |
| `patch(path="", body="", headers={})` | HTTP/2 PATCH |

```python
resp = api.public.get(path="/users/1")
assert_eq(resp.status, 200)
```

**Fault rules:**

Same as HTTP/1.1 plus stream-level:

| Rule | Match | Action |
|------|-------|--------|
| `response(method=, path=, status=, body=)` | HTTP/2 request | Custom response |
| `delay(method=, path=, delay=)` | HTTP/2 request | Delay specific stream |
| `drop(method=, path=)` | HTTP/2 request | RST_STREAM |
| `goaway()` | — | Send GOAWAY frame (graceful shutdown) |
| `window_exhaustion()` | — | Set flow control window to 0 (back-pressure) |

```python
stream_reset = fault_assumption("stream_reset",
    target = api.public,
    rules = [drop(path="/slow-endpoint")],  # RST_STREAM
)

back_pressure = fault_assumption("back_pressure",
    target = api.public,
    rules = [window_exhaustion()],
)
```

**Implementation notes:**
- HTTP/2 proxy handles HPACK header compression, stream multiplexing
- Can target individual streams (unlike HTTP/1.1 which is one
  request per connection)
- `goaway()` and `window_exhaustion()` are HTTP/2-specific fault types
- gRPC already uses HTTP/2 — this adds non-gRPC HTTP/2 support

**Complexity:** Medium-High. HTTP/2 framing, HPACK, stream multiplexing.
Can reuse parts of the gRPC proxy (which already speaks HTTP/2).

---

### MongoDB

**Interface declaration:**

```python
db = service("mongo",
    interface("main", "mongodb", 27017),
    image = "mongo:7",
    env = {"MONGO_INITDB_ROOT_USERNAME": "root", "MONGO_INITDB_ROOT_PASSWORD": "test"},
    healthcheck = tcp("localhost:27017"),
)
```

**Step methods:**

| Method | Description |
|--------|-------------|
| `find(collection="", filter={}, limit=0)` | Query documents |
| `insert(collection="", document={})` | Insert one document |
| `insert_many(collection="", documents=[])` | Insert multiple documents |
| `update(collection="", filter={}, update={})` | Update matching documents |
| `delete(collection="", filter={})` | Delete matching documents |
| `count(collection="", filter={})` | Count matching documents |
| `command(cmd={})` | Run raw MongoDB command |

```python
db.main.insert(collection="users", document={"name": "alice", "role": "admin"})
db.main.insert(collection="users", document={"name": "bob", "role": "user"})

resp = db.main.find(collection="users", filter={"role": "admin"})
# resp.data = [{"_id": "...", "name": "alice", "role": "admin"}]

resp = db.main.count(collection="orders", filter={"status": "pending"})
# resp.data = {"count": 42}

db.main.update(collection="users",
    filter={"name": "alice"},
    update={"$set": {"role": "superadmin"}},
)
```

**Fault rules:**

| Rule | Match | Action |
|------|-------|--------|
| `error(collection=, op=, message=)` | Collection + operation | Return MongoDB error |
| `delay(collection=, op=, delay=)` | Collection + operation | Slow query |
| `drop(collection=, op=)` | Collection + operation | Close connection |

```python
insert_fail = fault_assumption("insert_fail",
    target = db.main,
    rules = [error(collection="orders", op="insert", message="disk full")],
)

slow_queries = fault_assumption("slow_queries",
    target = db.main,
    rules = [delay(collection="*", op="find", delay="3s")],
)
```

**Seed / Reset:**

```python
def seed_mongo():
    db.main.command(cmd={"dropDatabase": 1})
    db.main.insert_many(collection="users", documents=[
        {"name": "alice", "role": "admin"},
        {"name": "bob", "role": "user"},
    ])

def reset_mongo():
    db.main.command(cmd={"dropDatabase": 1})

db = service("mongo", ...,
    reuse = True,
    seed = seed_mongo,
    reset = reset_mongo,
)
```

**Implementation notes:**
- MongoDB Wire Protocol (OP_MSG format, MongoDB 3.6+)
- Proxy parses OP_MSG to extract collection name and operation type
- JSON filter/document parameters map to BSON encoding
- Connection authentication (SCRAM-SHA-256) must be handled by proxy

**Complexity:** Medium. Wire protocol is well-documented (OP_MSG). Main
challenge is BSON encoding/decoding and SCRAM authentication passthrough.

---

### ClickHouse

**Interface declaration:**

```python
ch = service("clickhouse",
    interface("main", "clickhouse", 9000),
    image = "clickhouse/clickhouse-server:24",
    healthcheck = tcp("localhost:9000"),
)
```

**Step methods:**

| Method | Description |
|--------|-------------|
| `query(sql="")` | Execute SELECT, return rows |
| `exec(sql="")` | Execute INSERT/DDL, return affected count |

```python
resp = ch.main.query(sql="SELECT count() as n FROM events WHERE date = today()")
# resp.data = [{"n": 1000000}]

ch.main.exec(sql="INSERT INTO events (date, type, data) VALUES (today(), 'order', '{}')")
```

**Fault rules:**

| Rule | Match | Action |
|------|-------|--------|
| `error(query=, message=)` | SQL pattern | Return ClickHouse error |
| `delay(query=, delay=)` | SQL pattern | Slow query |
| `drop(query=)` | SQL pattern | Close connection |

```python
analytics_down = fault_assumption("analytics_down",
    target = ch.main,
    rules = [error(query="INSERT*", message="too many parts")],
)
```

**Seed / Reset:**

```python
def seed_clickhouse():
    ch.main.exec(sql="CREATE TABLE IF NOT EXISTS events (date Date, type String, data String) ENGINE = MergeTree() ORDER BY date")

def reset_clickhouse():
    ch.main.exec(sql="TRUNCATE TABLE events")

ch = service("clickhouse", ...,
    reuse = True,
    seed = seed_clickhouse,
    reset = reset_clickhouse,
)
```

**Implementation notes:**
- ClickHouse native protocol (port 9000), binary format
- Alternative: ClickHouse HTTP interface (port 8123) — could reuse
  HTTP proxy with SQL-in-body matching
- Native protocol: client hello, query packets, data blocks, progress
- HTTP interface is simpler but less common in Go/Java clients

**Complexity:** Medium. Native protocol is binary but well-documented.
HTTP interface alternative significantly reduces implementation effort
(reuse HTTP proxy, match SQL in request body).

---

### Cassandra

**Interface declaration:**

```python
cass = service("cassandra",
    interface("main", "cassandra", 9042),
    image = "cassandra:4.1",
    healthcheck = tcp("localhost:9042"),
)
```

**Step methods:**

| Method | Description |
|--------|-------------|
| `query(cql="", consistency="")` | Execute CQL SELECT, return rows |
| `exec(cql="", consistency="")` | Execute CQL INSERT/UPDATE/DELETE/DDL |

```python
cass.main.exec(cql="CREATE KEYSPACE IF NOT EXISTS test WITH replication = {'class': 'SimpleStrategy', 'replication_factor': 1}")
cass.main.exec(cql="CREATE TABLE IF NOT EXISTS test.orders (id UUID PRIMARY KEY, item TEXT, status TEXT)")
cass.main.exec(cql="INSERT INTO test.orders (id, item, status) VALUES (uuid(), 'widget', 'pending')")

resp = cass.main.query(cql="SELECT * FROM test.orders WHERE status='pending' ALLOW FILTERING")
# resp.data = [{"id": "...", "item": "widget", "status": "pending"}]

# Consistency level
resp = cass.main.query(cql="SELECT * FROM test.orders", consistency="QUORUM")
```

| Parameter | Type | Default | Description |
|-----------|------|---------|-------------|
| `cql` | string | required | CQL statement |
| `consistency` | string | `"ONE"` | Consistency level: ONE, QUORUM, ALL, LOCAL_ONE, LOCAL_QUORUM |

**Fault rules:**

| Rule | Match | Action |
|------|-------|--------|
| `error(query=, message=)` | CQL pattern | Return Cassandra error |
| `delay(query=, delay=)` | CQL pattern | Slow query |
| `drop(query=)` | CQL pattern | Close connection |
| `error(query=, message="unavailable", consistency=)` | CQL + consistency | Simulate unavailable replicas |

```python
write_fail = fault_assumption("write_fail",
    target = cass.main,
    rules = [error(query="INSERT*", message="write timeout")],
)

quorum_unavailable = fault_assumption("quorum_unavailable",
    target = cass.main,
    rules = [error(query="*", message="unavailable: 1 of 3 replicas")],
)

slow_reads = fault_assumption("slow_reads",
    target = cass.main,
    rules = [delay(query="SELECT*", delay="5s")],
)
```

**Seed / Reset:**

```python
def seed_cassandra():
    cass.main.exec(cql="CREATE KEYSPACE IF NOT EXISTS test WITH replication = {'class': 'SimpleStrategy', 'replication_factor': 1}")
    cass.main.exec(cql="CREATE TABLE IF NOT EXISTS test.orders (id UUID PRIMARY KEY, item TEXT, status TEXT)")
    cass.main.exec(cql="INSERT INTO test.orders (id, item, status) VALUES (uuid(), 'widget', 'pending')")

def reset_cassandra():
    cass.main.exec(cql="TRUNCATE test.orders")

cass = service("cassandra", ...,
    reuse = True,
    seed = seed_cassandra,
    reset = reset_cassandra,
)
```

**Implementation notes:**
- Cassandra CQL Binary Protocol v4 (used by all modern drivers)
- Proxy parses QUERY/EXECUTE frames to extract CQL text
- Consistency-level faults are Cassandra-specific — simulate partial
  replica failures without actually failing nodes
- Cassandra containers are multi-process (Java) — requires RFC-014
  (Unix socket fd passing) for syscall-level faults

**Complexity:** Medium. CQL binary protocol is well-documented. Main
challenge is frame parsing (similar to Postgres wire protocol) and
handling prepared statements (PREPARE/EXECUTE mapping).

---

## Priority and Ordering

| Protocol | Priority | Effort | Rationale |
|----------|----------|--------|-----------|
| **MongoDB** | P1 | Medium | High demand — Node.js/Python ecosystem, many users |
| **HTTP/2** | P1 | Medium | Needed for modern APIs + completes gRPC story |
| **Cassandra** | P1 | Medium | Common in Java ecosystems, consistency-level faults are unique |
| **ClickHouse** | P2 | Low-Medium | Growing demand, HTTP interface shortcut available |
| **UDP** | P2 | Medium | Unique fault types (corrupt, reorder), DNS/metrics use cases |
| **QUIC** | P3 | High | Future-looking, limited current adoption in backends |

**Suggested implementation order:** MongoDB → HTTP/2 → Cassandra → ClickHouse → UDP → QUIC

## Implementation Approach

All new protocols follow the existing plugin architecture:

1. Implement `protocol.Protocol` interface in `internal/protocol/<name>.go`
2. Implement `proxy.Handler` in `internal/proxy/<name>.go` (if fault rules needed)
3. Register in `internal/protocol/protocol.go` registry
4. Add documentation page in `docs/protocols/<name>.md`
5. Add to protocol reference table and site navigation

The plugin interface is stable — no architectural changes needed.

```go
type Protocol interface {
    Name() string
    ExecuteStep(ctx context.Context, addr string, method string, params map[string]string) (*StepResult, error)
}
```

## Open Questions

1. **ClickHouse: native protocol vs HTTP interface?**
   Native (port 9000) is what Go/Java clients use. HTTP (port 8123) is
   simpler to proxy. Could support both with `"clickhouse"` (native) and
   `"clickhouse-http"` (HTTP-based) protocol names.

2. **HTTP/2 + HTTP/1.1 coexistence.** Should `"http2"` be a separate
   protocol or should `"http"` auto-detect? Most services support both
   via ALPN negotiation. Proposal: keep `"http"` as HTTP/1.1, add `"http2"`
   for explicit HTTP/2. The proxy handles the difference.

3. **MongoDB authentication.** SCRAM-SHA-256 auth is mandatory in
   production. The proxy needs to passthrough authentication or handle
   it. Proposal: passthrough (proxy doesn't authenticate, client does).

4. **UDP reliability.** UDP is unreliable by design — `send()` may not
   receive a response. Should `send()` timeout be configurable per call?
   Proposal: `send(data="...", timeout="1s")` with default 5s.

5. **QUIC certificate management.** QUIC requires TLS. The proxy needs
   a certificate the client trusts. Proposal: auto-generate a self-signed
   CA at suite start, inject it into containers via volumes.

## References

- Existing protocol interface: `internal/protocol/protocol.go`
- Existing proxy interface: `internal/proxy/proxy.go`
- MongoDB Wire Protocol: https://www.mongodb.com/docs/manual/reference/mongodb-wire-protocol/
- ClickHouse Native Protocol: https://clickhouse.com/docs/en/native-protocol/basics
- QUIC RFC 9000: https://www.rfc-editor.org/rfc/rfc9000
- HTTP/2 RFC 9113: https://www.rfc-editor.org/rfc/rfc9113
