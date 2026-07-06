# Seeding Data & Initial State

Real services do not boot into a vacuum. Before the first request they
need a schema, reference data, accounts, topics - state that normally
lives in init migrations, entrypoint scripts, or a migrations tool.
This page maps each place that state lives today onto the Faultbox
mechanism that provides it, and states the guarantees seeding gets
(it runs fault-free and stays out of your trace).

Skipping this page is the most common way a first real-service spec
fails: the service starts, the healthcheck passes, and every test dies
on an empty database.

## The lifecycle

```
suite start:   create container
                 └─ image entrypoint runs (incl. /docker-entrypoint-initdb.d)
               healthcheck passes
               seed() runs        ← once, no faults active
test 1:        run test (faults active)
               faults cleared
               reset() runs       ← before each subsequent test (reuse=True)
test 2:        run test
               ...
```

Three hooks, three jobs:

| Hook | Runs | Use for |
|------|------|---------|
| Image entrypoint (`/docker-entrypoint-initdb.d`, baked-in scripts) | On first container boot, before the healthcheck | Schema, migrations - anything the image can do by itself |
| `seed=` callback | Once, after the first healthcheck | Application-level fixtures: reference rows, accounts, topics, cache keys |
| `reset=` callback | Before every test after the first, when `reuse=True` | Cheap cleanup back to the seeded state (`TRUNCATE`, `DEL`) |

If `reset=` is not defined, Faultbox falls back to calling `seed()`
again. If `reuse=True` and neither is defined, Faultbox warns:
state may leak between tests.

## Where your init state lives today → what to use

| Today | In Faultbox |
|-------|-------------|
| Plain `.sql` init scripts | Mount them into the image's init directory: `volumes={"./init.sql": "/docker-entrypoint-initdb.d/init.sql"}` - the database image runs them on first boot, before your healthcheck can pass |
| The service runs its own migrations on startup | Nothing to do. The healthcheck gates the suite until the service is ready; make the healthcheck strict enough to imply "migrated" |
| A migrations tool (Flyway, goose, Alembic, Liquibase) | Bake the migration step into the service image's entrypoint, or apply migrations when building the image. Faultbox has no one-shot job container and no exec-into-container - see the honest-limitations note below |
| Test fixtures, reference rows | `seed=` with interface calls: `postgres.main.exec(sql=...)` |
| Per-test cleanup | `reset=` with `TRUNCATE ... RESTART IDENTITY CASCADE` or key deletes |
| Mock dependencies | Pre-seed state declaratively: `redis.server(state={...})`, `mongo.server(collections={...})`, `mock_service(..., openapi=...)` |
| Large datasets | A pre-baked image or a mounted volume; for medium SQL blobs, `seed = lambda: db.main.exec(sql=load_file("./fixtures.sql"))` |

## A complete example

```python
def seed_db():
    # Runs once after the first healthcheck. No faults are active.
    postgres.main.exec(sql = load_file("./schema.sql"))
    postgres.main.exec(sql = "INSERT INTO plans (name) VALUES ('free'), ('pro')")

def reset_db():
    # Runs before each subsequent test. Keep it cheap and idempotent.
    postgres.main.exec(sql = "TRUNCATE orders RESTART IDENTITY CASCADE")

postgres = service(
    name    = "postgres",
    image   = "postgres:16-alpine",
    volumes = {"./init.sql": "/docker-entrypoint-initdb.d/init.sql"},
    reuse   = True,
    seed    = seed_db,
    reset   = reset_db,
    ...
)
```

Seeding is not SQL-only - any interface method works:

```python
def seed_all():
    redis.main.set(key = "config:max_retries", value = "3")
    kafka.main.publish(topic = "orders", key = "warmup", data = "{}")
    api.public.post(path = "/admin/accounts", body = '{"name": "test"}')
```

## Guarantees

- **Seeding runs fault-free.** `seed()` executes before any fault rule
  is installed; `reset()` executes after the previous test's faults are
  cleared. Your fixtures can never be corrupted by an injected fault.
- **Seeding stays out of the trace.** The event log gets single
  `service_seed` / `service_reset` markers, not the seeding traffic -
  temporal properties and invariants never match against setup noise.
- **Failure is loud.** A failed `seed()` aborts the suite; a failed
  `reset()` fails the test. There is no silent fall-through into tests
  running against half-initialized state.

## Pitfalls

- **Keep `reset()` cheap and idempotent.** `TRUNCATE`, not
  `DROP`/re-`CREATE` - reset runs before every test and its cost
  multiplies across the fault matrix.
- **Keep seed data deterministic.** Random names or timestamps in
  fixtures break trace comparison across replays. Fixed values, always.
- **Do not reset shared infrastructure.** With
  `service(remote=...)` pointing at a shared environment, a
  `TRUNCATE` in `reset()` hits the real thing. Seed/reset are for
  services the spec owns.
- **Healthcheck ≠ migrated.** If the service's own migrations run in
  the background after it starts listening, a port-only healthcheck
  passes too early and `seed()` races the migrations. Make the
  healthcheck query something the migrations create.

## Honest limitations

There is currently no way to run a one-shot init container (a job that
runs migrations and exits) and no `exec`-into-container primitive. If
your migrations can only run via a tool against a live database, bake
that step into the image entrypoint or apply it at image build time.
If this blocks you, say so - it is exactly the kind of demand that
promotes a roadmap item.

## See also

- [Container lifecycle](../spec-language.md) - §"Container Lifecycle": full seed/reset semantics
- Tutorial: [Containers](../tutorial/05-advanced/09-containers.md) - reuse + seed/reset in context
- Tutorial: [Go microservice end-to-end](../tutorial/05-advanced/22-go-microservice-end-to-end.md) - `seed=` with a real schema file
- [Mock services](../mock-services.md) - declarative state for simulated dependencies
