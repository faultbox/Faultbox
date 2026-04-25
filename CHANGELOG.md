# Changelog

All notable changes to Faultbox are documented here.

The format follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and the project adheres to [Semantic Versioning](https://semver.org/).

Per-release "What's new" pages live on the site at
[faultbox.io/releases/](https://faultbox.io/releases/).

## [Unreleased]

Next-version work is tracked in
[GitHub Issues](https://github.com/faultbox/Faultbox/issues).

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

[Unreleased]: https://github.com/faultbox/Faultbox/compare/release-0.12.0...HEAD
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
