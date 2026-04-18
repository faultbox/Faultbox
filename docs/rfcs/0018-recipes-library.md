# RFC-018: Recipes Library — Curated Failure Wrappers in Starlark

- **Status:** Draft
- **Author:** Boris Glebov, Claude Opus 4.7
- **Created:** 2026-04-17
- **Amended:** 2026-04-18 — namespace struct pattern (was top-level functions)
- **Branch:** `rfc/016-mongodb` (introduced alongside MongoDB); amended on `rfc/018-recipe-namespaces` (PR #37)

## Summary

Establish `recipes/<protocol>.star` as the **canonical place for curated,
protocol-specific failure helpers**. Each recipe is a thin Starlark wrapper
over the existing proxy fault primitives (`error`, `delay`, `drop`) that
encodes canonical error messages, codes, and realistic failure scenarios
for a given protocol.

Recipes are **data, not builtins.** Zero new Go code per recipe. Users can
read, copy, fork, or extend them. The Faultbox project maintains a core
set; users ship their own alongside.

## Motivation

### The error-string problem

Writing a realistic fault today requires knowing the exact error message
the real service would emit:

```python
# What the user wants to say:
rules = [error(query="INSERT*", message="disk full")]

# What the real Postgres would say:
rules = [error(query="INSERT*", message="ERROR: could not extend file \"base/...\": No space left on device (SQLSTATE 53100)")]
```

Most users write the short version on their first attempt. The SUT's error
handling might match on SQLSTATE codes or specific substrings — so the
injected fault passes, but the production fault would fail. The test gives
false confidence.

Same problem across protocols: MongoDB error codes, HTTP status + body
conventions, Kafka error codes, Cassandra replica-count semantics, Redis
OOM messages. Every protocol has a dialect of canonical errors.

### Why not Go builtins?

We considered shipping `mongo_disk_full()`, `postgres_deadlock()`, etc. as
Go builtins. Rejected because:

1. **Curation burden.** Every new realistic failure is a Faultbox PR.
2. **Versioning.** MongoDB 4 → 5 → 7 changed error messages. Go code rots.
3. **Abstraction bloat.** Two APIs (primitives + helpers) for the same
   thing creates docs ambiguity.
4. **Hidden machinery.** Users can't see what the helper actually injects
   without reading Go source.

### Why not "just use primitives"?

Core primitives (`error`, `delay`, `drop`) are the right abstraction for
the **engine**. They are too low-level for the **user**, who is thinking
in terms of incidents: "quorum loss," "disk full," "noisy neighbor," "DNS
flap." Recipes bridge the two.

## Design

### Directory layout

```
faultbox/
├── recipes/
│   ├── README.md                 # catalog + usage
│   ├── mongodb.star
│   ├── postgres.star             # to be added
│   ├── redis.star                # to be added
│   ├── kafka.star                # to be added
│   ├── grpc.star                 # to be added
│   └── http.star                 # to be added
```

### Recipe file structure

Each recipe file exports **exactly one top-level binding**: a `struct`
named after the protocol. Every recipe is a field on that struct,
implemented as a lambda that returns a `ProxyFaultDef` (the same value
`error()`/`delay()`/`drop()`/`response()` produce).

```python
# recipes/mongodb.star
mongodb = struct(
    # disk_full simulates a full data disk on insert.
    disk_full = lambda collection = "*": error(
        collection = collection,
        op = "insert",
        message = "assertion: 10334 disk full",
    ),

    # auth_failed rejects SASL handshakes.
    auth_failed = lambda: error(
        op = "saslStart",
        message = "Authentication failed",
    ),
)
```

The rules:

1. **One struct per file, named after the protocol.** Not `mongo`, not
   `MongoDBRecipes` — match the protocol name registered in `protocol.Get()`.
2. **Fields are lambdas.** Not `def`s inside the struct (Starlark syntax
   doesn't support that directly) and not top-level `def`s (defeats the
   namespace).
3. **One-line comment above each field** describing the real-world
   failure it simulates.
4. **Sensible defaults.** The zero-arg call should produce a useful fault.
5. **No other top-level bindings.** No helper functions, no constants.
   If you need shared logic, inline it.

### Why a struct (not top-level functions)

The earlier draft of this RFC had recipes as top-level `def`s. That
broke the moment a user loaded two recipe files with overlapping names:

```python
# What we want to write:
load("./recipes/mongodb.star",  "disk_full")
load("./recipes/postgres.star", "disk_full")  # <-- collision
```

Starlark's `load()` rename syntax (`load(..., m_disk_full="disk_full")`)
works but is clunky — every user reinvents naming conventions. The
namespace struct solves it structurally:

```python
load("./recipes/mongodb.star",  "mongodb")
load("./recipes/postgres.star", "postgres")

rules = [mongodb.disk_full(collection = "orders"), postgres.disk_full()]
```

One import per protocol, collision-free, reads naturally.

### Usage

```python
load("./recipes/mongodb.star",  "mongodb")
load("./recipes/postgres.star", "postgres")

broken_mongo = fault_assumption("broken_mongo",
    target = db.main,
    rules  = [
        mongodb.disk_full(collection = "orders"),
        mongodb.replica_unavailable(),
    ],
)

broken_pg = fault_assumption("broken_pg",
    target = pg.main,
    rules  = [postgres.disk_full(), postgres.deadlock()],
)
```

### Recipe catalog (per protocol)

Each protocol ships a baseline set covering:

| Category | Typical recipes |
|---|---|
| **Resource exhaustion** | `disk_full`, `oom`, `connection_exhausted`, `too_many_connections` |
| **Transient** | `slow_query`, `slow_writes`, `connection_drop`, `timeout` |
| **Consistency** | `replica_unavailable`, `quorum_lost`, `read_after_write_lag`, `write_conflict` |
| **Integrity** | `duplicate_key_error`, `constraint_violation`, `serialization_error` |
| **Auth/Security** | `auth_failed`, `permission_denied`, `token_expired` |
| **Noise** | `intermittent_errors`, `flappy_connection` |

Not every protocol needs every category. MongoDB won't have
`quorum_lost` until we support replica-set-aware fault injection;
HTTP won't have `duplicate_key_error`. We ship what makes sense per
protocol and grow from there.

### Discovery

Recipes are discovered via:

1. **`recipes/README.md`** — canonical catalog, kept in sync with files.
2. **Protocol docs** — each `docs/protocols/<name>.md` has a "Recipes"
   section linking to the recipes file with a 1-line summary of each
   exported function.
3. **VS Code autocomplete** — the schema tooling (future RFC) surfaces
   recipes alongside builtins.

### Stability contract

Recipes are **semver-stable within a major version**:

- **Adding a new recipe:** always safe, any release.
- **Removing a recipe:** deprecation warning for one minor version, removal
  in the next.
- **Changing a recipe's signature:** same rules as removing (deprecate first).
- **Changing what a recipe *injects* (error message, delay default):**
  patch release if the real service's canonical message changed; minor
  release if it's a judgment call. Document both in changelog.

### What recipes MUST NOT do

- **No top-level bindings other than the namespace struct.** The single
  `<protocol> = struct(...)` binding is the entire module's public API.
- **No Starlark control flow beyond the lambda body.** Recipes are
  "return a fault def." No `if target == ...`, no `for` loops, no I/O.
- **No new builtins.** A recipe that needs something `error()` can't
  express is a sign we need a new primitive, not a bigger recipe.
- **No state.** Recipes are pure — same args in, same fault def out.
- **No loads.** Recipes can't depend on other recipes or modules. Each
  file stands alone. Prevents cascading breakage.

### User-authored recipes

Users follow the same pattern with a namespace named after their domain
(not a protocol):

```python
# my-company/recipes/checkout.star

checkout = struct(
    # post_checkout_race simulates the specific race that took us down in Q2.
    post_checkout_race = lambda: [
        delay(path = "/api/v1/checkout", delay = "800ms"),
        error(path = "/api/v1/inventory/reserve", status = 409),
    ],
)
```

Usage is symmetric:

```python
load("./recipes/checkout.star", "checkout")

q2_regression = fault_assumption("q2_regression",
    target = api.main,
    rules  = checkout.post_checkout_race(),
)
```

No plugin API, no registration — just Starlark.

## Open questions

1. **Recipe composition.** Should recipes be allowed to return a **list**
   of rules? `replica_unavailable()` arguably wants to inject both an
   error response AND drop the replica heartbeat. Proposal: yes, allow
   lists, handled by existing `rules=[...]` flatten logic.

2. **Parameterization via `**kwargs`.** Some recipes want to thread
   arbitrary kwargs to the underlying primitive (e.g., `probability=`).
   Proposal: every recipe exposes `probability=` explicitly when it makes
   sense; no magic splat.

3. **Recipe tests.** How do we verify a recipe still matches the real
   service's behavior as versions change? Proposal: integration tests
   that run the recipe against a real container and assert the driver
   observes the intended error shape. Tracked as a follow-up.

4. **Translation for syscall-level recipes.** Syscall faults live on
   `ServiceDef`, not `InterfaceRef`. Does `disk_full()` return a proxy
   fault or a syscall fault? Proposal: distinct namespaces —
   `recipes/mongodb.star` for proxy; `recipes/mongodb_syscall.star` for
   syscall — and never mix in one file. Keeps the target type obvious
   at the call site.

## Non-goals

- **Not a plugin API.** Recipes are plain Starlark. No Go extension
  points, no runtime registration.
- **Not an error catalog.** We don't try to enumerate every error every
  protocol can emit. We pick the 5-10 that come up in real postmortems.
- **Not a mock library.** Recipes inject faults into real traffic. Mock
  services (RFC-017) stand up fake services. Different tool, different
  problem.

## Alternatives considered

1. **Go builtins per protocol.** Rejected — curation burden, rot, hidden
   machinery (see Motivation).
2. **In-spec helper functions.** Every user writes their own helpers.
   Rejected — no sharing, no canonical messages, every team reinvents.
3. **Error code registry.** Ship a Go file mapping logical names to
   error strings per protocol. Rejected — still Go code, still rots, and
   doesn't compose (a recipe is more than just a message).

## Success criteria

- Every RFC-016 protocol ships with a `recipes/<name>.star` in the same
  PR as the protocol plugin.
- Recipe files have ≥5 functions, covering at minimum the categories
  that make sense for that protocol.
- Protocol docs link to the recipes file and summarize each export.
- The recipes directory has a README with a matrix of
  (protocol × category) showing what's covered.

## Implementation plan

1. Land `recipes/mongodb.star` in the RFC-016 MongoDB PR (this branch).
2. Write `recipes/README.md` with the catalog structure.
3. For each RFC-016 protocol implemented, add `recipes/<name>.star` in
   the same PR.
4. Backfill recipes for existing protocols (HTTP, Postgres, Redis, Kafka,
   gRPC, MySQL, NATS) as a separate housekeeping PR after RFC-016 lands.
