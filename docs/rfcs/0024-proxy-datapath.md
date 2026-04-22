# RFC-024: Proxy on the SUT Data Path

- **Status:** Implemented (v0.9.5, 2026-04-22)
- **Target:** v0.9.5
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

## Follow-ups

- Container-mode pre-start: the current implementation targets
  `127.0.0.1:<port>` which assumes port-forward bindings to the host
  for HostPort>0. Container-internal traffic (service A dialing
  service B on Docker network) will need an extra hop via the host
  bridge — verify on truck-api before adding Docker `ExtraHosts`
  plumbing (potential Option B patch).
- **TCP proxy.** Not blocking v0.9.5; tcp services just bypass the
  proxy today. Ship if any customer hits the case.
- **Benchmark suite.** Quantify the pass-through overhead across all
  14 proxy protocols so the v0.9.5 claim "indistinguishable" has a
  number attached.
