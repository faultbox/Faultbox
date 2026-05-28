# RFC-002: `domain()` Primitive & Multi-Service Grouping

- **Status:** Withdrawn (RFC-044 §8.4, v0.13.0)
- **Author:** —
- **Created:** —

## Withdrawal rationale

RFC-002 was an early sketch for grouping N microservices into a single
`domain()` declaration that exported API/message contracts. The project
pivoted to a `service()`-first model: each service is independent and
contracts are expressed through `interface()` declarations on those
services rather than a higher-level wrapper.

`domain()` was never specified in detail, never prototyped, and never
appeared as a customer ask after the pivot. Three years of accumulated
usage with `service()` + `interface()` validated that the two-level
model is sufficient — adding a third level (`domain` → `service` →
`interface`) would have introduced abstraction overhead with no
corresponding power.

RFC-002 is withdrawn rather than deferred because:

- The `service()`-first model is now load-bearing across the runtime,
  the bundle format, the report, and the docs. Retrofitting
  `domain()` would touch all of them.
- RFC-004 and RFC-005 also reference `domain()` as a dependency
  primitive; those RFCs are themselves draft/deferred and will pick
  a different anchor when they're rewritten.
- Customer feedback in 2026 Q1 confirmed that multi-service grouping
  is needed at the *test organization* layer (which `fault_matrix`
  already covers) rather than the *spec topology* layer.

No code action is required — `domain()` was never implemented.

## Related

- `service()` and `interface()` — the surviving topology primitives,
  documented in [docs/spec-language.md](../spec-language.md).
- RFC-044 §8.4 — formal withdrawal under the v0.13.0 simplification
  epic.
