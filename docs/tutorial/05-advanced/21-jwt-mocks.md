# Chapter 21: JWT/JWKS Mocks — Stub Auth-Protected Services

**Duration:** 12 minutes
**Prerequisites:** [Chapter 17 (Mock Services)](17-mock-services.md), familiarity with JWT bearer tokens

## Goals & purpose

Every Go microservice that lives behind an OIDC issuer has the same
problem when it lands on a fault-injection harness: **the SUT verifies
JWT signatures against a JWKS endpoint that doesn't exist in the test
environment.** Customers reaching for Faultbox ended up writing their
own `jwtgen` Go tool, building a separate HTTP service, threading
private keys around — same shape every time.

`@faultbox/mocks/jwt.star` (shipped in v0.9.9) collapses all of that
to one constructor. It auto-generates an Ed25519 keypair at spec-load
time, stands up an HTTP service publishing the JWKS doc, and gives
your test driver a `.sign(claims=…)` method to mint tokens the SUT
will accept.

This chapter teaches you to:

- **Stand up a JWKS-publishing mock** with one Starlark call.
- **Mint tokens** in the test driver and pass them as
  `Authorization: Bearer …`.
- **Compose with other Faultbox primitives** — fault the JWKS
  endpoint to test client-side caching, or run JWT-protected requests
  through the data-path proxy from RFC-024.

## 1 · Stand up the issuer

```python
load("@faultbox/mocks/jwt.star", "jwt")

auth = jwt.server(
    name      = "auth",
    interface = interface("main", "http", 8090),
    issuer    = "https://faultbox-auth.invalid",
)

api = service("api", "/tmp/your-app",
    interface("public", "http", 8080),
    env = {
        "OIDC_ISSUER":   auth.service.main.addr,
        "OIDC_JWKS_URL": auth.service.main.addr + "/.well-known/jwks.json",
    },
    depends_on = [auth.service],
)
```

Two things are different from a plain `mock_service`:

- `auth` is a **struct**, not a `ServiceDef`. The actual service is
  at `auth.service` — that's what you pass to `depends_on=` and use
  for env wiring.
- The struct also carries `auth.sign(claims=…)` and `auth.jwks` so
  your tests can mint tokens and (if needed) inspect the published
  key.

## 2 · Routes the mock publishes

`jwt.server()` exposes three HTTP routes the SUT's OIDC client looks
for:

| Path | Purpose |
|---|---|
| `GET /.well-known/openid-configuration` | OIDC discovery doc with `issuer` + `jwks_uri` |
| `GET /.well-known/jwks.json` | JWKS document with the published Ed25519 public key |
| `GET /jwks` | Alias for clients that hit a non-standard path |

The discovery doc points `jwks_uri` at the issuer string you passed —
**make sure that URL resolves to the mock from inside the SUT**. In
binary mode, `auth.service.main.addr` is `localhost:8090`. In
container mode, the proxy data-path (RFC-024) points the SUT at
`host.docker.internal:<random>`.

## 3 · Mint a token

```python
def test_authorised_request():
    token = auth.sign(claims = {
        "sub":   "user-42",
        "email": "alice@example.com",
        "scope": "read:orders",
        "iat":   1700000000,
        "exp":   1700003600,
    })

    result = step(api.public, "get",
                  path    = "/orders",
                  headers = {"Authorization": "Bearer " + token})

    assert_true(result.status_code == 200)
```

Notes on claims:

- **All fields pass through verbatim.** `iat`, `exp`, `aud`, `iss` are
  yours to set — the mock doesn't auto-populate timestamps so spec
  authors stay in control of token expiry semantics.
- **Common claim names:** `sub` (subject), `iat`/`exp` (Unix
  timestamps), `aud` (audience), `iss` (issuer URL). Check what your
  app's middleware actually validates — the inDrive PoC lost hours
  to a `user_id` vs `uid` claim-name mismatch (FB §2.1 #2).

## 4 · Test rejection paths

What about tokens the SUT should reject? Two cheap variants:

```python
# Wrong audience: SUT's middleware demands aud="api.example.com",
# we issue with aud="other".
def test_wrong_audience_rejected():
    bad = auth.sign(claims = {"sub": "u1", "aud": "other"})
    result = step(api.public, "get",
                  path    = "/orders",
                  headers = {"Authorization": "Bearer " + bad})
    assert_true(result.status_code == 401)

# Expired token: exp in the past.
def test_expired_token_rejected():
    expired = auth.sign(claims = {"sub": "u1", "exp": 1000000000})
    result = step(api.public, "get",
                  path    = "/orders",
                  headers = {"Authorization": "Bearer " + expired})
    assert_true(result.status_code == 401)
```

For the "wrong signature" case, mint a token from a **different**
issuer's keypair (instantiate a second `jwt.server(...)` and call
`.sign()` on it). The SUT will fetch the published-issuer's JWKS,
fail to verify the signature, and 401.

## 5 · Compose with faults

Now the interesting part — combine JWT mocks with the rest of the
Faultbox toolkit.

### Fault the JWKS endpoint

Test how your SUT handles a JWKS outage. Most production OIDC
clients cache the JWKS for 5–10 minutes; a fault that survives that
window forces the cache-refresh path.

```python
def test_jwks_outage_uses_cache():
    # First request primes the JWKS cache in the SUT.
    token = auth.sign(claims = {"sub": "u1"})
    ok = step(api.public, "get",
              path    = "/orders",
              headers = {"Authorization": "Bearer " + token})
    assert_true(ok.status_code == 200)

    # Now cut the JWKS endpoint and request again. SUT should serve
    # from cache as long as we're inside its TTL.
    fault(auth.service.main, error(status_code = 503),
          run = lambda: assert_true(
              step(api.public, "get",
                   path    = "/orders",
                   headers = {"Authorization": "Bearer " + token},
              ).status_code == 200))
```

### Slow JWKS — exercise client timeouts

```python
def test_slow_jwks_breaks_login():
    fault(auth.service.main, response(delay_ms = 8000),
          run = lambda: assert_true(
              # Your client likely has a 5s JWKS-fetch timeout. The
              # POST should fail fast rather than hang for 8s.
              step(api.public, "get",
                   path    = "/orders",
                   headers = {"Authorization": "Bearer " + auth.sign(claims = {"sub": "u1"})},
              ).status_code >= 500))
```

## 6 · Algorithm scope and limits

`jwt.server()` is **EdDSA only** (Ed25519). Modern OIDC issuers
(Auth0, Okta, Keycloak with EdDSA enabled) and well-maintained
in-house auth services accept this. Older RSA/HS256-only stacks
need a different mock — file an issue if you hit one.

Other things v0.9.9 deliberately doesn't ship:

- **Key rotation.** One key per `jwt.server()`, no `kid` rolling.
- **Multi-issuer JWKS.** Stand up separate `jwt.server(...)` mocks
  for separate issuers.
- **Token introspection endpoints** (`/introspect`). If your SUT
  uses opaque tokens with introspection, `jwt.server` doesn't help
  — use `mock_service` directly with custom routes.

## Takeaways

- One `jwt.server(name, interface, issuer)` call replaces a Go
  binary, an HTTP service, and a key-management ritual.
- `auth.sign(claims = {…})` mints tokens the SUT verifies via the
  published JWKS.
- The mock composes with `fault()`, `depends_on=`, and the data-path
  proxy — fault the JWKS endpoint to exercise client-side caching;
  delay it to exercise client-side timeouts.

Next: [Chapter 22 — Reading Faultbox Reports →](22-reports.md) (v0.11.0)
