# RFC-037: Determinism for Remote Services

- **Status:** Draft (problem statement + options — design not yet committed)
- **Author:** Boris Glebov, Claude Opus 4.7
- **Created:** 2026-05-01
- **Target:** TBD (gated on real-customer feedback from RFC-036 v1)
- **Depends on:** RFC-025 (`.fb` bundle), RFC-036 (Remote Services)
- **Companion to:** RFC-036 — split out from that RFC on 2026-05-01 because
  the design space is large enough to warrant its own discussion thread.

## Status note (this RFC is intentionally exploratory)

This RFC is **not a committed design**. It frames the problem, lays out
the options we've considered, and flags the questions we want input on
before drafting an implementation plan. Treat the "Design options"
section as a menu, not a recommendation. Comments on which option (or
combination) is right for Faultbox's positioning are the explicit goal
of this RFC.

We intentionally did not gate RFC-036 on resolving this — the `remote=`
primitive is useful even with best-effort reproducibility, and shipping
it lets us learn from real customers what their actual determinism
needs are before locking in a design here.

## Problem

RFC-025 (`.fb` bundle) committed Faultbox to a strong reproducibility
contract: *"every `faultbox test` run drops a single `.fb` file that,
on any other machine, can be replayed to produce a byte-equivalent
trace."* Every primitive shipped since (replay, lock, report, drift
warning) has built on that contract.

RFC-036 (Remote Services) lets a `service()` declaration point at an
externally-running endpoint — typically a real pod in the customer's
k8s dev environment. The proxy machinery treats it like any other
upstream, faults are injected at the protocol layer, and the test
runs.

But the remote pod is **shared, mutable, and outside our control**:

- It can return different responses to the same request between runs
  (database state mutates, downstream feature flags flip, time-based
  responses change).
- It can be redeployed mid-test-suite.
- It can be unreachable from a developer's laptop the next day.
- The bytes it returned are not, today, captured anywhere.

A bundle produced by a run against `remote=` services therefore
**violates the RFC-025 contract**. `faultbox replay <bundle>` on a
different machine will either re-dial the remote (different bytes back,
or a connection refused) or fail entirely. The "one bundle, fully
reproducible run" promise is broken for any spec that uses a remote.

This is not a regression we can leave open — reproducibility is one of
Faultbox's core differentiators ("not just chaos engineering — every
failure is reproducible") and the customer demand for `remote=` is
real. We need a determinism story for remote runs that holds the same
shape as the rest of the contract.

## Why this is its own RFC

Five things make this design separate from "ship `remote=`":

1. **It's a contract change, not a primitive add.** It touches
   `manifest.json`, `env.json`, `replay.sh`, the bundle reader, and
   potentially the report renderer. Compare to RFC-036 which is one
   kwarg + one branch in `startServices`.
2. **Multiple legitimate approaches exist** — record-and-replay is one;
   contract pinning, schema snapshotting, and "explicitly non-deterministic"
   markers are others. The trade-offs are real and customer-dependent.
3. **Sensitive-data handling is non-trivial.** Recording a real pod's
   responses captures whatever it returns — credentials, PII, internal
   tokens. The redaction policy is its own design problem.
4. **Per-protocol fingerprinting is non-trivial.** What counts as "the
   same SQL query" is different from "the same Kafka message" is
   different from "the same HTTP request." Each protocol decoder needs
   a fingerprint definition.
5. **It interacts with `mock_service()`, `domain()`, and
   `fault_assumption()`.** A recording is conceptually a mock; the
   composition rules (can a fault rewrite a recorded response? does
   the recording capture pre- or post-fault bytes?) need design
   thought.

## Design options

Listing the options as a menu. None is committed.

### Option A: Record-and-replay (the original RFC-036 §Reproducibility section)

**Sketch:** the proxy snapshots every interaction with the remote
upstream into the run's `.fb` bundle. `faultbox replay <bundle>`
rewrites `service(remote=...)` to a synthetic `mock_service(routes=...)`
served from the recording. Identical fingerprint → identical response,
bit-for-bit.

**Pros**
- Reuses every existing primitive (`mock_service`, proxy, bundle).
- Self-contained: replay bundle has no external dependencies.
- Closest analog to what RFC-025 already promises — replay is replay.
- Customer demand is concrete (this is what most teams ask for first).

**Cons**
- Bundle size grows with response payloads — large JSON responses,
  binary protocols, paginated lists all bloat. Need a size cap +
  fingerprints-only fallback.
- Sensitive-data redaction must be correct by default or we ship a
  data-leak hazard. Default redaction list + opt-in extension list.
- Fingerprinting is per-protocol work (HTTP / gRPC / SQL / Kafka /
  Redis / Mongo / Cassandra / ClickHouse / NATS / HTTP2 / TCP / UDP).
- Captures responses post-fault by default — meaning a recorded run
  with `error(status=503)` records the 503, not the original 200.
  Replay reproduces *that run*, which is what we want, but is subtle.

**Cost:** medium-large. Per-protocol recorders, redaction engine,
replay-side spec rewriting, drift detection.

### Option B: Contract pinning + schema snapshot

**Sketch:** instead of recording response bodies, the bundle captures a
**contract signature** for each remote interaction (HTTP request +
response status + response schema, gRPC method + proto descriptor + a
hash of the response message). Replay verifies the live remote still
matches the signature; if it does, the test re-runs against the live
remote and the result is deemed reproducible. If the contract drifts,
replay fails loudly.

**Pros**
- Tiny bundles (signatures, not bodies).
- No sensitive-data exposure — we capture shapes, not values.
- Forces customers to write contract-conformant tests, which is
  arguably what "deterministic against a remote" should mean.

**Cons**
- Doesn't work offline — replay still requires cluster access. That's
  a meaningful regression from the RFC-025 promise.
- Doesn't help the case where the remote returns *different valid
  responses* to the same request (cache misses, time-dependent
  endpoints, A/B flags).
- The assertion "same contract = same test outcome" is unsound for
  many real systems.

**Cost:** medium. Per-protocol schema extraction; contract diff
engine; replay-side verification step.

### Option C: Hybrid — record by default, contract-only on opt-out

**Sketch:** Option A is the default; users with sensitive payloads or
size concerns flag specific interfaces with `record = "contract"`,
which falls back to Option B for those interfaces. Bundle layout
distinguishes the two.

**Pros**
- Lets users pick their own trade-off per interface.
- Matches how customers actually think about it ("payment responses I
  can't record; product-catalog responses I can").

**Cons**
- Two recording modes to maintain.
- More UX surface (per-interface kwargs, mixed-mode bundle layout).
- Risk of users picking wrong mode and either leaking data or losing
  determinism.

**Cost:** large. Both options' costs + the mixing logic.

### Option D: Explicit non-determinism marker

**Sketch:** declare the problem unsolved at the bundle level. `remote=`
services are flagged in `env.json` as `nondeterministic: true`;
`faultbox replay` errors loudly if a bundle has any nondeterministic
service unless `--accept-nondeterminism` is passed. We ship no recording
at all; users who want determinism use `mock_service()` (with the
contract they choose to encode).

**Pros**
- Zero new design surface. Ship RFC-036 as-is, document the gap, move on.
- Honest about the limitation.
- Customers self-select: those who care about reproducibility don't use
  `remote=`; those who don't, do.

**Cons**
- Cedes the reproducibility selling point for the use case that motivated
  RFC-036 in the first place.
- Makes `remote=` and `mock_service()` non-fungible (you have to pick at
  authoring time whether you want determinism), which fights the
  "swap one keyword" goal in RFC-036.

**Cost:** trivial. This is the no-op option.

### Option E: Server-side recording (cluster-side proxy)

**Sketch:** Faultbox ships a small in-cluster sidecar / mesh adapter
that records traffic to and from the remote pod, persisting it
somewhere durable (object storage, configmap, ...). Bundles reference
that recording by ID; replay fetches and serves it.

**Pros**
- Recording happens once per remote-pod-version, shared across many
  developer runs.
- Decouples recording size from individual bundle size.
- Aligns with future hosted-runner offering.

**Cons**
- Requires cluster install (CRDs, sidecars, RBAC) — exactly the
  "cluster-agnostic core" position RFC-036 took.
- Privacy model gets harder (shared recording is a shared dataset).
- Couples Faultbox's local CLI value prop to a cluster install.

**Cost:** very large. Cluster runtime + CLI integration + storage
backend + auth model.

## Sketch of cross-cutting questions

Whatever option we pick, several decisions show up:

1. **Pre-fault vs post-fault recording.** When a fault rule rewrites
   the response, do we record the original upstream bytes or the
   rewritten ones? Pre-fault preserves more information (the same
   recording can be replayed under different fault assumptions);
   post-fault matches what the SUT actually saw. Lean pre-fault — it's
   the more general primitive. RFC-024 already gives us the hook to
   capture pre-fault bytes.
2. **Order-sensitivity.** Match by fingerprint regardless of order
   (works for stateless lookups), or strict-ordered (works for
   stateful sequences)? Probably a per-protocol decision; fingerprint
   for HTTP/gRPC/SQL-read, ordered for Kafka/Redis-write.
3. **Redaction correctness.** Default redaction list (Authorization,
   Cookie, Set-Cookie, X-API-Key, JWT body claims) + per-service
   extension list. What's the test that says "I haven't leaked anything"?
4. **Drift surfacing.** When a recording exists and the live remote now
   returns something different, where does that signal land — a
   `replay_drift` warning in the bundle, a row in the HTML report, an
   alert in CI? Probably all three, gated on severity.
5. **Recording opt-out and bundle size cap.** Default on, opt out
   per-service, hard cap on bundle size with fingerprints-only
   fallback above the cap.
6. **What does `faultbox replay` mean for `remote=` bundles?** Does
   `replay` always serve the recording (offline-first) or always
   re-dial when a connection is available (live-first)? We probably
   need a flag.

## Customer signal needed

Before committing to an option, we want at least one customer running
on RFC-036 v1 (`remote=` without determinism support) for ~2 weeks. The
specific signals we'd use to decide:

- **Did they hit drift in practice?** Daily? Weekly? Never?
- **What broke when drift hit?** False fault-matrix outcomes, or just
  noisy retries?
- **What's the biggest payload they recorded?** (sizing the cap)
- **Did they need offline replay?** (CI builds that can't reach the
  cluster, or ones that can)
- **What did they redact, and why?** (real redaction list vs theoretical)

That data would point us to A, B, C, D, or E with much less guesswork
than we have today.

## Tentative recommendation (subject to the customer signal above)

Lean **Option A (record-and-replay)** with:

- Default-on recording, opt-out per-service via `record=False`
- Pre-fault byte capture (via the existing RFC-024 proxy hook)
- Per-protocol fingerprinting reusing existing decoders
  (`internal/proxy/sqlmatch` etc.)
- 50 MB per-remote bundle-size cap with fingerprints-only fallback
- Default redaction list + per-service `record_redact = [...]`
- `faultbox replay` defaults to offline-first (serve from recording);
  `--re-record` flag re-dials and overwrites the recording

It's the option most customers ask for first, and it preserves the
"single bundle, replay anywhere" property RFC-025 committed to. The
others stay viable if the customer signal contradicts this.

But — the point of this RFC being separate from RFC-036 is **we
shouldn't lock this in until we have signal**. The recommendation
above is a starting position, not a decision.

## Non-goals (for this RFC's discussion)

- **Recording from sources other than `remote=` services.** Local
  services are already deterministic via container image digests
  (RFC-030); there's no problem to fix there.
- **Promoting recordings into checked-in mocks.** That's a separate UX
  problem (canonicalization, generalization, diff workflow). Out of
  scope here even if Option A ships.
- **Server-side / hosted-runner architecture.** Option E is listed for
  completeness; building it is a 1.x conversation.

## Open questions (the actual ones we want input on)

1. Which option, or combination, fits Faultbox's positioning best?
2. Is offline replay ("works on a plane") a hard requirement, or a nice-to-have?
3. How much bundle-size growth is the customer willing to pay for determinism?
4. What's the redaction policy that's correct-by-default *and* doesn't
   block matching on replay?
5. Does the answer change if we assume RFC-036 customers are running
   `faultbox test` from CI (in-cluster, fast cluster access) vs from a
   developer laptop (over Telepresence, possibly offline later)?
6. Should `remote=` bundles refuse to be checked into git unless
   `record=` is explicit (forcing the determinism decision to be
   visible in the spec)?

## What lands when

This RFC produces a design document, not code, until the questions
above are resolved. Once an option is chosen:

- Drafting the implementation plan happens in a follow-up amendment to
  this RFC (or a successor RFC referencing it).
- Implementation is sequenced after RFC-036 v1 ships and we have
  customer signal.
- Target release is **TBD** — likely v0.14.x or v0.15.x once RFC-036
  is real.
