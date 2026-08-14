# Feature Manifest

The authoritative list of what Faultbox does, grouped by support tier,
with the confidence mechanism gating each row. Purpose: turn
"does it work?" into a binary query with an owner and a signal.

Every Critical or Supported feature must have a row. A feature without
a row is not claimed as supported. A feature whose confidence mechanism
is "manual" is a release risk — either invest automation or reclassify
the tier.

Tiers reflect how much users rely on a feature, not how hard it was to
build. A complex feature can be Experimental if it's auxiliary; a
simple one can be Critical if the primary workflow depends on it.

---

## Tiers

| Tier | Meaning | Required confidence | Release gate |
|---|---|---|---|
| **1 — Critical** | Core workflow. Faultbox cannot do its primary job if this breaks. Documented prominently in the README and tutorial. | Golden-trace regression + integration test against the real dependency (pinned image digest). Green on every PR. | Hard gate — no release if any Critical row is red. |
| **2 — Supported** | Material feature, documented, actively maintained. Users rely on it, but workarounds exist if it breaks temporarily. | Golden-trace on a mock variant **or** thorough Go unit coverage on the critical path. Green on every PR. | Soft gate — reds block the specific feature area, not the release. |
| **3 — Experimental** | Works but evolving, niche, or auxiliary. May change between releases without notice. | `go test ./...` passes + release-time manual smoke on a checklist. | Advisory — reds are follow-up tickets, not blockers. |

---

## Manifest

Status legend: **🟢 green** (CI signal proves it), **🟡 partial** (some mechanism, gaps known), **🔴 red** (no mechanism, manual verification only), **⚪ n/a**.

### Core engine — Critical (proposed)

| Feature | Tier | Mechanism | Status | Notes |
|---|---|---|---|---|
| `service()` binary mode (fork+exec) | 1 | testops corpus (poc_example, poc_demo) | 🟢 | Gated in CI on amd64 ubuntu-latest |
| `service()` container mode (Docker) | 1 | testops corpus (nginx_container_basic) | 🟢 | Container lifecycle + proxy-level HTTP fault injection against real nginx:1.27-alpine; ubuntu-latest provides Docker natively |
| `fault(deny)` on syscalls | 1 | testops corpus (poc_example test_api_cannot_reach_db; poc_demo test_wal_fsync_failure, test_disk_full) | 🟢 | |
| `fault(delay)` on syscalls | 1 | testops corpus (poc_example test_db_slow; poc_demo test_inventory_slow) | 🟢 | |
| `parallel()` concurrent scenarios + hold/release scheduler | 1 | testops corpus (parallel_basic) | 🟢 | Hold is an internal scheduler primitive exercised by `parallel()` setup/teardown; `--explore=all/sample` adds interleaving enumeration on top |
| `assert_eventually` temporal | 1 | testops corpus (poc_demo test_happy_path on openat) | 🟢 | |
| `assert_never` temporal | 1 | testops corpus (poc_demo test_inventory_unreachable) | 🟢 | |
| `--seed` deterministic replay | 1 | testops harness itself asserts identical traces across 5 runs | 🟢 | Already proven by corpus entries |
| `depends_on` + healthcheck ordering | 1 | testops corpus covers transitively | 🟢 | |
| Starlark spec language | 1 | Parser unit tests + corpus exercises | 🟢 | |
| `fault_assumption` / `fault_scenario` / `fault_matrix` | 1 | testops corpus (fault_matrix_basic) | 🟢 | Protocol-level proxy rules now propagate through fault_scenario (bug fixed in same PR as corpus seed) |

### Protocol-aware faults — Critical (proposed)

Protocol-level fault proxy rewrites wire-level responses. Critical because this is the main differentiator over raw seccomp.

| Feature | Tier | Mechanism | Status | Notes |
|---|---|---|---|---|
| HTTP proxy faults | 1 | `internal/proxy/http_test.go` + corpus (fault_matrix_basic) | 🟢 | Path-matched error rules via `error(path=, status=, message=)` |
| Postgres proxy faults | 1 | `internal/proxy/postgres_test.go` + `postgres_auth_test.go` + corpus (postgres_fault_basic) | 🟢 | SQL-matched error rule against real postgres:16-alpine. This row previously claimed "auth bypassed because the proxy intercepts pre-backend" — that was wrong. The proxy must relay the startup exchange in **both** directions; relaying server→client only deadlocked SCRAM-SHA-256 (the postgres:14+ default), MD5 and cleartext. Fixed v0.16.0. |
| Redis proxy faults | 1 | `internal/proxy/redis_test.go` + corpus (redis_fault_basic) | 🟢 | Key-pattern matched error rules via stdlib recipes (oom/loading/readonly) |
| MySQL proxy faults | 2 | unit + `sqlmatch` canonicalizer | 🟢 | Strong unit coverage. The proxy's `forwardHandshake` handles `caching_sha2_password` (MySQL 8 default) including the fast-auth-success path. |
| Protocol step clients reach the real server | 1 | `internal/protocol/{addr,mysql_dsn,postgres_connstr}_test.go` + `internal/star/{ready_credentials,kwargs}_test.go` + `internal/proxy/{mysql_resultset,nats_framing,postgres_auth}_test.go` + `poc/protocol-audit/` against real containers | 🟡 | **10 of 13 step protocols verified end-to-end** as of v0.16.1: postgres, mysql, redis (±password), mongodb, clickhouse, nats, cassandra via `poc/protocol-audit/`; kafka, http, tcp via the existing `poc/` specs; **http2, udp and grpc by unit coverage only — no server spec exists for them, which is the remaining gap this row tracks.** Nine bugs shipped behind this row before anything asserted on a step result — see the v0.16.1 changelog. 🟡 not 🟢 because the corpus is run manually: no CI gate, since it needs Docker plus ~4 minutes of container starts. |
| Other protocols (HTTP2, UDP, Mongo, Cassandra, ClickHouse, Kafka, NATS, gRPC) | 2 | unit tests | 🟢 | 13 protocols, all have parse/proxy unit tests |
| TLS-aware proxy (RFC-038) | 2 | `internal/proxy/{http,grpc,kafka,redis,tcp}_tls_test.go` + Phase 1/2 unit tests | 🟡 | 6 of 14 plugins migrated (http/http2/gRPC/Kafka/Redis/TCP). Postgres/MySQL/Mongo/Cassandra/ClickHouse/memcached/NATS/AMQP deferred to [RFC-039](https://github.com/faultbox/Faultbox/issues/106); UDP has no TLS story. Declarations against unmigrated plugins emit `proxy_tls_pending` event. |

### Spec-language surface — Supported (proposed)

| Feature | Tier | Mechanism | Status | Notes |
|---|---|---|---|---|
| `mock_service()` + HTTP routes | 2 | testops corpus (http_basic, mock_demo) | 🟢 | |
| `mock_service()` reachable from a containerized SUT | 2 | `internal/star/mock_reachability_test.go` (bind + env substitution, faulted and unfaulted, plus the binary-consumer case) | 🟢 | Fixed in v0.18.0 (F-2). Mocks bind via `FAULTBOX_PROXY_BIND` and resolve to `host.docker.internal` for container consumers; exempt from the RFC-035 fault gate, since a mock has no DNS name to fall back on. Corpus still lacks an end-to-end container+mock spec |
| Seccomp supervisor survives dropped notifications | 1 | `internal/engine/launch_linux_test.go` (errno classification, ENOENT-is-never-a-closed-fd, supervisor-failure reporting) + `poc/pool-sut/` (24-connection SUT under filter) | 🟢 | Fixed in v0.18.0 (F-1/F-4). ENOENT from `NOTIF_RECV` is a per-notification transient, not a closed listener. A loop that ends while its target lives now fails the test loudly instead of leaving every intercepted syscall blocked forever. The poc spec is a liveness gate, not a deterministic reproduction of the race |
| Filter installed for multi-line / spaced `fault()` | 1 | `internal/star/spec_statements.go` + scan behaviour verified in Lima (`rule_count`/`seccomp` on both spellings) | 🟡 | Fixed in v0.18.0. Still a source heuristic: a fault built through a variable or a helper function installs no filter, and `FAULT_NOT_FIRED` is the only backstop. Needs an AST-based scan to close properly |
| Redis mock (miniredis) | 2 | testops corpus (redis_basic, mock_demo) | 🟢 | |
| Kafka mock (kfake) | 2 | testops corpus (kafka_basic, mock_demo) | 🟢 | |
| MongoDB mock | 2 | testops corpus (mongo_basic, mock_demo) | 🟢 | |
| gRPC mock | 2 | included in mock_demo | 🟡 | No isolated corpus entry |
| `client()` — contract-driven callers (RFC-055) | 2 | `internal/protocol/{client_contract,openapi_client,grpc_client}_test.go` (naming, collisions, binding, conformance) + `internal/star/client{,_trace}_test.go` (spec load, events, clocks, HTTP + gRPC end-to-end against the real mocks) | 🟡 | Go coverage is thorough on the critical path; no testops golden yet, so trace-shape regressions across releases are unguarded |
| Client contract validation (`validate=response`/`strict`) | 2 | Unit tests for schema mismatch, undeclared status, gRPC unknown fields; `poc/client-rfc055` end-to-end | 🟡 | OpenAPI 3.0 only (3.1 untested); JSON bodies only |
| Stdlib recipes (embedded .star) | 2 | 13 shipped, tested via protocol unit tests | 🟡 | No regression against recipe contents |
| `monitor()` / `partition()` / `nondet()` / `events()` | 2 | No dedicated coverage | 🔴 | Covered only if a corpus spec uses them |
| Named operations `op()` | 2 | Unit tests | 🟡 | |
| `iface.proxy_addr` / `proxy_host` / `proxy_port` (RFC-033) | 2 | `internal/star/runtime_rfc033_test.go` (6 tests) | 🟢 | Late-bound proxy address attrs for host-binary SUTs talking to Docker upstreams; placeholder + buildEnv-time substitution covered |
| `service(remote=...)` (RFC-036) | 2 | `internal/star/builtins_remote_test.go` (32 spec-load tests) + `internal/star/runtime_remote_test.go` (10 runtime/proxy tests incl. TLS×remote interop) + `internal/bundle/bundle_test.go` (env.json round-trip + omitempty) + `cmd/faultbox/replay_test.go` (warning printer) | 🟢 | Remote services point at externally-running endpoints (k8s pods, port-forwards). Healthcheck required; syscall faults rejected at spec load; protocol faults work as usual via the existing proxy datapath; combines with RFC-038 `tls=tls_cert(...)` for TLS upstreams; `env.json` records remotes used and `faultbox replay` warns. RFC-037 tracks deterministic offline replay. |
| `remotes()` typed per-interface override (RFC-036) | 2 | covered in `builtins_remote_test.go` | 🟢 | Per-interface upstream-host map for services whose interfaces live on different hosts |
| `@faultbox/discovery/k8s.star` (RFC-036) | 2 | `builtins_remote_test.go` (3 helper tests: cluster-DNS, default namespace, local short form) | 🟢 | String-only sugar for `<name>.<namespace>.svc.cluster.local`; no runtime k8s client |
| `--explore=all` exhaustive | 2 | No dedicated coverage | 🔴 | |
| `--virtual-time` | 2 | No dedicated coverage | 🔴 | |
| `--runs N` counterexample search | 2 | No dedicated coverage | 🔴 | |
| `determinism()` builtin + L0/L1 levels (RFC-040) | 2 | `internal/star/builtins_determinism_test.go` (parse-time + helpers, 24 tests) | 🟢 | Spec-level determinism declaration with reserved-syntax gating for L2..L5 + `runtime="gvisor"`; spec-wide `allow=` and per-service `nondeterministic_ok=` escape hatches |
| L1 `unmediated_io` detection — clock / rand / dns / network-unmediated (RFC-040) | 2 | `internal/star/builtins_determinism_test.go` (detection helpers + isMediatedAddress) + `internal/proxy/proxy_test.go` (IsListenPort, 6 tests) + testops corpus (determinism_clock_read, determinism_rand_read, determinism_dns_leak, determinism_raw_socket, determinism_tolerated; LinuxOnly via `poc/leaker`) | 🟢 | Detection wires into the syscall callback for already-faulted services; goldens lock down each category end-to-end via a leak-on-demand HTTP harness (`/tmp/faultbox-leaker`) faulted at `write=allow()`. Tolerated categories still emit unmediated_io events into the trace — tolerance only suppresses the strict-mode failure. |
| Strict mode + `strict_determinism_violation` outcome (RFC-040) | 2 | `internal/star/builtins_determinism_test.go` (strict helpers, 7 tests) | 🟢 | Default `strict=True` at L1 fails the test on the first untolerated `unmediated_io` event; `--strict-determinism[=true|false]` and `--no-strict-determinism` CLI overrides |
| `eventually(p, anchor=)` + `always(p, between=)` (RFC-041) | 2 | `internal/star/temporal_test.go` (Expectation lifecycle, anchors, finalize-cause matrix) + `internal/star/lifecycle_test.go` (end-to-end via test() builtin) | 🟢 | Declarative liveness/safety primitives; predicates query the trace API; final evaluation at Termination per §5.5 verdict table |
| `await_stable(quiescence_window=, ignore=)` + `await_event(matcher_or_predicate)` (RFC-041) | 2 | `internal/star/await_test.go` (quiescence, timer reset, ignore filtering, eager check, ctx cancellation) | 🟢 | Body-blocking primitives bounded by the per-test context; `clock="virtual"` reserved with explicit gVisor-path error |
| `monitor(name, on=, state_init=, update=, check=)` (RFC-041) | 2 | `internal/star/monitor_test.go` (state machine, on= filter, per-registration state) + `internal/star/monitor_sandbox_test.go` (denylist coverage) | 🟢 | State-machine monitor with mandatory `on=` matcher; update/check lambdas validated against a denylist at spec load (no fault/await/recursive monitor calls) |
| `test(name, body=, expect=, timeout=, terminate_when=, setup=, clock=)` builtin (RFC-041) | 2 | `internal/star/lifecycle_test.go` (test registration, per-test timeout, terminate_when, setup, virtual-clock reservation) | 🟢 | Declarative test declaration with explicit temporal config; coexists with legacy `def test_*()` |
| Trace API: `trace.event/events/first/last/count/causal_chain` + EventVal causal operators (RFC-041) | 2 | `internal/star/trace_test.go` (lookup operators, secondary indexes, causal relations) + `internal/star/match_test.go` (matcher composition) | 🟢 | Foundational query surface for predicates and monitors; backed by event-log secondary indexes (by type / service) |
| PASS/FAIL/INCONCLUSIVE three-valued lifecycle (RFC-041 §5.5, §8.6) | 1 | `internal/star/lifecycle_test.go` (Termination causes, verdict mapping, panic-as-error preservation, Inconclusive counter) | 🟢 | Inconclusive distinguished from Fail in SuiteResult, TraceOutput, and CLI exit code (3 for inconclusive-only vs 2 for any fail) |
| `faultbox plan` subcommand (RFC-042 §8.1, §5.1) | 2 | `cmd/faultbox/plan_test.go` (text/json/dot output, missing-spec, unknown-format, deferred-format rejection) | 🟢 | Static plan-tree analysis without launching services; supports `--format=text\|json\|dot` |
| Plan-tree enumeration (RFC-042 §8.2) | 2 | `internal/plan/enumerate_test.go` (def-only, fault_matrix collapse, fault_scenario standalone, test() builtin, determinism metadata, topology surfacing, deterministic-across-calls) | 🟢 | Pure function `plan.Enumerate(rt) → *PlanTree`; deterministic byte-stable output |
| Plan JSON/DOT output (RFC-042 §8.3) | 2 | `internal/plan/render_test.go` (round-trip JSON unmarshal, byte stability, DOT well-formedness) | 🟢 | `schema_version=1`; same shape as bundle's `plan.json` |
| Bundle `plan.json` artifact (RFC-042 §8.7) | 1 | `internal/bundle/bundle_test.go` (TestBuildEmitsPlanJSON, TestBuildOmitsPlanJSONWhenEmpty) | 🟢 | Every `faultbox test` run writes the plan tree alongside trace.json; `Reader.PlanJSON()` accessor; `--no-plan` opt-out |
| Coverage analysis + `--coverage` (RFC-042 §8.4) | 2 | `internal/plan/coverage_test.go` (covered/uncovered edge marking) + `cmd/faultbox/plan_test.go` (TestPlanCmd_CoverageAddsTable) | 🟢 | Edge × test cross-reference; covered/uncovered protocol summary; `WithCoverage(pt, rt)` |
| Rule-based `--suggest` for uncovered edges (RFC-042 §8.5) | 2 | `internal/plan/coverage_test.go` (TestWriteSuggestions_*) + `cmd/faultbox/plan_test.go` (TestPlanCmd_SuggestEmitsStubs) | 🟢 | Conservative per-protocol syscall picks; copy-pasteable fault_assumption + fault_scenario stubs; `--strategy=llm` reserved for v0.14.0 |
| `--check-cost --max-instances N` gate (RFC-042 §8.6) | 2 | `cmd/faultbox/plan_test.go` (TestPlanCmd_CheckCost*) | 🟢 | Exit code 2 when budget exceeded; pre-commit / CI integration |
| Report Plan tab (RFC-042 §8.10) | 2 | `internal/report/report_test.go` (TestGatherDataIncludesPlanWhenPresent, TestGatherDataOmitsPlanWhenAbsent) | 🟢 | Self-contained HTML render of the plan tree + embedded coverage table; reads `plan.json` from the bundle |
| `choose([opts])` + `nondet()` (RFC-043 §5.1, §5.2) | 2 | `internal/star/choose_test.go` (arity, empty/non-list rejection, named form, recording, existing nondet(svc) variant, rc2 leaf-pinned selection + cross-product fan-out) | 🟢 | rc2: named `choose("k", [opts])` axes fan out the plan tree — one execution per option, each carrying a stable LeafID. Anonymous `choose()` and `nondet()` remain single-leaf. |
| Probability fan-out for syscall faults (RFC-042 §8.9) | 2 | `internal/star/probability_fanout_test.go` (max_fires/mode parsing + validation; ProbabilityFire vector contract; mixed-radix enumeration; site dedup; runtime decider closure) | 🟡 | rc2 syscall-level only: `delay()` / `deny()` accept `max_fires=N` and `mode="exhaustive"\|"stochastic"`. Exhaustive (default) fans out 2^N leaves with deterministic per-occurrence vector; stochastic preserves legacy RNG. Protocol-level (response/error/drop) probability fan-out + static trigger-count analysis are follow-ups. |
| `parallel(interleavings=)` fan-out (RFC-042 §8.8) | 2 | `internal/star/interleavings_test.go` (kwarg parsing + reserved values; per-policy cardinality; permutation decoding; site recording + dedup; reset between leaves; end-to-end RunAll fan-out) | 🟡 | rc2 launch-ordering only: `parallel(...)` accepts `interleavings=` (`1` / `"all"` / `"critical"` / int N); reserved values `"dpor"`/`"sut-internal"` error explicitly. Plan walker fans out per-leaf and engine launches branches in the per-leaf order. **Limit:** launch ordering, not mediated-event-level interleaving. Follow-up extends RFC-014 hold queue. |
| Multi-leaf bundle attribution (RFC-042 §8.8/§8.9, RFC-043 §5.2) | 1 | `internal/star/probability_fanout_test.go::TestRunAll_MultiLeafBundleShape` + `internal/star/choose_test.go` fan-out tests | 🟢 | `TestResult.LeafID` → `bundle.TestRow.LeafID` → HTML report's tests table ("[leaf N]" suffix). Single-leaf executions preserve the rc1 manifest shape byte-identically. |
| `halt(reason="")` + halted outcome (RFC-043 §5.3) | 2 | `internal/star/halt_test.go` (sentinel, reason, kwarg/arity rejection, top-level rejection, setup rejection, RunTest path) | 🟢 | New SuiteResult.Halted counter + bundle.Summary.Halted + HTML "halted" outcome (grey pill, distinct from pass/fail/inconclusive) |
| `assume(predicate)` + `test(assume=)` (RFC-043 §5.4) | 2 | `internal/star/assume_test.go` (top-level true/false, lambda choices inspection, type/arity rejection, per-test halt + pass, predicate raise → "error", per-leaf choices visibility, sandbox AST denylist rejections) | 🟢 | rc2: per-test predicates see the current leaf's axis assignment at body entry (body-time choose() calls included). Predicate Starlark errors map to `Result="error"`. §8.7 AST denylist enforced at spec load for lambda predicates (named `def`s slip past — same monitor-sandbox limitation). Plan-walker-time pruning + cost guard are follow-ups. |

### Packet-level and filesystem mediation — Supported / Experimental

Everything here needs `determinism(runtime = "gvisor")` and is Linux-only.

| Feature | Tier | Mechanism | Status | Notes |
|---|---|---|---|---|
| Packet faults on the netstack gateway (RFC-054) | 2 | `internal/netfault/*_test.go` (matcher, defer queue, endpoint, fake-clock) + `poc/gvisor-rfc054` | 🟡 | `packet_loss`/`packet_delay`/`packet_reorder` per segment on a TUN gateway. Needs CAP_NET_ADMIN. No CI gate — the gateway needs a TUN device, so coverage is unit + manual VM run. |
| `bandwidth(rate, dir=, queue=)` + `mtu(size)` (v0.14.1) | 2 | `internal/netfault/shape_test.go` (rate parsing, single-server pacing, taildrop, MTU override) | 🟡 | Link shapers on the same endpoint as packet faults. Bare numbers in `rate=` are rejected — bit vs byte units must be explicit. |
| `sleep(duration, clock=)` (v0.14.1) | 2 | `internal/star/sleep_test.go` | 🟢 | Wall-clock wait. `sleep("0ms")` is a deliberate no-op so it works as a `choose()` axis baseline; refuses a duration longer than the remaining test budget. |
| `watch(svc, files=, ops=, run=)` — filesystem observation (RFC-056) | 2 | `internal/star/watch_{guards,ops}_test.go` + `internal/gvisor/*_test.go` + `poc/gvisor-rfc056` against real postgres:16-alpine | 🟡 | Resolves paths from inside the sandbox, closing `fs-unmediated`. Observation only — no `fsync` trace point exists in gVisor, so write *ordering* is provable and durability is not. Honesty guards fail the run on dropped/unattributed points or a sandbox that never connected. No CI gate: needs runsc + a host registered by `setup-trace`. |
| `faultbox setup-trace` host registration | 2 | `internal/gvisor/hostconfig_test.go` (plan/apply/change, point sets) | 🟡 | Writes `/etc/faultbox/trace.json` + a `faultbox-trace` Docker runtime; prints the daemon restart rather than performing it. `--check` is the idempotence probe. The planning layer is covered; the `cmd/faultbox` command itself has **no test** — it writes to `/etc`, so exercising it needs a sandboxed root prefix. |
| `ready(timeout=)` — protocol-aware readiness | 2 | `internal/star/ready_test.go` + `ready_credentials_test.go` + `internal/protocol/addr_test.go` | 🟢 | Asks the service through its protocol plugin, carrying declared credentials, instead of asking whether a port is open. Replaces the `tcp()` guess that let broken step clients look healthy. |

### DX and outputs — Supported / Experimental (proposed)

| Feature | Tier | Mechanism | Status | Notes |
|---|---|---|---|---|
| `trace()` + labels | 2 | Unit tests | 🟡 | |
| `--shiviz` visualization output | 3 | None | 🔴 | |
| `--normalize` + `faultbox diff` | 2 | testops harness **uses** this directly | 🟢 | Highest-confidence path in the product |
| Structured JSON output (`--format json`) | 2 | Agent schema smoke test | 🔴 | Needed for LLM-facing surface |
| `--debug` logging | 3 | Manual | 🔴 | |

### CLI subcommands — mixed

| Feature | Tier | Mechanism | Status | Notes |
|---|---|---|---|---|
| `faultbox test` | 1 | testops corpus is literally this | 🟢 | |
| `faultbox diff` | 2 | Used by testops harness | 🟢 | |
| `faultbox run` (single service) | 3 | Manual | 🔴 | |
| `faultbox check` (RFC-052 Gap 1) | 1 | `internal/check/check_test.go` — including a test that fails if it ever needs Docker | 🟢 | Validates without executing. The no-launch property is the whole value, so it is asserted rather than assumed |
| Error-code taxonomy (RFC-052 Gap 2) | 2 | `internal/star/codes_test.go` — every code must carry a suggestion | 🟢 | Typed errors, not message matching. Adoption is incremental and `Classify` reports gaps honestly rather than guessing |
| Vacuity diagnostics (RFC-052 Gap 8) | 1 | `internal/star/vacuity_test.go` + a known-bad/known-good gate over `poc/protocol-audit/` | 🟢 | `NO_POSITIVE_CONTROL`, `TEST_NO_ASSERTIONS`. Found two vacuous specs in this repo, one of them in the CI golden corpus |
| ~~`faultbox generate`~~ | — | — | ⚪ | **Removed in v0.17.0**; `faultbox plan --suggest` replaced it. The command name still reports where it went |
| `faultbox init` (starter .star) | 3 | Manual | 🔴 | |
| `faultbox mcp` (LLM server) | 2 | `internal/mcp/server_test.go` — contract tests: every advertised tool dispatches, schemas are valid and self-consistent, result shapes, malformed arguments | 🟢 | Was 🔴 with no coverage at all until v0.17.0, on the one surface whose declared primary user is an agent |
| `faultbox recipes list` / `show` | 3 | Manual | 🔴 | |
| `faultbox self-update` | 3 | Manual | 🔴 | |

### Platform integration — Supported (proposed)

| Feature | Tier | Mechanism | Status | Notes |
|---|---|---|---|---|
| Event sources (stdout, wal_stream, topic, tail, poll) | 2 | `internal/eventsource/*_test.go` + `internal/star/observe_decoder_test.go` (RFC-044 `observe.*` namespace + deprecated aliases) | 🟢 | RFC-044 §8.6: top-level `stdout()` / `stderr()` **removed in v0.17.0** → `observe.stdout` / `observe.stderr`; the names remain registered as stubs that name their replacement. |
| Decoders (json, logfmt, regex) | 2 | `internal/eventsource/decoder` unit tests + `internal/star/observe_decoder_test.go` (unified dispatcher + deprecated aliases) | 🟢 | RFC-044 §8.7: unified `decoder("name", ...)` dispatcher. The three legacy constructors were **removed in v0.17.0**; the names remain registered as stubs that name their replacement. |
| Container reuse (`reuse=True`) | 2 | No coverage | 🔴 | |
| Lima VM dev environment | 3 | Manual (`make env-verify`) | 🟡 | |

---

## Summary counts

- Critical (Tier 1): 15 rows, **100% green**. 🎉
- Supported (Tier 2): 24 rows, **~55% green**.
- Experimental (Tier 3): 8 rows, **~0% green** — expected; these are checklist-gated.

PR #64 (proxy teardown, closes #57 + #61) + PR #62 (openat matching,
closes #56) unlocked 5 Critical rows at once by turning on real
seccomp fault-injection coverage in CI on amd64 ubuntu-latest.
Next largest gap: Docker-backed cases (#poc_demo_container,
#poc_kafka_rfc014), which require CI Docker provisioning.

---

## How to update

1. **New Critical or Supported feature** → PR must add a manifest row in the same commit. CODEOWNERS on this file keeps it reviewed.
2. **Confidence mechanism landed** → update status column in the same PR as the test.
3. **Feature deprecated** → move to a "Removed" section at the bottom with a date. Don't delete — institutional memory.
4. **Tier change** → requires project-maintainer sign-off. Tier is a project-level promise about stability, not a per-PR decision.

The automated coverage script (see `testops/` — future work) reads
this file and emits red/green counts; stale status columns cause a CI
warning.
