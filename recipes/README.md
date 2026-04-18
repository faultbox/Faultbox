# Faultbox Recipes

Curated, protocol-specific failure helpers built on top of the core proxy
fault primitives (`response`, `error`, `delay`, `drop`).

See [RFC-018](../docs/rfcs/0018-recipes-library.md) for the design.

## The namespace struct pattern

Each recipe file exports **one struct** named after the protocol:

```python
load("./recipes/cassandra.star", "cassandra")

quorum_lost = fault_assumption("quorum_lost",
    target = cass.main,
    rules  = [cassandra.unavailable()],
)
```

One import per protocol, zero name collisions across recipe files.

> **Runtime requirement:** this pattern needs the `struct()` builtin,
> wired in PR #37. Without it, recipe files fail to load at runtime.

## Protocols

| Protocol | Recipes file | Status |
|----------|--------------|--------|
| Cassandra | [cassandra.star](cassandra.star) | ✅ Shipped |
| Postgres | — | To be added |
| Redis | — | To be added |
| Kafka | — | To be added |
| HTTP | — | To be added |
| gRPC | — | To be added |
| MySQL | — | To be added |
| NATS | — | To be added |
