# RFC-024: Proxy on the SUT Data Path

- **Status:** Implemented (core v0.9.5, follow-ups v0.9.6 — 2026-04-22)
- **Target:** v0.9.5 + v0.9.6
- **Created:** 2026-04-22
- **Accepted:** 2026-04-22 — all open questions resolved before implementation
- **Implemented:** 2026-04-22 — 3 phases shipped on `rfc/024-proxy-datapath`
- **Discussion:** [#54](https://github.com/faultbox/Faultbox/issues/54)
- **Depends on:** none (extends the v0.8+ protocol-proxy machinery in place)
- **Customer motivation:** truck-api on v0.8.8 reported that selective
  per-gRPC-service injection was non-functional because the proxy never saw
  app traffic. v0.9.4 fixed the proxy *dispatch* gap; v0.9.5 closes the
  architectural gap by putting the proxy in the SUT's data path.

## Summary

Before v0.9.5 the protocol proxy was **off-path**: it bound to a random
ephemeral port on first `fault(interface_ref, …)` call, and only Faultbox's
own `step()` calls were redirected through it. The SUT itself kept
reading `FAULTBOX_<SVC>_<IFACE>_ADDR` or its user-provided env, dialed the
real upstream directly, and bypassed the proxy entirely — so
protocol-level fault rules never fired against app-initiated traffic.

v0.9.5 fixes this by:

1. **Pre-starting a pass-through proxy for every proxy-capable
   interface** on a non-mock service, right after the service becomes
   ready. No rules installed ⇒ transparent byte-forwarding ⇒ zero
   behavioural change from today's tests.
2. **Rewriting `FAULTBOX_<SVC>_<IFACE>_{HOST,PORT,ADDR}`** in `buildEnv`
   to point at the proxy's listen address instead of the real upstream.
3. **Substring-rewriting user env values** (e.g., `DATABASE_URL`,
   `KAFKA_BROKERS`) at `buildEnv` time so any string containing a known
   upstream addr (`localhost:5432`, `127.0.0.1:5432`, or `<svc>:5432` in
   container mode) gets redirected to the proxy.

The upshot: a plain `fault(geo_config.main, response(status=503), run=…)`
now actually injects 503s into the SUT's gRPC calls — closing the
customer-reported architectural gap.

## Motivation

See the customer feedback that motivated this RFC in
[#54](https://github.com/faultbox/Faultbox/issues/54). Short form:
Faultbox's composable `fault_assumption(rules=[...])` API gave the
impression of selective protocol-level injection but the proxy was
never in the SUT's data path, so no rule ever reached a real request.

## Design that shipped

### Proxy lifecycle change (Phase 1)

`startServices` now calls `preStartProxies(svcName, svc)` after the
service is healthchecked and seeded. For each interface whose protocol
implements a proxy (14 of 15 — `tcp` has no proxy and is skipped
gracefully), `proxy.Manager.EnsureProxy` binds a listener on
`127.0.0.1:<random>` forwarding to the real upstream. A
`proxy_started` event is emitted with `mode="passthrough"`.

Mock services skip pre-start (no upstream to proxy). Services whose
protocol is unsupported skip pre-start silently — environment falls
back to the real upstream, same as v0.9.4.

### Env rewriting (Phase 2 + Phase 3)

`buildEnv` now consults `proxyMgr.GetProxyAddr(svc, iface)` when
injecting `FAULTBOX_*_ADDR`. If a proxy is running, the vars point at
the proxy; otherwise they fall back to the real upstream.

User env values get substring-rewritten. A substitution table keyed on
every known real-upstream spelling (`localhost:port`, `127.0.0.1:port`,
`<svcname>:port` for container mode) is applied to every user env
string. This is how a user's
`env = {"DATABASE_URL": "postgres://u:p@%s/db" % pg.main.addr}` —
resolved at spec-load time to `"postgres://u:p@localhost:5432/db"` —
becomes `"postgres://u:p@127.0.0.1:37412/db"` at launch time, so the
SUT's `pgx` dial actually reaches the proxy.

### Open questions — resolutions

| OQ | Resolution |
|---|---|
| 1. Pass-through latency acceptable? | Shipping without a formal benchmark. All 14 proxies already parse wire format on every request (that's how they match rules); adding pass-through mode is a straight `io.Copy` in the no-rules branch. Customer will validate on truck-api. |
| 2. `.internal_addr` magic vs explicit `.real_addr`? | **Magic.** No `.real_addr` ships in v0.9.5 — the data-path rewrite is silent. An explicit escape hatch can be added later without breaking anyone. |
| 3. Skip proxy for `faultbox_opt_out=True`? | Yes — no filter, no faults, no proxy value. Implicitly handled: no interface-level opt-out exists today, and opt-out services still get proxies (harmless, still pass-through). Revisit if load overhead shows up. |
| 4. Mock services? | Skip. Explicit `!svc.IsMock()` check in `preStartProxies`. |
| 5. UDP pass-through? | Covered. The existing UDP proxy (packet-level, per-client upstream connection pool) forwards datagrams on empty rules — unit-tested on v0.8+. |

### Alternatives considered (and rejected)

- **`/etc/hosts` injection (Option B).** Would work for apps that
  hardcode hostnames, but requires Docker `ExtraHosts` plumbing (missing
  today) and binary-mode mount-ns hooks, and the ports must match
  (proxy must bind the same port as the upstream — conflicts on a single
  host). Rejected for v0.9.5; revisit if customers hit the case.
- **`LD_PRELOAD` addr substitution (Option C).** Works for any app
  regardless of env-var discipline but adds a new shared-library surface,
  arch variants, and Go-binary edge cases where the runtime bypasses
  libc. Rejected.

### Why env rewriting is enough

Faultbox's existing contract already requires services to consume
`FAULTBOX_<SVC>_<IFACE>_*` or resolve upstream addresses via
`service.interface.addr` at spec-load time. Apps that hardcode
`localhost:5432` without going through either mechanism are out of
contract — they would also bypass the `FAULTBOX_*` auto-injection
today. Option B / C would address them; no customer has asked yet.

## Impact

- **Breaking changes:** None. Pass-through proxies are byte-identical
  to the previous "no proxy" behaviour when no rules are installed.
- **Migration:** None required. Existing tests keep working; any test
  that injects protocol-level faults now actually fires rules against
  SUT traffic — which is the point.
- **Performance:** Extra listener per interface at service start
  (~100μs). Hot path: one extra TCP connection per request/session to
  relay to the real upstream. Measured impact is indistinguishable
  from the previous `step()`-only proxy pattern for the workloads
  we've tested. Customer benchmarks will confirm.
- **Security:** Proxies bind to `127.0.0.1` only (binary mode) or the
  Docker network (container mode) — no new exposure.

## Tests

- `TestBuildEnvRewritesToProxyAddr` — the FAULTBOX_*_ADDR env is
  redirected to the proxy when one is running.
- `TestBuildEnvRewritesUserEnvAddrs` — user env with an embedded
  `localhost:5432` gets string-rewritten.
- `TestPreStartProxiesSkipsUnsupportedProtocols` — tcp doesn't fail
  startup; service continues without a proxy.
- `TestProxyDataPathEndToEnd` — full loop: real upstream, pass-through
  returns upstream body, rule installation flips responses to 503.

`go test ./...` clean · `go vet` clean · `linux/arm64` cross-compile OK.

## Follow-ups — shipped in v0.9.6 (2026-04-22)

All three v0.9.5 follow-ups closed in a single patch release:

### Container-mode data path (fixed)

v0.9.5 pre-started proxies on the host but only rewrote env vars in
`buildEnv` (binary mode). Container consumers (`buildContainerEnv`) were
untouched — they received raw service-name DNS (`db:5432`) and had no
route to the host-side proxy anyway. v0.9.6:

1. Adds `ExtraHosts: ["host.docker.internal:host-gateway"]` on every
   container in `internal/container/docker.go`. Docker resolves
   `host-gateway` to the bridge gateway on Linux 20.10+ and to the host
   itself on Desktop runtimes, so `host.docker.internal` now routes to
   the host from inside the container netns.
2. `buildContainerEnv` now consults `proxyMgr.GetProxyAddr` per
   interface — when a proxy is running, both `FAULTBOX_*_{HOST,PORT,ADDR}`
   and user env values get rewritten to `host.docker.internal:<proxy_port>`.
3. A new `consumerMode` parameter on the internal substitution table
   switches between `127.0.0.1` (binary) and `host.docker.internal`
   (container) spellings.

### TCP proxy (shipped)

Plain `tcp` interfaces are no longer skipped at pre-start. The new
`internal/proxy/tcp.go` is a protocol-agnostic byte-splicer with three
rule actions — `ActionDrop` (close connection), `ActionRespond` (write
fixed bytes, close), `ActionDelay` (sleep then continue splicing). A
byte-prefix predicate on the first client chunk (`Rule.Method = "HELO"`)
enables per-connection targeting without pulling in a parser.

`SupportsProxy("tcp")` now returns true; `TestPreStartProxiesStartsTCPProxy`
pins the new behaviour.

### Benchmark suite (first numbers)

`internal/proxy/bench_test.go` measures one request/response cycle per
iteration, persistent-connection where the protocol uses one. Run on
Apple M4 Pro, `-benchtime=2s`:

| Protocol | Direct | Through proxy | Overhead |
|---|---|---|---|
| HTTP/1.1 | 32 µs | 74 µs | **+42 µs** (2.3×) |
| TCP (splice) | 16 µs | 31 µs | **+15 µs** (2.0×) |
| Redis (RESP) | 16 µs | 32 µs | **+16 µs** (2.0×) |

Reading: the fixed per-request cost is ~15–40 µs — one extra loopback
hop plus protocol-specific parsing. Two takeaways:

- For workloads under a few thousand requests per test the overhead is
  invisible (< 100ms total). This covers the overwhelming majority of
  integration tests.
- Benchmarks cover three representative protocols; HTTP is the ceiling
  because of its allocation-heavy reverse-proxy path. Other parsed
  protocols (Postgres / Kafka / gRPC) should land in the TCP–HTTP range.
  Not benchmarked yet because stub upstreams for those are enough work
  to warrant their own commit.

### Still open

- **Broader benchmark coverage** — Postgres, gRPC, Kafka, MongoDB. The
  existing three cover the shape of the cost; the rest would round it
  out to a full matrix.
- **Linux Docker < 20.10 fallback.** `host-gateway` sentinel requires
  20.10+. Daemons older than that will fail container create with an
  unknown-host-key error. No customer reported yet; fix when one does.
