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
| `service()` binary mode (fork+exec) | 1 | testops corpus (LinuxOnly) + integration | 🟡 | Blocked on #57 for CI coverage |
| `service()` container mode (Docker) | 1 | testops corpus with pinned image digest | 🔴 | CI Docker provisioning not yet added |
| `fault(deny)` on syscalls | 1 | testops corpus + syscall-family test | 🔴 | Blocked on #57 |
| `fault(delay)` on syscalls | 1 | testops corpus | 🔴 | Blocked on #57 |
| `fault(hold)` on syscalls | 1 | testops corpus | 🔴 | No corpus entry yet |
| `assert_eventually` temporal | 1 | testops corpus (real syscall trace) | 🟡 | Spec-level matching fix in #62; un-skip depends on #57 + #61 |
| `assert_never` temporal | 1 | testops corpus | 🔴 | No corpus entry yet |
| `--seed` deterministic replay | 1 | testops harness itself asserts identical traces across 5 runs | 🟢 | Already proven by corpus entries |
| `depends_on` + healthcheck ordering | 1 | testops corpus covers transitively | 🟢 | |
| Starlark spec language | 1 | Parser unit tests + corpus exercises | 🟢 | |
| `fault_assumption` / `fault_scenario` / `fault_matrix` | 1 | No dedicated corpus entry | 🔴 | Domain-centric builtins are v0.3.0 headline |

### Protocol-aware faults — Critical (proposed)

Protocol-level fault proxy rewrites wire-level responses. Critical because this is the main differentiator over raw seccomp.

| Feature | Tier | Mechanism | Status | Notes |
|---|---|---|---|---|
| HTTP proxy faults | 1 | `internal/proxy/http_test.go` + corpus | 🟡 | Unit tests exist, no integration corpus |
| Postgres proxy faults | 1 | `internal/proxy/postgres_test.go` + corpus | 🟡 | Unit tests exist, no integration corpus |
| Redis proxy faults | 1 | corpus | 🔴 | Unit tests only |
| MySQL proxy faults | 2 | unit + `sqlmatch` canonicalizer | 🟢 | Strong unit coverage |
| Other protocols (HTTP2, UDP, Mongo, Cassandra, ClickHouse, Kafka, NATS, gRPC) | 2 | unit tests | 🟢 | 13 protocols, all have parse/proxy unit tests |

### Spec-language surface — Supported (proposed)

| Feature | Tier | Mechanism | Status | Notes |
|---|---|---|---|---|
| `mock_service()` + HTTP routes | 2 | testops corpus (http_basic, mock_demo) | 🟢 | |
| Redis mock (miniredis) | 2 | testops corpus (redis_basic, mock_demo) | 🟢 | |
| Kafka mock (kfake) | 2 | testops corpus (kafka_basic, mock_demo) | 🟢 | |
| MongoDB mock | 2 | testops corpus (mongo_basic, mock_demo) | 🟢 | |
| gRPC mock | 2 | included in mock_demo | 🟡 | No isolated corpus entry |
| Stdlib recipes (embedded .star) | 2 | 13 shipped, tested via protocol unit tests | 🟡 | No regression against recipe contents |
| `monitor()` / `partition()` / `nondet()` / `events()` | 2 | No dedicated coverage | 🔴 | Covered only if a corpus spec uses them |
| Named operations `op()` | 2 | Unit tests | 🟡 | |
| `--explore=all` exhaustive | 2 | No dedicated coverage | 🔴 | |
| `--virtual-time` | 2 | No dedicated coverage | 🔴 | |
| `--runs N` counterexample search | 2 | No dedicated coverage | 🔴 | |

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
| `faultbox generate` (scenario generator) | 2 | Unit tests in `internal/generate` | 🟡 | No end-to-end corpus |
| `faultbox init` (starter .star) | 3 | Manual | 🔴 | |
| `faultbox mcp` (LLM server) | 2 | No coverage | 🔴 | Contract tests needed — LLM is a target user |
| `faultbox recipes list` / `show` | 3 | Manual | 🔴 | |
| `faultbox self-update` | 3 | Manual | 🔴 | |

### Platform integration — Supported (proposed)

| Feature | Tier | Mechanism | Status | Notes |
|---|---|---|---|---|
| Event sources (stdout, wal_stream, topic, tail, poll) | 2 | `internal/eventsource/*_test.go` | 🟢 | |
| Decoders (json, logfmt, regex) | 2 | `internal/eventsource/decoder` unit tests | 🟢 | |
| Container reuse (`reuse=True`) | 2 | No coverage | 🔴 | |
| Lima VM dev environment | 3 | Manual (`make env-verify`) | 🟡 | |

---

## Summary counts (provisional, pre-confirmation)

- Critical (Tier 1): 17 rows, **~12% green**.
- Supported (Tier 2): 22 rows, **~55% green**.
- Experimental (Tier 3): 8 rows, **~0% green** — expected; these are checklist-gated.

The Critical gap is the story: core-workflow coverage is underwater.
Issue #57 (shared-runner user-namespace + AppArmor interaction) is the
single highest unlock — resolving it turns ~8 Critical rows from 🔴 to
🟢 at once. Hypothesis confirmed in [#57 comment](https://github.com/faultbox/Faultbox/issues/57#issuecomment-4295465481):
Ubuntu 24.04's `kernel.apparmor_restrict_unprivileged_userns=1` breaks
faultbox's sandbox on GitHub-hosted runners while Lima works. Three
fix paths documented there, ranging from a one-line CI sysctl to a
proper `namespaces=` service kwarg.

---

## How to update

1. **New Critical or Supported feature** → PR must add a manifest row in the same commit. CODEOWNERS on this file keeps it reviewed.
2. **Confidence mechanism landed** → update status column in the same PR as the test.
3. **Feature deprecated** → move to a "Removed" section at the bottom with a date. Don't delete — institutional memory.
4. **Tier change** → requires project-maintainer sign-off. Tier is a project-level promise about stability, not a per-PR decision.

The automated coverage script (see `testops/` — future work) reads
this file and emits red/green counts; stale status columns cause a CI
warning.
