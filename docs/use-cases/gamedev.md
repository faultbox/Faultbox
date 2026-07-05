# Gamedev - multiplayer backends, anti-cheat, MMORPG live ops

A multiplayer game is one of the most asymmetric distributed systems in the
industry. The hot path runs at 30-60Hz over custom UDP. Around it sits a
backend zoo - matchmaker, auth, leaderboards, persistent world, anti-cheat,
player store, social, telemetry - every one of which can ruin a session if it
misbehaves. Engineers running these backends need to know what happens when
the matchmaker gets slow, when the anti-cheat backend disappears mid-match,
when Postgres drops a write at the moment a raid loot drops.

Faultbox is for the engineers who own that backend zoo. The hot-path engine
traffic is a separate problem; everything that surrounds it is microservices
and message queues, which is exactly what Faultbox already faults.

## What goes wrong (and why integration tests don't catch it)

- The **anti-cheat backend** is a third-party HTTPS service that's mostly up.
  When it isn't, your game's behaviour is one of two: fail-open (let the
  player play, log the gap) or fail-closed (kick the player, lose them to
  the lobby). Integration tests don't exercise this path because the
  vendor's sandbox doesn't expose "be down" as a mode.
- **Cross-shard trades and instance migrations** depend on multiple stateful
  services being available simultaneously. Network partitions inside the
  cluster turn a 200ms RPC into a 30s hang. The retry/circuit-breaker code
  was written but rarely fires, so when it does, it doesn't.
- **Persistent-world writes** during high-value events (raid loot, PvP
  rating updates, IAP completion) need to survive `EIO`, `ENOSPC`, and
  partial writes. No integration test induces those.
- **Auth / JWKS rotation** breaks every session that fetches a JWT during
  the rotation window. The bug looks like "every player got logged out at
  03:14 last Tuesday."
- **Matchmaker degradation** under load isn't itself a Faultbox question
  (load testers handle that), but matchmaker behaviour when its *upstream*
  player-rating service is slow is - and that's where the real outages
  start.

## How Faultbox helps

| Need | Primitive | Read |
|---|---|---|
| Real cluster pods (k8s) without distributing dep images | `service(remote=...)` | [Spec language - Remote Services](/docs/reference/spec-language#remote-services) |
| Anti-cheat / store / billing HTTPS upstream faults | `tls=tls_cert(...)` + `error()` on the interface | [TLS upstreams](/docs/guides/connectivity#tls-upstreams-rfc-038) |
| Network partitions across game-server replicas | `partition()` | [Tutorial 16 - Partitions](/docs/tutorial/04-safety/16-partitions) |
| Persistent-world write failures (`EIO`, `ENOSPC`, partial) | syscall `write=deny()` / `pwrite=delay()` | [Tutorial 03 - Fault injection](/docs/tutorial/02-syscall-level/03-fault-injection) |
| Auth / JWKS mocks for offline tests | `@faultbox/mocks/jwt.star` | [Tutorial 21 - JWT mocks](/docs/tutorial/05-advanced/21-jwt-mocks) |
| Reproduce production incidents from any machine | `.fb` bundles + `faultbox replay` | [Tutorial 20 - Bundles](/docs/tutorial/05-advanced/20-bundles) |
| Verify retry / circuit-breaker / timeout code fires | `assert_eventually()` / `assert_never()` | [Tutorial 14 - Invariants](/docs/tutorial/04-safety/14-invariants) |

## A real scenario - anti-cheat fail-open under unreachable telemetry

The single most security-critical decision in a multiplayer-game backend:
what happens when the anti-cheat vendor's telemetry endpoint is unreachable?
A 60-second outage shouldn't ban every player; a permanent outage shouldn't
let a cheat engine play unchallenged. Both extremes are real bugs.

The spec below stands up the SUT (`game-api`) against a real Postgres + Redis
in containers, points at a TLS-required anti-cheat backend (using
[`remote=`](/docs/reference/spec-language#remote-services) so the test runs
against a staging endpoint, not a mock), and asserts the documented
fail-open semantic: when the backend is unreachable, the player connects
with `fair_play_verified=false` and the audit event lands in Kafka.

```python
load("@faultbox/discovery/k8s.star", "k8s")
load("@faultbox/recipes/postgres.star", "postgres")

# Anti-cheat backend lives in the customer's k8s dev cluster (or via
# Telepresence connect from the dev laptop). RFC-036 lets us point at it
# without distributing the image.
anti_cheat = service("anti-cheat",
    interface("public", "http", 443,
        tls = tls_cert(insecure = True),  # accept self-signed cluster cert
    ),
    remote      = k8s.service("anti-cheat", namespace = "staging"),
    healthcheck = tcp(k8s.endpoint("anti-cheat", 443, namespace = "staging")),
)

db = service("db",
    interface("main", "postgres", 5432),
    image       = "postgres:16-alpine",
    env         = {"POSTGRES_PASSWORD": "test", "POSTGRES_DB": "game"},
    healthcheck = tcp("localhost:5432"),
)

cache = service("cache",
    interface("main", "redis", 6379),
    image       = "redis:7-alpine",
    healthcheck = tcp("localhost:6379"),
)

api = service("game-api",
    interface("public", "http", 8080),
    image       = "game-api:dev",
    depends_on  = [db, cache, anti_cheat],
    env         = {
        "ANTI_CHEAT_URL": "https://%s/v1/verify" % anti_cheat.public.addr,
        "DB_URL":         "postgres://postgres:test@%s/game" % db.main.addr,
        "REDIS_URL":      "redis://%s" % cache.main.addr,
    },
    healthcheck = http("localhost:8080/healthz"),
)

# Audit event source: the game-api stamps every join attempt with the
# anti-cheat decision. We tail Kafka to verify those events land.
events_topic = topic("audit.player_joined", brokers = ["kafka:9092"])

# --- Failure modes worth testing ---

ac_unreachable = fault_assumption("anti_cheat_unreachable",
    target = anti_cheat.public,
    rules  = [error(path = "/v1/verify", status = 503)],
)

ac_slow = fault_assumption("anti_cheat_slow",
    target = anti_cheat.public,
    rules  = [slow(path = "/v1/verify", delay = "5s")],
)

# --- Scenario ---

def player_joins_match():
    resp = api.public.post(path = "/v1/match/join", body = {
        "player_id": "test-player-001",
        "match_id":  "match-42",
    })
    return resp

# --- Oracle ---
# The fail-open contract: under anti-cheat-unreachable, the join still
# succeeds, the response carries fair_play_verified=false, and the audit
# event lands in Kafka so the security team can see it later.

fault_matrix(
    scenarios = [scenario("join", run = player_joins_match)],
    faults    = [ac_unreachable, ac_slow],
    expect    = lambda result: (
        assert_eq(result.status, 200, "join must succeed when anti-cheat is down"),
        assert_eq(result.json()["fair_play_verified"], False,
                  "fair_play flag must reflect the gap"),
        assert_eventually(
            events()
                .where(source = "audit.player_joined")
                .where_field("anti_cheat_status", "unreachable")
                .count() >= 1,
            timeout = "3s",
        ),
    ),
)
```

Run it:

```sh
$ telepresence connect                # one-time, gives the laptop cluster DNS
$ faultbox test game-api.star

scenario=join × fault=anti_cheat_unreachable     PASS  812ms
scenario=join × fault=anti_cheat_slow            PASS  6.1s
scenario=join × fault=(none)                     PASS  142ms

3/3 passed
Bundle: run-2026-05-03T09-14-22-3326879141550591551.fb
```

The bundle replays bit-equivalent on any other engineer's machine, so when
QA finds an oracle mismatch in CI, you get the exact byte-level failure
artifact instead of a stack trace screenshot.

## More scenarios this shape covers

- **Cross-shard trade rollback** - `partition()` between two `game-server`
  containers, inject `error()` on the trade-completion gRPC mid-flight,
  assert both sides agree on the rollback state.
- **Save-game write failures** - `write=deny("ENOSPC")` on the persistence
  service during a raid completion, verify the loot ends up in the correct
  state (granted-and-recovered, not lost-and-charged).
- **JWKS rotation** - swap the JWT mock's signing key during a scenario,
  verify the SUT picks up the new public key on the next request and
  doesn't return cached 403s.
- **Matchmaker degraded upstream** - slow the player-rating service by 2s,
  verify matchmaker either times out gracefully or falls back to an open
  rating bracket. (Pure HTTP+DB, no gaming-specific primitives.)
- **Anti-cheat full outage drill** - combine `anti_cheat_unreachable` with
  `db.write=deny()` to test the worst-case path: anti-cheat down AND the
  audit event can't be persisted. What does the game do?

## Read next

- [Tutorial - Containers and real services](/docs/tutorial/05-advanced/09-containers) - the Docker-mode mechanics behind `image=`
- [Tutorial - Mock services](/docs/tutorial/05-advanced/17-mock-services) - for the auth-stub / leaderboard-stub patterns
- [Tutorial - End-to-end Go microservice](/docs/tutorial/05-advanced/22-go-microservice-end-to-end) - the closest existing tutorial to a full game-backend shape
- [Spec language reference](/docs/reference/spec-language) - every primitive in one page
