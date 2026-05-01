# RFC-036: Remote Services — Faulting Dependencies in Customer k8s Dev Environments

- **Status:** Draft
- **Author:** Boris Glebov, Claude Opus 4.7
- **Created:** 2026-05-01
- **Target:** v0.13.0 (subject to scheduling)
- **Depends on:** RFC-017 (mock services), RFC-024 (proxy datapath)
- **Companion:** RFC-037 (Determinism for Remote Services) — split out from this
  RFC on 2026-05-01; remote services break the `.fb` bundle reproducibility
  contract from RFC-025, and the right shape of the fix is its own design
  problem worth discussing separately. This RFC ships the primitive; RFC-037
  decides how runs against it are made deterministic.
- **Customer motivation:** large customers run their development on shared k8s
  platforms ("DevPlatform"). Their service-under-test has 10+ runtime
  dependencies whose Docker images are not distributed to developers (built
  centrally, deployed via internal CI). Standing those dependencies up under
  Faultbox today requires either a `mock_service()` per dependency (high
  authoring cost, drift risk) or convincing platform owners to publish
  images (often impossible). This blocks the FB §6.3 #9 "DevPlatform
  integration" lane that v0.9.x explicitly deferred.

## Summary

Add a fourth source for `service()` alongside `binary=` / `image=` / `build=`:
a **`remote=`** kwarg that points at an externally-running endpoint (a k8s
Service hostname, a port-forwarded address, an IP). Faultbox does not launch
the service; instead it stands up its existing protocol proxy in front of
each declared interface, dials the remote upstream, and lets the SUT
exercise it through the proxy unchanged.

Everything that already runs on the proxy data path keeps working —
`response()`, `error()`, `slow()`, gRPC-method targeting, SQL matchers,
event emission, observability. Faults that require process control —
syscall-level `deny()` / `delay()` / `hold()`, `seed=` / `reset=` /
`reuse=` — are **rejected at spec-load** with a clear error directing the
user to the protocol layer or to `mock_service()`.

**Out of scope of this RFC:** how runs against a remote service are made
deterministic / reproducible. A remote pod is shared, drifts between runs,
and isn't captured by the `.fb` bundle (RFC-025) — that's a real
regression to Faultbox's reproducibility contract, but the design space
(record-and-replay, snapshotting, contract pinning, drift detection) is
big enough to warrant its own RFC. **See RFC-037 (Determinism for Remote
Services).** This RFC ships the primitive; runs against `remote=`
services are explicitly best-effort on reproducibility until RFC-037
lands.

## Motivation

### The customer problem

inDrive-shape teams keep running into the same wall. Their SUT
(`truck-api`) has dependencies — `geo-config`, `pricing`, `auth-server`,
`feature-flags`, `dispatch`, `payments`, ... — that are real services
maintained by other teams, deployed continuously to a shared k8s dev
namespace, and **not distributed as Docker images**. Reasons vary: builds
are 8 GB and live on internal artifact stores; private base images;
licensed binaries; or simply that nobody on the platform side considered
"a developer pulls this and runs it" a supported workflow.

Today the developer's only options are:

1. **`mock_service()` for each dependency.** Faithful for trivial
   stubs, drifts the moment the real service ships a new contract.
   Authoring cost grows linearly with the dep graph; many dependencies
   have 50+ endpoints.
2. **Skip the test.** The thing the customer most wanted to test —
   "what happens to truck-api when the *real* `geo-config` returns 503" —
   is the thing they can't express.
3. **Lobby the platform team.** Out of scope for an engineering tool.

### Why mocks aren't the answer alone

Mocks are great for *contract-stable* dependencies (JWKS endpoints,
feature flags, payment fixtures). They are wrong for *evolving* services
where the whole point is to test against the real protocol surface. The
customer's fault matrix is the cross-product of `truck-api` × `each real
peer in some failure mode`; we want them to express that at the
**source** layer (the real peer responds with 503), not the
**replication** layer (a stub I wrote yesterday returns 503).

### Why this is the right time

Two pieces in tree make this almost-free:

| Piece | What it gives us |
|---|---|
| `mock_service()` (RFC-017) | Proves the type system already accepts a service with interfaces but no image/binary. Fault rules, events, assertions all work without process attachment. |
| Pre-started proxy datapath (RFC-024) | Every proxy-capable interface already gets a local listener that forwards to "the upstream." Today the upstream is a container we launched. Repointing it is one `Dial`. |
| `seccomp=False` opt-out (builtins.go:280) | Establishes the precedent that some services run protocol faults only. Same validation pattern. |

What's missing is a single kwarg on `service()` and the validation
that protects users from configurations that can't possibly work.

## Design

### DSL — one new kwarg

```python
geo = service("geo-config",
    interface("public",   "http", 8080),
    interface("internal", "grpc", 9090),
    remote      = "geo-config.staging.svc.cluster.local",
    healthcheck = http("geo-config.staging.svc.cluster.local:8080/healthz"),
)

api = service("truck-api",
    interface("main", "http", 8000),
    image       = "truck-api:dev",
    depends_on  = [geo],
    env         = {"GEO_CONFIG_URL": "http://%s/" % geo.public.addr},
)
```

Behaviour at session start:

1. `service("geo-config", remote=...)` registers a `ServiceDef` with no
   launch source — same data shape as `mock_service()`.
2. The proxy manager pre-starts a listener on `127.0.0.1:NNN` per
   interface (RFC-024 path). The upstream addr is
   `<remote>:<iface.port>` for each interface.
3. `buildEnv` / `buildContainerEnv` substitute the proxy address into
   every consumer's env exactly as today — no consumer-side change.
4. The remote healthcheck (run on the host, not against the proxy) gates
   session start. Connection failure surfaces the configured `remote=`
   value plus a setup hint (see "Connectivity" below).
5. No seccomp filter installs. No container/binary lifecycle. No `seed`,
   `reset`, `reuse`.

### Kwarg surface

| Kwarg | On remote services | Notes |
|---|---|---|
| `remote=` | **Required exactly one of** binary/image/build/remote | Plain `host` (interface ports added) or `host:port` per-interface override (see below) |
| `interface(...)` | Required | One or more; same as today |
| `healthcheck=` | Required | We can't `wait_for_listening_socket()` on a remote pod we don't own; the spec must declare what "ready" means |
| `depends_on=` | Allowed | Topological sort sees `remote` services as roots that just need to be reachable |
| `env=` | Allowed | The values are `geo.public.addr` style — they resolve to the proxy address, same as today |
| `seccomp=` | Rejected | Already implied — there's no process to filter |
| `seed=` / `reset=` / `reuse=` | Rejected | We don't own the lifecycle |
| `volumes=` / `ports=` / `args=` / `binary=` / `image=` / `build=` | Rejected | All meaningless without a launch path |
| `observe=` | **Restricted** — see "Event sources" |

### Per-interface remote override

For services where interfaces live on different hosts (rare, but real —
e.g., separate sidecar exposing metrics):

```python
remote = remotes({
    "public":   "geo-config.staging.svc.cluster.local",            # uses iface.port
    "internal": "geo-config-grpc.staging.svc.cluster.local:9090",  # explicit
})
```

`remotes(dict)` returns a typed value the same way `op()` and
`json_response()` do — the runtime knows how to resolve per-interface
upstreams without us bending the string-typed common case.

### Spec-load validation

When a `fault()` rule targets a remote-service interface:

- **Protocol-layer rules** (`response`, `error`, `slow`, gRPC method,
  SQL matcher, body match, etc.) — **accepted**, dispatched to the proxy
  identically to a local container.
- **Syscall-layer rules** (anything that needs a seccomp filter — `write
  = deny()`, `connect = delay()`, named-op `hold()`, etc.) — **rejected
  at spec load** with:

  ```
  fault.geo-config.public.write: service "geo-config" is remote
    (remote="geo-config.staging.svc.cluster.local"); syscall-level faults
    are not available on remote services. Use a protocol fault
    (response=, error=, slow=) at the interface layer, or replace
    `remote=` with `mock_service()` if you need full control.
  ```

The check lives where rules are bound to interfaces (today's
`registerFault`/`installFilter` boundary). Same place we already error
on a fault targeting a non-existent interface.

`seccomp=False` becomes a no-op on remote services (already implied).

### Proxy lifecycle change

`internal/star/runtime.go::startServices` learns one branch:

```go
if svc.IsRemote() {
    if err := healthcheckRemote(svc); err != nil { return wrapHint(err) }
    proxyMgr.PreStartForRemote(svcName, svc)
    return nil   // skip launch, skip seccomp, skip seed/reset
}
```

`proxy.Manager.PreStartForRemote(svc, iface)` is the same code path as
the existing `EnsureProxy(svc, iface)` with one difference: the upstream
addr is `iface.RemoteHost:iface.Port` instead of the launched container's
ephemeral port. No protocol plugin changes.

A `service_started` event is emitted with `kind="remote"` and the
upstream addr in the payload — visible in the `.fb` bundle so a reader
can tell at a glance which services were proxied to a real pod.

### Connectivity — Faultbox stays cluster-agnostic

The `remote=` value is a plain hostname. Whether that hostname resolves
is the user's responsibility. We document the supported workflows in
order of preference:

1. **Telepresence connect** (recommended). User runs `telepresence
   connect` on their workstation; cluster Service DNS resolves on the
   host; Faultbox's proxy dials normally. No changes to Faultbox.
2. **In-cluster execution.** If the customer runs `faultbox test` in a
   pod inside the cluster (CI-on-cluster, dev container), Service DNS
   works natively.
3. **kubectl port-forward / kubefwd.** Lower-overhead alternative for a
   handful of dependencies. User points `remote=` at the local forwarded
   address.
4. **VPN.** Same as Telepresence connect with a different mechanism.

Healthcheck failure on session start emits a hint:

```
healthcheck failed: dial tcp geo-config.staging.svc.cluster.local:8080:
  no such host

Faultbox does not manage cluster connectivity. Verify one of:
  - `telepresence connect` is running and the namespace is in scope
  - `kubectl port-forward` covers this service
  - You are running faultbox from inside the target cluster
```

We considered shipping `telepresence connect` orchestration inside
Faultbox; rejected — adds a runtime dependency, conflicts with users
who already have a connection up, and ties our binary to Telepresence's
release cadence. The hint is enough.

### Optional stdlib helper

For ergonomics, ship `@faultbox/discovery/k8s.star` (RFC-019
distribution pattern) wrapping the standard k8s DNS shape:

```python
load("@faultbox/discovery/k8s.star", "k8s")

geo = service("geo-config",
    interface("public", "http", 8080),
    remote      = k8s.service("geo-config", namespace = "staging"),
    healthcheck = http(k8s.service("geo-config", namespace = "staging") + ":8080/healthz"),
)
```

`k8s.service(name, namespace)` returns
`"<name>.<namespace>.svc.cluster.local"`. Pure string sugar — no
runtime k8s client, no kubeconfig parsing in Faultbox itself. A future
`k8s.from_kubeconfig()` resolver could discover ports from `Service`
specs; not in v1.

### Event sources on remote services

`observe = [stdout(), tail(...)]` are **rejected** — we have no process.
`observe = [topic(...), poll(...)]` are **conditionally allowed**: a
Kafka topic source is meaningful regardless of who runs the broker; an
HTTP poll observation works against any URL. The runtime gates per
event-source kind. Same validation place as the fault rule check.

The proxy itself emits the same protocol events for remote services as
for local ones — request/response pairs land in the event log, monitors
and assertions don't care that the upstream is a remote pod.

### Composition with other primitives

- **`mock_service()` ↔ `remote=`** are siblings. A team can author one
  spec that uses `remote=` against staging in CI and swaps to
  `mock_service()` for offline runs (or vice versa). RFC-002's
  `domain()` primitive is the natural place for this swap — a domain
  exposes a contract; whether it's served by a mock, a real container,
  or a remote pod is implementation.
- **`fault_assumption()` rules** with `target = geo.public` work
  unchanged — the assumption doesn't know or care that `geo` is remote.
  Matrix runs are unchanged.
- **`expect_*()` and `events().where(...)`** see the same proxy events.

## Reproducibility — deferred to RFC-037

Remote services break the `.fb` bundle reproducibility contract from
RFC-025: a remote pod is shared, drifts between runs, and isn't
captured in the bundle today. That's a real regression and we need to
fix it — but the design space (record-and-replay, contract pinning,
schema snapshotting, drift detection, sensitive-data handling) is big
enough that it deserves its own RFC and shouldn't gate landing the
primitive. **See RFC-037.**

For v1 of this RFC, runs against `remote=` services are explicitly
best-effort on reproducibility:

- The bundle records *that* a remote was used (in `env.json` —
  `remotes: [{svc, iface, host, resolved_at}]`) so replay attempts can
  detect "this run used a remote and we can't reconstruct it offline".
- `faultbox replay <bundle>` against a bundle whose `env.json` declares
  remotes prints a clear warning: *"this run used N remote services;
  replay will attempt to re-dial them; for offline replay see
  RFC-037."*
- Customers who care about reproducibility can already swap `remote=`
  for `mock_service()` and check that into git — the same primitive
  surface, just with frozen contracts.

The proxy hook surface for "every interaction with a remote upstream"
is shipped as part of this RFC (it's the same surface RFC-024 already
exposes for protocol-rule matching). RFC-037 will consume that surface;
no further plumbing is needed in this RFC to keep RFC-037's options
open.

## Non-goals

- **Faultbox does not run a k8s control plane.** No CRDs, no operator,
  no in-cluster install. The cluster is somebody else's. v1.x at
  earliest.
- **Faultbox does not orchestrate Telepresence.** Documented as the
  recommended setup, not invoked by us.
- **Faultbox does not intercept inbound traffic to remote pods.**
  This is `telepresence intercept` territory and a different shape
  (modifying the remote cluster). Out of scope.
- **No mirrord-style pod injection.** Inverts the model — we want the
  SUT to remain under our seccomp control, not be teleported into a
  remote netns.
- **No deterministic replay of remote interactions.** Out of scope for
  this RFC; see RFC-037.

## Alternatives considered

1. **Make `remote=` an interface-level kwarg.** Rejected: forces every
   interface to repeat the host string; conceptually the *service* is
   remote, the *interfaces* are just how you reach it. The
   per-interface override (`remotes({...})`) covers the rare split case.
2. **New top-level `remote_service()` builtin.** Rejected: redundant
   with `mock_service()` already existing as a sibling of `service()`.
   A third sibling adds surface area without expressive gain. Keeping
   `service(remote=...)` mirrors the launch-source kwargs (`binary` /
   `image` / `build`) and reuses every downstream concept.
3. **Build `mirrord`-style network-namespace injection.** Rejected:
   requires kernel work on the customer's hosts, conflicts with our
   seccomp story, and doesn't help the central problem (the customer
   still can't fault `geo-config`'s responses).
4. **Build a Faultbox-native cluster runner.** Rejected for v1; this is
   the "hosted runner" deferred to 1.x. Worth revisiting once the
   `remote=` workflow is exercised by real users — the natural way a
   hosted runner offers value is by removing the "you set up
   Telepresence" prerequisite.
## Phasing

| Phase | Scope | Target |
|---|---|---|
| **v1** | `service(remote=...)` plumbing; spec-load validation; healthcheck; per-interface `remotes({...})`; `@faultbox/discovery/k8s.star` helper; `env.json` records remotes used. | v0.13.0 |
| **v2** | Replay-warning UX when a bundle declaring remotes is replayed; report-side annotation that flags "this run used remote services" at the top of the HTML report. | v0.13.x |
| **Determinism** | See RFC-037. | TBD |

## Regression tests

Required to land alongside the v1 plumbing — anything in this list missing on
the merge candidate is a release blocker. The goal is to pin every behaviour
the RFC promises so future refactors (and the eventual RFC-037 changes) can
move fast without re-litigating them.

### Spec-load validation (Starlark layer)

- `TestRemote_RejectsBinaryAndRemote` — `service(binary=..., remote=...)`
  errors at load time naming both kwargs.
- `TestRemote_RejectsImageAndRemote`, `TestRemote_RejectsBuildAndRemote` —
  same for the other two launch sources.
- `TestRemote_RequiresHealthcheck` — `service(remote=...)` without
  `healthcheck=` errors with the documented message.
- `TestRemote_RejectsSeedResetReuse` — each of `seed=` / `reset=` / `reuse=`
  on a remote service errors and names the offending kwarg.
- `TestRemote_RejectsVolumesPortsArgsSeccomp` — table-driven test asserting
  every meaningless-on-remote kwarg fails at load with a stable message.
- `TestRemote_AcceptsTopicAndPollObserve` — `observe=[topic(...)]` /
  `[poll(...)]` accepted; `[stdout()]` / `[tail(...)]` rejected.
- `TestRemote_PerInterfaceRemotesValidates` — `remotes({...})` keys must
  match declared interface names; mismatch errors at load.
- `TestRemote_FaultRule_RejectsSyscallTarget` — registering a syscall fault
  against a remote interface errors with the documented multi-line hint
  pointing to `response()` / `error()` / `slow()` / `mock_service()`.
- `TestRemote_FaultRule_AcceptsProtocolTarget` — `response()`, `error()`,
  `slow()`, gRPC-method, SQL-matcher, body-matcher all install cleanly.

### Runtime / proxy lifecycle

- `TestRemote_NoSeccompFilterInstalled` — verify no BPF filter is built
  for any remote service (assert `seccomp.Manager` count == count of
  non-remote services with at least one syscall fault rule).
- `TestRemote_NoLaunchAttempted` — Docker mock asserts `ContainerCreate`
  / binary fork are never called for a remote service.
- `TestRemote_PreStartProxyDialsRemote` — `proxy.Manager.PreStartForRemote`
  binds a local listener; first connection through it dials
  `remote:iface.port` (verified against an httptest.Server stand-in for
  the "remote pod").
- `TestRemote_HealthcheckGatesStartup` — unreachable `remote=` aborts
  session start with the documented hint string.
- `TestRemote_ServiceStartedEvent_KindRemote` — emitted event's `kind`
  field is `"remote"` and payload contains the upstream addr.
- `TestRemote_BuildEnvSubstitutesProxyAddr` — consumer env values
  containing `<remote>:<port>` get rewritten to the proxy addr (binary
  mode and container mode, both via `consumerMode` parameter from
  RFC-024 v0.9.6).
- `TestRemote_BundleEnvJSONRecordsRemotes` — `env.json` after a run
  contains a `remotes: [{svc, iface, host, resolved_at}]` array shape
  matching the documented schema.

### End-to-end (golden / integration)

Add to `internal/star/runtime_test.go` and `cmd/faultbox/`:

- `TestE2E_RemoteHTTP_PassThrough` — fake HTTP server stands in for the
  remote pod; SUT reaches it via the proxy; bundle records the
  interaction shape; trace events match.
- `TestE2E_RemoteHTTP_FaultRewrite` — same setup with
  `error(path="/v1/regions/**", status=503)`; SUT sees 503; remote
  fake server logs only the request, not a response (proxy short-circuits).
- `TestE2E_RemoteGRPC_FaultRewrite` — same shape for gRPC method-level
  targeting.
- `TestE2E_RemoteUnreachable_StartupHint` — `remote=` points at
  `127.0.0.1:1` (RST); session-start error contains the
  Telepresence/port-forward/in-cluster hint block verbatim.
- `TestE2E_RemoteMidRunDeath` — fake server dies mid-test; verifies the
  Open-Question-3 lean-(b) behaviour (synthetic protocol error reaches
  oracle).
- `TestE2E_RemoteVsLocal_ParityFaultMatrix` — same fault matrix
  authored against a local container vs a fake-remote endpoint produces
  bit-equivalent fault outcomes (modulo timing); locks in the "swap one
  keyword" success criterion.
- `TestE2E_ReplayWarning_RemoteBundle` — `faultbox replay` against a
  bundle whose `env.json` declares remotes prints the documented
  warning (regex-matched against stderr).

### Stdlib helper

- `TestK8sDiscovery_ServiceHelper` — `k8s.service("name", namespace="ns")`
  returns `"name.ns.svc.cluster.local"`; rejects empty name/namespace
  with a clear error.

### Documentation regression

- `TestDocs_SpecLanguageRemoteSection` — `docs/spec-language.md`
  contains a "Remote services" subsection (string-grep gate so we don't
  ship the feature without docs).
- `TestDocs_TutorialRemoteChapter` — equivalent gate on
  `docs/tutorial/`.
- Audit `docs/feature-manifest.md` — `remote=` row appears with
  greenlight tier and links to the test files above (the manifest is
  already a release-gate per `/release` skill — RFC-036 entries must
  exist before tagging).

### Pre-release sanity

- `make test` clean on linux/arm64 + linux/amd64.
- `make demo-container` covers a `remote=` flow against a side-launched
  fake (no Lima-only paths; fake-remote runs as a sibling container so
  the regression survives without Telepresence/cluster access).
- `go vet ./...` clean — including new `internal/proxy` and
  `internal/star` files.
- `.fb` bundle written by a remote-using run round-trips through
  `faultbox inspect` cleanly (no schema-version mismatch warnings).

### What this catches

The list is sized to lock down every behaviour the RFC promises: the
DSL surface (which kwargs error / which work), the runtime contract
(no launch, no seccomp, healthcheck-gated start), the data path
(proxy actually forwards, env actually rewrites), the artifact
contract (`env.json` shape, replay warning), and the documentation
gate. It deliberately does *not* cover RFC-037 territory (recording
correctness, replay determinism) — those tests land with that RFC.


## Open questions

1. **Healthcheck mandatoriness.** Is requiring `healthcheck=` on remote services a usability tax, or the right pressure on users to declare ready-criteria? Lean toward required — silent failures against an unreachable remote are worse than a noisy spec error.
2. **`remotes({...})` syntax vs flat `remote=` host:port string.** For services where every interface lives on the same host, `remote = "geo.staging:0"` (port 0 = use interface ports) is shorter than `remotes({...})`. Worth a sugar pass before locking the typed form.
3. **Connection-failure during a run** (vs at startup): the remote pod stays healthchecked at start but goes away mid-test. Do we (a) treat as an unhandled error, (b) emit a synthetic protocol error and let oracles handle it, or (c) abort the run? Lean (b) — matches what would happen with a real container that dies mid-test.
4. **Determinism story** — entirely RFC-037's problem; flagging here so reviewers know to tag that RFC for the substantive determinism discussion rather than this one.

## Success criteria

- A customer can convert a `mock_service()` to `service(remote=...)` and
  back by changing one keyword — no other spec edits.
- `fault(geo.public, response(status=503), run=scenario)` fires against
  a real remote pod with the same UX as against a local container.
- Spec-load errors for unsupported configurations (syscall faults,
  `seed=`, `reset=`) name the offending kwarg and suggest the right
  alternative — measurable: zero "I tried X, it silently did nothing"
  customer reports in the first month.
- The end-to-end customer story — *truck-api locally, geo-config /
  pricing / auth-server in dev cluster, fault matrix runs in CI* —
  works in ≤30 lines of spec and one `telepresence connect` command in
  the docs. (The "every failure is reproducible from the bundle alone"
  half of the original goal moves to RFC-037.)

## Appendix: end-to-end example

```python
load("@faultbox/discovery/k8s.star", "k8s")

# Three remote dependencies in the dev cluster.
geo = service("geo-config",
    interface("public", "http", 8080),
    remote      = k8s.service("geo-config", namespace = "dev"),
    healthcheck = http(k8s.service("geo-config", namespace = "dev") + ":8080/healthz"),
)

auth = service("auth-server",
    interface("public", "grpc", 50051),
    remote      = k8s.service("auth-server", namespace = "dev"),
    healthcheck = tcp(k8s.service("auth-server", namespace = "dev") + ":50051"),
)

flags = service("feature-flags",
    interface("public", "http", 8080),
    remote      = k8s.service("feature-flags", namespace = "dev"),
    healthcheck = http(k8s.service("feature-flags", namespace = "dev") + ":8080/healthz"),
)

# SUT runs locally as a container.
api = service("truck-api",
    interface("main", "http", 8000),
    image       = "truck-api:dev",
    depends_on  = [geo, auth, flags],
    env         = {
        "GEO_CONFIG_URL":   "http://%s/" % geo.public.addr,
        "AUTH_SERVER_ADDR": auth.public.addr,
        "FEATURE_FLAGS_URL":"http://%s/" % flags.public.addr,
    },
)

# Fault assumptions cross the cluster boundary unchanged.
geo_unavailable = fault_assumption("geo_unavailable",
    target = geo.public,
    rules  = [error(path = "/v1/regions/**", status = 503)],
)

auth_slow = fault_assumption("auth_slow",
    target = auth.public,
    rules  = [slow(method = "/auth.v1.Auth/Verify", delay = "2s")],
)

# Syscall-level faults on remotes would error at spec load:
#   fault_assumption("net_blip", target = geo.public,
#                    rules = [op("write", deny("EIO"))])
#   -> spec-load error: "service 'geo-config' is remote ..."

def request_pricing():
    resp = api.main.get(path = "/quote?from=A&to=B")
    return resp.status

fault_matrix(
    scenarios = [scenario("quote", run = request_pricing)],
    faults    = [geo_unavailable, auth_slow],
    expect    = expect_error_within("5s"),
)
```

Run flow:

```sh
$ telepresence connect
$ faultbox test truck-api.star
... runs against real geo/auth/flags ...
=> run-2026-05-01T10-14-22-1.fb

# Replay against the same cluster (deterministic offline replay → RFC-037):
$ faultbox replay run-2026-05-01T10-14-22-1.fb
WARNING: this run used 3 remote services (geo-config, auth-server,
  feature-flags); replay will re-dial them. Offline replay is tracked
  in RFC-037.
```
