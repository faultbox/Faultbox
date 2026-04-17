# Faultbox Recipes

Curated, protocol-specific failure helpers built on top of the core proxy
fault primitives (`response`, `error`, `delay`, `drop`). Each recipe
encodes a canonical error shape or incident pattern so specs can say
"rate limited" instead of remembering the right status code and body.

See [RFC-018](../docs/rfcs/0018-recipes-library.md) for the design.

## Usage

```python
load("./recipes/http2.star", "rate_limited", "slow_endpoint")

faulty = fault_assumption("faulty_api",
    target = api.public,
    rules  = [rate_limited(path = "/api/**")],
)
```

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
2. Define a new function that returns a `ProxyFaultDef` (the output of
   `response()`, `error()`, `delay()`, or `drop()`).
3. Use sensible defaults — the zero-arg call should do something useful.
4. Add a one-line comment describing the real-world failure it simulates.
5. Update this README.

Recipes must be pure: no I/O, no control flow beyond the function body,
no `load()` from other recipes.
