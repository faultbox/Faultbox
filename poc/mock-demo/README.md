# mock-demo: end-to-end Faultbox mock service example

Demonstrates every mock service primitive shipped in **v0.8.0** running
together in a single spec — auth (HTTP) + feature flags (gRPC) + cache
(Redis) + event bus (Kafka) + user database (MongoDB).

No containers, no Dockerfiles, no `python -m http.server` — just
Starlark.

## Run

```bash
faultbox test poc/mock-demo/faultbox.star
```

## What it shows

| Mock | Construct | Interesting bit |
|---|---|---|
| `auth_stub` | `mock_service` + `routes={...}` | OIDC handshake (issuer + JWKS), `dynamic()` JWT minting, `status_only(204)` health |
| `flags_stub` | `mock_service` + `routes={...}` | gRPC reflection, `grpc_response()` + `grpc_error("UNAVAILABLE", ...)` |
| `cache` | `redis.server` from `@faultbox/mocks/redis.star` | Seeded state, real RESP client reads it |
| `bus` | `kafka.broker` from `@faultbox/mocks/kafka.star` | kfake-backed broker, real `kafka-go` produces messages |
| `users_db` | `mongo.server` from `@faultbox/mocks/mongodb.star` | BSON OP_MSG handshake + `find` returns seeded docs |

## Step through the spec

The spec is heavily annotated — read [faultbox.star](faultbox.star)
top-to-bottom for the recommended walk-through. Key teaching points:

1. **Generic vs stdlib mocks.** HTTP / HTTP/2 / TCP / UDP / gRPC use
   `mock_service()` with `routes={}`. Kafka / Redis / MongoDB use
   `@faultbox/mocks/<name>.star` constructors that translate
   protocol-specific kwargs (`topics=`, `state=`, `collections=`)
   into the same generic primitive.
2. **Dynamic responses.** The auth stub's `/token` endpoint uses
   `dynamic(lambda req: ...)` to mint a JWT whose subject reflects
   the `?user=` query parameter — covers JWT auth flows where the
   stub needs to produce different tokens per test.
3. **Real clients, real wire formats.** Tests use the same step
   methods (`get`, `post`, `find`, `publish`) as if these were real
   services, exercising the protocol stack end-to-end.

## What it doesn't show

The spec keeps the topology focused on the mocks themselves. In a
real test you would add:

- A `service("api", image=..., depends_on=[auth_stub, cache, bus, users_db])`
  to test your application against the mock stack.
- `fault_assumption()` rules to fault the mocks (mocks accept the same
  `fault()` rules as real services — that flow is covered in the
  recipes docs and tutorial).
- Assertions over `events()` to verify call ordering, request
  payloads, and SUT behavior under failure.
