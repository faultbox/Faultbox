# Faultbox Recipes

Curated, protocol-specific failure helpers built on top of the core proxy
fault primitives (`error`, `delay`, `drop`). Each recipe encodes a
canonical error message, code, or incident pattern drawn from real
postmortems — so specs can say "disk full" instead of remembering
`SQLSTATE 53100` or `assertion: 10334`.

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

> **Runtime requirement:** this pattern needs the `struct()` builtin,
> wired in PR #37 (RFC-018 amendment). Until that lands, the recipe
> files here will fail to load in a spec. The recipe shape is correct
> per the amended RFC-018.

## Protocols

| Protocol | Recipes file | Status |
|----------|--------------|--------|
| MongoDB | [mongodb.star](mongodb.star) | ✅ Shipped |
| Postgres | — | To be added |
| Redis | — | To be added |
| Kafka | — | To be added |
| HTTP | — | To be added |
| gRPC | — | To be added |
| MySQL | — | To be added |
| NATS | — | To be added |

## Coverage matrix

| Protocol | Resource exhaustion | Transient | Consistency | Integrity | Auth | Noise |
|----------|---------------------|-----------|-------------|-----------|------|-------|
| MongoDB  | `disk_full` | `slow_query`, `slow_writes`, `connection_drop` | `replica_unavailable`, `write_conflict` | `duplicate_key_error` | `auth_failed` | — |

## Contributing

Adding a recipe:

1. Open the relevant `recipes/<protocol>.star` file.
2. Add a new field to the existing struct (NOT a top-level def).
3. The field must return a `ProxyFaultDef` (the output of `response()`,
   `error()`, `delay()`, or `drop()`).
4. Use sensible defaults — the zero-arg call should do something useful.
5. Add a one-line comment above describing the real-world failure it simulates.
6. Update this README's coverage matrix.

Recipes must be pure: no I/O, no control flow beyond the lambda body,
no `load()` from other recipes.
