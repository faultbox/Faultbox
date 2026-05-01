# RFC-038: TLS-aware proxy

> **Status: Draft (problem statement + three options — design not yet committed.)**

## Summary

Today every Faultbox protocol proxy speaks plain TCP. Customer dependencies that require mTLS (gRPC over TLS, HTTPS upstreams, mutual-auth Postgres / MySQL / Redis) cannot be faulted at the protocol layer — there is no path to put a Faultbox proxy between an mTLS client and an mTLS server without breaking the connection.

This RFC frames the gap and lays out three implementation options. It does **not** commit to an option; the choice depends on which fault-injection capabilities we preserve vs. how much per-protocol cert plumbing we accept.

In-tree document: [`docs/rfcs/0038-tls-aware-proxy.md`](https://github.com/faultbox/Faultbox/blob/main/docs/rfcs/0038-tls-aware-proxy.md).

Companion to RFC-036 (#91, `remote=`) — `remote=` lets the SUT dial real upstreams in a customer's dev cluster, but those upstreams almost always speak TLS in production-shaped environments. Without TLS support the proxy is bypassed for the protocols that matter most.

Customer-driven (inDrive Freight, 2026-04-30 customer-gap list, item #3): *"TLS-aware proxy. Some prod upstreams dial mTLS. Today's proxies are plain TCP; we'd need `interface(..., protocol='grpc-tls', cert=…)` or fail-open passthrough for ALPN. The TLS gap is what blocks expanding from truck-api to the rest of the Freight stack."*

## Motivation

### What problem does this solve?

Real-world distributed systems use TLS pervasively. The Freight stack — and any production-shaped environment — looks like:

```
truck-api  ──HTTPS──▶  geo-config (gRPC over TLS, mutual auth)
           ──HTTPS──▶  pricing    (HTTPS REST, server cert only)
           ──TLS────▶  postgres   (TLS-required, server cert)
           ──TLS────▶  redis      (TLS via stunnel sidecar)
```

With v0.12.x's plain-TCP proxies, every one of those connections bypasses Faultbox the moment the client negotiates TLS. The proxy sees encrypted bytes, can't parse them, can't match SQL/HTTP/gRPC/RESP rules against them, and effectively becomes a tunnel that loses every fault-injection capability the protocol-aware plugins provide.

Concretely, the customer's three current limitations:

1. **Cannot fault gRPC-TLS calls.** `truck-api → geo-config` over mTLS. The customer wants to inject `grpc.error(method='/geo.Service/GetCity', code=UNAVAILABLE)` but the proxy can't parse the gRPC frame because TLS encrypts it.
2. **Cannot fault HTTPS responses.** `truck-api → pricing` over HTTPS. `http.error(path='/v1/quote', status=503)` is unreachable.
3. **Cannot fault TLS-Postgres/Redis.** Customer's prod databases enforce TLS. Proxy parses the first byte, sees the SSL request marker (`\x00\x00\x00\x08\x04\xd2\x16\x2f` for Postgres) and either denies TLS (forcing client to fall back to plaintext, which the customer's drivers don't do) or passes it through opaque (no fault matching).

### Why is this important now?

- **Blocks RFC-036 adoption at scale.** `remote=` ships the primitive for pointing at a customer's dev cluster; the moment that cluster is anything more sophisticated than localhost-Docker, TLS is the default. Without TLS support the customer can use `remote=` only against the few unencrypted services in their stack.
- **Blocks Freight-shape customers.** Per the 2026-04-30 customer-gap list, this is the single largest blocker between truck-api (the PoC) and the rest of the Freight stack.
- **Compounds with other observability gaps.** RFC-034 (proxy traffic observability) just shipped; on a TLS upstream those events fire but the proxy can only see encrypted bytes — `bytes_c2s` / `bytes_s2c` are still useful for stall detection, but `proxy_handshake_complete` distinguishes only TCP-handshake-complete, not TLS-handshake-complete. That distinction is load-bearing for diagnosing TLS failures.

### What happens if we don't do this?

- Customers stay limited to plaintext upstreams. The proxy story ("Faultbox lets you inject any fault into any protocol-level interaction") becomes "any fault into any *plaintext* protocol-level interaction" — a footnote that erodes the positioning.
- Workaround: customers stand up unencrypted shadow copies of their dependencies. Operationally heavy, drifts from prod.
- Alternative workaround: customers use `service(image=..., env={"NO_TLS": "1"})` to disable TLS on each dep. Couples Faultbox tests to internal feature flags; degrades on the next dependency that doesn't expose such a flag.

## Current state

Search for any TLS support in the proxy layer surfaces nothing:

- `internal/proxy/*.go` (15 files): zero `crypto/tls` imports. All listeners are `net.Listen("tcp", ...)`; all upstream dials are `net.DialTimeout("tcp", ...)`.
- The only `crypto/tls` usage in tree is in `internal/protocol/mock.go:39` (mock services serving TLS) and `internal/protocol/mock_test.go` — fully separate from the proxy data path.
- `internal/proxy/http2.go:56` has a comment acknowledging the gap: *"For TLS upstream, the transport would need its TLS config wired from the container trust store; out of scope for v1."*

So we're starting from zero on the proxy side. This is a deliberate choice from RFC-024 (proxy datapath) — every shipped protocol plugin can be implemented and tested without TLS, and the customer's PoC ran without it. The gap is now visible because customers want to point at real upstreams.

## Three implementation options

### Option A — Per-protocol TLS termination

Each parse-aware proxy plugin gets cert/key configuration. The plugin wraps its listener in `tls.NewListener(ln, cfg)` so clients negotiate TLS with the proxy directly. The plugin then dials the upstream with `tls.Dial` (using a separate cert config — typically the same cert chain re-established to upstream) and forwards plaintext between the two TLS streams while parsing.

**Spec surface:**

```python
db = service("db", remote="postgres-prod.svc.cluster.local",
    interface("main", "postgres", 5432,
        tls=tls_cert(cert="/secrets/db-client.crt",
                     key="/secrets/db-client.key",
                     ca="/secrets/db-ca.crt"),
    ),
)
```

**Pros:**
- Faultbox keeps **every fault-injection capability** the parse-aware plugins have — SQL matching, HTTP path matching, gRPC method matching, Redis key globs.
- Per-protocol behavior already differs (SQL canonicalization, RESP3 framing, MySQL handshake quirks); adding TLS fits the existing per-plugin pattern.
- Clean separation: TCP-only customers get zero TLS overhead.

**Cons:**
- **8+ plugins to retrofit** (postgres, mysql, redis, http, http2, grpc, mongodb, kafka, plus tcp/udp passthrough variants if we want them).
- Cert / key / CA / SNI plumbing is non-trivial — every plugin needs to handle cert reload, CA bundle resolution, mTLS verify hooks, ALPN selection (HTTP/1.1 vs HTTP/2), and TLS error handling.
- Fault-injection rules now span two layers (TCP error + TLS error), increasing the surface plugins must reason about.
- Per-protocol matrix grows: each new protocol must implement TLS from scratch.

**Estimated effort:** ~1 week per plugin × 8 plugins = ~2 months sequential, or ~2-3 weeks if parallelized. Plus shared TLS-config infrastructure.

### Option B — ALPN-passthrough at the transport layer ⭐ *(recommended)*

The proxy wraps its **listener** in `tls.NewListener` (so the client negotiates TLS with the proxy), and the proxy **dials upstream with `tls.Dial`** (so it negotiates TLS with the real server). Between the two, the proxy sees plaintext and runs the existing protocol-aware logic unchanged. This is the standard "TLS-terminating reverse proxy" pattern.

**Spec surface (single source of truth):**

```python
db = service("db", remote="postgres-prod.svc.cluster.local",
    interface("main", "postgres", 5432,
        tls=tls_cert(cert="/secrets/db.crt", key="/secrets/db.key"),
    ),
)
```

The same `tls_cert(...)` value is consumed by every plugin via a shared transport helper. Only the listener-creation site (the v0.12.17 `proxy.Listen()` helper from RFC-035) needs TLS awareness; per-plugin `handleConn` keeps reading plaintext from the wrapped client and writing plaintext to the wrapped upstream.

**Pros:**
- **Single implementation path** — extend `proxy.Listen()` and add a `proxy.Dial()` helper, both in `internal/proxy/listen.go`. Per-plugin code unchanged except for the dial call (`net.Dial("tcp", ...)` → `proxy.Dial(target, tls)`).
- **All fault-injection capabilities preserved** — SQL matching, gRPC method matching, etc. all work because the plugin still sees plaintext.
- **Customer's exact ask** — `interface(..., protocol="grpc-tls", cert=…)` translates directly to `interface(..., "grpc", tls=...)`.
- Naturally supports both server-cert-only and mTLS (cert/key/CA combinations).
- Future-friendly — when we instrument a new plugin, it inherits TLS support from the helper without re-doing the work.

**Cons:**
- **The proxy itself needs a cert.** For most dev/test deployments this is a generated self-signed cert; for stricter environments we need a path to provide a trusted cert (e.g. cert-manager-issued for in-cluster proxies). Not insurmountable but requires thought.
- **SNI awareness** if a single proxy fronts multiple TLS upstreams. Initially each interface gets its own listener (current model), so SNI is per-interface. Multi-tenant later if needed.
- **TLS handshake parsing** for the proxy's own connection lifecycle event (`proxy_handshake_complete` from RFC-034) — the existing event fires after the protocol's auth phase; with TLS we may want a separate `proxy_tls_handshake_complete` to distinguish the two.
- **ALPN negotiation** — the proxy must agree on the same protocol with both sides (`h2` for gRPC/HTTP2, `http/1.1` for HTTPS). Slightly tricky but standard library handles it.

**Estimated effort:** ~1 week for the helper + cert plumbing, ~1-2 days per plugin to verify (mostly making the upstream-dial call go through `proxy.Dial`), plus a small `tls_cert()` Starlark builtin. **Total ~2-3 weeks for a complete TLS rollout.**

### Option C — TLS-blind tunnel (TCP-only)

The proxy never decrypts. It accepts the client's TLS bytes and forwards them verbatim to the upstream; responses likewise. The client-side TLS handshake completes against the real upstream (via the proxy as a transparent tunnel).

**Pros:**
- Simplest possible implementation — `tcp.go` with no TLS additions; `io.Copy` already works.
- Zero cert plumbing on Faultbox's side.

**Cons:**
- **No fault injection inside TLS streams.** Only connection-level faults work: `drop`, `delay`, byte-level corruption (if we cared to add it). All protocol-level rules — `error(query=...)`, `http.error(path=...)`, `grpc.error(method=...)` — are inert because the proxy can't see what's inside.
- Severely limits Faultbox's value proposition for TLS upstreams. Customer has explicitly said *"the TLS gap blocks expanding from truck-api to the rest of the Freight stack"* — Option C doesn't unblock them; it just lets them point a tunnel at the upstream.
- Doesn't compose with RFC-034 well — `proxy_handshake_complete` would never fire (no protocol parsing happened), `bytes_c2s` / `bytes_s2c` count encrypted bytes (less useful for stall diagnosis).

**Estimated effort:** ~1 day. **This is the "do nothing meaningful" option.** Useful only as a fallback for protocols we explicitly don't want to terminate.

## Comparison matrix

| Capability | Option A (per-protocol) | Option B (ALPN-passthrough) ⭐ | Option C (TCP-only) |
|---|---|---|---|
| Fault on SQL query content | ✅ | ✅ | ❌ |
| Fault on HTTP path | ✅ | ✅ | ❌ |
| Fault on gRPC method | ✅ | ✅ | ❌ |
| Fault on Redis key/command | ✅ | ✅ | ❌ |
| Inject TLS-level errors (cert expired, handshake fail) | ✅ | ✅ | ❌ |
| Drop/delay TCP-level | ✅ | ✅ | ✅ |
| `proxy_handshake_complete` distinguishes TLS vs protocol auth | ✅ | ✅ (with new field) | ❌ |
| Effort to ship | ~2 months | ~2-3 weeks | ~1 day |
| Per-plugin code added | High | Minimal (~5 LOC per plugin) | None |
| Future plugin coverage | Each plugin reimplements TLS | Inherited | Inherited (but useless) |

## Recommended direction

**Option B (ALPN-passthrough at the transport layer).** It preserves every fault-injection capability that customers care about, ships in ~2-3 weeks instead of ~2 months, and rides on the existing `proxy.Listen()` helper from RFC-035 so the implementation has a single seam. The downside (the proxy needs its own cert) is well-understood and has standard solutions (auto-generated self-signed for dev, configurable cert path for stricter environments).

Option A is the only choice if there's a per-protocol TLS quirk Option B can't handle (e.g. gRPC's HTTP/2 ALPN conflict with mTLS client certs in some environments). I haven't found such a quirk in the protocols we ship today, but Option A remains a fallback if one surfaces.

Option C ships only as the TCP-plugin variant — for protocols Faultbox doesn't terminate (raw TCP services, custom binary protocols).

## Impact

- **Breaking changes:** none in v1. Existing plain-TCP behavior is preserved when no `tls=` is set on the interface. New `tls=` opt-in is purely additive.
- **Migration:** customer specs that previously used `mock_service()` to stand in for TLS upstreams can switch to `service(remote=..., interface(..., tls=...))` once Option B ships.
- **Performance:** TLS handshake adds ~1-5ms per connection on a warm runtime; with `proxy_conn_open/close` events from RFC-034 customers can verify the cost is bounded. Connection-pool scenarios pay the cost once.
- **Bundle size:** Option B may add a `proxy_tls_handshake_complete` event family (one per connection on TLS interfaces) — bounded by the same per-test event cap RFC-034 introduced. <1% bundle growth in typical workloads.
- **Security:** the proxy holds private keys. Same trust boundary as the bundle itself (which already captures unencrypted bytes for non-TLS interfaces); cert paths in the spec must point to dev/test material, never prod secrets. Documentation will state this explicitly.

## Open questions

1. **Proxy-side cert source.** Three sub-options:
   - **(1a) Auto-generate self-signed at proxy start.** Easy for dev; clients must trust the proxy's CA. Re-generate per session means new fingerprint every run — fine for testing, hostile to certificate-pinned clients.
   - **(1b) Configurable path.** `tls_cert(proxy_cert=..., proxy_key=...)` on the interface. Customer manages cert lifecycle; works with cert-manager / their existing PKI.
   - **(1c) Persistent fixture cert in `~/.faultbox/proxy.pem`.** Fingerprint stable across runs; client trust setup is one-time. Default for Lima / local dev.

   Lean: ship **1a + 1b together** — auto-generate for the no-config path, override for stricter environments. Defer 1c until customer ask.

2. **mTLS requires per-interface client cert.** When upstream demands mTLS, the proxy needs a client cert to dial it (separate from the proxy's server cert). RFC-038 v1 should support both via a single `tls_cert()` value carrying client cert + server cert + CA. Schema:
   ```python
   tls_cert(
       proxy_cert="/path/to/proxy-server.crt",     # cert proxy presents to clients
       proxy_key="/path/to/proxy-server.key",
       client_cert="/path/to/proxy-client.crt",    # cert proxy presents to upstream (mTLS)
       client_key="/path/to/proxy-client.key",
       ca="/path/to/upstream-ca.crt",              # CA the proxy trusts for upstream
   )
   ```

3. **ALPN selection.** For gRPC/HTTP2 we need `h2`; for HTTPS we need `http/1.1`. The interface's protocol kwarg already specifies which it is, so the proxy can pick automatically. Open question: should we expose `alpn=["h2", "http/1.1"]` for explicit override?

4. **Should a TLS-fault rule API ship as part of this RFC, or as a follow-up?** E.g. `tls.handshake_failure()`, `tls.cert_expired()`. Lean: separate RFC, after Option B's plumbing is in place. Keeps this RFC scoped to "TLS works at all."

5. **Composition with RFC-036's `remote=`.** When a remote upstream's cert is signed by a private CA the proxy doesn't trust, what's the failure mode? Lean: explicit error at spec-load time (`remote=...,interface(..., tls=tls_cert(ca="..."))` is required for non-public-CA remotes), with a warning if `tls=` is omitted on an interface that probably needs it. The customer's stack will hit this immediately; the explicit error is friendlier than a "connection reset by peer" at first dial.

6. **`proxy_handshake_complete` event split.** RFC-034 emits one event per connection at protocol-handshake completion. With TLS in the picture, do we want:
   - **(6a)** Existing event fires after both TLS + protocol handshakes complete (one event, includes a `tls=true` field).
   - **(6b)** New `proxy_tls_handshake_complete` event before the existing one, so TLS-level vs protocol-level stalls are distinguishable.

   Lean: 6b — the customer's most likely TLS bug is a stalled TLS handshake (SNI mismatch, CA not trusted). Distinguishing it from a stalled MySQL auth is exactly the diagnostic value RFC-034 set out to deliver.

## Implementation plan (when committed to Option B)

**Phase 1 — Foundation**
- Add `proxy.Dial(target, tlsCfg)` helper in `internal/proxy/listen.go` (companion to `Listen()`).
- Extend `Listen()` with a `tlsCfg` arg that wraps the listener via `tls.NewListener` when set.
- Auto-generate self-signed cert when `tls=` is set without explicit `proxy_cert`.

**Phase 2 — Spec language**
- New `tls_cert(...)` Starlark builtin returning a `TLSConfigDef` carrying cert paths.
- `interface(..., tls=tls_cert(...))` plumbs through to the proxy plugin.
- Validation at spec-load: cert/key files exist, CA is parseable, etc.

**Phase 3 — Plugin migration**
- One plugin at a time: switch upstream dial from `net.DialTimeout` to `proxy.Dial`. Verify byte-identity passthrough test under TLS.
- Order: postgres → mysql → redis → http → http2 → grpc → mongodb → kafka. ~1-2 days each.

**Phase 4 — RFC-034 integration**
- New `proxy_tls_handshake_complete` event family (per Open Question 6).
- Stall detection works unchanged on encrypted bytes (the goroutine sees byte counts via the wrapped TLS conn's `Read`/`Write`, which is fine).

**Phase 5 — Docs + corpus**
- `docs/spec-language.md` — new `tls_cert()` section, examples.
- `testops/corpus/postgres_tls_basic.star` — golden against `postgres:16` with `--ssl-mode=require` to lock in the integration.
- `docs/reports.md` — update Proxy Traffic section to mention TLS handshake events.

**Phase 6 — Customer rollout**
- inDrive Freight: switch one of their internal mTLS upstreams (likely `geo-config`) from a workaround to `remote=` + `tls=`. Verify a `grpc.error()` rule fires.

## Dependencies

- Builds on RFC-024 (proxy datapath), RFC-033 (proxy address surface), RFC-035 (proxy bind helper).
- Composes with RFC-034 (proxy traffic observability) — TLS adds a new handshake event family.
- Composes with RFC-036 (`remote=`) — TLS is the natural follow-on for non-localhost upstreams.
- No incoming dependencies from other RFCs.

## Alternatives considered

- **A separate "SSL Proxy" service that customers stand up alongside Faultbox.** Rejected: violates Faultbox's single-binary, single-spec contract. The proxy must be in-process.
- **Use a public TLS-terminating reverse proxy (envoy, traefik) and let Faultbox sit downstream of it.** Rejected: introduces an external dependency, breaks bundle reproducibility, and the customer's security posture rarely permits running an off-the-shelf reverse proxy in test environments.
- **Defer until the customer explicitly hits the wall on a specific upstream.** That moment was 2026-04-30. They explicitly hit the wall.
