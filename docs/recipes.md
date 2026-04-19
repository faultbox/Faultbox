# Recipes

Curated, protocol-specific failure helpers that ship embedded in the
`faultbox` binary. Each recipe wraps the core fault primitives
(`response`, `error`, `delay`, `drop`) with the canonical error
message, status code, or body shape that a real server emits — so
specs say "disk full" instead of remembering
`"assertion: 10334 disk full"`.

## Why recipes exist

Writing a realistic fault today requires knowing the exact error shape
the real service would emit:

```python
# What the user wants to say:
rules = [error(query="INSERT*", message="disk full")]

# What the real Postgres would say (if you match on SQLSTATE):
rules = [error(query="INSERT*",
    message='ERROR: could not extend file "base/...": No space left on device (SQLSTATE 53100)')]
```

Most users write the short version on their first attempt. The SUT's
error handling might match on SQLSTATE codes or specific substrings —
so the injected fault passes, but the production fault would fail. The
test gives false confidence.

Recipes bridge the gap: they encode the **canonical** shape of each
failure once, in the stdlib, so your specs stay readable and your
faults stay realistic.

See [RFC-018](../rfcs/0018-recipes-library.md) for the design and
[RFC-019](../rfcs/0019-recipe-distribution.md) for how recipes reach
your specs.

## How to use them

Recipes ship embedded in the `faultbox` binary. Load them via the
`@faultbox/` prefix — no local `recipes/` directory needed:

```python
load("@faultbox/recipes/mongodb.star",    "mongodb")
load("@faultbox/recipes/cassandra.star",  "cassandra")
load("@faultbox/recipes/clickhouse.star", "clickhouse")

broken = fault_assumption("broken",
    target = db.main,
    rules  = [
        mongodb.disk_full(collection = "orders"),
        cassandra.unavailable(),
        clickhouse.too_many_parts(),
    ],
)
```

### The namespace struct pattern

Each recipe file exports **one struct** named after the protocol
(RFC-018). This prevents name collisions when you load recipes for
multiple protocols — `mongodb.disk_full` and `postgres.disk_full`
coexist naturally:

```python
load("@faultbox/recipes/mongodb.star",  "mongodb")
load("@faultbox/recipes/postgres.star", "postgres")

rules = [
    mongodb.disk_full(collection = "orders"),
    postgres.disk_full(),   # same recipe name, different namespace
]
```

One import per protocol, clean call sites, zero collisions.

### CLI discovery

Browse the catalog from the command line, no source checkout needed:

```
$ faultbox recipes list
Available stdlib recipes (load via @faultbox/recipes/<name>.star):
  amqp
  cassandra
  clickhouse
  grpc
  http
  http2
  kafka
  memcached
  mongodb
  mysql
  nats
  postgres
  redis
  udp

$ faultbox recipes show mongodb
# prints the full mongodb.star source
```

## Shipped recipes

All recipes shipped in the current release. Each bullet describes what
the recipe injects at the proxy level.

### `@faultbox/recipes/http.star`

HTTP/1.x twin of `@faultbox/recipes/http2.star` — same status-code semantics, with `connection_drop` in place of HTTP/2's `stream_reset`.

| Recipe | What it simulates |
|---|---|
| `http.rate_limited(path="/*")` | HTTP 429 with Retry-After body — client back-off + jitter tests |
| `http.server_error(path="/*")` | HTTP 500 — generic internal error |
| `http.service_unavailable(path="/*")` | HTTP 503 — retryable per HTTP semantics |
| `http.gateway_timeout(path="/*")` | HTTP 504 — upstream timeout at an intermediary |
| `http.slow_endpoint(path="/*", duration="3s")` | Fixed-latency injection — tests client read-timeout |
| `http.maintenance_window(path="/*")` | 503 with long Retry-After — LB "we're deploying" response |
| `http.connection_drop(path="/*")` | TCP close mid-request — keep-alive pool eviction tests |
| `http.flaky(path="/*", probability="20%")` | Probabilistic 500s — retry-policy and exponential-backoff tests |
| `http.unauthorized(path="/*")` | HTTP 401 — token-expiry + refresh-flow tests |
| `http.forbidden(path="/*")` | HTTP 403 — authorization-failure paths |

### `@faultbox/recipes/postgres.star`

| Recipe | What it simulates |
|---|---|
| `postgres.deadlock(query="*")` | SQLSTATE 40P01 — deadlock detected; victim transaction aborted. Triggers retry or crash in no-retry paths. |
| `postgres.lock_not_available(query="*")` | SQLSTATE 55P03 — `lock_timeout` / `statement_timeout` canceled statement |
| `postgres.serialization_failure(query="*")` | SQLSTATE 40001 — concurrent update invalidated snapshot; SERIALIZABLE / REPEATABLE READ only |
| `postgres.too_many_connections()` | SQLSTATE 53300 — `max_connections` saturated; surfaces pool-init error-swallowing bugs |
| `postgres.read_only_transaction(query="INSERT*")` | SQLSTATE 25006 — writes routed to a hot-standby / read-replica |
| `postgres.disk_full(query="INSERT*")` | SQLSTATE 53100 — "No space left on device" during file extension |
| `postgres.admin_shutdown(query="*")` | SQLSTATE 57P01 — server shutting down; pools must evict rather than retry |
| `postgres.connection_failure(query="*")` | Drop connection mid-query — drivers surface SQLSTATE 08006 |
| `postgres.slow_query(duration="3s", query="*")` | Delays any statement — tests `statement_timeout` + context deadlines |
| `postgres.slow_writes(duration="3s", query="INSERT*")` | Delays writes only |

### `@faultbox/recipes/redis.star`

| Recipe | What it simulates |
|---|---|
| `redis.oom(key="*")` | "OOM command not allowed..." — maxmemory reached on writes |
| `redis.cluster_down(key="*")` | "CLUSTERDOWN The cluster is down" — quorum lost |
| `redis.loading(key="*")` | "LOADING Redis is loading..." — server replaying RDB/AOF after restart |
| `redis.readonly_replica(key="*")` | "READONLY You can't write against a read only replica." |
| `redis.busy(key="*")` | "BUSY Redis is busy running a script." — Lua script blocking |
| `redis.noauth(key="*")` | "NOAUTH Authentication required." — server restarted into authenticated mode |
| `redis.wrongtype(key="*")` | "WRONGTYPE Operation against a key holding the wrong kind of value" |
| `redis.slow_command(duration="3s", key="*")` | Delays every command — tests pool timeout cascade |
| `redis.connection_drop(key="*")` | Connection close mid-command — pool reconnect path |

### `@faultbox/recipes/mysql.star`

| Recipe | What it simulates |
|---|---|
| `mysql.deadlock(query="*")` | ER_LOCK_DEADLOCK (1213) — circular row-lock wait. Triggers retry or crash in no-retry paths. |
| `mysql.lock_wait_timeout(query="*")` | ER_LOCK_WAIT_TIMEOUT (1205) — `innodb_lock_wait_timeout` exceeded |
| `mysql.too_many_connections()` | ER_CON_COUNT_ERROR (1040) — surfaces nil-pointer bugs in pool init paths that don't check for connect errors |
| `mysql.read_only_replica(query="INSERT*")` | ER_OPTION_PREVENTS_STATEMENT (1290) — writes routed to a replica |
| `mysql.disk_full(query="INSERT*")` | ER_RECORD_FILE_FULL (1114) — "The table is full" |
| `mysql.gone_away(query="*")` | Drop connection mid-query — driver sees classic "MySQL server has gone away" |
| `mysql.slow_query(duration="3s", query="*")` | Delays any statement — tests client query timeouts |
| `mysql.slow_writes(duration="3s", query="INSERT*")` | Delays writes only |

### `@faultbox/recipes/grpc.star`

| Recipe | What it simulates |
|---|---|
| `grpc.unavailable(method="*")` | Code 14 (UNAVAILABLE) — most retried gRPC error; transient server outage |
| `grpc.deadline_exceeded(method="*")` | Code 4 (DEADLINE_EXCEEDED) — per-call deadline missed |
| `grpc.resource_exhausted(method="*")` | Code 8 (RESOURCE_EXHAUSTED) — quota / rate limit / inflight cap |
| `grpc.unauthenticated(method="*")` | Code 16 (UNAUTHENTICATED) — missing/invalid/expired credentials |
| `grpc.permission_denied(method="*")` | Code 7 (PERMISSION_DENIED) — identity known but unauthorized |
| `grpc.internal(method="*")` | Code 13 (INTERNAL) — generic server-side failure; non-retryable |
| `grpc.not_found(method="*")` | Code 5 (NOT_FOUND) — target resource doesn't exist |
| `grpc.aborted(method="*")` | Code 10 (ABORTED) — transactional/optimistic-concurrency conflict |
| `grpc.slow_method(method="*", duration="3s")` | Delays RPC response — tests client deadline propagation |
| `grpc.connection_drop(method="*")` | TCP close mid-call — resolver + subchannel reconnect paths |

### `@faultbox/recipes/kafka.star`

| Recipe | What it simulates |
|---|---|
| `kafka.not_leader_for_partition(topic="*")` | Error 6 — produced to a broker no longer the partition leader |
| `kafka.rebalancing(topic="*")` | Error 27 (REBALANCE_IN_PROGRESS) — simplified trigger for consumer rebalance handlers |
| `kafka.offset_out_of_range(topic="*")` | Error 1 — consumer offset past log head / before retention cutoff |
| `kafka.message_too_large(topic="*")` | Error 10 — produce payload exceeds `message.max.bytes` |
| `kafka.coordinator_not_available(topic="*")` | Error 15 — consumer-group coordinator unavailable; exposes shutdown-order bugs |
| `kafka.broker_overloaded(topic="*")` | Request quota exceeded — tests client back-pressure |
| `kafka.slow_produce(duration="3s", topic="*")` | Delays produce requests — tests linger.ms + batching |
| `kafka.connection_drop(topic="*")` | TCP drop mid-request — forces driver reconnect |

### `@faultbox/recipes/mongodb.star`

| Recipe | What it simulates |
|---|---|
| `mongodb.disk_full(collection="*")` | Full data disk on insert — `assertion: 10334 disk full` |
| `mongodb.auth_failed()` | SASL authentication rejection |
| `mongodb.replica_unavailable(collection="*")` | Write concern failure; no primary available during election |
| `mongodb.slow_query(collection="*", duration="3s")` | Delays `find()` — tests client read-timeout and retry |
| `mongodb.slow_writes(collection="*", duration="3s")` | Delays `insert` — tests write-timeout + transaction rollback |
| `mongodb.connection_drop(collection="*", op="*")` | Closes connection mid-command — triggers driver reconnect path |
| `mongodb.duplicate_key_error(collection="*")` | Unique-index violation on insert (E11000) |
| `mongodb.write_conflict(collection="*")` | Transient transaction error — drivers retry per protocol |

### `@faultbox/recipes/http2.star`

| Recipe | What it simulates |
|---|---|
| `http2.rate_limited(path="/*")` | HTTP 429 with Retry-After |
| `http2.server_error(path="/*")` | HTTP 500 — generic internal error |
| `http2.service_unavailable(path="/*")` | HTTP 503 — retryable |
| `http2.gateway_timeout(path="/*")` | HTTP 504 — upstream timeout |
| `http2.slow_endpoint(path="/*", duration="3s")` | Fixed-latency injection |
| `http2.maintenance_window(path="/*")` | 503 with Retry-After — typical LB "we're deploying" response |
| `http2.stream_reset(path="/*")` | RST_STREAM via drop |
| `http2.flaky(path="/*", probability="20%")` | Probabilistic 500s — retry tests |
| `http2.unauthorized(path="/*")` | HTTP 401 — auth / token-refresh tests |
| `http2.forbidden(path="/*")` | HTTP 403 — authorization failures |

### `@faultbox/recipes/cassandra.star`

| Recipe | What it simulates |
|---|---|
| `cassandra.write_timeout(query="INSERT*")` | Coordinator-level write timeout |
| `cassandra.read_timeout(query="SELECT*")` | Read timeout — driver retries |
| `cassandra.unavailable(query="*")` | Insufficient replicas for consistency level |
| `cassandra.overloaded(query="*")` | `OverloadedException` — driver tries different coordinator |
| `cassandra.slow_reads(duration="3s")` | Delays SELECTs — tests speculative execution |
| `cassandra.slow_writes(duration="3s")` | Delays INSERT/UPDATE/DELETE |
| `cassandra.connection_drop(query="*")` | Connection reset mid-statement |
| `cassandra.schema_mismatch(query="*")` | Stale schema version — drivers refresh cache |

### `@faultbox/recipes/nats.star`

| Recipe | What it simulates |
|---|---|
| `nats.slow_consumer(subject=">")` | "Slow Consumer Detected" — server dropped messages the subscriber couldn't drain |
| `nats.no_responders(subject=">")` | "503 No Responders" — request-reply sent to a subject with zero subscribers |
| `nats.max_payload(subject=">")` | "Maximum Payload Exceeded" — publish larger than `max_payload` |
| `nats.authorization_violation(subject=">")` | Authorization violation — credential/account-level denial |
| `nats.permissions_violation(subject=">")` | Permissions violation — subject-level denial |
| `nats.stale_connection(subject=">")` | "Stale Connection" — server-side keep-alive failure; forces reconnect |
| `nats.slow_delivery(duration="3s", subject=">")` | Delays message delivery — consumer processing deadlines |
| `nats.connection_drop(subject=">")` | TCP close mid-stream — server-list failover test |

### `@faultbox/recipes/memcached.star`

| Recipe | What it simulates |
|---|---|
| `memcached.server_error(command="*", key="*")` | `SERVER_ERROR out of memory` — classic under-provisioned cache |
| `memcached.client_error(command="*", key="*")` | `CLIENT_ERROR` — protocol error (non-retryable) |
| `memcached.not_stored(command="add", key="*")` | `NOT_STORED` — add-finds-existing or replace-finds-missing |
| `memcached.exists(command="cas", key="*")` | `EXISTS` — stale CAS token; optimistic-concurrency retry path |
| `memcached.item_too_large(command="set", key="*")` | `SERVER_ERROR object too large` — `-I`/`item_size_max` exceeded |
| `memcached.busy(command="*", key="*")` | `SERVER_ERROR busy` — slab reassignment / LRU maintenance |
| `memcached.slow_command(duration="3s", command="*", key="*")` | Delays every command — read-timeout tests |
| `memcached.connection_drop(command="*", key="*")` | TCP close mid-command — pool evict + reconnect backoff |

### `@faultbox/recipes/clickhouse.star`

| Recipe | What it simulates |
|---|---|
| `clickhouse.too_many_parts(query="INSERT*")` | Insert rate exceeds merge rate — drivers back off |
| `clickhouse.memory_limit(query="SELECT*")` | Query exceeds memory quota (code 241) |
| `clickhouse.table_not_exists(query="*")` | Missing table (code 60) |
| `clickhouse.readonly_mode(query="INSERT*")` | Server refuses writes during maintenance |
| `clickhouse.slow_analytics(duration="5s")` | Delays SELECTs — dashboard / ETL timeout tests |
| `clickhouse.slow_ingest(duration="3s")` | Delays INSERTs — producer back-pressure tests |
| `clickhouse.connection_drop(query="*")` | HTTP connection reset mid-query |
| `clickhouse.replica_stale(query="SELECT*")` | Replica too far behind leader |

### `@faultbox/recipes/amqp.star`

| Recipe | What it simulates |
|---|---|
| `amqp.channel_error(routing_key="#")` | Channel-level soft error — channel closed, connection alive |
| `amqp.connection_error(routing_key="#")` | `CONNECTION_FORCED` — hard error, full reconnect + redeclare |
| `amqp.resource_locked(routing_key="#")` | `RESOURCE_LOCKED` — exclusive-consumer contention |
| `amqp.access_refused(routing_key="#")` | `ACCESS_REFUSED` — vhost or exchange-level permission denial |
| `amqp.precondition_failed(routing_key="#")` | `PRECONDITION_FAILED` — queue redeclared with different args |
| `amqp.publish_nack(routing_key="#")` | Publisher-confirm nack — broker refused the publish |
| `amqp.broker_unavailable(routing_key="#")` | Broker unreachable — rolling restart / cluster failover |
| `amqp.slow_publish(duration="3s", routing_key="#")` | Delays publishes — confirm-deadline + back-pressure tests |
| `amqp.connection_drop(routing_key="#")` | TCP close mid-frame — reconnect + topology-redeclare path |
### `@faultbox/recipes/udp.star`

| Recipe | What it simulates |
|---|---|
| `udp.packet_loss(probability="100%")` | Datagram drops (default 100% blackout) |
| `udp.dns_flap(probability="50%")` | Aggressive 50% loss typical of unreliable DNS |
| `udp.metrics_slow(duration="1s")` | Delays datagrams — StatsD / metrics slow-path |
| `udp.jitter(duration="100ms")` | Fixed per-packet delay — congestion simulation |
| `udp.blackhole()` | Drops every datagram — total UDP partition |


## User-authored recipes

The `@faultbox/` prefix is reserved for the stdlib. Your own recipes
live on the filesystem and follow the same namespace struct pattern:

```python
# my-company/recipes/checkout.star
checkout = struct(
    # post_q2_race simulates the specific race that took us down in Q2.
    post_q2_race = lambda: [
        delay(path = "/checkout", delay = "800ms"),
        error(path = "/inventory/reserve", status = 409),
    ],
)
```

Load them via a relative path:

```python
load("@faultbox/recipes/mongodb.star", "mongodb")   # stdlib
load("./recipes/checkout.star",        "checkout")  # your project

rules = [mongodb.disk_full(), checkout.post_q2_race()]
```

## Forking a stdlib recipe

To customize a stdlib recipe, copy its source into your project and
load from there:

```
$ faultbox recipes show mongodb > recipes/mongodb-custom.star
$ # edit recipes/mongodb-custom.star as needed
```

```python
load("./recipes/mongodb-custom.star", "mongodb")
```

## Contributing to the stdlib

Recipes are **data, not code** — no Go changes, just Starlark. To add
a new one:

1. Open the relevant `recipes/<protocol>.star` file in the Faultbox repo.
2. Add a new field to the existing struct (not a top-level `def`).
3. The field must return a `ProxyFaultDef` (output of `response()`,
   `error()`, `delay()`, or `drop()`).
4. Use sensible defaults — the zero-arg call should do something useful.
5. Add a one-line comment above describing the real-world failure.
6. Update this catalog and `recipes/README.md` in the repo.

See RFC-018 for the stability contract and what recipes must not do
(no I/O, no control flow, no nested loads).
