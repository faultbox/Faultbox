# Changelog

All notable changes to Faultbox are documented here.

The format follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and the project adheres to [Semantic Versioning](https://semver.org/).

Per-release "What's new" pages live on the site at
[faultbox.io/releases/](https://faultbox.io/releases/).

## [Unreleased]

Work targeting v0.11.3 and v0.12 is tracked in
[GitHub Issues](https://github.com/faultbox/Faultbox/issues).

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

[Unreleased]: https://github.com/faultbox/Faultbox/compare/release-0.11.3...HEAD
[0.11.3]: https://github.com/faultbox/Faultbox/compare/release-0.11.2...release-0.11.3
[0.11.2]: https://github.com/faultbox/Faultbox/compare/release-0.11.1...release-0.11.2
[0.11.1]: https://github.com/faultbox/Faultbox/compare/release-0.11.0...release-0.11.1
[0.11.0]: https://github.com/faultbox/Faultbox/compare/release-0.10.1...release-0.11.0
[0.10.1]: https://github.com/faultbox/Faultbox/compare/release-0.10.0...release-0.10.1
[0.10.0]: https://github.com/faultbox/Faultbox/compare/release-0.9.9...release-0.10.0
[0.9.9]: https://github.com/faultbox/Faultbox/compare/release-0.9.8...release-0.9.9
[0.9.8]: https://github.com/faultbox/Faultbox/compare/release-0.9.7...release-0.9.8
[0.9.7]: https://github.com/faultbox/Faultbox/releases/tag/release-0.9.7
