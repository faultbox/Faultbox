# Faultbox Recipes

Curated, protocol-specific failure helpers built on top of the core proxy
fault primitives (`response`, `error`, `delay`, `drop`).

See [RFC-018](../docs/rfcs/0018-recipes-library.md) for the design.

## The namespace struct pattern

Each recipe file exports **one struct** named after the protocol. This
prevents name collisions when users load recipes for multiple protocols
(e.g. `mongodb.disk_full` vs `postgres.disk_full`):

```python
load("./recipes/mongodb.star", "mongodb")
load("./recipes/postgres.star", "postgres")

rules = [
    mongodb.disk_full(collection = "orders"),
    postgres.disk_full(),
]
```

One import per protocol, clean call sites, zero collisions.

## Protocols

| Protocol | Recipes file | Status |
|----------|--------------|--------|
| MongoDB | [mongodb.star](mongodb.star) | ✅ Shipped |
| HTTP/2 | [http2.star](http2.star) | ✅ Shipped |
| UDP | [udp.star](udp.star) | ✅ Shipped |
| Cassandra | [cassandra.star](cassandra.star) | ✅ Shipped |
| ClickHouse | [clickhouse.star](clickhouse.star) | ✅ Shipped |
| Postgres | — | To be added |
| Redis | — | To be added |
| Kafka | — | To be added |
| HTTP | — | To be added |
| gRPC | — | To be added |
| MySQL | — | To be added |
| NATS | — | To be added |

## Coverage matrix

| Protocol | Resource | Transient | Consistency | Integrity | Auth | Noise |
|----------|----------|-----------|-------------|-----------|------|-------|
| MongoDB | `disk_full` | `slow_query`, `slow_writes`, `connection_drop` | `replica_unavailable`, `write_conflict` | `duplicate_key_error` | `auth_failed` | — |
| HTTP/2 | `rate_limited`, `service_unavailable` | `slow_endpoint`, `gateway_timeout`, `stream_reset`, `maintenance_window` | — | — | `unauthorized`, `forbidden` | `flaky`, `server_error` |
| UDP | — | `metrics_slow`, `jitter` | — | — | — | `packet_loss`, `dns_flap`, `blackhole` |
| Cassandra | `overloaded` | `write_timeout`, `read_timeout`, `slow_reads`, `slow_writes`, `connection_drop` | `unavailable`, `schema_mismatch` | — | — | — |
| ClickHouse | `too_many_parts`, `memory_limit` | `slow_analytics`, `slow_ingest`, `connection_drop`, `replica_stale` | — | `table_not_exists` | — | `readonly_mode` |

## Contributing

Adding a recipe:

1. Open the relevant `recipes/<protocol>.star` file.
2. Add a new field to the existing struct (NOT a top-level def).
3. The field must return a `ProxyFaultDef` (the output of `response()`,
   `error()`, `delay()`, or `drop()`).
4. Use sensible defaults — the zero-arg call should do something useful.
5. Add a one-line comment above describing the real-world failure it simulates.
6. Update this README's coverage matrix.

Adding a new protocol:

1. Create `recipes/<protocol>.star`.
2. Export a single struct field named `<protocol>`.
3. Populate with 5+ recipes covering the categories that make sense for
   the protocol (see matrix above).
4. Add a row to both tables in this README.

Recipes must be pure: no I/O, no control flow beyond the lambda body,
no `load()` from other recipes. If what you need can't be expressed in
one lambda over existing primitives, open an RFC for a new core primitive.
