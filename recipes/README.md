# Faultbox Recipes

Curated, protocol-specific failure helpers built on top of the core proxy
fault primitives (`error`, `delay`, `drop`). Each recipe encodes a
canonical error message, code, or incident pattern drawn from real
postmortems — so specs can say "disk full" instead of remembering
`SQLSTATE 53100` or `assertion: 10334`.

See [RFC-018](../docs/rfcs/0018-recipes-library.md) for the design.

## Usage

```python
load("./recipes/mongodb.star", "disk_full", "replica_unavailable")

broken = fault_assumption("broken_mongo",
    target = db.main,
    rules  = [disk_full(collection = "orders")],
)
```

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

Categories covered per protocol. Blank cells are open for contribution.

| Protocol | Resource exhaustion | Transient | Consistency | Integrity | Auth | Noise |
|----------|---------------------|-----------|-------------|-----------|------|-------|
| MongoDB  | `disk_full` | `slow_query`, `slow_writes`, `connection_drop` | `replica_unavailable`, `write_conflict` | `duplicate_key_error` | `auth_failed` | — |

## Contributing

Adding a recipe:

1. Open the relevant `recipes/<protocol>.star` file.
2. Define a new function that returns a `ProxyFaultDef` (the output of
   `error()`, `delay()`, or `drop()`).
3. Use sensible defaults — the zero-arg call should do something useful.
4. Add a one-line comment describing the real-world failure it simulates.
5. Update this README's coverage matrix.

Recipes must be pure: no I/O, no control flow beyond the function body,
no `load()` from other recipes. If what you need can't be expressed in
one function with one primitive, open an RFC for a new core primitive.
