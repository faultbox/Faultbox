# Faultbox Recipes

Curated, protocol-specific failure helpers built on top of the core proxy
fault primitives (`response`, `error`, `delay`, `drop`).

See [RFC-018](../docs/rfcs/0018-recipes-library.md) for the design.

## The namespace struct pattern

Each recipe file exports **one struct** named after the protocol:

```python
load("./recipes/udp.star", "udp")

broken = fault_assumption("broken",
    target = dns.main,
    rules  = [udp.packet_loss(probability = "30%")],
)
```

One import per protocol, zero name collisions across recipe files.

> **Runtime requirement:** this pattern needs the `struct()` builtin,
> wired in PR #37. Without it, recipe files fail to load at runtime.
> The recipe shape here is the canonical one per amended RFC-018.

## Protocols

| Protocol | Recipes file | Status |
|----------|--------------|--------|
| UDP | [udp.star](udp.star) | ✅ Shipped |
| Postgres | — | To be added |
| Redis | — | To be added |
| Kafka | — | To be added |
| HTTP | — | To be added |
| gRPC | — | To be added |
| MySQL | — | To be added |
| NATS | — | To be added |
