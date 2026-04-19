# RFC-020: SQL Mock Services — Postgres & MySQL Stubs

- **Status:** Withdrawn (2026-04-19)
- **Target:** ~~v0.9.0~~ (withdrawn)
- **Created:** 2026-04-19
- **Withdrawn:** 2026-04-19
- **Spun off from:** #31 (RFC-017 OQ4)

## Why withdrawn

The mock-service approach drifts from Faultbox's core premise — verifying **real systems** under real failure. A hand-rolled MySQL/Postgres wire-protocol stub with canned tables isn't a real system; it's a different product category (testcontainers + sqlmock / pgmock) we'd be worse at. Customers testing a DB-backed service against a mock DB aren't verifying DB behavior, they're verifying their own fixture.

The valuable core — query-pattern-matched faults on a real DB — already ships in v0.8.1. The parts worth keeping (SQL canonicalizer on the match path, Postgres recipe backfill) migrated to **v0.8.2** as incremental sharpening of the existing real-DB fault-injection story.

What survived from Phase 0: `@faultbox/recipes/postgres.star` — 10 canonical Postgres fault recipes (deadlock 40P01, lock_not_available 55P03, etc.). Shipped in v0.8.2.

The original RFC body below is preserved as design history.

---

## Summary

Extend the mock service facility (RFC-017) to cover **Postgres** and **MySQL** — the two most-requested relational databases in Faultbox topologies. SQL mocks let users stub a database dependency entirely in Starlark, answering `SELECT` queries with canned result sets shaped like real wire-level responses, accepting `INSERT`/`UPDATE`/`DELETE` as recorded events, and returning canonical error frames (deadlock, lock timeout, disk full, etc.) on demand.

This is explicitly deferred from RFC-017 because SQL mock semantics are their own design problem — unlike request/response protocols (HTTP, gRPC) or simple KV (Redis), a SQL mock must parse or pattern-match query text and return result sets whose **column names, types, and row shape** the driver validates at decode time.

## Motivation

### Real usage is SQL-heavy

From the truck-api customer intake (pre-RFC-017 feedback): MySQL is the **P0** dependency — every request touches it. Postgres comes up repeatedly in other deployments. A mock story that covers HTTP auth stubs but forces users back to container-based MySQL/Postgres for database dependencies leaves the hardest half of real topologies uncovered.

### Recipes aren't enough

The recipe library (RFC-018) wraps fault primitives over **real** servers. It lets you say "make this Postgres deadlock," but it still needs a running Postgres. For specs that want to assert SUT behavior under database unavailability without standing up the real DB, a mock is the only path.

### What a SQL mock must do

1. **Answer handshake**: Postgres `StartupMessage`, MySQL `HandshakeV10` — drivers won't even send the first query without a valid handshake.
2. **Answer metadata queries**: Drivers send `SELECT version()`, `SHOW VARIABLES`, `pg_catalog` probes at connection time. Failing these breaks the connection before user queries run.
3. **Match user queries**: Pattern-match SQL text against user-declared routes.
4. **Return typed result sets**: Not just bytes — drivers decode rows by column type. Wrong type → client-side error, test fails for the wrong reason.
5. **Emit canonical errors**: Deadlock (MySQL 1213, Postgres 40P01), lock timeout (1205, 55P03), disk full, too-many-connections, read-only replica, etc.

## Technical Details

### Proposed shape

Protocol-specific constructors shipped via the `@faultbox/mocks/` stdlib prefix, consistent with the pattern established for non-request/response protocols in RFC-017:

```python
load("@faultbox/mocks/mysql.star", "mysql")

users_db = mysql.server(
    name = "users-db",
    interface = interface("main", "mysql", 3306),
    tables = {
        "users": {
            "columns": [("id", "BIGINT"), ("email", "VARCHAR(255)"), ("role", "ENUM('admin','user')")],
            "rows":    [(1, "alice@example.com", "admin"), (2, "bob@example.com", "user")],
        },
    },
    queries = {
        "SELECT * FROM users WHERE id = ?": mysql.rows_by_id(table = "users", key = "id"),
        "INSERT INTO users*":               mysql.ok(affected_rows = 1, insert_id = "auto"),
        "UPDATE users SET*":                mysql.error(1205),  # lock wait timeout
    },
)
```

Postgres follows the same shape with `postgres.server()`, `postgres.rows_by_id()`, `postgres.error()`.

## Resolved Design Decisions

The six open questions from the Draft are resolved as follows:

### D1 — Query matching: canonicalized-match (not literal, not full parser)

Match user-declared route keys against the incoming SQL after the following normalization:

- Strip trailing `;` and surrounding whitespace.
- Collapse internal whitespace runs to single spaces.
- Lowercase the SQL **keywords only** (identifiers and literals stay case-sensitive).
- Replace `?` and `$1`/`$2`/`$N` placeholders with a canonical `$?` marker.
- Trailing `*` in the route key is a glob suffix (`INSERT INTO users*` matches any INSERT starting that way).

**Rejected:** literal match (too brittle — drivers rewrite queries). **Rejected:** full SQL parser (multi-quarter scope, brings its own bug surface). The canonicalizer is ~150 lines of Go and covers the 90% case.

### D2 — Result-set encoding: real wire frames

Emit actual Postgres `RowDescription` + `DataRow` frames and actual MySQL column definition packets + row packets. Drivers' decode paths are exactly what the mock must exercise; a lookup table diverges from production behavior precisely where bugs hide.

**One-time cost:** encoder for the v1 type set (see D5). Worth it.

### D3 — Prepared statements: v1 must support

Non-negotiable. `database/sql`, `pgx`, and `go-sql-driver/mysql` all default to prepared statements for parameterized queries. Skipping prepared-statement support ships an unusable mock.

- Postgres: Parse / Bind / Describe / Execute / Sync flow.
- MySQL: `COM_STMT_PREPARE` / `COM_STMT_EXECUTE` / `COM_STMT_CLOSE`.

The canonicalizer in D1 normalizes the prepared-statement text; the route table is indexed by canonical form, so prepared and direct queries hit the same routes.

### D4 — Transactions: accept but don't track

`BEGIN` / `COMMIT` / `ROLLBACK` / `SAVEPOINT` return success. No isolation semantics, no snapshot tracking, no row visibility rules. Per-connection transaction state simulation is a separate RFC if ever justified.

Drivers that issue `BEGIN` before every INSERT (most ORMs) will work; tests that depend on transactional rollback behavior will not — and that's fine, those tests belong against a real database.

### D5 — Column type coverage v1

Ship these types only:

- `INT` / `INTEGER`
- `BIGINT`
- `VARCHAR(N)` / `TEXT`
- `BOOL` / `BOOLEAN`
- `TIMESTAMP`
- `JSON`
- `NULL` sentinel

Deferred to v2: `DECIMAL` / `NUMERIC`, arrays, `UUID`, `BYTEA` / `BLOB`, `ENUM`, spatial types, custom types.

### D6 — Error catalog: shared with recipes

`@faultbox/mocks/mysql.star` reuses the canonical error codes + messages from `@faultbox/recipes/mysql.star` (shipped v0.8.1). `@faultbox/mocks/postgres.star` reuses `@faultbox/recipes/postgres.star` (backfill, shipped in this release — see Implementation Plan).

Single source of truth for error strings. A driver error-type check (`errors.Is(err, pq.ErrDeadlockDetected)`) works identically whether the fault came from the recipe (real server) or the mock.

## Implementation Plan

### Phase 0 — Prereqs (this epic)

0. Create `epic/v0.9.0` branch, v0.9.0 milestone.
1. Write `@faultbox/recipes/postgres.star` (recipe backfill; OQ6 prereq). Ships the canonical error catalog the mock will reuse.

### Phase 1 — MySQL mock (customer P0)

2. MySQL handshake: `HandshakeV10` packet, auth OK (mock ignores credentials).
3. Canned metadata responses: `SELECT @@version`, `SELECT @@version_comment`, `SELECT @@sql_mode`, `SHOW VARIABLES`, `USE <db>`.
4. SQL canonicalizer (D1) + query router.
5. Result-set encoder (D2) for the v1 type set (D5).
6. Prepared-statement handling (D3): `COM_STMT_PREPARE` / `EXECUTE` / `CLOSE`.
7. Transaction no-ops (D4).
8. Error-frame emitter with codes from the recipe catalog (D6).
9. `@faultbox/mocks/mysql.star` — exposes `mysql.server()`, `mysql.rows_by_id()`, `mysql.rows()`, `mysql.ok()`, `mysql.error()`.
10. Integration test: `database/sql` + `go-sql-driver/mysql` round-trip.

### Phase 2 — Postgres mock

11. Postgres startup: `StartupMessage` → `AuthenticationOk` → `ParameterStatus` (server_version, client_encoding) → `ReadyForQuery`.
12. Canned metadata responses: `SELECT version()`, `pg_catalog.pg_type` probes pgx issues at connect.
13. Reuse the shared canonicalizer from Phase 1.
14. Result-set encoder: `RowDescription` + `DataRow` frames.
15. Extended query protocol (D3): Parse / Bind / Describe / Execute / Sync.
16. Transaction no-ops.
17. Error-frame emitter with codes from `@faultbox/recipes/postgres.star`.
18. `@faultbox/mocks/postgres.star` — `postgres.server()`, `postgres.rows_by_id()`, `postgres.rows()`, `postgres.error()`.
19. Integration test: `database/sql` + `pgx` round-trip.

### Phase 3 — Docs & release

20. Docs update: `docs/mock-services.md` section for SQL, tutorial examples.
21. Faultbox-site sync.
22. Tag `release-0.9.0`, GH release.

## Impact

- **Breaking changes**: None. New `@faultbox/mocks/mysql.star` and `@faultbox/mocks/postgres.star` modules. New `@faultbox/recipes/postgres.star` module.
- **Migration**: Opt-in. Existing specs using real Postgres/MySQL containers continue to work.
- **Performance**: Mock SQL servers target low RPS (<1000 QPS). Not a production-shaped benchmark.

## Alternatives Considered

- **Parse full SQL.** Rejected for v1 — a full SQL parser is a multi-quarter project and brings its own bug surface.
- **Run real Postgres/MySQL in embedded mode.** Rejected — reintroduces the container problem RFC-017 aimed to remove.
- **Lua/Starlark callbacks per query.** Deferred — dynamic handlers (RFC-017) could apply here too, but raise Starlark-on-hot-path cost for high-QPS mocks. Revisit post-v0.9.0 if users ask for it.
- **Lookup-table row responses (no wire frames).** Rejected in D2 — defeats the purpose of exercising driver decode paths.

## Dependencies

- RFC-017 (mock_service core) — ✅ shipped in v0.8.0.
- `@faultbox/recipes/mysql.star` (error catalog) — ✅ shipped in v0.8.1.
- `@faultbox/recipes/postgres.star` (error catalog) — ships in this epic (Phase 0).
