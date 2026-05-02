# Changelog

All notable changes to Faultbox are documented here.

The format follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and the project adheres to [Semantic Versioning](https://semver.org/).

Per-release "What's new" pages live on the site at
[faultbox.io/releases/](https://faultbox.io/releases/).

## [Unreleased]

Next-version work is tracked in
[GitHub Issues](https://github.com/faultbox/Faultbox/issues).

## [0.12.26] - 2026-05-02

RFC-038 Phase 3 (3 of 4) — Kafka plugin TLS migration. Brokers
configured with SSL listeners (the prod-shaped Kafka deployment) can
now sit behind the proxy with topic-glob fault rules still firing.

### Changed

- **`kafka` plugin migrated to TLSAware.** Kafka has no in-band
  SSL upgrade — brokers expose plain and TLS on separate ports —
  so the wrap-and-dial pattern from http.go applies cleanly:
  - Listener: `proxy.ListenTLS(serverTLS)` when set, plain
    `Listen()` otherwise.
  - Upstream: `proxy.Dial(ctx, target, clientTLS, 5s)` replaces
    the bare `net.DialTimeout` call so TLS handshake honours
    both ctx cancellation and the 5s budget.
  - Plaintext path runs unchanged.
- **Coverage gate exemption dropped** for `kafka.go`. The new
  `kafka_test.go` (5 tests, including a byte-identity passthrough)
  satisfies the #84 coverage requirement; the exemption that
  predated this PR was the last "backfill candidate" tagged in the
  list.

### Tests

5 new tests in `internal/proxy/kafka_test.go` (file did not exist
before this PR — kafka was on the #84 backfill list):

| Test | Covers |
|---|---|
| `TestKafkaProxy_Passthrough` | byte-identity round trip (#84 baseline) |
| `TestKafkaProxy_TLSEndToEnd` | Kafka-over-TLS at both legs |
| `TestKafkaProxy_TLSRuleInjection` | topic-glob drop fires inside TLS tunnel |
| `TestKafkaProxy_PlaintextStillWorks` | plaintext regression |
| `TestKafkaProxy_ImplementsTLSAware` | type-assertion contract |

Full repo `go test ./... -race` green; cross-compile + Lima
demo-container 4/4 PASS.

Version 0.12.25 → 0.12.26.

## [0.12.25] - 2026-05-02

RFC-038 Phase 3 (2 of 4) — gRPC plugin TLS migration. Closes the
remaining half of the customer's gap #1 (gRPC-TLS) — `truck-api →
geo-config` over mTLS now flows through the proxy with rules still
firing.

### Changed

- **`grpc` plugin migrated to TLSAware.** `SetTLS(server, client)`
  threads the resolved configs into:
  - Server side: `grpc.Creds(credentials.NewTLS(serverCfg))` —
    routed via the gRPC framework's own creds path rather than
    pre-wrapping the listener (which double-handshakes the
    connection). The listener stays plain `Listen()`.
  - Client side: `credentials.NewTLS(clientCfg)` instead of
    `insecure.NewCredentials()` for the upstream dial. ALPN h2 is
    forced on the client cfg (gRPC-go forces it server-side
    automatically).
- Plaintext path runs unchanged — without `SetTLS`, the plugin
  retains its `insecure.NewCredentials()` h2c behavior verbatim.

### Why a different listener strategy than http2

The http2 plugin pre-wraps its listener via `proxy.ListenTLS` because
`http.Server` integrates TLS via the wrapped conn. gRPC-go's server
owns its own TLS handshake via `grpc.Creds` and gets confused when
handed an already-encrypted conn. Routing through `grpc.Creds` is the
framework-idiomatic seam and avoids a double-handshake bug.

The customer-facing surface is identical: `interface("geo", "grpc",
443, tls=tls_cert(...))` works the same way — only the internal
plumbing differs.

### Tests

4 new tests in `internal/proxy/grpc_tls_test.go`:

| Test | Covers |
|---|---|
| `TestGRPCProxy_TLSEndToEnd` | gRPC-over-TLS at both legs, raw-bytes byte-identity round trip |
| `TestGRPCProxy_TLSRuleInjection` | `grpc.error(method=...)` rule fires through TLS |
| `TestGRPCProxy_PlaintextStillWorks` | regression — h2c + insecure.NewCredentials |
| `TestGRPCProxy_ImplementsTLSAware` | type-assertion contract |

Full repo `go test ./...` green; `go vet` clean.

Version 0.12.24 → 0.12.25.

## [0.12.24] - 2026-05-02

RFC-038 Phase 3 (1 of 4) — first plugin migrations. The `http` and
`http2` proxies now terminate TLS at their listener and / or dial the
upstream over TLS when the spec declares `tls=tls_cert(...)`. The
plaintext path is unchanged: tests written before RFC-038 keep
running bit-identical to v0.12.21. Per the customer's gap list, this
ships items #2 (HTTPS) and partially #1 (gRPC-TLS — Phase 3 PR 2
wires the gRPC plugin specifically).

### Added

- **`TLSAware` interface** in `internal/proxy/proxy.go` —
  `SetTLS(server, client *tls.Config)`. Plugins implement this to
  opt into Phase 3 TLS handling. Plugins that don't implement it
  stay plain-TCP only and `proxy_tls_pending` is emitted.
- **`Manager.EnsureProxyTLS(ctx, …, server, client)`** — TLS-aware
  variant of `EnsureProxy`. Returns a `tlsApplied bool` so the
  runtime can detect plugins that haven't migrated yet and warn
  the customer. Existing `EnsureProxy` is unchanged for callers
  that don't pass TLS material.
- **`http` plugin migrated** — wraps the listener via `ListenTLS`
  when `serverTLS` is set; reverse-proxy `Transport.TLSClientConfig`
  hooks the customer's CA / mTLS material when `clientTLS` is set.
  Plaintext path runs unchanged.
- **`http2` plugin migrated** — same pattern, with ALPN `h2`
  forced on both legs and `http2.ConfigureServer` installed when
  the listener side speaks TLS so HTTP/2 dispatch works at the
  http.Server layer. Plaintext h2c upgrade keeps working.

### Wiring

- `preStartProxies` resolves `iface.TLS.ResolveServerConfig` /
  `ResolveClientConfig` against the spec directory and routes
  through `EnsureProxyTLS`. The `proxy_started` event's `mode`
  field is now `"tls"` when the migration applied (formerly
  always `"passthrough"`); the `proxy_tls_pending` warning only
  fires when `tlsApplied=false`.
- Auto self-signed cert path includes the upstream host portion
  in its SAN list so customers pointing at
  `interface("main", "http", 8080)` against
  `target=truck-api.svc.cluster.local:443` get a proxy cert that
  covers the hostname without spelling out a SAN list.

### Tests

9 new tests in `internal/proxy/http_tls_test.go`:

| Test | Covers |
|---|---|
| `TestHTTPProxy_TLSEndToEnd` | client HTTPS → proxy → upstream HTTPS |
| `TestHTTPProxy_TLSRuleInjection` | path-glob fault rule fires inside the TLS tunnel |
| `TestHTTPProxy_PlaintextStillWorks` | regression — no SetTLS = pre-RFC-038 behaviour |
| `TestHTTPProxy_ImplementsTLSAware` | type-assertion contract |
| `TestHTTP2Proxy_TLSEndToEnd` | h2-over-TLS at both legs, ALPN negotiation |
| `TestHTTP2Proxy_TLSRuleInjection` | rule fires through h2 + TLS |
| `TestHTTP2Proxy_PlaintextStillWorks` | h2c regression |
| `TestHTTP2Proxy_ImplementsTLSAware` | type-assertion contract |
| `TestEnsureProxyTLS_AppliedFlag` | manager flags TLS-aware vs plain plugins |

Full repo `go test ./...` green; Lima `demo-container` 4/4 PASS
(no TLS in demo yet — regression check on the http path that the
demo uses).

Version 0.12.23 → 0.12.24.

## [0.12.23] - 2026-05-02

RFC-038 Phase 2 — Starlark spec-language surface for TLS. Customers
can now declare `interface(..., tls=tls_cert(...))` on a service
interface; the spec validates at load time and the resolved cert
material flows through to the proxy lifecycle. Phase 3 plugin
migration is what actually wraps the listener; Phase 2 ships the
spec contract so customers can write their TLS specs ahead of the
per-plugin work.

### Added

- **`tls_cert(...)` Starlark builtin** in `internal/star/builtins_tls.go`.
  Kwargs-only — positional args are refused so a typo can't silently
  swap server / client material:
  - `proxy_cert` / `proxy_key` — server cert + key the proxy
    presents to clients connecting to its listener. Both must be
    set or both omitted; empty pair = auto-generated self-signed
    cert at proxy-start time (RFC sub-option 1a).
  - `client_cert` / `client_key` — mTLS client material the proxy
    presents to upstream when dialing. Same paired-or-omitted rule.
  - `ca` — PEM file the proxy trusts when verifying the upstream
    cert. Parsed at spec-load to fail fast on garbage CAs.
  - `insecure=True` — escape hatch for dev clusters with self-signed
    upstream certs the proxy can't trust. Mutually exclusive with
    `ca=` (refused at spec-load).
  - Relative paths resolve against the spec's directory (rt.baseDir),
    matching the load_file convention.
- **`interface(..., tls=tls_cert(...))`** — kwarg accepted on every
  interface. Switched the builtin from `UnpackPositionalArgs` to
  `UnpackArgs` with `spec?` and `tls?` declared, so unknown kwargs
  now produce clean errors instead of silently being ignored.
- **`TLSConfigDef.ResolveServerConfig(baseDir, extraHosts)`** —
  builds the `*tls.Config` Phase 3 plugins will hand to
  `proxy.ListenTLS`. Auto-falls-through to
  `proxy.GenerateSelfSignedCert` when no `proxy_cert` was set.
- **`TLSConfigDef.ResolveClientConfig(baseDir)`** — builds the
  upstream-side `*tls.Config` for `proxy.Dial`. Honours mTLS client
  cert + CA pool + InsecureSkipVerify.
- **`proxy_tls_pending` event** — emitted from `preStartProxies`
  when an interface declares `tls=` but Phase 3 hasn't migrated
  that protocol yet. The starlark logger also warns. Silence here
  would let a "TLS handshake fails against proxy" debugging
  session burn an hour, so we surface the gap explicitly.

### Validation guarantees

Spec authors get fast errors at load time, not at first dial:
- Half-set proxy / client cert+key pairs.
- Missing cert / key / CA files on disk.
- CA file that doesn't contain any PEM certificates.
- `insecure=True` combined with `ca=` (contradictory).
- `tls=` value that isn't a `tls_cert(...)` (string, bool, etc.) —
  error names `tls_cert(...)` so customers know how to fix it.

### Tests

12 new tests in `internal/star/builtins_tls_test.go`:
- `tls_cert()` no-args / kwargs-only / pair-validation /
  file-existence / CA-parse / insecure×ca-exclusion / relative-path
  resolution.
- `interface(..., tls=...)` stores the value on InterfaceDef and
  rejects wrong types.
- `ResolveServerConfig` auto-cert path + load-from-disk path.
- `ResolveClientConfig` mTLS+CA path + insecure path.

### Internal

- `interface()` builtin moved from `UnpackPositionalArgs` (which
  silently ignored anything not in its 3-arg list) to `UnpackArgs`
  with explicit `spec?` and `tls?` declarations. Net effect:
  unknown kwargs now error at spec-load. Tests confirm no
  regressions across the 17 packages that exercise interface().

Full repo `go test ./...` green; `go vet` clean.

Version 0.12.22 → 0.12.23.

## [0.12.22] - 2026-05-02

RFC-038 Phase 1 — TLS-aware proxy foundation. Lays the transport-
layer plumbing every plugin will inherit when the per-plugin migration
lands in Phase 3. No plugin behavior changes in this release; pure
infrastructure addition.

### Added

- **`proxy.ListenTLS(cfg)`** — TLS-aware variant of `Listen()` that
  wraps the bind-side listener via `tls.NewListener`. Returns the
  same `(net.Listener, listenAddr, error)` triple, so plugins flip
  one call site to opt into TLS termination at the proxy without
  touching their handler code (the handler still reads/writes
  plaintext via the wrapped `*tls.Conn`).
- **`proxy.Dial(ctx, target, cfg, timeout)`** — upstream-side
  companion. With a nil cfg it's `net.DialTimeout("tcp", …)`; with
  a non-nil cfg it negotiates TLS via `tls.Client.HandshakeContext`,
  honouring both ctx cancellation and the timeout argument so
  stalled handshakes don't outlive the call.
  - Auto-fills `cfg.ServerName` from `target`'s host portion when
    unset, so the customer's `tls=tls_cert(ca=...)` material matches
    upstream certs without per-plugin SNI plumbing.
- **`proxy.GenerateSelfSignedCert(hosts)`** — returns a `*tls.Config`
  with a freshly minted ECDSA P-256 self-signed cert in memory. SAN
  always includes `localhost` + `127.0.0.1` + `::1` so the host-side
  dial address from `Listen()` works without per-test config; extra
  hosts get added on top. 24-hour validity window. New cert per call
  (intentional — Phase 1 ships sub-option 1a from the RFC; persisted
  fixture path 1c lands when a customer asks).

### RFC-038 scope notes

- Phase 1 = foundation only. `internal/proxy/{listen.go,tls.go}` are
  the only files touched on the data path — the 14 existing `Listen()`
  callsites in plugins are unchanged. Adding the sibling helper
  rather than extending `Listen()`'s signature kept the diff
  reviewable and lets per-plugin migration roll in one PR at a time.
- Spec-language surface (`tls_cert(...)` Starlark builtin) is
  Phase 2; per-plugin upstream-dial migration is Phase 3.
- `proxy_tls_handshake_complete` event family (Open Question 6 in
  the RFC) lands with Phase 4 once at least one plugin actually
  terminates TLS — no point shipping the event before something
  fires it.

### Tests

9 new tests cover the foundation:
- Loopback SAN defaults / custom hosts / fresh-on-each-call for
  `GenerateSelfSignedCert`.
- `ListenTLS` rejects nil cfg and round-trips bytes through a real
  TLS handshake.
- `Dial` plaintext path, TLS path, handshake-timeout (timeout
  argument is honoured), and ServerName-defaulting-from-target
  verification.

Full repo `go test ./...` green; no plugin behavior changes so
Lima sweep mirrors v0.12.21.

## [0.12.21] - 2026-05-01

RFC-034 Phase 2 — proxy traffic observability extended to 9 more
plugins. v0.12.20 wired conn lifecycle + handshake + stall events
into 4 plugins (tcp, mysql, postgres, redis). This release covers
9 of the remaining 11; the customer's bundle now carries
proxy_conn_open / _close / _handshake_complete events for nearly
every protocol Faultbox proxies.

### Added

- **6 frame/text-based plugins instrumented** (same pattern as
  mysql/postgres/redis):
  - `amqp.go` — protocol-header acts as handshake marker;
    bidirectional frame forwarders count C2S/S2C bytes per
    direction.
  - `cassandra.go` — frame-aware on client→server (count C2S
    inline), `io.Copy` on server→client (wrapped via
    `tracker.WrapServerReader` to count S2C). Handshake fires
    after first request-response round-trip.
  - `kafka.go` — length-prefix framed; conn_open/close + first
    request marks handshake.
  - `memcached.go` — text-line + binary-data hybrid; bytes
    counted at every `bufio.Reader.ReadString` and
    `Fprintf`/`Write`. Handshake fires on first round-trip.
  - `mongodb.go` — LE32 framed; same pattern as kafka.
  - `nats.go` — text-line via `bufio.Scanner`; first server line
    (typically `INFO`) marks handshake_complete; byte counts
    include the trailing newline.

- **3 HTTP-family plugins instrumented via
  `http.Server.ConnState`** (http, http2, clickhouse):
  - New `HTTPConnStateTracker` helper in `observability.go`
    maps `StateNew` → `proxy_conn_open`, `StateClosed` →
    `proxy_conn_close`, first `StateActive` →
    `proxy_handshake_complete`. Idempotent on keep-alive
    (handshakeDone CAS).
  - HTTP/2 emits per underlying-TCP-conn (one open/close per
    physical connection regardless of stream count).
  - **Byte counts not yet wired** for HTTP-family — `http.Server`
    reads/writes through the listener internally; a Listener
    wrapper that returns counting Conns is the natural follow-up.
    `bytes_c2s` / `bytes_s2c` report 0 for these plugins until
    that ships.

### Out of scope (final follow-up)

- **`udp.go`** — datagram protocol, connectionless. RFC-034's
  conn_open/conn_close model doesn't fit. Likely needs a new
  event family (`proxy_datagram_received` / `_sent`) — separate
  RFC.
- **`grpc.go`** — gRPC's per-RPC handler is not connection-scoped
  and the standard library's grpc.Server does not expose a
  per-connection lifecycle hook compatible with RFC-034. The
  `grpc.StatsHandler` interface gives per-RPC stats but not
  per-connection lifecycle. Defer until we can either build a
  StatsHandler-based variant or wrap the listener pre-gRPC.

### Internal

- `proxy_test.go::TestHTTPProxyErrorRule` updated to count only
  legacy rule-fired events (`ProxyEvent.Type == ""`); the
  added connection-lifecycle events would otherwise inflate the
  per-test count from 1 to 3.

## [0.12.20] - 2026-05-01

RFC-034: proxy traffic observability. The Faultbox transparent
proxy emits four new event families through the existing
`OnProxyEvent` hook so the bundle's report shows connection
lifecycle, byte flow, and stall conditions at the proxy layer:

- `proxy_conn_open` — accepted client + dialed upstream
- `proxy_conn_close` — connection done; carries `duration_ms`,
  `bytes_c2s`, `bytes_s2c`, `reason` (`client_eof` / `server_eof`
  / `context_cancel` / `io_error` / `stall_timeout` / `rule_drop`)
- `proxy_handshake_complete` — protocol-aware proxies only;
  emitted after auth phase completes (mysql, postgres, redis)
- `proxy_stall` — read direction blocked on pending bytes for
  ≥ stall threshold (default 5s warn, 30s extend; one stall event
  per direction per tier per connection)

Customer-driven (inDrive Freight, 2026-04-28). The v0.12.15.x
arc spent multi-day debug cycles on every proxy-forwarding bug
because the report timeline showed `proxy_started → 60s of
silence → exit_code=2` with no hint that the proxy was the
issue. Diagnosis required SUT-side instrumentation. With these
events, a stalled MySQL handshake or a half-duplex deadlock
shows up directly in the bundle.

### Added

- **New `ProxyEvent.Type` field** on `internal/proxy.ProxyEvent`.
  Empty defaults to `"proxy"` for backward compatibility with
  every existing rule-fired emit site that doesn't set it; new
  RFC-034 emit sites set it explicitly to one of the four event
  type constants. Runtime callback dispatches on Type.

- **`internal/proxy/observability.go`** — `connTracker` per
  connection, `EmitOpen` / `EmitHandshakeComplete` / `EmitClose`
  / `EmitStall` methods, `WrapClientReader` / `WrapServerReader`
  helpers for io.Copy byte counting, `AddBytesC2S` / `AddBytesS2C`
  for per-packet plugins, `classifyCloseReason` shared error
  mapping. Short-hex `conn_id` correlates open/close/stall
  events for the same connection in the bundle.

- **Wired in 4 plugins**: `tcp.go` (open/close + stall watcher),
  `mysql.go` (open/close + handshake + per-packet bytes),
  `postgres.go` (open/close + handshake + per-message bytes),
  `redis.go` (open/close + first-command handshake + per-RESP
  bytes). The remaining 9 plugins (http, http2, grpc, kafka,
  mongodb, cassandra, clickhouse, amqp, nats, memcached, udp)
  follow in a separate PR — same pattern, no schema changes.

- **`docs/spec-language.md`** event-types table extended with
  the four new types so spec authors can write monitors and
  assertions against them (`assert_eventually(where=lambda e:
  e.type == "proxy_stall")`).

### Internal

- New `internal/proxy/observability_test.go` covers
  open/close/handshake-once/nil-onEvent/byte-flow/close-reason
  classification — satisfies #84 proxy-coverage gate for the
  new file.

- **Subtle bug avoided in tcp.go**: the splice block was
  rewritten to use wrapped readers for byte counting, but the
  initial draft also added a second `<-done` wait after the
  first to ensure byte counts settled before EmitClose. That
  hung healthy long-lived connections (redis pipelining,
  keepalives) — neither io.Copy returns until peer closes, so
  waiting on the second drain blocked forever. Reverted to
  single `<-done` semantics; byte counts at EmitClose may be
  slightly under-final (last io.Copy buffer in flight) but the
  conn lifecycle stays unblocked. Caught in Lima sweep before
  commit.

### Out of scope (follow-up PRs)

- 9 remaining protocol plugins (http, http2, grpc, kafka,
  mongodb, cassandra, clickhouse, amqp, nats, memcached, udp)
  still need conn_open/close emits.
- Renderer-side rich rendering of the new event types in the
  swim-lane (proxy_stall ring, proxy_handshake_complete tick).
  Currently they fall through the report's generic event-display
  path; readable but not specially styled.
- CLI flags `--max-proxy-events` and `--proxy-stall-threshold`
  for ops/CI tuning. Defaults work today.

## [0.12.19] - 2026-05-01

Container-mode `observe=` wiring + regex decoder bugfix.

v0.12.18 added `stderr()` but only wired it for binary-mode services.
This release closes the gap for container services: `observe=[stdout(...),
stderr(...)]` now works against any Docker image, with logs captured
via Docker's multiplexed log stream and demuxed inside Faultbox.

### Added

- **Container-mode `observe=` capture.** Service services launched
  via `image=` / `build=` now stream their stdout and stderr through
  Faultbox's event log, same as binary services. Implementation
  reads the Docker multiplexed log channel via
  `client.ContainerLogs(ctx, id, LogsOptions{ShowStdout, ShowStderr,
  Follow})` and demultiplexes with `stdcopy.StdCopy`. Console output
  preserved via `io.MultiWriter` tee — `docker logs` watchers and
  Faultbox's bundle both see the same lines.

  Lima smoke (`redis:7-alpine` with `observe=[stdout(decoder=regex_decoder(...))]`)
  produced 14 decoded `stdout` events with `pid` / `role` / `rest`
  capture-group fields populated end-to-end.

### Fixed

- **`regex_decoder(pattern=...)` was silently failing on every
  observation site.** The Starlark builtin stored decoder kwargs
  in `ObserveConfig.Params` with a `decoder_` prefix to avoid
  collisions with source-level params, but the runtime called
  decoder factories with that map unstripped — so the regex
  factory's lookup of `params["pattern"]` always missed and
  returned the "regex decoder requires 'pattern' parameter"
  error. Cosmetic for `json_decoder`/`logfmt_decoder` (zero
  params), load-bearing for `regex_decoder`. New helper
  `decoderParams()` strips the prefix at all three factory call
  sites (binary stdout, binary stderr, container stdout/stderr).

### Internal

- New `Client.StreamLogs(ctx, containerID, stdoutW, stderrW)` in
  `internal/container/docker.go` — thin wrapper around
  `cli.ContainerLogs` with stdcopy demux. Either writer may be nil
  to discard that stream.
- New `Runtime.setupContainerObservation(ctx, svcName, svc, containerID)`
  in `internal/star/runtime.go`. Mirrors the binary-mode wiring
  pattern; spawns one goroutine per container that pulls from the
  log stream until container exit or context cancel.
- `internal/star/runtime.go` factory call sites at lines 1122,
  1161, 1523 all routed through `decoderParams()`.

## [0.12.18] - 2026-05-01

`stderr()` event source. Counterpart to the existing `stdout()` source
— captures the service's stderr stream and emits each line as a
first-class trace event. Customer-driven (inDrive Freight,
2026-04-30): every default-configured Go service using zap, slog, or
logrus writes to fd 2; pre-v0.12.18 the only way to observe those
logs through Faultbox was to inject an env-gate (e.g.
`FB_LOG_TO_STDOUT=1`) into the SUT and rebuild it. With `stderr()`
the SUT stays unchanged.

### Added

- **`stderr(decoder=...)` event source.** Same kwargs surface as
  `stdout()`; same decoder catalog (`json_decoder`, `logfmt_decoder`,
  `regex_decoder`). Emits events with type `"stderr"` so the
  report timeline filters and the event-log table can distinguish
  the two streams. Use both simultaneously when the SUT splits
  log streams (e.g. business events on stdout, errors on stderr).

  ```python
  observe=[
      stdout(decoder=json_decoder()),
      stderr(decoder=json_decoder()),
  ]
  ```

### Internal

- New `internal/eventsource/stderr.go` + `stderr_test.go` mirror
  the stdout source byte-for-byte; only the registered name and
  emission type differ. Decoder interface is source-agnostic so
  existing decoders apply unchanged.

- `internal/star/runtime.go` binary-mode launch path now branches
  on `obs.SourceName ∈ {"stdout", "stderr"}` and wires a
  separate OS pipe per stream. The two flow into the engine
  session config's `Stdout` / `Stderr` fields independently;
  console output is preserved via `io.MultiWriter` tee.

- Container-mode launch path is unchanged — neither stdout nor
  stderr observation is wired there yet, tracked separately.

## [0.12.17] - 2026-05-01

RFC-035: container-consumer fault paths on Linux Docker. Closes the
silent breakage that has been hiding in `poc/demo-container` since
v0.9.6 — when a container SUT dials an upstream through the
fault-injection proxy on Lima/Linux Docker, `host.docker.internal`
resolves to the docker0 bridge gateway (`172.17.0.1`), but every
proxy plugin bound on `127.0.0.1` only — so the connection RST'd
before the SUT's first byte. Two independent fixes; together they
close the design hole.

### Changed

- **Proxy listeners now bind on `0.0.0.0` on Linux** (or
  `FAULTBOX_PROXY_BIND` if set). New `proxy.Listen()` /
  `proxy.ListenUDP()` helpers in [internal/proxy/listen.go](https://github.com/faultbox/Faultbox/blob/main/internal/proxy/listen.go)
  centralise the bind decision and normalise the dial address to
  `127.0.0.1:<port>` regardless of bind interface, so host-binary
  consumers keep their loopback dial unchanged. All 13 protocol
  plugins (amqp, cassandra, clickhouse, grpc, http, http2, kafka,
  memcached, mongodb, mysql, nats, postgres, redis, tcp, udp)
  migrated to the helper. Defaults: `0.0.0.0` on Linux,
  `127.0.0.1` everywhere else (Mac/Windows Docker Desktop already
  tunnels `host.docker.internal` to host loopback). Override with
  `FAULTBOX_PROXY_BIND=127.0.0.1` for shared CI runners on public
  NICs where LAN exposure is unwanted.

- **Container-consumer address substitution gated on registered
  proxy faults.** `proxyAddrSubstitutionsFor` now only emits
  rewrites for `containerConsumer` mode when at least one
  `fault_scenario` in the suite registers a proxy-level rule
  against the interface. Without a fault, container SUTs use
  Docker's container DNS directly — no proxy round-trip, no
  reachability dependency on the bridge bind. New
  `Runtime.faultedInterfaces()` helper does static-analysis at
  spec-load over `rt.faultScenarios`. Binary consumers are not
  gated: substitution doubles as DNS translation for them
  (`postgres:5432` is unresolvable on the host), so they always
  rewrite to the proxy listener — same as pre-RFC-035 behavior.

### Fixed

- **`poc/demo-container` no longer silently broken on Lima.**
  Was hidden because `TestGoldens` doesn't run container-to-container
  faults, the demo passed on Mac Docker Desktop (loopback tunneling),
  and a pre-RFC-035 hotfix had already reverted the `api` service
  to a host binary. With RFC-035 the underlying bug is gone, so
  future container SUTs reaching containerised upstreams via
  `host.docker.internal` work on Linux Docker.

### Internal

- New `internal/proxy/listen.go` + `listen_test.go` (covers
  default platform behavior, FAULTBOX_PROXY_BIND override, UDP,
  byte-identity passthrough) — satisfies the #84 proxy-coverage
  gate.

- `cmd/faultbox/replay_test.go::TestEnforceReplayVersionPolicySame`
  now reads the binary's compiled-in version dynamically rather
  than hard-coding `"dev"` — was a latent test-brittleness that
  fired on the v0.12.16 → v0.12.17 bump.

## [0.12.16] - 2026-04-30

Report UX overhaul. The HTML report (`faultbox report <bundle.fb>`)
was the customer's working window during the v0.12.15.x triage and
the timeline view turned out to be the bottleneck — too much
chrome, fault markers buried under framework chatter, causal
arrows that connected service-ready events to errors instead of
the actual fault. v0.12.16 reshapes the timeline + drill-down
without changing the bundle format or any spec-language surface.

### Changed

- **Causal links now follow cause, not chronology.** `findCausalAncestors`
  switched from vector-clock partial order to seq-based strict
  precedence and restricted both the target and candidate sets to
  *cause* events (faults, violations, errored steps). Lifecycle
  events (`service_started`, `service_ready`, etc.) are no longer
  drawn as ancestors; in real bundles their vector clocks were the
  only complete ones, which left the spaghetti pointing at
  `service_ready` instead of the actual `proxy_fault_applied`.
  Hovering an ordinary success step now draws zero lines.

- **Timeline filter bar** above every Event Trace block. Three
  presets — **Compact** (default; hides `proxy_started`,
  `proxy_active`, `service_*`, `mock.*`, `service_seed/_reset`,
  `session_completed`), **Anchors only** (faults / violations /
  errored steps only), **All events** (historical default) — plus
  a free-text search input that live-matches on event type,
  headline, and field values. The filter applies to both the
  swim-lane markers and the event-log table; pinned-event state
  survives filter rebuilds.

- **`proxy_fault_applied` / `proxy_fault_removed` are now
  first-class fault markers** (red, not the default-syscall blue)
  in `markerKind`, `severityScore`, `isAnchorEvent`, and
  `eventHeadline`. Added to `report.go`'s `anchorTypes` so they
  survive Phase 3 downsampling. The proxy fault headline now
  reads `+ proxy fault [mysql_deadlock] · mysql/main` end-to-end.

### Added

- **Per-test "Faults applied" section** in the drill-down dialog
  pairs each `proxy_fault_applied` with its matching
  `proxy_fault_removed` and renders one row per assumption
  (service · protocol · interface · assumption · seq window).
  Reflects both seccomp and proxy-level fault mechanisms; if the
  test had neither, the section explicitly says so.

- **Recent block in the Assertion drill-down interleaves fault
  events** between captured step rows by seq, so a failed
  `assert_eq` reads as `→ call → + proxy fault [redis_oom] →
  ← reply ERR: read EOF` instead of just call/reply pairs.
  Fault rows render in the fail tint with a small seq-numbered
  pill prefix; step rows get a neutral pill.

- **Fade-and-expand on the Assertion block.** Long Recent lists +
  big Actual reprs were dominating the dialog. The pairs grid now
  caps at 220px with a CSS `mask-image` gradient that fades the
  bottom of the content to transparent and a "Show full
  assertion ▾" pill below; click expands the cap to full size with
  a 320ms transition. Compact assertions auto-detect and skip the
  affordance.

- **Group-members table on folded markers.** Clicking a `×N` chip
  on the timeline shows the underlying member events as a
  scrollable, paginated table (`seq · type · summary`, 100 rows
  per page, sticky header) — the runs that hide a 5xx among 99
  successes are now legible. Replaces the old "collapsed run"
  one-liner that listed only the first/last seq.

- **Fullscreen toggle on the test details modal** (⤢ ↔ ⤡ next to
  the close button) — the dialog is the main working surface
  during triage, and a single 95%-width column was tight when
  paired with a big spec source listing.

- **STDOUT JSON renders as a 2-column key/value table.** `data`
  fields containing JSON-encoded log lines flatten via dot-paths
  (`req.method`, `items[0].id`) so every leaf has its own row in
  the event log expansion drawer.

- **Source block falls back to `fault_matrix(...)` for
  generated test names.** Matrix-generated tests (e.g.
  `test_matrix_get_order_feed_mysql_slow`) have no literal `def`
  in the spec; the Source drill-down now anchors on the matrix
  call site and surfaces three jump links — scenario
  (`def get_order_feed()`), fault (`mysql_slow =
  fault_assumption(...)`), matrix (`fault_matrix(...)`). Was
  previously "Could not locate def..." for every matrix cell.

### Fixed

- **Timeline tooltips no longer overflow off-screen.** The
  absolute-positioned tooltip was shrink-fitting against the
  remaining space-to-edge when `word-break: break-word` was
  enabled, which produced a vertical strip of one character per
  line on narrow gaps. Switched to `width: max-content` +
  `max-width: min(520px, calc(100vw - 24px))` and added
  four-side viewport clamping.

- **Detail panel summary no longer truncates.** `eventHeadline()`
  takes an `opts.full` flag that callers in the drill-down detail
  view set, so the SUMMARY row shows full SQL queries, full error
  messages, full HTTP paths. The event-log table headlines stay
  truncated to keep the table layout intact. The Recent context
  list also dropped its `text-overflow: ellipsis` — long lines
  wrap inside the assertion box now.

- **Folded marker click routes to the chip, not the underlying
  singleton.** `markerEvBySeq` keeps a parallel map from seq to
  the chip object (carrying `_runMembers`); the click handler
  prefers it before falling back to the raw event lookup. Without
  this, clicking a `×120` chip resolved to one of the 120
  underlying events and the Group-members table never rendered.

## [0.12.15.2] - 2026-04-30

Hotfix on top of v0.12.15.1. Customer (inDrive Freight) verified the
v0.12.15.1 Redis RESP3 fix landed clean — cold-start path went green
end-to-end for the first time (smoke `test_health_check` PASS in
16.3 s, both MySQL and Redis handshakes traverse the proxy). The
failure point then moved to the **reuse path**: in the dbmatrix run,
cell 1 (cold) passes; cells 2–18 (reused proxy) all fail identically
with `error connect to db: invalid connection` /
`connection reset by peer` from the redis reset hook. **Finding K.**

### Fixed

- **Proxy lifetime is the Manager's lifetime, not the caller's.**
  `Manager.EnsureProxy` was rooting each proxy's `Start` context at
  the caller's ctx. `preStartProxies` is called from `RunTest` under
  a per-test `testCtx` that cancels via `defer cancel()` at end of
  test ([runtime.go:767](https://github.com/faultbox/Faultbox/blob/main/internal/star/runtime.go#L767)).
  At end of cell 1, that cancellation propagated into the proxy's
  Accept goroutine — which exited cleanly. **The listener fd stayed
  bound** (only `Stop()` closes it), and the cached `m.proxies[key]`
  entry stayed in place. Cells 2..N hit `EnsureProxy` → cache hit →
  `proxy_active(reused)` event fired → but no userspace `Accept` was
  pulling connections off the queue. Clients saw TCP-level reset
  (kernel SYN/ACK + queue overflow) or refused (post-RST), surfacing
  as `driver.ErrBadConn` from go-sql-driver and
  `read: connection reset by peer` from go-redis.

  v0.12.15.2 roots the proxy's `pCtx` at `context.Background()` so
  the goroutine's lifetime is bound to `Manager.StopAll` /
  `StopService` (which already cancel and close per-proxy explicitly)
  instead of any per-call ctx. Single `EnsureProxy` line change
  ([proxy.go:165](https://github.com/faultbox/Faultbox/blob/main/internal/proxy/proxy.go#L165));
  no per-protocol churn.

  Why this latched onto v0.12.13's reuse work: pre-v0.12.13,
  no-seccomp containers (recipe-based MySQL / Redis upstreams) were
  destroyed and recreated every cell, which forced fresh proxies via
  the cold path. v0.12.13 fixed reuse so the runtime kept containers
  AND proxies alive across cells — exposing this latent ctx-rooting
  bug.

  New regression test
  `TestManagerEnsureProxy_SurvivesCallerCtxCancel`: cancels the
  caller's ctx after a successful first round-trip, then verifies a
  fresh client can still complete a request through the listener.
  Hangs / `connection refused` on v0.12.15.1, passes on v0.12.15.2.

### Note on what landed in 48 hours

Three hotfixes in the same arc:
- v0.12.15 — MySQL caching_sha2 fast-auth-success (Finding H, handshake)
- v0.12.15.1 — Redis RESP3 HELLO map (Finding J, post-handshake parsing)
- v0.12.15.2 — proxy goroutine ctx-rooting (Finding K, reuse path)

Each release moved the failure deeper into the boot/test sequence.
Cold-start and reuse are now both unblocked; the bidirectional
`io.Copy` passthrough refactor flagged alongside RFC-034 remains the
right durable fix for the per-protocol parsing class.

## [0.12.15.1] - 2026-04-29

Hotfix on top of v0.12.15. Customer (inDrive Freight) verified the
MySQL `caching_sha2_password` fast-auth-success fix landed clean
(Finding H closed, smoke test progressed past the MySQL stage). The
failure point moved one step forward to Redis: `truck-api` now hangs
6 s on its first `Ping()` because **go-redis v9 unconditionally sends
`HELLO 3` from `initConn`**, which forces the server into RESP3 and
returns a map (`%N`) reply that v0.12.15's redis proxy didn't know
how to parse.

### Fixed

- **Redis proxy parses RESP3 aggregate types.** `readRESPRaw` only
  recognised RESP2 framing (`+`, `-`, `:`, `$`, `*`); on a RESP3 map
  header (`%`) it fell through to the default branch, returned just
  the header line, and left the map body unread on the upstream
  socket. Subsequent reads stalled until the client's read deadline
  fired. Wire-level evidence from the customer:

  ```
  redis-cli -p $PROXY    PING       (RESP2)        → PONG in 7 ms ✓
  redis-cli -p $PROXY -3 PING       (RESP3, HELLO) → timeout 6 s ✗
  redis-cli -p 16379  -3 PING       (direct)       → PONG in 8 ms ✓
  ```

  v0.12.15.1 widens the parser to cover RESP3 aggregates and scalars:

  | Type | Marker | Framing |
  |------|--------|---------|
  | Map | `%N` | 2N elements follow |
  | Set | `~N` | N elements follow |
  | Push | `>N` | N elements follow |
  | Attribute | `\|N` | 2N elements + a regular reply |
  | Verbatim string | `=N` | bulk-string framing |
  | Blob error | `!N` | bulk-string framing |
  | Null / boolean / double / big number | `_` `#` `,` `(` | single-line scalar |

  Maps and sets re-use the existing array recursion. Attribute
  additionally consumes the trailing real reply so callers see one
  logical value.

  New regression tests in `internal/proxy/redis_test.go`:
  - `TestRedisProxy_RESP3_HelloMap` — round-trips the customer's
    `%7` map (server / version / proto / id / mode / role / modules
    with a nested `*0`). Hangs on v0.12.15 binaries, passes on
    v0.12.15.1.
  - `TestRedisProxy_RESP3_Set` — `~3` SMEMBERS reply.
  - `TestRedisProxy_RESP3_Attribute` — `|1` attribute followed by
    `+OK`.
  - `TestRedisProxy_RESP2_Ping` — no-regression guard on the
    classic `+PONG` path.

### Note on the underlying design

This is the second protocol where structural read-and-forward has
bitten us in 48 hours (MySQL handshake → RESP3 framing). The
bidirectional `io.Copy` passthrough refactor flagged alongside
RFC-034 moves up the priority list — once handshake-aware framing
recognises end-of-handshake, the post-handshake path should be a
plain pump rather than a per-protocol parser.

## [0.12.15] - 2026-04-29

Hotfix on top of v0.12.14. Customer (inDrive Freight) verified that
v0.12.14 didn't unblock Finding H — both `caching_sha2_password` and
`mysql_native_password --default-auth` still hung. Independent
`mysql -P $PROXY_PORT` probe through the proxy reproduced it without
touching the SUT, ruling out spec or driver concerns.

### Fixed

- **MySQL handshake handles `caching_sha2_password` fast-auth-success.**
  v0.12.14's loop assumed strict client/server alternation after the
  initial handshake-response pair. That broke the fast-auth-success
  path (server already has the user in its auth cache), where the
  server emits **two server-side packets back-to-back** with no
  client packet between:

  ```
  S→C  AuthMoreData(0x01, 0x03 = "fast_auth_success")
  S→C  OK(0x00)
  ```

  v0.12.14 read the AuthMoreData, didn't recognize the `0x03` status,
  tried to read from the client, and deadlocked until the client's
  connect timeout fired. v0.12.15 peeks the **second byte** of every
  AuthMoreData packet — `0x03` (fast_auth_success) skips the client
  read and continues to the next server packet (the OK). Other
  AuthMoreData states (`0x04` perform_full_authentication, public-key
  payloads) and AuthSwitchRequest still expect a client reply.

  How Freight hit it: their `seed_db` Starlark hook polls MySQL via
  `db.mysql.exec(sql="SELECT 1", dsn=DB_DSN_POLL)` — `dsn=` overrides
  the proxy address, so seed connects directly to MySQL and populates
  the server's auth cache. By the time `truck-api` connected through
  the proxy, every connection took the fast-auth-success path that
  v0.12.14 deadlocked on. The same happened to a manual
  `mysql -P $PROXY_PORT` probe (any cached user → fast-auth path).

  New regression test: `TestMySQLProxy_Handshake_CachingSha2FastAuthSuccess`
  hangs on v0.12.14 binaries, passes on v0.12.15.

### Note on the underlying design

The protocol-aware turn-taking model in `internal/proxy/mysql.go` keeps
producing edge cases. v0.12.14 missed full-auth-but-cold-cache; v0.12.15
catches fast-auth-success. A bidirectional `io.Copy` refactor (with
out-of-band SQL parsing for rule matching) would close the whole class.
Filed as a follow-up — not in v0.12.15 scope.

## [0.12.14] - 2026-04-29

Hotfix on top of v0.12.13. Customer (inDrive Freight) confirmed the
v0.12.13 reuse-path fix landed cleanly, then surfaced **Finding H**:
the MySQL proxy deadlocks on `caching_sha2_password` full-auth (the
default for MySQL 8). Server greeting reaches the client; client's
auth response goes into the proxy and never reaches the upstream
MySQL backend; driver hangs 60s and the cell fails.

### Fixed

- **MySQL proxy handshake loops until OK / ERR.** Pre-v0.12.14
  `forwardHandshake` assumed a strict 3-packet exchange (server
  greeting, client auth, server OK). That's correct for
  `mysql_native_password` but wrong for `caching_sha2_password` —
  full-auth is 6+ alternating packets (`AuthMoreData` "perform full
  auth", client `request_pubkey`, server pubkey, client encrypted
  password, server OK). The proxy returned after packet 3, entered
  the command loop expecting `COM_QUERY`, and the auth state machine
  drifted off until the read deadline fired.

  v0.12.14 reads the first byte of every server-side packet, returns
  on `0x00` (OK) or `0xFF` (ERR), and continues alternating
  client→server / server→client through `AuthMoreData` (`0x01`) and
  `AuthSwitchRequest` (`0xFE`). Bounded at 16 rounds so a malformed
  peer can't stall the goroutine. Three regression tests cover
  native_password, caching_sha2 full-auth, and ERR termination.

### Changed

- **Step `summary` field cap raised 80 → 500 chars.** Drill-down's
  Summary row now reads the full statement for typical multi-statement
  DDL/DML — pre-v0.12.14 a `DELETE FROM \`order\`; DELETE FROM offer;
  DELETE FROM purchase; ...` reset hook was cut at the second
  statement. Lane tooltips line-clamp visually so the longer summary
  doesn't bloat the UI.

## [0.12.13] - 2026-04-28

Hotfix on top of v0.12.12. Customer's dbmatrix bundle made visible a
pre-existing bug (RFC-015 vintage) that v0.12.12's `proxy_active` event
couldn't help with: **container services without seccomp filters
weren't tracked in `rt.sessions`**, so `reuse=True` was silently
ignored for proxy-only Docker upstreams.

### Fixed

- **`reuse=True` now honoured for no-seccomp container services.**
  The no-seccomp branch of `startContainerService` populates
  `rt.sessions[svcName]` with a no-engine entry, mirroring the
  mock-service pattern. Without this, `stopServices` built its
  `reused` set by iterating `rt.sessions` (which didn't include
  no-seccomp containers) and destroyed the container every test —
  while leaving the proxy in `proxyMgr` pointing at a dead host
  port. Symptom in the v0.12.12 dbmatrix bundle: matrix cells
  emitted no `proxy_started` or `proxy_active` events for proxy-only
  DB upstreams, and host-binary SUTs failed healthcheck because the
  stale proxy couldn't reach the new container's auto-assigned host
  port.
- **Nil-session guard in `stopServices` reuse path.**
  `rs.session.ClearDynamicFaultRules()` now nil-checks `rs.session`
  before dereferencing — mock services and no-seccomp container
  services don't carry an `engine.Session`, so cleanup must not
  panic on them. Mirrors existing nil guards in `removeTrace` /
  `removeFaults`.

### Compatibility

No spec or API changes. Bundles produced by v0.12.12 and earlier
remain readable. Re-run a suite on v0.12.13 to benefit — the
`proxy_active` events that were supposed to fire in v0.12.12's
reuse path will actually fire now.

## [0.12.12] - 2026-04-27

Proxy-address surface for host-binary SUT + Docker upstream
([RFC-033](https://github.com/faultbox/Faultbox/issues/87)). Two layered
fixes, one P0 trace correctness issue and one P1 connectivity bug, both
surfaced by a customer running the recipe-based `mysql.deadlock()` /
`redis.timeout()` matrix against `truck-api` (host binary) connecting to a
Docker `db` (mysql:8) — 18/18 cells failed for these reasons, not for any
fault-injection-relevant reason.

### Added

- **`iface.proxy_addr` / `proxy_host` / `proxy_port`.** Late-bound
  interface-ref attributes that resolve to the host-side proxy listener at
  `buildEnv` time. Use them to wire host-binary SUTs through the
  fault-injection proxy without `rsplit()` games or guessing
  Docker-auto-mapped ports. The values survive string concat (e.g.
  `"tcp(" + db.main.proxy_addr + ")/appdb"`) and resolve to the right
  thing once proxies are running.
- **`proxy_active` event in the reuse path.** When `startServices` skips a
  service because its session was kept alive from a previous test
  (`reuse=True`), the runtime now re-emits one `proxy_active` event per
  running interface proxy. Per-cell trace partitions become
  self-describing — fault_matrix cell 5 looking at its own trace can see
  which proxies are wired up at cell start, instead of inferring "no
  proxy started" from a missing `proxy_started` event the renderer never
  saw because emission only fires on fresh starts.

### Fixed

- **Trace looked like proxy lifecycle was broken under reuse.** Before:
  cell 1 emits `proxy_started` for `db.mysql` (fresh start), cells 2..N
  show no proxy events because `startServices` skips `preStartProxies`
  for reused services. Customers reasonably concluded "the proxy didn't
  run in cell 2." Proxy was running fine — the trace was lying. Now
  `proxy_active` fires per cell with `mode: "reused"` so the per-cell
  partition tells the truth.
- **Host-binary SUT couldn't reach the proxy of a Docker upstream.**
  Documentation pointed users at `iface.internal_addr` for env wiring.
  For a Docker service `db.main.internal_addr` returns `"db:3306"` (the
  Docker DNS name, useless to host processes). The runtime's
  `buildEnv` substitution catches a literal `db:3306` substring in env
  values and rewrites it to the proxy addr — but customers commonly
  decompose with `internal_addr.rsplit(":")` to feed separate `MYSQL_HOST`
  / `MYSQL_PORT` env vars, which silently breaks the substitution. SUT
  ends up dialing an unroutable address; healthcheck times out.
  `proxy_addr` / `proxy_host` / `proxy_port` are the supported path:
  resolved at runtime, no decomposition tricks needed.

### Changed

- **`proxyTargetAddr(iface)` helper.** The four call sites that hardcoded
  `127.0.0.1:<port>` (preStartProxies, fault_scenario rule application,
  fault() builtin, fault_assumption proxy-rule loop) now share a single
  function. Behavior is unchanged today; future RFC-024 follow-ups (e.g.
  proxy-side container-network targeting) have one site to edit.

### Documentation

- New "Wiring SUTs to the proxy" section in [`docs/recipes.md`](docs/recipes.md)
  with the canonical host-binary-SUT-against-Docker-upstream pattern.
- `iface.proxy_addr` / `proxy_host` / `proxy_port` documented in
  [`docs/spec-language.md`](docs/spec-language.md) as the preferred path
  for host SUTs; `internal_addr` re-scoped to container-to-container.
- New troubleshooting entry "Host-binary SUT can't connect to a Docker
  DB upstream" in [`docs/troubleshooting.md`](docs/troubleshooting.md).
- Recipe headers for `mysql.star`, `redis.star`, `postgres.star` updated
  with the `proxy_addr` wiring pattern.

## [0.12.11] - 2026-04-26

### Changed

- **Compact fold-count labels.** Run-marker badges now display as
  `× 3.9k` / `× 86k` / `× 4M` instead of full numerals. The exact
  count remains in the badge's `title` tooltip so the precise
  value is one hover away. Decimals truncate rather than round, so
  a `× 3.9k` chip always represents ≥ 3900 events — never an
  overstatement.

## [0.12.10] - 2026-04-26

Spec-anchored event highlighting — the user's own step calls
(`api.http.post`, `kafka.publish`, `db.exec` from the test body)
visually pop out from background traffic so they read as familiar
landmarks against monitor / proxy / syscall noise.

### Added

- **Runtime tagging.** `executeStep` walks the Starlark call stack
  and sets `fields.spec = <test_name>` whenever the step originates
  from inside the currently-running test function. Helper functions
  written by the user (`def post_order(): api.http.post(...)`)
  still register because the test_* frame remains on the stack.
  Monitor evaluations, recipe internals, and background syscall
  paths fail the check by construction — they don't have a
  test_* frame above them.
- **Renderer highlight.** Markers with `fields.spec` get:
  - a warm gold ring (`#d4af37`) so the eye finds them in a busy
    lane;
  - a +50 severity bump so the slot picker prefers them over
    monitor/error events at the same rank;
  - **fold bypass** — spec-anchored events render individually
    regardless of cardinality (typical tests have 1–10, and
    folding them into a chip would defeat the highlight);
  - a ★ prefix in the lane balloon and event-log headline.

### Changed

- `severityScore` adds the +50 spec bonus across all event types,
  including happy-path step events that previously scored 0
  (so a `→ call · api.http.post /orders [200]` from the test body
  now beats a `← reply · monitor.poll` in the same slot).

### Compatibility

- Bundles produced before v0.12.10 don't carry `fields.spec`, so
  the highlight is a no-op there. Re-render an old bundle and
  nothing changes; re-run the suite on v0.12.10 to start
  capturing the tag.

## [0.12.9] - 2026-04-26

Three small UX polishes from a customer second-read of v0.12.8:

### Changed

- **Run-marker radius scales with log10(fold count).** Side-mounted
  count badges (`× 434`) used to overlap and become unreadable when
  several folded chips landed close together on the lane. Now the
  marker disc itself grows proportional to its fold count (base 8 px
  → ~26 px at 10k events) and the count text moves *inside* the
  disc. Magnitude is legible at-a-glance regardless of horizontal
  packing.
- **Drill-down section open state persists across pin changes.**
  Previously, expanding "All fields" / "Vector clock" on event A
  collapsed back to the compact default when the user clicked event
  B. The viewer now remembers per-section open state in a closure-
  scoped map, keyed by section title, so a user who's "All fields"
  oriented stays oriented across the whole drilling session.
- **Step summaries pair arrows with `call` / `reply` words.** A
  bare `→` / `←` was ambiguous to first-time readers — was the
  arrow the request direction or the response direction? Headlines
  now read `→ call · truck-api.get /orders` and
  `← reply · truck-api.get /orders [500]`. Arrows still scan
  faster once learned; the word is the disambiguator.

## [0.12.8] - 2026-04-26

Three follow-ups from a customer second-read of v0.12.7:

### Changed

- **Lane filter folds by key before budgeting.** v0.12.5 bucketed
  blindly into 50 visual slots, so a lane with 1737 identical
  `db.exec SELECT 1 ERR` events still rendered ~50 markers (one
  per slot) all visually indistinguishable. New two-pass:
  - Pass 1: group by `(target.method.summary)`. Groups with > 10
    members collapse to a single chip at the median rank, with
    `_runCount` / `_runMembers` carrying the rest.
  - Pass 2: if the post-fold list still exceeds `LANE_BUDGET=50`,
    fall back to slot bucketing on the post-fold events.
  Net effect on the customer's db lane: 1787 events → 1 chip
  (`× 1787`), no longer 50 identical red dots.
- **Causal hover lines restored** for the v0.12.7 lane routing.
  `findCausalAncestors` now keys on `laneFor()` not raw service —
  step events folded onto their target service's lane no longer
  count as same-lane and so cross-lane lines draw again.
  `drawCausalLines` resolves an ancestor seq to its containing
  chip via `_runMembers` (stashed on the marker DOM via
  `data-members`), so folded ancestors no longer silently miss
  the lookup.

### Added

- **Click-to-add Type filter chips** in the event log. The Type
  axis no longer pre-renders every option as a chip; a hint
  ("click a type cell below to filter") sits in the empty toolbar
  until the user clicks a Type cell in the table, at which point
  a removable chip appears with an inline X. Cuts the at-a-glance
  filter-bar weight; Service stays pre-rendered (small, useful
  set).

## [0.12.7] - 2026-04-25

Two fixes from a customer second-read of v0.12.6:

### Changed

- **Step events now lane on their target service.** Previously a
  `db.exec(...)` call landed on the `test` lane (because the runtime
  emits the event from the test driver), and the `db` lane only
  showed db's own lifecycle. Users expected to see DB activity on
  the db lane, not on the test driver lane buried among other
  cross-service interactions. New `laneFor()` helper routes
  `step_send` / `step_recv` to `ev.fields.target` when present,
  with a fallback to `ev.service` for older bundles and non-step
  events.
- **Event-log filter applies to the full event set.** v0.12.6 loaded
  the first 200 rows then hid non-matching ones — meaning a filter
  for a service whose events sat past row 200 returned no visible
  rows. Now the table maintains a `filteredEvents` view; toggling
  filters rebuilds the view and resets the page so the first 200
  *matching* events render. Caption updates to "Showing X of Y
  matching events (out of Z total)" when filters are active.
- **Service column display + filter axis follow lane routing.** The
  service cell now shows `laneFor(ev)`, so filtering by `truck-api`
  matches both truck-api's own lifecycle and the step events
  pointed at it.

## [0.12.6] - 2026-04-25

Three UX fixes from a customer read of the v0.12.5 report:

### Changed

- **Lane markers now color by severity, not just type.** A
  `step_recv` with `success=false`, an `error` field, or
  `status_code ≥ 500` paints with the fault palette (red);
  `status_code` 4xx paints amber. Without this every step rendered
  in the same yellow/warn colour and the eye couldn't find the DB
  invalid-connection or the truck-api 500 among a sea of `SELECT 1`
  markers.
- **Slot picker prefers severity over first-anchor.** `severityScore`
  ranks events: violation 100 → fault 90 → errored step 80 →
  5xx 75 → 4xx 60 → lifecycle 30 → 0. The slot's representative is
  the highest-scoring event in the bucket, so a slot containing 30
  step events plus one violation always shows the violation marker.
- **Recent trail ellipsizes long lines** with the full text in the
  native `title` tooltip. Stops a 2 KB SQL preview from pushing the
  drill-down off-screen; `cursor: help` signals the hover.

### Added

- **Two-axis event-log filter (Service + Type).** Replaces the
  v0.11 single-select-by-type chip bar. Both axes multi-select.
  Clicking a Type / Service cell in the table sets that axis to
  the cell's value (`step_recv` only on `truck-api`: two clicks).
  Active chips highlight; click again to deselect.

## [0.12.5] - 2026-04-25

Hard per-lane marker budget. Walks back the v0.12.2/3 consecutive-runs
dedup *and* the v0.12.4 anchor-window filter — neither gave a hard
upper bound on rendered DOM nodes, and on the customer's 86k-event
bundle the lane was still allocating 86k marker nodes (mostly invisible
because they crushed into the same pixel cluster). Performance lag was
the symptom; visual ambiguity ("these markers look identical but have
different sequences") was the second symptom.

### Changed

- **Lane filter rewritten as `applyLaneBudgetFilter`**
  ([internal/report/app.js](internal/report/app.js)). Per lane:
  - If `laneEvents.length ≤ LANE_BUDGET` (50): render every event,
    rank-positioned as before.
  - Otherwise: bucket into 50 visual *slots* in seq order. Each slot
    picks one representative — anchor first, else most-common
    fold-key head, else first event. Slot's `_runCount` /
    `_runMembers` carry the rest, so the existing drill-down path
    expands the cluster without code changes.
  - Hard guarantee: every lane renders ≤ 50 DOM markers regardless
    of input size. 86k → 50, 1M → 50, 50 → 50.
- **Lanes split happens before budgeting** so each service lane gets
  its own 50-marker budget. A 7-lane test renders ≤ 350 markers
  total (down from 86874 — a ~250× DOM-node reduction).
- Trace axis caption updated from "X repeat steps collapsed" to
  "X events folded into slots" — accurate for the new mechanic.

### Removed

- `applyAnchorWindowFilter`, `LANE_WINDOW`, `LANE_FOLD_KEEP_THRESHOLD`
  (v0.12.4 internals — replaced by the budget filter).

### Why slots over windows

The v0.12.4 anchor-window approach was right in spirit but had no
bound. When most step events are themselves anchors (which happens
on any test that hits failure paths — DB network errors, retry loops,
500s) every event ends up in a window and the filter degrades to
identity. Slot-based aggregation has a constant-bounded output by
construction; the trade-off is that a non-anchor event in a quiet
slot can be absorbed into its slot's representative, but the full
member list is still in `_runMembers` and the event-log table has
every original row.

## [0.12.4] - 2026-04-25

Two follow-ons from a customer second-read of the v0.12.3 report on
a noisy proxy-mode test (one HTTP POST, ~80k events).

### Added

- **`AssertionDetail.Context`** — when an `assert_*` builtin fails,
  the runtime snapshots the last 8 step events onto the assertion
  detail. The drill-down renders them as a "Recent" mini-trail
  (`← api.http.post /orders [500]`, etc.) so the user sees the
  *actual values* Starlark already folded away, without having to
  pin a lane marker and read the event-log fields. The lane balloon
  (hover tooltip) prefers the runtime-emitted `summary` field as
  its headline, and surfaces `status_code` / `error` inline for
  failed step events.

### Changed

- **Lane filter rewritten: anchor windows + global cardinality
  fold** ([applyAnchorWindowFilter](internal/report/app.js)).
  Replaces v0.12.2/v0.12.3's consecutive-runs dedup, which missed
  the common case of monitor `SELECT 1` polls *interleaved* with
  the test body (no two adjacent → no fold).
  - Anchor events (faults, violations, lifecycle, errored steps)
    plus a ±10-position window around each render per-event.
  - Outside the windows, events bucket globally by
    `(target, method, summary)`. Buckets ≤ 5 render every member;
    larger buckets fold into a single `× N` chip placed at the
    *median* rank of the bucket so the chip approximates *when*
    the activity peaked.
  - Failed step events (`success=false`, or carrying an `error`
    field) are anchors, so the customer's "DB network error"
    floods become anchors, not noise.
- Lane axis caption switched from "X repeat steps collapsed" to
  the more accurate "X events folded outside anchor windows".
- Lane tooltip headline now prefers the runtime-emitted `summary`
  field (`← api.http.post /orders [500]`) over the bare event
  type, with `status_code` / `error` inline for failed steps.

### Limitations

- Context is heuristic: it captures the *last* step events at
  fail time. Tests that assert about a value 5 steps back will
  see the most recent steps in Context, not the relevant one.
  An explicit `assert_that(actual, predicate, msg)` builtin or
  `actual=` kwarg on the existing builtins would be the crisp
  upgrade — deferred to v0.13 once we see how often the
  heuristic misses in practice.

## [0.12.3] - 2026-04-25

Three drill-down ergonomics fixes from a customer first-read of the
v0.12.2 report:

1. **Assertion drill-down lifts the original expression text out of
   the spec.** `assert_true(resp.status in [200, 201], "msg")` no
   longer shows only "Actual: False" — it shows the original
   `resp.status in [200, 201]` expression and a clickable
   `spec.star:42` location row alongside Expected/Actual.
2. **Lane marker click no longer scrolls the page.** Highlight on
   the matching event-log row stays; the disorienting page-jump
   does not.
3. **Lane dedup also keys on summary text.** A 1500-iteration
   `db.exec` loop with mixed SQL no longer flattens into a single
   chip — different SQL → different summary → different marker. A
   monitor's `SELECT 1` polls still collapse cleanly.

### Added

- `AssertionDetail.File` and `AssertionDetail.Line` carry the
  source location of the failing assert call. Populated from
  Starlark's `thread.CallFrame(1).Pos`. The renderer pulls the
  matching line out of the bundled spec, slices the assert call's
  first argument with paren/bracket/string-aware parsing, and
  surfaces both Expression and Location rows in the drill-down.
- New CSS for `.dd-assertion-link` so the Location row reads as a
  spec-anchor link, not a static label.

### Changed

- Lane dedup key (`laneRunKey`) now folds in the event's `summary`
  / `sql` / `query` / `path` / `command` / `topic` field — only
  events with *both* the same `(target, method)` *and* the same
  preview text collapse into a `× N` marker.
- `pinSelection` no longer calls `row.scrollIntoView()`. The
  highlighted row remains visible if the user scrolls; the click
  itself is now a pure no-jump operation.

## [0.12.2] - 2026-04-25

Step-event readability pass. The v0.12.1 swim-lane fix solved syscall
spam but left two follow-on problems Boris flagged on a regenerated
`test_order_feed` report: 81k step events still drowned the lane,
and a drill-down for `step_recv.db` showed only `target/method/
event_type/partition` — nothing about *what* the step did. v0.12.2
attacks both.

### Added

- **Enriched `step_send` / `step_recv` events.** The runtime now
  copies allow-listed kwargs (`sql`, `query`, `args`, `params`,
  `path`, `body`, `headers`, `table`, `key`, `value`, `topic`,
  `message`, `payload`, `db`, `command`) into the event field bag,
  truncated to 200 bytes per field. `step_recv` additionally carries
  `status_code`, `duration_ms`, `success`, an `error` (when
  `Success=false`), and any `Fields` the protocol plugin populates
  on `StepResult` (e.g. mongodb's `collection`/`documents`,
  cassandra/clickhouse's `rows`).
- **`summary` field on every step event.** A one-line
  protocol-aware preview shaped for the swim-lane tooltip and the
  drill-down primary-summary row — `→ db.exec INSERT INTO orders…`,
  `← api.get /orders/42  [200]`, `← api.get  ERR: context deadline
  exceeded`. Replaces the old `step_recv · seq 22754` headline that
  forced users to read the spec source to learn what was compared.
- **Lane dedup for repeated step pairs.** Consecutive step events
  with identical `(target, method)` collapse into a single canonical
  marker tagged with `_runCount` and `_runMembers`; the marker
  shows a `× N` count badge. The full per-event rows stay in the
  event-log table for forensic access. A 1500-iteration `db.exec`
  loop now renders one chip instead of 3000. The trace axis label
  surfaces the collapse: `seq A → B · N markers · M repeat steps
  collapsed · K syscalls in event log`.

### Changed

- The drill-down's "summary" row prefers `fields.summary` (new in
  v0.12.2) when present, falling back to a JS-built composition
  using the enriched fields. Old bundles (no `summary`,
  no enriched kwargs) still render — just without the new preview.

### Docs

- Added an FAQ entry to `docs/reports.md` explaining that bundles
  are frozen at run time and that re-rendering an old bundle
  through a newer binary cannot invent fields the runtime didn't
  emit. To benefit from v0.12.x additions (Expected/Actual,
  enriched step fields), the suite must be re-run on the new
  binary — not just re-rendered.

### Customer note

The v0.12.1 → v0.12.2 polish was driven by a customer who
re-rendered an existing v0.11.2 bundle through the v0.12.1
`faultbox report`. The visual fixes shipped, but the *event
content* couldn't change because the bundle was frozen. v0.12.2
makes that explicit (the new FAQ) and ensures that any run executed
on v0.12.2+ produces drill-downs rich enough to diagnose without
opening the spec.

## [0.12.1] - 2026-04-25

Drill-down + report-shape polish driven by Boris's first read of a
regenerated v0.12 report. Three pain points addressed in one patch:

1. **Services section now shows up for proxy-mode runs.** The
   "Observed coverage" section was hidden whenever `syscall_summary`
   was empty — exactly the case for container/proxy tests that
   capture step events but no syscalls. The section now derives
   services from the event log as a fallback, relabelling its
   activity column from "Syscalls" to "Events".
2. **Failed tests carry an Expected vs Actual block.** A failing
   `assert_eq` / `assert_true` now attaches a structured
   `AssertionDetail` (`{func, expected, actual, message}`) to the
   `TestResult`, surfaced at the top of the drill-down body. Users
   no longer need to open the spec source to learn what the test
   compared.
3. **Swim-lane stays legible at 80k+ events.** The lane renders
   only "interesting" events (faults, lifecycle, steps, violations,
   anything non-syscall) on a *rank-based* axis — uniform spacing
   instead of linear seq scaling. Syscalls remain in the event-log
   table below for forensic access. Without this, a run with
   `seq=1` and `seq=83549` anchors collapsed 99.9% of the timeline
   into invisible whitespace.

### Added

- `AssertionDetail` (`{func, expected, actual, message}`) on
  `TestResult` and trace-output rows; populated by `assert_eq`
  and `assert_true` on failure, rendered in the report drill-down
  as an "Assertion" block above the swim-lane.
- Event-log fallback for `Observed coverage`: services that
  emitted any event (proxy-mode `step_send` / `step_recv` /
  faults) are now listed even when no syscall events were
  captured. The activity column auto-relabels to "Events" /
  "Top event kinds" in this mode.

### Changed

- **Swim-lane axis is now rank-based.** Markers for the kept
  events get uniform horizontal spacing regardless of how many
  syscalls were emitted between them. Linear-seq positioning
  rendered usefully only when `maxSeq - minSeq` was small;
  production runs above ~10k events became unreadable.
- **Swim-lane filters syscalls out by default.** Lane markers are
  reserved for fault, lifecycle, step, and violation events; the
  syscall noise stays in the event-log table where filter chips
  already live. If a run produces only syscalls, the lane falls
  back to showing them so binary-mode tests still render.
- Trace axis label updated from "seq X / seq Y" to
  `seq A → B · N markers · M syscalls in event log` to make the
  filtering visible at a glance.

## [0.12.0] - 2026-04-25

The "23 MB report" release. The headline customer pain from the
inDrive Freight v0.11.1 report — that the HTML artifact was too
big to attach and laggy to render — is closed by a three-layer
report-architecture redesign (RFC-031). On a 120k-event simulated
run, the v0.11 baseline of ~10 MB shrinks to ~137 KB by default,
~75× smaller, with no loss of forensic value for the common case.
`--full-events` recovers everything when needed.

Plus six adjacent improvements driven by the same customer report:
panic-safe bundle flush, binary-digest pinning, actionable lock
drift output, the `grpc.retryable()` composite recipe, the
`internal/proxy/` test-coverage CI gate, and the canonical
"where Faultbox fits" positioning doc.

### Added

- **RFC-031 — Scalable HTML report architecture** ([#83](https://github.com/faultbox/Faultbox/issues/83))
  - **Phase 1**: payload inlined as gzip + URL-safe base64 in a
    `<script type="application/octet-stream">` tag and decompressed
    in-browser via `DecompressionStream` (Chrome 80+, Safari
    16.4+, Firefox 113+). New `--summary` flag drops the trace
    entirely (KB-sized, CI-friendly). Header carries a "size
    banner" telling readers the mode and inlined payload size.
  - **Phase 2**: drill-down event-log table renders in pages of
    200 rows with "Load next 200 (X remaining)" and "Show all"
    buttons. Filter chips re-apply across loaded pages.
  - **Phase 3**: events downsample at report-build time. Anchors
    (faults / violations / lifecycle / steps) always survive;
    first 50 + last 50 events per test survive; ±25 around each
    anchor survives; everything else dropped. New `--full-events`
    flag opts out for forensic deep-dives. Drill-down header
    shows "downsampled from X events" when applicable.
- **Panic-safe bundle flush** ([#76](https://github.com/faultbox/Faultbox/issues/76)) —
  per-test recover wraps `RunTest`, so a Go runtime panic inside
  a test becomes an `errored` row instead of taking the whole
  suite — and the `.fb` bundle — down with it. The first captured
  panic surfaces as `manifest.crash` so consumers know the run
  is partial. Customer-reported v0.11.1 panic in `applyFaults`
  would have produced a usable bundle under this fix.
- **Binary-digest pinning in `faultbox.lock`** ([#77](https://github.com/faultbox/Faultbox/issues/77)) —
  `faultbox lock` now hashes every binary-mode service's
  executable and records `sha256:<hex>` in `lock.binaries`
  alongside `lock.images`. CI gates close the supply-chain
  drift gap for teams that ship volume-mounted binaries (the
  inDrive Freight model). Schema unchanged — `Binaries` field
  was reserved in v0.10.
- **`grpc.retryable()` composite recipe** ([#79](https://github.com/faultbox/Faultbox/issues/79)) —
  one-line "flapping upstream" mix replacing three hand-composed
  status-code rules. Default 60% UNAVAILABLE / 25%
  DEADLINE_EXCEEDED / 15% ABORTED, weights and overall
  probability both overridable. Drive-by fix: `probability=`
  kwargs on every fault builtin now accept Float values
  (previously silently coerced to 0 via `starlark.AsString`).
- **`docs/positioning.md` + homepage four-layer matrix** ([#85](https://github.com/faultbox/Faultbox/issues/85)) —
  canonical "where Faultbox fits" doc covering complementarity
  with integration tests, load testers, and production chaos.
  3-minute read. Site homepage surfaces the four-layer
  capability matrix above the fold with deep links into the
  relevant tutorial chapters.
- **CI coverage gate for `internal/proxy/`** ([#84](https://github.com/faultbox/Faultbox/issues/84)) —
  `TestProxyPluginsHaveCoverage` fails the build if any
  `internal/proxy/*.go` source file ships without a sibling
  `_test.go`. Closes the process gap that let v0.11.1's gRPC
  passthrough corruption ship — `internal/proxy/grpc.go` had
  zero tests at the time. Eight existing untested plugins live
  in `coverageExemptions` pending backfill.

### Changed

- **`faultbox lock --check` actionable drift output** ([#82](https://github.com/faultbox/Faultbox/issues/82)) —
  output is now a per-row "locked vs current" table that names
  every drifted entry with both digests, instead of the prior
  category-summary view that forced a re-run to diagnose:
  ```
  drift detected (3 entries):
    image   mysql:8           locked sha256:abc…   current sha256:def…
    binary  /tmp/truck-api    locked sha256:111…   current sha256:222…
    binary  /tmp/upstream     locked sha256:333…   current <not found on disk>
  ```
- **Default `faultbox report` is now downsampled.** Existing CI
  pipelines that gate on report size will see dramatic shrink;
  pipelines relying on every event being present should add
  `--full-events`.

## [0.11.3] - 2026-04-25

### Changed

- **MySQL driver log noise suppressed** ([#80](https://github.com/faultbox/Faultbox/issues/80)) —
  during seed-poll retry loops, `go-sql-driver/mysql` emitted `[mysql]
  packets.go:58 unexpected EOF` for every connection attempt, drowning
  real signal. A filtering logger now drops known retry-noise
  substrings (unexpected EOF, invalid connection, bad connection,
  broken pipe, connection refused) while passing genuine errors
  through. Customer ask from inDrive Freight v0.11.1 feedback #12.

### Added

- **`CHANGELOG.md` + per-release pages on the site**
  ([#81](https://github.com/faultbox/Faultbox/issues/81)) — release
  notes previously lived only on GitHub Releases, which teams reported
  as an adoption blocker ("discovered features from `--help` rather
  than docs"). A root-level changelog mirrors the site.

## [0.11.2] - 2026-04-24

Hotfix for two P0 regressions reported by inDrive Freight against v0.11.1. Both
now have direct regression test coverage — zero before this release.

### Fixed

- **gRPC proxy no longer corrupts passthrough** —  rule_count=0 RPCs
  through an interface declared `protocol="grpc"` were rejected with
  `message is *[]uint8, want proto.Message`. The forwarding path used
  `grpc.ServerStream.RecvMsg` with `*[]byte` while the default proto
  codec rejected non-proto receivers. Fix: raw-bytes codec registered
  via `grpc.ForceCodec` + `grpc.ForceServerCodec`, plus a `forwardRPC`
  lifecycle that waits for both directions to finish so unary
  cardinality checks pass. Regression coverage at
  [internal/proxy/grpc_test.go](https://github.com/faultbox/Faultbox/blob/main/internal/proxy/grpc_test.go).
- **`fault_matrix` on mock targets no longer panics** — mock services
  register `runningSession{session: nil}`; `applyFaults` dereferenced
  it and crashed mid-suite, losing the bundle. Fix:
  `applyFaults`/`applyTrace`/`removeFaults` detect nil sessions and
  emit `fault_skipped_no_seccomp`. Belt-and-braces, all `Session.*`
  methods are nil-safe at the receiver too.

### Added

- **`--test` accepts glob and regex** — `--test='test_matrix_*'` for
  glob, `--test='~test_(matrix|smoke)_.*'` for regex. Exact match
  preserved.
- **`faultbox test` defaults to `./faultbox.star`** when no spec is
  supplied.
- **README capability matrix** — "What Faultbox injects" documents all
  four layers (syscall, protocol-request, protocol-response, mock)
  and "Where Faultbox fits" clarifies the relationship to integration
  tests, load tests, and prod chaos tooling.

## [0.11.1] - 2026-04-24

Completes RFC-027 (#67) and ships issue #75. Every `fault_matrix()` row now
lands in one of five buckets — rendered with a distinct colour in the HTML
report's matrix and tests table.

### Added

- **`expectation_violated` outcome (amber)** — scenario passed body
  asserts, but the `expect_success()` / `expect_error_within(ms)` /
  `expect_hang()` predicate rejected the result. Refinement of
  `failed`; legacy CI gates on `summary.failed` keep seeing the row.
- **`fault_bypassed` outcome (grey)** — opt in via
  `fault_matrix(require_faults_fire=True)`. Demotes passing rows whose
  installed faults never matched a syscall (the silent-green case
  where a service served from cache). Drill-down lists every
  unmatched rule.
- **Manifest additions** (additive, no `schema_version` bump):
  `tests[].outcome`, `tests[].expectation`, `tests[].bypassed_rules`,
  `summary.expectation_violated`, `summary.fault_bypassed`.
- **Report palette upgraded to 5 colours** with distinct icons
  (✓ ✗ ≠ ∅ !) and a header pill that breaks out the new outcomes.

## [0.11.0] - 2026-04-24

### Added

- **Interactive HTML reports** ([RFC-029](https://github.com/faultbox/Faultbox/issues/60)) —
  `faultbox report <bundle.fb>` builds a single self-contained HTML
  file from any `.fb` bundle (CSS, JS, and data all inlined, no
  network access required). Shareable by email, commit it to git,
  publish to a static host. Offline forever.
- **Hero stats** — matrix size, faults delivered, services observed,
  duration.
- **Attention list** — failed tests + warning diagnostics first, each
  with a copy-paste replay command.
- **Fault matrix grid** — scenarios × faults, click any cell for
  drill-down.
- **Swim-lane event trace viewer** — services as lanes, markers per
  syscall / fault / lifecycle / step / violation, hover tooltips,
  vector-clock causal overlays.
- **Event log table** — filter chips by event type, grouped expansion
  (Request / Response / Fault / System / Meta).
- **Reproducibility panel** — versions, image digests, replay command.
- **Spec viewer** — syntax-highlighted Starlark, collapsible per file.

## [0.10.1] - 2026-04-23

### Fixed

- **Assumption `ProxyRules` applied in `fault_scenario` and
  `fault_matrix`** — proxy-level faults declared in a named
  `fault_assumption` reached the proxy layer only when referenced
  directly. Now also applied via scenario/matrix composition.

### Added

- **testops corpus** — `redis_fault_basic`, `postgres_fault_basic`,
  `parallel_basic`, `nginx_container_basic`. Critical tier 100% green.

## [0.10.0] - 2026-04-23

Closes the third customer payment blocker (reproducibility). The bundle →
replay → report trio (v0.9.7 → v0.10.0 → v0.11.0) is two-thirds shipped.

### Added

- **`faultbox replay <bundle.fb>`** — re-execute any captured run
  end-to-end with the recorded seed. Opens the bundle (refuses on
  unknown `schema_version`), enforces same-major version compat
  (major drift refuses), extracts the `spec/` tree and re-invokes
  `faultbox test` with the recorded seed.
- **`faultbox lock` + `faultbox.lock`** ([RFC-030](https://github.com/faultbox/Faultbox/issues/69)) —
  pin every container image's content digest so two runs on different
  machines reach identical bytes. `faultbox lock --check` exits 2 on
  drift for CI gating. `FAULTBOX_LOCK_STRICT=1 faultbox test` makes a
  missing lock a hard error. Schema reserves fields for binary
  checksum and stdlib-hash pinning (Phase 2/3 of RFC-030).

## [0.9.9] - 2026-04-23

### Added

- **JWT/JWKS mock** ([`@faultbox/mocks/jwt.star`](https://github.com/faultbox/Faultbox/blob/main/recipes/mocks/jwt.star)) —
  auto-generated Ed25519 keypair at spec-load, publishes JWKS +
  OpenID configuration, `auth.sign(claims=...)` mints tokens. Compose
  with `fault()` to test JWKS outage / slow-JWKS / rejection paths.
- **Documentation overhaul** (~1500 lines, six new pages): JWT tutorial
  chapter, end-to-end Go microservice chapter, Starlark dialect
  reference, seccomp cheatsheet, troubleshooting playbook, CI on Linux
  guide with GitHub Actions + BuildKite templates.
- **Primitive index** in `spec-language.md` — every builtin one click
  away.

## [0.9.8] - 2026-04-23

Six small primitives addressing customer asks from the inDrive feedback
analysis — Group B + C3.

### Added

- **`load_file()` / `load_yaml()` / `load_json()`** ([RFC-026](https://github.com/faultbox/Faultbox/issues/66)) —
  spec-load-time file readers. Path resolution spec-relative.
  Network schemes refused. 50 MB size cap
  (`$FAULTBOX_LOAD_FILE_MAX_BYTES` to override).
  `$FAULTBOX_HERMETIC=1` rejects symlinks escaping the spec dir.
  Files captured into the `.fb` bundle's `spec/` automatically.
- **Expectation predicates** ([RFC-027](https://github.com/faultbox/Faultbox/issues/67)) —
  `expect_success()`, `expect_error_within(ms)`, `expect_hang()` for
  `fault_matrix(default_expect=, overrides=)`. Replaces hand-rolled
  outcome helpers.
- **gRPC status shorthands** — `grpc.unavailable()`,
  `grpc.deadline_exceeded()`, `grpc.permission_denied()`,
  `grpc.unauthenticated()`, `grpc.not_found()`,
  `grpc.resource_exhausted()`, plus `grpc_error()` builder.

## [0.9.7] - 2026-04-22

Closes the customer-reported reproducibility gap: *"we found bugs but nobody
could re-run them later."* Every `faultbox test` run now emits a single `.fb`
archive (tar.gz) — shareable by email, committable to git, uploadable as a CI
artifact.

### Added

- **`.fb` bundle format** ([RFC-025](https://github.com/faultbox/Faultbox/issues/59)) —
  always-on tar.gz containing `manifest.json`, `env.json`,
  `trace.json`, executable `replay.sh`, and `spec/` (user .star tree
  snapshot with transitive `load()`s). Opt-out via `--no-bundle`.
  Path override via `--bundle=<path>` or `$FAULTBOX_BUNDLE_DIR`.
- **`faultbox inspect <bundle.fb>`** — summary mode (header + file
  list), dump mode (pipe a single file to stdout), extract mode
  (unpack to a directory).
- **Terminal observability** — replay hint per failed test;
  zero-traffic summary at session end for any rule that matched no
  syscalls during its fault window.
- **Version compatibility gates** — unknown `manifest.schema_version`
  refuses (forward-compat safety); `faultbox_version` drift warns and
  proceeds; `faultbox replay` refuses major-version drift.

[Unreleased]: https://github.com/faultbox/Faultbox/compare/release-0.12.21...HEAD
[0.12.21]: https://github.com/faultbox/Faultbox/compare/release-0.12.20...release-0.12.21
[0.12.20]: https://github.com/faultbox/Faultbox/compare/release-0.12.19...release-0.12.20
[0.12.19]: https://github.com/faultbox/Faultbox/compare/release-0.12.18...release-0.12.19
[0.12.18]: https://github.com/faultbox/Faultbox/compare/release-0.12.17...release-0.12.18
[0.12.17]: https://github.com/faultbox/Faultbox/compare/release-0.12.16...release-0.12.17
[0.12.16]: https://github.com/faultbox/Faultbox/compare/release-0.12.15.2...release-0.12.16
[0.12.0]: https://github.com/faultbox/Faultbox/compare/release-0.11.3...release-0.12.0
[0.11.3]: https://github.com/faultbox/Faultbox/compare/release-0.11.2...release-0.11.3
[0.11.2]: https://github.com/faultbox/Faultbox/compare/release-0.11.1...release-0.11.2
[0.11.1]: https://github.com/faultbox/Faultbox/compare/release-0.11.0...release-0.11.1
[0.11.0]: https://github.com/faultbox/Faultbox/compare/release-0.10.1...release-0.11.0
[0.10.1]: https://github.com/faultbox/Faultbox/compare/release-0.10.0...release-0.10.1
[0.10.0]: https://github.com/faultbox/Faultbox/compare/release-0.9.9...release-0.10.0
[0.9.9]: https://github.com/faultbox/Faultbox/compare/release-0.9.8...release-0.9.9
[0.9.8]: https://github.com/faultbox/Faultbox/compare/release-0.9.7...release-0.9.8
[0.9.7]: https://github.com/faultbox/Faultbox/releases/tag/release-0.9.7
