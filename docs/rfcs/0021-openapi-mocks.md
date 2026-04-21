# RFC-021: OpenAPI-Driven Mock Service Generation

- **Status:** Implemented (v0.9.3, 2026-04-22)
- **Target:** v0.9.3
- **Created:** 2026-04-19
- **Accepted:** 2026-04-22 — open questions resolved
- **Implemented:** 2026-04-22 — 3 phases shipped on `rfc/021-openapi-mocks`
- **Discussion:** [#40](https://github.com/faultbox/Faultbox/issues/40)
- **Depends on:** RFC-017 (Native Mock Services — v0.8.0)
- **Parallel gRPC/proto path:** shipped separately via RFC-023 in v0.9.0; this
  RFC is scoped to OpenAPI/HTTP only.

## Summary

Add an OpenAPI-driven path to `mock_service()` that generates routes
automatically from an OpenAPI 3.x document. Users point Faultbox at a
schema file and get a working mock whose routes and response shapes are
derived from the contract — no manual route table.

```python
load("@faultbox/mocks/http.star", "http")

auth = http.server(
    name      = "auth",
    interface = interface("main", "http", 8090),
    openapi   = "./specs/auth.openapi.yaml",
    examples  = "first",   # "first" | "<name>" | "random" | "synthesize"
    validate  = "strict",  # "off" | "warn" | "strict"
    overrides = {
        "GET /admin/health": json_response(status = 503, body = {"down": True}),
    },
)
```

## Motivation

### Manual route tables don't scale

RFC-017 ships `routes={"METHOD /path": response(...)}`. This is clean
for the 3-route JWKS stub but breaks down for any real API:

- An auth service with 15 OIDC endpoints → 15 entries, most boilerplate
- A feature-flag service with 30 operations → 30 entries, drift with producer
- A backend-for-frontend with dozens of routes → hundreds of lines of mock

Every manually-authored route is a place the mock and the real contract
can diverge silently. Contract-driven generation closes that gap.

### OpenAPI is already the source of truth

Most teams already maintain OpenAPI specs for real services. Consuming
them directly means:

1. **No drift.** Mock stays in sync with the producer spec automatically.
2. **Less boilerplate.** Generated routes use example values from the spec.
3. **Request validation.** Malformed requests from the SUT fail at the
   mock (as they would at the real service), catching integration bugs.
4. **Type-correct responses.** Response shapes come from the schema, not
   from what the mock author happened to remember.

## Technical details

### Shape that shipped

```python
# Basic — generate everything from examples declared in the spec.
auth = http.server(
    name      = "auth",
    interface = interface("main", "http", 8090),
    openapi   = "./specs/auth.openapi.yaml",
)

# With explicit strategies + overrides.
flags = http.server(
    name      = "flags",
    interface = interface("main", "http", 9000),
    openapi   = "./specs/flags.openapi.yaml",
    examples  = "success",     # pick the example named "success" on each op
    validate  = "strict",      # reject requests that fail requestBody schema
    overrides = {
        # Patterns accept OpenAPI-style `{id}` and are normalised to
        # `*` internally to match generated glob patterns.
        "GET /admin/**":     status_only(401),
        "POST /tokens":      dynamic(lambda req: json_response(body = {"token": "fake"})),
    },
)
```

### Response-selection strategies

- **`examples = "first"`** (default) — deterministic, picks the inline
  `example:` if present, else the first entry in the op's `examples:` map
  (sorted alphabetically for determinism).
- **`examples = "<name>"`** — selects the named example across all
  operations. If an op is missing that name, falls back to `"first"` —
  real specs rarely declare every named variant on every op, so
  hard-erroring would make the feature unusable.
- **`examples = "random"`** — seeded random per operation (seed fixed
  to 1 for reproducibility). Useful for exercising client code paths
  that depend on different response bodies.
- **`examples = "synthesize"`** — behaves like `"first"` for ops that
  declare examples; for ops with only a schema and no example, synthesizes
  a minimal type-correct value (empty string, 0, empty object/array).
  Opt-in because the synthesized data is deliberately uninteresting —
  real examples are always better when available.

### Request validation (`validate=`)

- **`"off"`** (default) — no validation, match behaviour of RFC-017 routes.
- **`"warn"`** — emit a `mock.<METHOD> <path>` event with a
  `validate_error` field but serve the generated response anyway.
  Useful for passively catching contract drift in CI.
- **`"strict"`** — reject requests whose body doesn't match the
  operation's `requestBody` schema with HTTP 400. Matches the behaviour
  of OpenAPI-enforcing gateways like Prism.

Only JSON bodies are validated in v0.9.3. Non-JSON content types pass
through without inspection (future enhancement if asked).

### Overrides (`overrides=`)

Overrides replace OpenAPI-generated routes with the same normalised
pattern. They're dispatched first in the match order, so a user's
override always wins over the generated entry.

OpenAPI-style path templates (`{id}`) are accepted in override keys and
converted to glob segments (`*`) internally. This means override keys
can be copied verbatim from the OpenAPI spec:

```python
overrides = {
    "GET /pets/{id}":        json_response(status = 503),  # → GET /pets/*
    "POST /admin/users":     status_only(401),             # literal
},
```

Overrides also accept `dynamic(fn)` values — same semantics as
`routes={}` entries.

### Open design questions — resolutions

| OQ | Resolution |
|---|---|
| 1. 3.0 vs 3.1 | **3.0** (kin-openapi v0.135) ships in v0.9.3. 3.1 support deferred — no customer has asked, and 3.1 adoption is <5% of specs observed so far. |
| 2. External `$ref` | **Filesystem only.** `http://`/`https://` refs error at load time to avoid surprising network I/O during `faultbox test`. Escape hatch: inline the referenced content. |
| 3. Example synthesis quality | **Minimal type-correct values** (empty string, 0, empty object/array). Opt-in via `examples="synthesize"`. Realistic fakes (emails, names) deferred — not obviously better than explicit examples, which spec authors should declare anyway. |
| 4. Overrides: replace or layer | **Replace.** Simpler mental model, no precedence rules to memorise. Users who want "keep body, change status" write a new `mock_response()`. |
| 5. `dynamic(fn)` composition | **Yes, fully composes with overrides.** An override value can be any `mock_response`-producing expression, including `dynamic(fn)`. |
| 6. Load-time validation | **Parse + structural validate** the OpenAPI document at `LoadString` time; malformed specs fail fast. Missing examples don't error until a route that lacks them is actually built (unless `examples="synthesize"` is enabled). |

### Alternatives considered (and rejected)

- **Regenerate routes manually via a CLI.** `faultbox generate openapi ...`
  emits a `routes={...}` block the user copies in. Rejected — reintroduces
  the drift this RFC is meant to close.
- **Wrap Prism as a subprocess.** Rejected — third-party runtime, and the
  "no containers" goal of RFC-017 is a non-negotiable for mock ergonomics.
- **Ship schema support as a library without generation.** Rejected —
  solves validation but not the boilerplate problem.

## Impact

- **Breaking changes:** none. New kwargs on `mock_service()`; existing
  `routes={}` callers are unaffected. Bump: dependency on
  `github.com/getkin/kin-openapi`.
- **Migration:** opt-in. Manual `routes={}` continues to work and can be
  combined with `openapi=` (user routes serve alongside generated ones).
- **Performance:** OpenAPI parsing happens once at spec load. Request
  validation is opt-in (`validate="strict"`); `"off"` is zero overhead.
- **Security:** `http://` `$ref` fetches are rejected at load time so
  `faultbox test` can never be tricked into downloading content.

## Implementation notes (for future-us)

- `internal/protocol/openapi.go` holds the loader, `GenerateRoutes`,
  selector implementations, and `ValidateRequest`.
- `internal/protocol/mock.go` carries `OpenAPI *OpenAPISpec` and
  `ValidateMode string` on `MockSpec`; the HTTP mock handler consults
  both at serve time.
- `internal/star/mock_builtins.go` parses `openapi=`, `examples=`,
  `validate=`, `overrides=` and routes them through `MockConfig`.
- `internal/star/mock_runtime.go` composes the final route table as
  `[overrides ... generated ... user-routes]` so overrides eclipse
  generated entries with the same (normalised) pattern.
- `mocks/http.star` is the user-facing stdlib entry point.

## Tests

- `internal/protocol/openapi_test.go` — 14 cases: loader (valid, malformed,
  missing-file), selectors (first, named, named-fallback, random determinism,
  synthesize-missing, missing-example-errors), path normalisation,
  validation (strict, template-matching, malformed-JSON), `http://` `$ref` rejection.
- `internal/star/mock_runtime_test.go` — 5 e2e cases through the
  `@faultbox/mocks/http.star` stdlib: happy path (Petstore), overrides,
  strict validation, schema synthesis, malformed-spec fail-at-load.
