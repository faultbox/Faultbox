# Faultbox Recipes

Curated, protocol-specific failure helpers built on top of the core proxy
fault primitives (`response`, `error`, `delay`, `drop`).

See [RFC-018](../docs/rfcs/0018-recipes-library.md) for the design.

## The namespace struct pattern

Each recipe file exports **one struct** named after the protocol:

```python
load("./recipes/http2.star", "http2")

faulty = fault_assumption("faulty_api",
    target = api.public,
    rules  = [http2.rate_limited(path = "/api/**")],
)
```

One import per protocol, zero name collisions across recipe files.

> **Runtime requirement:** this pattern needs the `struct()` builtin,
> wired in PR #37 (RFC-018 amendment). Until that lands, the recipe
> files here will fail to load in a spec. The recipe shape is correct
> per the amended RFC-018.

## Protocols

| Protocol | Recipes file | Status |
|----------|--------------|--------|
| HTTP/2 | [http2.star](http2.star) | ✅ Shipped |
| Postgres | — | To be added |
| Redis | — | To be added |
| Kafka | — | To be added |
| HTTP | — | To be added |
| gRPC | — | To be added |
| MySQL | — | To be added |
| NATS | — | To be added |

## Contributing

Adding a recipe:

1. Open the relevant `recipes/<protocol>.star` file.
2. Add a new field to the existing struct (NOT a top-level def).
3. The field must return a `ProxyFaultDef` (the output of `response()`,
   `error()`, `delay()`, or `drop()`).
4. Use sensible defaults — the zero-arg call should do something useful.
5. Add a one-line comment above describing the real-world failure it simulates.
6. Update this README.

Recipes must be pure: no I/O, no control flow beyond the lambda body,
no `load()` from other recipes.
