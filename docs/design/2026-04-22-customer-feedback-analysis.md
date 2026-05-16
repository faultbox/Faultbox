# inDrive / truck-api PoC — Feedback Analysis & 0.9.x / 0.11.0 Roadmap

**Authored:** 2026-04-22
**Inputs:** [FAULTBOX_FEEDBACK.md](FAULTBOX_FEEDBACK.md), [FAULTBOX_REPRODUCIBILITY.md](FAULTBOX_REPRODUCIBILITY.md), [TEST_COVERAGE.md](TEST_COVERAGE.md)
**Validated against:** v0.9.6 (current main)
**Scope per owner:** finish the asks in the feedback docs in the 0.9.x
train; cap with **v0.11.0 Reports** (rich HTML artifact per run). **No
big features** outside the reports or the Reports story.

---

## 0. Executive summary

The customer found 16 real bugs and ranks the core product unique ("the
seccomp user-notify approach is the reason the PoC was worth doing") but
says it isn't shippable yet because of three systemic gaps — **gRPC
proxy on the data path, Linux CI, and docs.** Two of those are
already substantially closed on current main:

| Customer ask | Status on main | Delta |
|---|---|---|
| Native gRPC mocks | **Shipped** (RFC-023, v0.9.0) | Tutorial Ch 18 exists |
| Proxy on SUT data path | **Shipped** (RFC-024, v0.9.5 + v0.9.6) | Container mode + TCP + benchmarks done |
| Per-gRPC-method fault targeting | **Shipped** via `error(method="/pkg/Method")` at proxy layer | Needs better surfacing in docs |
| `fault_assumption` dispatch + `ops=` filter + `fault_zero_traffic` | **Shipped** (v0.9.4) | Customer knows |

The **remaining** asks cluster into five groups:

1. **Documentation overhaul** (customer lost ~47h / 20% of engagement to docs gaps).
2. **Small missing primitives** — `load_file()`, JWT/JWKS, richer expectations, gRPC error shorthands.
3. **Reproducibility-by-default** — artifacts dir, replay_command surfacing, version/digest pinning.
4. **CI / deployment ergonomics** — Linux CI recipe, service-binary rebuild, healthcheck types.
5. **Processes / commercial** — API stability promise, roadmap visibility, reference customer.

Effort/Profit summary below. **Recommended release sequence:**

- **v0.9.7** — `.fb` archive bundle format (every run → one self-contained file), replay on failure, seed default. Load-time validation.
- **v0.9.8** — Small primitives: `load_file()`, JWT/JWKS, `expect_*`.
- **v0.9.9** — Documentation overhaul + CI-on-Linux recipe + Starlark reference.
- **v0.10.0** — Determinism guardrails (digest pinning, version mismatch detection, `faultbox replay <bundle>`).
- **v0.11.0** — **Interactive HTML Reports** consumed from a `.fb` bundle (shareable static artifact, SaaS-neutral).

Each release builds on the previous; v0.11.0 is the capstone that
turns the reproducibility and determinism work into something users
actually look at.

---

## 1. Validation — what's already shipped vs the reports

The customer reports were written on v0.9.4. Current main is v0.9.6. This
table catches items the customer may not know are already closed.

| # | Customer ask | Report §ref | Status on v0.9.6 | Evidence |
|---|---|---|---|---|
| 1 | Native gRPC mock | FB §3 row 1 | ✅ Shipped v0.9.0 | RFC-023, `@faultbox/mocks/grpc.star`, tutorial Ch 18 |
| 2 | Proxy on SUT data path | FB §3 row 2 | ✅ Shipped v0.9.5+6 | RFC-024, `preStartProxies`, `host.docker.internal` |
| 3 | Per-gRPC-method targeting | FB §3 row 3 | ✅ Shipped (surfacing gap) | `error(method="/pkg/Method")` at proxy layer |
| 4 | `fault_assumption(rules=)` in matrix | FB §2.1 #4 | ✅ Shipped v0.9.4 | Customer-reported fix #1 |
| 5 | Custom `ops=` in seccomp filter | FB §2.1 #3 | ✅ Shipped v0.9.4 | Customer-reported fix #2 |
| 6 | `fault_zero_traffic` signal | FB §2.1 | ✅ Shipped v0.9.4 | Customer-reported fix #3 |
| 7 | `--seed` / `--output json` / `replay_command` | Repro §3.3 | ✅ Exists — opt-in only | `cmd/faultbox/main.go:225`, `results.go:56` |
| 8 | `--runs N --show fail` | Repro §3.3 | ✅ Exists | `cmd/faultbox/main.go:220` |
| 9 | HTTP healthcheck | FB §2.1 #5 | ✅ Exists (gap: docker-proxy timing) | `builtins.go` `http()` healthcheck |
| 10 | OpenAPI mock generation | (not in reports) | ✅ Shipped v0.9.3 | RFC-021 — bonus for any HTTP consumer |
| 11 | `grpc_error(code, message)` builder | FB §5 row 10 | ✅ Exists (no shorthand) | `grpc_error("UNAVAILABLE", "...")` |

**Customer-to-do:** they're still on v0.9.4. Upgrading to v0.9.6 closes
three of the top-10 blockers they filed. Every future conversation with
them should lead with "you already have this."

---

## 2. Grouped analysis

Each row has: **ask** (what they want), **status** (what exists on main),
**gap** (what's left to do), **E** = effort (XS<½d, S=½–1d, M=2–3d, L=1w,
XL=2w+), **P** = profit 1–5, and a **SaaS-vision alignment** note.

### Group A — Documentation

The single biggest lever. Customer lost ~47h to doc gaps out of a
~240h engagement; they've explicitly said "documentation that lets a
non-expert engineer onboard a new service in under one day" is a **hard
blocker** for payment (FB §6.1 #3).

| # | Ask | Status | Gap | E | P | SaaS align |
|---|---|---|---|---|---|---|
| A1 | End-to-end tutorial: Go + gRPC + Kafka + MySQL + Redis + JWT | Partial (Ch 17 covers JWT; Ch 18 typed gRPC; nothing ties full stack together) | Write one composite chapter | M | 5 | Onboarding → SaaS trial conversion |
| A2 | Primitive reference: every kwarg documented | `spec-language.md` exists; some kwargs (`config=`, `depends_on=`) thin | Complete audit + backfill | M | 4 | Self-serve onboarding |
| A3 | Starlark dialect reference (no file I/O, no regex, kw-only args) | Missing as a standalone page | One-page "Starlark in Faultbox" | S | 4 | Reduces "why doesn't this work" tickets |
| A4 | Seccomp cheatsheet (Go stdlib op → syscall names) | Missing | Short tables per stdlib area (net, os, io) | S | 4 | Makes named-ops authoring obvious |
| A5 | Troubleshooting playbook per failure class | Missing | One page, ~10 common failure modes | S | 5 | Deflects support load |
| A6 | Lima-on-Mac supported-path docs | Fragments exist; not a coherent story | Dedicated setup guide | S | 3 | Blocks Mac onboarding today |
| A7 | CI-on-Linux recipe (GitHub Actions, BuildKite) | Missing | Template + documented privilege requirements (seccomp, ptrace_scope) | M | 5 | **Hard blocker** for any customer paying |
| A8 | JWT/JWKS worked example (claim names, key formats) | Fragment in Ch 17 | Expand to runnable example | S | 3 | Every auth-gated service |
| A9 | Customer-visible roadmap | Missing (RFCs are public but not grouped by release) | Curated `ROADMAP.md` | XS | 4 | Trust-building for paid conversion |

**Group A totals:** ~8 PMd of effort, addresses the customer's
#1 hard blocker. Cheapest large lever in this whole list.

### Group B — Small primitives

None of these are big features. All are 1–3d each and each closes a
named customer gap.

| # | Ask | Status | Gap | E | P | SaaS align |
|---|---|---|---|---|---|---|
| B1 | `load_file("seed.sql")` Starlark builtin | Missing (Go-level `LoadFile` exists, not exposed) | New builtin; read-only, path relative to .star | S | 5 | Unlocks every DB/fixture scenario |
| B2 | `load_yaml()` / `load_json()` | Missing | Natural siblings of B1 | S | 4 | OpenAPI, config fixtures |
| B3 | `jwks_mock(keys=, claims=)` primitive | Partial (examples only) | Curated stdlib mock wrapping existing `mock_service` + signing | S | 4 | Every auth-protected service |
| B4 | `expect_success()`, `expect_error_within(ms)`, `expect_hang()` | Missing (`assert_eventually` exists but isn't this) | New assertion builders keyed to scenario outcome | M | 5 | Fault-matrix readability |
| B5 | `expect_retry_budget(n)` | Missing | Higher-order primitive; counts retry attempts in trace | M | 3 | Customer-named; maps to prod SLO language |
| B6 | `grpc_unavailable()` / `grpc_deadline_exceeded()` shorthands | Partial (use `grpc_error("UNAVAILABLE", …)`) | Stdlib helpers in `@faultbox/mocks/grpc.star` | XS | 3 | Ergonomics; drops a line of boilerplate |
| B7 | HTTP healthcheck richness (`service_ready(http_probe=…, expect_status=…)`) | Partial (`http()` healthcheck exists) | Document + extend to path/body matchers | S | 3 | Avoids docker-proxy false-ready (FB §2.1 #5) |
| B8 | Undocumented kwarg = spec-load parse error | Partial (some kwargs error, others silently ignored) | Audit every builtin for strict kwarg validation | S | 4 | Kills whole class of "spec passed, nothing ran" |

**Group B totals:** ~7 PMd. Each is a dopamine hit for the customer
because they named them specifically. Shipping all eight is a strong
signal that we read the report carefully.

### Group C — Reproducibility & `.fb` archive bundle

Customer wrote a whole dedicated 1800-word doc on this. Positioned as
"the single biggest gap between PoC and production quality gate."

**Design decision (owner, 2026-04-22):** instead of an auto `artifacts/`
directory, every `faultbox test` run emits a **single `.fb` archive
bundle** — tar.gz with a fixed internal layout. One file per run,
emailable, check-in-able, the canonical input to `faultbox report`
(Group G). Every downstream tool (replay, reports) takes a `.fb` as
its sole input.

Bundle layout (contract):

```
run-<timestamp>-<seed>.fb  (tar.gz)
├── manifest.json          # schema version, test list, summary
├── trace.json             # full event log w/ vector clocks (today's --output format)
├── env.json               # Faultbox version, Go toolchain, kernel, Docker, image digests
├── replay.sh              # one-liner that reproduces this run
└── services/
    ├── <svc-a>.stdout
    ├── <svc-a>.stderr
    └── <svc-b>.stdout …
```

| # | Ask | Status | Gap | E | P |
|---|---|---|---|---|---|
| C1 | `.fb` bundle always emitted — trace + stdout/stderr + env.json + replay.sh | Missing | New archive format, always-on | M | 5 |
| C2 | Print `replay_command` at end of every failed test | Missing (in JSON only) | One-line addition in stderr summary | XS | 5 |
| C3 | Default `--seed` recorded in manifest.json, reused on rerun | Partial | Seed persisted in bundle; `faultbox replay` picks it up | S | 4 |
| C4 | Image digest auto-pinning (`faultbox.lock`) | Missing | First resolve records digest; warn if tag target changes | M | 4 |
| C5 | Version-mismatch warning vs `.faultbox-version` | Missing | Emit warning at test start if tool version ≠ declared | XS | 3 |
| C6 | Go toolchain / kernel / Docker version in env.json | Missing | Bundled with C1 | XS | 3 |
| C7 | pprof on timeout/hang → `services/<svc>.goroutines.txt` in bundle | Missing | Bundle auto-captures on timeout | M | 3 |
| C8 | Panic capture (GOTRACEBACK=crash) → in stdout/stderr | Missing | Default env on binary services | XS | 3 |
| C9 | Optional `--capture-pcap` → `services/<svc>.pcap` in bundle | Missing | `tcpdump` sidecar per interface | M | 2 |
| C10 | `faultbox replay <bundle.fb>` subcommand | Missing | Single command re-runs from a bundle | M | 4 |
| C11 | `fault_zero_traffic` in default stderr output | Partial (JSON only) | Also print to terminal at test end | XS | 4 |
| C12 | `faultbox inspect <bundle.fb>` — list contents / extract individual files | Missing | Mirror of `tar -tf` specialised for `.fb` | S | 3 |

**Group C totals:** ~7 PMd. Centred on one artifact contract. Every
downstream tool (replay, report, aggregation) consumes `.fb` and
nothing else — single interface to evolve.

**Explicit non-goal:** no SaaS-/cloud-specific concerns in this group.
`.fb` lives on local disk; uploading it to S3 or rendering it in a
hosted dashboard is a 1.x concern. The bundle is just a file.

### Group D — CI / deployment ergonomics

| # | Ask | Status | Gap | E | P | SaaS align |
|---|---|---|---|---|---|---|
| D1 | Linux CI recipe (GitHub Actions) | Missing | Docs + template workflow with privilege requirements | M | 5 | **Hard blocker** — FB §6.1 #2 |
| D2 | Linux CI recipe (BuildKite) | Missing | Docs | S | 3 | Customer-named for their stack |
| D3 | Privilege requirements docs (`seccomp`, `ptrace_scope`, rootless) | Missing | One-page ops guide | S | 4 | Required to even write D1/D2 |
| D4 | `service(binary=…)` auto-rebuild on source change | Missing | `go build` watch or hash-diff | M | 3 | Developer ergonomics; currently manual |
| D5 | Offline build / GOMODCACHE support | Partial | Document + verify | S | 2 | CI ergonomics |

**Group D totals:** ~4 PMd. D1+D3 is the hardest blocker (§6.1 #2).

### Group E — Processes & commercial

Softer work but customer explicitly called these out as payment
blockers (FB §6).

| # | Ask | Gap | E | P | SaaS align |
|---|---|---|---|---|---|
| E1 | API stability promise across 2 minor versions | Write and publish policy | XS | 4 | Trust foundation for commercial |
| E2 | Public roadmap | Convert RFC README into release-grouped roadmap | XS | 4 | Precondition for customer planning |
| E3 | Reference customer (public or NDA) | Sales process; not an engineering task | — | — | Usually precedes first paid customer |
| E4 | Support SLA in writing | Legal / commercial | — | — | Required for contract |
| E5 | Case-study writeup of inDrive v0.9.4 turnaround | Marketing; 1d to co-author w/ customer | S | 4 | Strong GTM artifact |

### Group G — v0.11.0 Interactive HTML Reports

Inspired by the customer's own [TEST_COVERAGE.md](TEST_COVERAGE.md): a
matrix view of "what got tested, with what faults, how it went" is the
single highest-leverage artifact they produced on their side — and it
was all hand-written. v0.11.0 ships this as a machine-generated HTML
from the `.fb` bundle.

**Contract:** `faultbox report <bundle.fb>` → `report.html`. Self-
contained (all JS/CSS/data inlined), no server calls, shareable by
email/Slack/published as a static asset. Inspired by R-Studio
notebooks (single `.Rmd` → single `.html`).

**What the report shows:**

| Section | Data source (from bundle) | Visual |
|---|---|---|
| Summary dashboard | `manifest.json` aggregate | Pass/fail/error counts, duration histogram, seed, env fingerprint |
| **Fault matrix** | fault_matrix test names + outcomes in `trace.json` | Scenarios × faults grid with colour-coded cells (green/red/yellow/grey-not-run) — the shape of TEST_COVERAGE.md §4 |
| Observed coverage | `trace.json` event log | Per-service: # tests touching it · which syscalls were faulted · which proxy rules fired · `fault_zero_traffic` warnings |
| Per-test drill-down | Click row → detail panel | Faults applied, assertions, duration, link to trace viewer |
| Reproducibility panel | `env.json` + `replay.sh` | Versions + digests + "Copy replay.sh" button |
| Coverage gaps | — | **Not prescriptive** — report shows what WAS tested; what SHOULD be tested is authored by the user in their `.star` specs |

**Shiviz integration:** separated sidecar (owner-decided). Per-test
trace view is rendered by Shiviz (MIT-licensed, vendored) via vector
clocks already present in the event log. v0.11.0 Phase 2 ships this as
an **experiment** — separate `trace-<test>.html` files linked from the
main report rather than inlined. Graduates to main report if it proves
valuable.

**Technical approach:** three source files, no frontend build step.

```
internal/report/
├── template.html       # shell: <div id="app"></div>
├── app.js              # vanilla JS: router, matrix, drill-down
├── style.css           # Golang-web aesthetic (match existing site)
├── shiviz/             # vendored MIT sidecar (Phase 2)
└── build.go            # go:embed + inline data injection
```

Output is a single HTML file with `window.__FAULTBOX__ = {…bundle data…}`
inlined. All interactivity is client-side JS operating on that object.

| # | Ask | Status | Gap | E | P |
|---|---|---|---|---|---|
| G1 | `faultbox report <bundle.fb>` subcommand | Missing | New subcommand + Go HTML builder | M | 5 |
| G2 | Fault matrix grid (scenarios × faults × outcome) | Missing | React-less UI; reads from trace.json | M | 5 |
| G3 | Per-test drill-down panel | Missing | Click-to-expand from matrix | M | 4 |
| G4 | Shiviz sidecar per failed test | Missing | Vendor + converter from `Event.VectorClock` → Shiviz log format | L | 4 |
| G5 | Observed-coverage view (services × syscalls × proxy hits) | Missing | Aggregated from event log | M | 4 |
| G6 | Reproducibility panel w/ replay.sh button | Missing | Inline env.json, one-click copy | S | 3 |
| G7 | Aggregate multi-bundle mode (`faultbox report --aggregate a.fb b.fb …`) | Missing | Diff view across runs — **local only**, not SaaS | M | 3 |
| G8 | Tutorial chapter + spec docs | Missing | Part of v0.11.0 release | S | 3 |

**Group G totals:** ~3 weeks of work for v0.11.0 — the natural capstone
after the `.fb` bundle contract (v0.9.7) and `faultbox replay` (v0.10.0)
are in place.

**Explicit non-goals for v0.11.0:**

- **No server/cloud/SaaS code.** Bundle and report are local artifacts.
  A hosted-runner offering is 1.x territory.
- **Report does not prescribe coverage.** The user decides what to
  test in their `.star` specs; the report visualises what happened.
- **Report does not call external services.** No telemetry, no auth,
  no network. The HTML opens in any browser offline.

### Group F — Out-of-scope for 0.9.x (per owner)

The following are in the feedback but fit the "new big features" bucket
the owner said to skip. Flagging here so we don't lose them:

- SaaS-hosted runner (FB §6.3 #8) → 1.x scope.
- DevPlatform integration (FB §6.3 #9).
- Spec templates / library per stack shape (FB §6.3 #10).
- Clock skew / DNS chaos / packet-level chaos (Repro §5 matrix).
- cgroup OOM / memory-pressure primitives (Repro §5).
- Crash-recovery primitives (SIGKILL mid-commit) (Repro §5).
- `race` detector integration (Repro §5).
- Fuzzer × matrix (Repro §5).

---

## 3. Effort / Profit synthesis

Plotting (E, P) for every actionable item:

| Priority | Items | Why here |
|---|---|---|
| **Do first (P≥4, E≤S)** | C2, C5, C8, C11, E1, E2, B6 | Trivial to ship, pays back immediately |
| **Core 0.9.x batch (P≥4, E=M)** | A1, A2, A5, A7, B1, B4, C1, C3, C10, D1, D3 | Medium work, high leverage |
| **Second wave (P=3, any E)** | B2, B3, B5, B7, B8, C7, D4, E5 | Worth shipping; sequence after first two |
| **Defer / low return** | B9, C9, D5 | Small audience or high cost |

The **P≥4 / E≤S** quadrant alone has 7 items and together would take
~4 PMd to ship. That's the single cheapest morale+trust move.

---

## 4. Proposed 0.9.x sequencing

Kept to the owner's rule — **no features outside the reports**, closing
named gaps only. Each release is ~1 week scope.

### v0.9.7 — ".fb bundle + reproducibility-by-default"

Targets Group C P≥4 items + cheap wins from A and E.

- C1: `.fb` archive bundle emitted on every run — trace.json + env.json + per-service stdout/stderr + replay.sh (M)
- C2: print `replay_command` on every failed test (XS)
- C11: emit `fault_zero_traffic` to stderr too (XS)
- C5: version-mismatch warning vs `.faultbox-version` (XS)
- C8: `GOTRACEBACK=crash` default for binary services (XS)
- C12: `faultbox inspect <bundle.fb>` subcommand (S)
- B8: strict kwarg validation — undocumented kwarg = spec-load error (S)
- E1: publish API stability policy (XS, docs only)
- E2: curate RFC README into `ROADMAP.md` grouped by release (XS, docs only)

**Total:** ~4 PMd. Lands the bundle contract that v0.10.0 replay and
v0.11.0 reports both consume. Customer-facing story: "every run drops a
single `.fb` file you can email to a colleague or check into git."

### v0.9.8 — "Small missing primitives"

Targets Group B.

- B1: `load_file()` Starlark builtin (S)
- B2: `load_yaml()` / `load_json()` (S)
- B3: `jwks_mock()` stdlib primitive in `@faultbox/mocks/jwt.star` (S)
- B4: `expect_success()` / `expect_error_within(ms)` / `expect_hang()` (M)
- B6: `grpc.unavailable()` / `grpc.deadline_exceeded()` helpers (XS)
- C3: default-recorded `--seed` between runs (S)

**Total:** ~4 PMd. Ships the "I asked for these specifically" list.
Customer sees every one of their P0 primitives land together.

### v0.9.9 — "Documentation + Linux CI"

Targets Group A and D1/D3 — the **payment blockers** per §6.1.

- A7: Linux CI recipe (GitHub Actions template, privilege docs) (M)
- D3: privilege requirements guide (S)
- A1: end-to-end tutorial (Go + gRPC + Kafka + MySQL + Redis + JWT) (M)
- A2: primitive reference audit + backfill (M)
- A3: Starlark dialect reference (S)
- A4: seccomp cheatsheet (S)
- A5: troubleshooting playbook (S)

**Total:** ~6 PMd, mostly writing. Once this lands we can in good
conscience stop saying "docs are thin" in sales conversations.

### v0.10.0 — "Determinism guardrails"

Targets Group C P≤4, finishing the reproducibility story.

- C4: image-digest auto-pinning + `faultbox.lock` (M)
- C6: Go toolchain / kernel version in env.json (XS, bundled)
- C7: pprof-on-timeout → captured into bundle `services/<svc>.goroutines.txt` (M)
- C10: `faultbox replay <bundle.fb>` subcommand (M)
- D4: binary auto-rebuild (M)

**Total:** ~5 PMd. Closes every P≥3 ask in the reports; the `.fb`
bundle format is now the stable contract for v0.11.0 to build on.

### v0.11.0 — "Interactive HTML Reports"

All of Group G. New subcommand `faultbox report <bundle.fb>` produces
a single-file `report.html` with everything inlined — matrix view,
per-test drill-down, observed coverage, replay button. Shiviz trace
viewer ships as a separate sidecar experiment.

- G1: `faultbox report <bundle.fb>` subcommand + shell HTML skeleton (M)
- G2: fault matrix grid UI (M)
- G3: per-test drill-down panel (M)
- G5: observed-coverage view (M)
- G6: reproducibility panel (S)
- G4: Shiviz sidecar pages per failed test (L — Phase 2)
- G7: `--aggregate` multi-bundle mode (M)
- G8: tutorial chapter + docs (S)

**Total:** ~3 weeks. Customer-facing story: "every `faultbox test` run
produces a bundle AND a self-contained HTML report you can drop into
Slack or publish as a static site." Local only; SaaS/cloud is not in
this release.

---

## 5. Proposed RFCs to draft

The larger items warrant RFCs so the customer can comment before we
ship (they explicitly asked for this — FB §7 #1):

1. **RFC-025: `.fb` Bundle Format + Reproducibility Contract.** Covers
   C1 + C2 + C6 + C8 + C11 + C12. Defines the archive layout, manifest
   schema, and guarantees. Customer is the primary reviewer. Target:
   v0.9.7. **Precondition for RFC-029.**
2. **RFC-026: Starlark Module System — `load_file` and friends.**
   Covers B1 + B2. Needs design work because introducing file I/O to
   Starlark has security implications (path traversal, SSRF via
   `http://` paths). Target: v0.9.8.
3. **RFC-027: Matrix Expectation Language.** Covers B4 + B5. Ships a
   small DSL of outcome predicates so fault_matrix rows carry explicit
   pass/fail semantics. Target: v0.9.8.
4. **RFC-028: Self-Hosted CI Recipes.** Covers A7 + D1 + D3. Docs-heavy;
   might not need a full RFC — could be a design doc. Target: v0.9.9.
5. **RFC-029: Interactive HTML Reports.** Covers all of Group G. Shell
   HTML + vanilla JS + inlined `.fb` data; Shiviz vendored as a
   separate sidecar experiment. Depends on RFC-025 (`.fb` format) and
   RFC-027 (expect_* outcomes drive matrix colours). Target: v0.11.0.

No RFC for the docs work (A1–A5) or the commercial items (E1–E5); those
are execution.

---

## 6. SaaS vision alignment — by design, not by code

Owner decision (2026-04-22): **v0.9.x through v0.11.0 ship local-only
code.** No cloud, no hosted runner, no multi-tenant. What makes the
future SaaS possible without changing v0.11.0 is the **artifact
contract**:

- The **`.fb` bundle** is an addressable, portable, self-describing
  object. Whether it lives at `./run-*.fb` or
  `s3://faultbox/<org>/<project>/<run>.fb` is a deployment concern,
  not a code concern. The archive is the same bytes.
- The **`report.html`** opens from disk or from a URL identically.
  Publishing a CI-built report as a GitHub Pages static asset is
  "copy this file"; same for a future hosted dashboard — "serve this
  file."
- **`faultbox replay <bundle.fb>`** takes a path or URL (trivial
  extension later). The CLI flow and any future web-UI flow share
  the verb.

So SaaS alignment in the 0.9.x/0.11.0 train is a **design discipline,
not a feature set**: every new tool operates on one `.fb` file, every
rendering step produces one self-contained HTML, no hidden network
dependencies anywhere. If and when we ship a hosted offering, the
bundle is what it stores and the HTML is what it renders.

**Not in scope for this train:** web UI, billing, multi-tenant auth,
hosted runners, cross-org sharing, access controls. Those are 1.x.

---

## 7. Resolved decisions

Owner-decided 2026-04-22, baked into RFCs.

1. **Artifact format: single `.fb` tar.gz bundle per run**, not a
   directory. Default location `./` (cwd). Filename
   `run-<timestamp>-<seed>.fb`. `.gitignore` convention documented in
   RFC-025. Supersedes the earlier "artifacts/ directory" proposal.
2. **Report is local-only.** No SaaS, no cloud, no network calls in
   v0.11.0. The HTML opens offline in any browser. Hosted-runner
   offerings are a 1.x concern and do not affect the v0.11.0 report
   shape — the same `.fb` bundle is the atomic object either way.
3. **Coverage is observed, not prescribed.** The report shows what was
   tested and how. Users author intent in their `.star` specs; we do
   not ship a coverage-intent DSL in v0.11.0.
4. **Shiviz is a separate sidecar experiment.** Per-failed-test trace
   viewer ships as a stand-alone HTML page linked from the main
   report. If the experiment succeeds we can promote to an inline
   panel in v0.12.x; if it falls flat we retire it cheaply.
5. **Interactive, no server calls.** Constraint on v0.11.0: the report
   must work when saved to disk, emailed, or published as a static
   asset. All state is inline; all interactivity is client-side JS.
6. **`.fb` bundle is the one input contract.** `faultbox report`,
   `faultbox replay`, `faultbox inspect`, and any future tool operate
   on exactly one `.fb` file. This is the interface that insulates us
   from later refactors — we can change the internal archive layout
   as long as tools keep reading via a versioned manifest.

Still to decide (not blocking v0.9.7):

- **`.faultbox-version` file format** — string vs constraint. Punt
  until v0.10.0 determinism work; prototype with a plain string.
- **`load_file` security model** — detailed in RFC-026 draft.
- **`expect_*` scope (row vs global)** — RFC-027 will go with per-row
  override on a `default_expect` (pytest parametrize shape).
- **Case-study writeup timing (E5)** — after v0.9.9 so the narrative
  spans hotfix → follow-ups → docs → reports.

---

## 8. What we will **not** do in 0.9.x

Per owner instruction ("don't do new big features which not mentioned
in the reports"), the following — even though they are in the reports
— are deferred to 1.x:

- Any SaaS-native feature (hosted runner, dashboard, multi-tenant auth).
- DevPlatform integration.
- Chaos primitives not currently present: DNS / clock-skew /
  packet-reorder / cgroup-OOM / mid-transaction SIGKILL.
- `race` detector integration.
- Fuzzer × fault-matrix.
- Kafka rebalance modeling.

Everything in §4 above closes named gaps without introducing a new
feature category.
