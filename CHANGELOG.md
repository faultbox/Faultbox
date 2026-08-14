# Changelog

All notable changes to Faultbox are documented here.

The format follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and the project adheres to [Semantic Versioning](https://semver.org/).

Per-release "What's new" pages live on the site at
[faultbox.io/releases/](https://faultbox.io/releases/).

## [Unreleased]

Next-version work is tracked in
[GitHub Issues](https://github.com/faultbox/Faultbox/issues).

### Fixed

- **Kafka `consume()` is reproducible at a fixed seed.** The default
  consumer group was the constant `"faultbox"`. A group's committed
  offsets live in the broker's `__consumer_offsets` and outlive the reader
  that wrote them, so the second test to consume resumed after whatever
  the first had committed — and a broker container kept across tests
  carried that state between whole runs. What a test saw depended on what
  had run before it, at any seed; the seed could never have fixed it,
  because the state is in the broker, not in Faultbox. The default is now
  scoped to (run, test), which has committed nothing, so the read starts
  at the beginning of the topic every time. A spec that is *about*
  consumer-group semantics passes `group=` and still gets a stable name.
  This makes the result reproducible; it does not isolate topic contents
  between tests, which needs a per-test topic.
- **The `topic` event source no longer shares one consumer group either.**
  The fix above was described as covering both consumer paths; it reached
  only the `consume()` step, and the observer kept the constant. Nothing
  is broken today — no builtin emits a `topic` source, so the path is
  unreachable — but wiring it up would have reintroduced the defect. Its
  default is scoped per (run, topic); an event source is built from a flat
  param map with no access to the running test, so it cannot match the
  step path's (run, test) exactly.
- Four cross-references in the v0.14.0 RFC-054 plan pointed at
  "Decision record" sections that RFC-054 never contained. They now name
  the sections that carry the findings.

## [0.18.0] - 2026-08-14

Field-report fixes from the first onboarding of a large production Go
service (the customer orders: Uber Fx, ~30 gateways, MySQL + Redis + Kafka).
Seven issues reported against v0.17.0, plus one found while building the
regression corpus for the first of them.

**Why a minor and not a patch.** Every change here is a fix, but three of
them change what an existing spec does:

- Faults written across multiple lines, or with spaces around `=`, now
  actually install a filter. A suite that was green because a fault was
  silently inert can go red on upgrade — which is the point, but it is
  not a no-op.
- `faultbox report` writes `report_<bundle>.html` instead of
  `report.html`. Anything scripted against the fixed name needs
  `--output`.
- `TIMEOUT_DURING_FAULT` no longer fires on a run with no faults; two new
  codes cover what it used to absorb. CI that greps for the old code sees
  a change.

Mock listeners also bind `0.0.0.0` on Linux now, as proxies already did —
an exposure change on a shared machine, overridable with
`FAULTBOX_PROXY_BIND`.

### Fixed — a dropped notification killed the seccomp supervisor

Reported as two separate issues — a host-binary SUT losing every
outbound socket, and the proxy data plane freezing at the test-phase
boundary. They were one defect, and the proxy was incidental.

`SECCOMP_IOCTL_NOTIF_RECV` returns ENOENT when the notification has
already been discarded: the notifying thread died or took a signal
before the supervisor reached it. It says nothing about the listener fd.
`isClosedFdErr` matched it anyway by substring-searching the errno text,
so the notification loop returned `nil` on the first one and stopped
supervising permanently. The caller suppressed its log line precisely
because a nil error reads as a clean shutdown, so **nothing was
printed**. The child kept its seccomp filter with nobody to answer it,
and every intercepted syscall from that point blocked forever.

That accounts for the whole reported shape: reads *and* new dials both
failing, never recovering, the container staying healthy while only the
SUT was affected, and a busy connection pool always losing while a quiet
client survived — ENOENT probability scales with concurrent notification
volume, and Go's `SIGURG` preemption interrupts syscalls constantly.

ENOENT is now a per-notification transient: counted, skipped, and the
loop keeps going. Only EBADF, EPIPE and a new `ErrListenerClosed`
sentinel end it, matched with `errors.Is` so classification cannot drift
with a wrapper's wording.

The `fs.ErrNotExist` branch was **deleted rather than modernized**:
`syscall.Errno.Is` maps ENOENT onto `fs.ErrNotExist`, so switching
`os.IsNotExist` to the modern spelling would have silently restored the
bug. Both the code and a test record this.

Silence was the expensive part, so a premature exit is now loud. If the
loop ends while its target is still running, the session records a
supervisor failure and the runtime fails the test with it — outranking
every other verdict, because a filtered process with no supervisor
produces no evidence. Dropped notifications are counted and reported at
teardown.

### Fixed — `fault()` written the normal way installed no filter

Found while building the regression corpus above, and the same class of
defect as the v0.13.3 silent no-ops.

Filter installation is decided by searching spec source for literal
substrings — `fault(db,` and `write=deny(`. Two idiomatic spellings
never matched, and both produced a fault that silently did nothing:

```python
fault(db, write = delay("1ms"), run = s)   # spaces around `=`

fault(                                     # split across lines
    db,
    write = deny("EIO"),
    run = scenario,
)
```

Measured: `write = delay(...)` gave `rule_count 0, seccomp false`;
`write=delay(...)` gave `rule_count 3, seccomp true`. The multi-line form
is what any spec looks like once a fault has more than two arguments.

Source is now folded into paren-balanced statements with whitespace
stripped before matching. It remains a heuristic — a fault built through
a variable or a helper function is still invisible to it, and
`FAULT_NOT_FIRED` stays the backstop.

### Fixed — `mock_service()` was unusable from a containerized SUT

Two independent gaps, either of which alone broke it. Mock listeners
hardcoded `127.0.0.1`, which no container can reach through the docker0
bridge — proxies solved this in RFC-035 and mocks were never brought
along. And the env builder classified a mock as "not a container", so it
fell through to `localhost`, handing the SUT
`FAULTBOX_<MOCK>_MAIN_ADDR=localhost:<port>` — which inside its own
namespace resolves to the SUT itself.

Mocks now bind via the shared platform-aware helper (`FAULTBOX_PROXY_BIND`
governs both) and resolve to `host.docker.internal` for container
consumers. They are also exempt from the RFC-035 fault gate on address
substitution: that gate exists because an unfaulted real service is still
reachable over Docker's DNS, which is not true of a mock.

### Fixed — containers left the Faultbox network after the first test

Reported as Docker's embedded DNS black-holing from the second test
onward. The mechanism was simpler and entirely ours: from test 2, the
containers were not on the network at all.

`EnsureNetwork` ran only at Docker-client init. `stopServices` removed
the network after each test and cleared the ID, but the client stayed
non-nil, so nothing recreated it — every later container launched with
an empty network ID onto the default bridge, which has no embedded DNS
for container names. Meanwhile `CleanupStale`, which removes
faultbox-prefixed networks, ran before *every* test; it could never have
been once-per-run where it sat, because the client is created lazily
during the first test, so it first fired on test 2 and destroyed the
network test 1 had created.

The sweep now runs once at client init, before anything of the run
exists. `EnsureNetwork` runs per container start (idempotent, and
self-heals if the network is removed underneath). `stopServices` leaves
it alone. Measured on `poc/demo-container`: 4 network creations before,
1 after.

### Fixed — timeouts blamed on faults that were not there

A spec with **zero faults** timed out on an image that would not pull,
and the diagnostics engine reported `[TIMEOUT_DURING_FAULT] test timed
out while faults were active`, suggesting retry loops and deadlocks in a
service that had never started. The classifier had no check that a fault
existed.

Three outcomes now, by what actually happened: `TIMEOUT_DURING_FAULT`
(unchanged, when a fault fired), `TIMEOUT_NO_FAULT_FIRED` (declared but
never hit), and `TIMEOUT_NO_FAULTS` (none at all, pointing at startup and
healthchecks).

An absent `:local`-tagged image now fails immediately with "build it
first" instead of spending the pull timeout to arrive at `denied:
requested access to the resource is denied`. The heuristic is
deliberately narrow — a missing registry host is not enough on its own,
since `mysql:8` and `postgres:16` have none either. Every other denial-
or manifest-shaped registry error carries the same hint appended.

### Fixed — `replay` on a spec in a subdirectory, and the hint it prints

Two bugs that compounded. `spec_root` records the path as typed on the
command line (`faultbox/spec.star`), but the bundle stores specs relative
to the root spec's own directory, so the root is archived under its bare
basename — joining the extraction dir with `spec_root` looked for a
directory the archive does not contain. A spec at the repo root makes the
two identical, which is why every spec in this repo always replayed.

And the `Replay:` hint printed after every failure carried `--seed N`,
which `replay` does not accept and cannot sensibly accept, since it takes
the seed from the manifest. Every copy-pasted suggestion failed on an
unknown flag before it could reach the path bug. The flag is dropped; a
test now walks the flags in the printed hint and checks each against what
`replay` parses.

### Fixed — packet faults never reached a containerized consumer

Reported as the netstack gateway attaching "on some runs and not others,
same spec, same seed", with the run correctly refusing to report a result
when it did not attach.

The gateway address for a container consumer is allocated inside a branch
gated on "does a **proxy** fault target this interface". Packet faults are
invisible to that question: the gate reads `fault_assumption` proxy rules,
and packet faults cannot be declared there at all — `partition()` and
`packet_*` are body-time calls recorded in a separate registry. A spec
whose only faults were packet faults therefore got no gateway address,
the gateway never attached, and the run ended at *"packet faults were
installed N time(s) but no netstack gateway was attached"*.

The gate no longer applies when the packet gateway is enabled: a spec
declaring `determinism(runtime = "gvisor")` has asked to be mediated at
the packet layer.

The reporter also asked for this to fail at setup rather than after the
body runs. That is not implementable as stated — packet faults are
body-time calls, so their arguments are validated inside the body, and
failing at setup replaces a spec error the author can fix (`source=`
naming the interface owner) with an environment one they cannot (no
`CAP_NET_ADMIN`). What was missing was the *reason*: the gateway is now
attached at setup purely to capture why it failed, and the failure
carries it.

The reporter's second half — that packet faults and a multi-test suite
were mutually exclusive — should be resolved by the supervisor and
network fixes above. Host ports were a workaround for the proxy freeze,
and container names failing from test 2 was the network being destroyed
and never recreated.

### Changed

- `faultbox report <bundle.fb>` derives its output name from the bundle
  (`run-<ts>-<seed>.fb` → `report_<ts>-<seed>.html`) instead of always
  writing `report.html`, so reporting a second bundle no longer silently
  overwrites the first. `--output` still pins a fixed name.

Next-version work is tracked in
[GitHub Issues](https://github.com/faultbox/Faultbox/issues).

## [0.17.0] - 2026-07-30

Agent-first surface, first slice — and detecting suites that cannot fail.

[RFC-052](docs/rfcs/0052-agent-first-surface.md) Gaps 1, 2 and 8, plus the
deprecation removals promised for v0.14.0 and never made.

### Added — `faultbox check`: validate without running

```
faultbox check spec.star [--format json] [--max-instances N]
```

Launches no processes, pulls no images, needs no Docker — milliseconds against
the tens of seconds a run costs. The runtime could always do this; it simply was
not exposed, so the only way to learn a spec was malformed was to run it.

Findings carry machine-readable codes and a suggested next move. Exit 0 for
clean or warnings-only, 2 for errors. Also available as the MCP tool
`check_spec`, which runs exactly the same code — a check that behaved
differently through MCP would give an agent a wrong model of the tool.

### Added — diagnostics for suites that cannot fail

Two new diagnostics, from a specific and uncomfortable finding: a CI spec
exercised a **broken Postgres client on every pull request for three releases
and passed**.

```python
env = {"POSTGRES_HOST_AUTH_METHOD": "trust", ...}   # removes the credential path

resp = pg.main.query(sql = "SELECT 1")
assert_true(not resp.ok, "expected failed query under injected fault")
```

It asserts the query **fails**. A client that cannot connect at all satisfies
that identically to the injected fault. Its own comment stated the intent — *"so
this test doesn't depend on authentication round-tripping"*. A careful test, and
the care is what hid the bug.

- **`NO_POSITIVE_CONTROL`** — an interface is stepped, but no test ever asserts a
  step on it *succeeds*. Suite-level, which is what makes it new: a single
  fault-injection test asserting failure is correct; a *suite* where that is the
  only assertion an interface receives proves nothing. No per-test lint sees it.
- **`TEST_NO_ASSERTIONS`** — a test passed having evaluated nothing.

Both are warnings. They found two vacuous specs in this repository, one of them
in the CI golden corpus, each carrying the same false belief in a comment:
*"the assertion is the absence of a panic"*. A failed step returns `ok = False`;
it does not raise.

A third diagnostic, `STEP_RESULT_DISCARDED`, was implemented and **cut** on its
own evidence: across 54 specs it produced 15 findings of which 13 were
legitimate side-effect steps. The rule that cut it was written down before the
measurement, so the result could not be rationalised afterwards.

### Added — machine-readable error taxonomy

Eight codes over spec-load and infrastructure failures — `SPEC_SYNTAX`,
`SPEC_LOAD_FAILED`, `SPEC_FORBIDDEN_LAMBDA`, `SPEC_RECIPE_NOT_FOUND`,
`HEALTHCHECK_TIMEOUT`, `LAUNCH_FAILED`, `DOCKER_UNAVAILABLE`,
`TRACE_HOST_NOT_REGISTERED` — each carrying the reader's next move.

Implemented as typed errors, not by matching message text. The shortcut would
reproduce the exact fragility this release removes: reword an error and the code
changes silently. So adoption is incremental, and `Classify` reports an uncoded
error as uncoded rather than guessing — a gap in the taxonomy is discoverable, a
wrong code is something an agent acts on.

Full reference: [docs/diagnostic-codes.md](docs/diagnostic-codes.md).

### Fixed — `FAULT_FIRED_BUT_SUCCESS` fired on correct specs

Its heuristic — a fault fired and the test passed — describes the single most
common correct shape in the tool: inject a fault, assert the service degrades
gracefully. It has been miscalibrated since v0.12 and nobody noticed, because
per-test diagnostics were never printed. Making them visible exposed it on the
first end-to-end run.

Now requires the test to have asserted nothing. With assertions present the
author checked the behaviour; with none they checked nothing.

### Fixed — per-test diagnostics were never printed

`FAULT_FIRED_BUT_SUCCESS` and its five siblings have existed since v0.12 but
were only ever written to JSON and the bundle. Anyone running `faultbox test`
interactively has never seen one. Warnings now print after the summary.

`TestResult.assertions` is also exposed in JSON, because a green result means
something different depending on whether anything was checked.

### Removed — the deprecations promised for v0.14.0

Deprecated in v0.13.0, then shipped in five further releases while warning
`Removal in v0.14.0.` Anyone reading carefully concluded the removal had already
happened.

| Removed | Use instead |
|---|---|
| `faultbox generate` | `faultbox plan --suggest` |
| `stdout()` / `stderr()` | `observe.stdout` / `observe.stderr` |
| `json_decoder()` / `logfmt_decoder()` / `regex_decoder()` | `decoder("json"\|"logfmt"\|"regex")` |

**The names still resolve**, to a stub that fails naming its replacement and the
release that removed it. Deleting them outright would give `undefined: stdout` —
true and useless, particularly to an agent working from documentation that
predates the change.

### Added — MCP contract tests

`faultbox mcp` had **no test coverage at all**, on the one surface whose declared
primary user is an agent. Now covered: every advertised tool dispatches, schemas
are valid and internally consistent, result shapes hold, malformed arguments are
rejected rather than panicking.

### Documentation

- [Driving Faultbox as an agent](docs/guides/driving-faultbox-as-an-agent.md) —
  the loop, the JSON shapes, the codes, and the trap that cost three releases.
- [Diagnostic codes](docs/diagnostic-codes.md) — every code and its remedy.
- `docs/positioning.md` records the agent-first premise, which RFC-052 cited and
  which was written down nowhere.

### Verification

`go build` + `go vet` + `go test -race ./...` green. `NO_POSITIVE_CONTROL` fires
on a reconstructed known-bad spec and stays silent on all eight
`poc/protocol-audit` specs; both new diagnostics show **zero false positives**
across 11 runnable specs.

## [0.16.1] - 2026-07-30

What the rest of the protocols were hiding.

Eleven fixes. Ten were pre-existing and invisible to the test suite; one was a
v0.16.0 regression of my own.

v0.16.0 fixed two Postgres bugs that had survived because no spec ever asserted
on the result of a database step. This release repeats that audit across the
other twelve protocols — a spec per protocol, real server, every result checked
([`poc/protocol-audit/`](poc/protocol-audit/)). It found more, including a bug
that was hiding behind the one v0.16.1 fixes first.

### Fixed — `ready()` could never succeed for eight protocols

A **v0.16.0 regression.** `ready()` resolved to `<protocol>://host:port` and
handed the whole string to the protocol plugin, but only `postgres`, `http` and
`http2` parsed a URL. The rest dialled it verbatim — attempting to reach a host
literally named `redis://localhost` — so the check burned its entire timeout
and reported the service not ready.

Affected `cassandra`, `clickhouse`, `grpc`, `mongodb`, `mysql`, `nats`, `redis`
and `udp`. Measured: Redis with `ready(timeout="60s")` failed at exactly 60 s
against a container that was serving in under a second; after the fix, 407 ms.

Address parsing is now shared (`protocol.ParseAddr`) instead of reimplemented
per plugin, which is what let the two halves disagree.

### Fixed — MySQL steps could not authenticate, or select a database

Pre-existing since the plugin was written, and the exact shape of the Postgres
bug fixed in v0.16.0. `buildMySQLDSN` emitted a bare `root@tcp(host:port)/` —
no password, no database — while the runtime computed both from the service's
`env=` and dropped them.

Against a stock `mysql:8` with `MYSQL_ROOT_PASSWORD` set, every step failed
with *Access denied for user 'root' (using password: NO)*, surfaced to the spec
as the far less legible `invalid connection`. Against a passwordless server,
statements failed with *Error 1046: No database selected*.

Measured: the audit spec went from a 181-second readiness timeout to a 9.8-second pass.

### Fixed — the MySQL proxy hung forever on every result set

**Found only because the credential fix let a step reach the command phase for
the first time.** `forwardResponse` forwarded one packet per iteration, then
peeked with a 100 ms deadline to decide whether more was coming. The peek
consumed the terminator, so the next iteration issued an unconditional,
deadline-free read for a packet the server would never send.

`exec()` was unaffected — a single OK packet returns before the loop — so the
bug was specific to `query()`. Every result set through the MySQL proxy wedged
permanently.

Worse, the hang landed in teardown: `Stop()` waited on the connection
WaitGroup, so one stuck handler hung the **whole run** after the test body had
already finished, which is why no per-test timeout fired and the process had to
be killed.

`forwardResponse` now parses the result set — column count, N column
definitions, rows, terminator — handling both the classic EOF framing and
`CLIENT_DEPRECATE_EOF` without needing the negotiated capability flags.

### Fixed — a stuck proxy connection can no longer hang a run

All twelve proxies called `wg.Wait()` unbounded in `Stop()`. Any handler
blocked on a read that never returns therefore hung teardown forever. Stop now
waits at most `ProxyStopTimeout` (5 s) and emits a `proxy_stop_timeout` event
when it abandons a handler — bounded, and not silent. The listener is closed
and the context cancelled by then, so an abandoned handler unblocks on its own
deadline.

The MySQL server-side read is also bounded now (30 s). A mis-parse should fail
a test with a legible error, not wedge a goroutine.

The Redis proxy also created a new `bufio.Reader` on the server connection per
command. A buffered reader reads ahead, so recreating it discarded anything
buffered beyond the current reply — silent data loss as soon as a client
pipelines or Redis answers two commands in one write. It is now created once per
connection.

### Fixed — `args=` was silently ignored for container services

Accepted by the spec loader, then dropped: it only ever reached binary mode. A
spec configuring a server through its command line —
`args = ["redis-server", "--requirepass", "secret"]` — got a default server and
no warning. It now overrides the image's `CMD` in both the plain and shim
launch paths.

### Fixed — dict and list step arguments were mangled

The runtime converted dict kwarg **values** with `starlark.AsString`, which
returns `""` for anything that is not a string. So every integer, float, bool,
nested dict and list inside a dict silently became an empty string:

```python
db.main.insert(collection = "t", document = {"id": 1, "payload": "row-1"})
# stored {"id": "", "payload": "row-1"}
```

It hid because the mangling was **self-consistent**: a filter of `{"id": 1}` was
flattened the same way, matched the mangled document, and the round trip looked
correct. `docs/protocols/mongodb.md` documents dicts as encoded to BSON on the
wire, and for anything but strings they were not.

List kwargs fared worse — they fell through to `v.String()` and reached plugins
as Starlark source text, so:

- `mongodb.insert_many(documents=[...])` could never satisfy its own `[]any`
  type assertion; the path had never run.
- Redis's documented `command(cmd="EXPIRE", args=["user:1", "3600"])` form
  silently dropped every argument.

Conversion now goes through the same recursive `starlarkToGo` used for mock
bodies. Values it cannot represent keep the previous string rendering rather
than erroring.

### Fixed — the NATS proxy corrupted every line it forwarded

It read with a `bufio.Scanner` (whose `ScanLines` strips the trailing `\r`) and
wrote with `fmt.Fprintln` (which appends only `\n`). NATS frames on **CRLF**, so
every control line lost a byte in transit. The Go client reported the memorable
`nats: expected 'PONG', got 'PONG'` — same text, different framing — and
publishing failed outright with `EOF`.

It also split length-prefixed `PUB`/`MSG` payloads on newlines, corrupting any
payload containing one and letting payload bytes be mistaken for a protocol
verb. A dropped message left its payload behind, desynchronising the connection.

The relay is now byte-oriented: control lines keep their terminator, payloads
are read by declared length and forwarded opaquely, and dropping a message
drops its payload with it.

### Fixed — `ready()` made a single attempt for MongoDB and Cassandra

Both retried the TCP connect — which succeeds the instant Docker's port proxy
binds — and then made exactly **one** protocol-level attempt, at the moment the
server is least likely to be up. MongoDB failed after ~2 s against a
`ready(timeout="90s")`; Cassandra, which needs a minute or more, could never
have passed.

Both now retry to the timeout through a shared `ReadyAfterTCP` helper, which
also fixes a doubled budget: the old shape gave the TCP phase and the protocol
phase a full `timeout` each, so `ready(timeout="90s")` could take 180 s before
reporting failure.

Cassandra's `gocql` logger is also filtered the way the MySQL driver's already
was — a healthy 60-second cold start emitted a dozen lines of
"unable to dial control conn" before passing, which reads as failure.

### Fixed — NATS `publish` reported success without confirming delivery

`nc.Publish` queues into a client-side buffer; it returning nil means "queued",
not "delivered". The flush that makes it a round trip had its error discarded,
so a publish that never reached the server still reported `ok = True`. The step
now fails with `publish not confirmed by server`.

`ready()` for NATS was also a bare TCP connect, which reports ready the moment
nats-server binds — before it will serve. Roughly one run in four, the first
publish failed with a bare `EOF`. It now completes NATS's own handshake and
flushes, so "ready" means the server answered.

### Fixed — every container leaked its anonymous volume

`RemoveContainer` passed `Force: true` but not `RemoveVolumes`. Every stock
database image declares a `VOLUME` for its data directory, so each container
Faultbox started got a fresh anonymous volume that outlived it — one per test,
forever.

Measured on the dev VM after a session of protocol-audit runs: **290 orphaned
volumes, 18.7 GB**, which filled a 30 GB disk. Every container after that failed
with `no space left on device`, and because the errors named whichever spec ran
next, the shape was a flaky test rather than a leak three specs earlier.

Only anonymous volumes are affected; named volumes and the host bind mounts that
`volumes=` produces are untouched. On an older build, recover with
`docker volume prune -f`.

### Fixed — flaky `netfault` fake clock (test-only)

Two independent causes, one symptom.

`fakeClock.Advance` fired only the timers armed at that instant. The defer queue
arms one timer and re-arms after each release, so the re-armed deadline was
computed from the already-advanced clock and never fired — the test reported
"delivered 12 of 20", which reads as a lost packet rather than a stalled clock.
Advance now drains to a fixed point.

The settle step in `advanceWhenArmed` also allowed only 2 s for the queue's
background goroutine to absorb the delivered packets. That is a bound on a wait,
not an assertion about speed, and it was tight enough to fail under CPU
contention during a full `-race ./...` run. Raised to 15 s, which costs nothing
when the queue is fast.

Honest status: after the Advance fix the failure appeared once more, under full-
suite load, and then not in 28 further runs — the signature of a load-sensitive
threshold rather than remaining logic error. Six consecutive full `-race` suite
runs are clean after both changes. The ordering assertions themselves are driven
by the fake clock and were never timing-dependent.

### Added — credentials for the remaining protocols

Declared once in `env=`, using each image's own published convention, and used
by steps, healthchecks and `ready()` alike. An explicit `user=` / `password=` /
`database=` on a step still wins.

| Protocol | Read from `env=` |
|---|---|
| MySQL | `MYSQL_USER` + `MYSQL_PASSWORD`, else `root` + `MYSQL_ROOT_PASSWORD`; `MYSQL_DATABASE` |
| Redis | `REDIS_PASSWORD`, `REDIS_USER` — sent as `AUTH` before each command |
| MongoDB | `MONGO_INITDB_ROOT_USERNAME`, `MONGO_INITDB_ROOT_PASSWORD`, `MONGO_INITDB_DATABASE` |
| ClickHouse | `CLICKHOUSE_USER`, `CLICKHOUSE_PASSWORD`, `CLICKHOUSE_DB` (HTTP basic auth) |
| Cassandra | `CASSANDRA_USER`, `CASSANDRA_PASSWORD`, `CASSANDRA_KEYSPACE` |
| NATS | user/password from the address |

Two details worth knowing:

- **MongoDB authenticates against `admin`, not the database being read.**
  Credentials live in the database that holds the user, which for the account
  the official image creates from `MONGO_INITDB_ROOT_USERNAME` is always
  `admin`. Override per step with `auth_source=`.
- Credentials reach both the healthcheck and the steps. Plugins are handed an
  address from two places and only one carries credentials — `Healthcheck` gets
  the URL `ready()` builds, `ExecuteStep` gets a bare `host:port` plus kwargs —
  so a plugin reading only the address would authenticate during the healthcheck
  and anonymously during every step. `protocol.CredentialsFor` resolves both in
  one place.

### Documentation

The three releases since v0.13.3 had outgrown the reference docs.

- **`faultbox setup-trace` was missing from the CLI reference entirely** —
  added, with flags and the one-time host-setup flow.
- `docs/feature-manifest.md` had no rows for anything in v0.14.1 or v0.16.0
  (`watch()`, `setup-trace`, `ready()`, `sleep()`, `bandwidth()`, `mtu()`,
  packet faults). Added. One existing row claimed the Postgres proxy "bypassed
  auth because it intercepts pre-backend" — that was wrong, and is corrected.
- All nine protocol pages that showed a healthcheck recommended `tcp()`; they now use `ready()` and
  document credentials. `docs/protocols/mysql.md` documented a topology its own
  client could not connect to.
- `docs/guides/spec-patterns.md` gains **Pattern 0: assert on every step** — the
  habit both credential bugs needed in order to survive, including the subtler
  form where a `watch()` audit passes on a service's boot I/O while its workload
  fails silently.
- `docs/troubleshooting.md`: two new entries keyed on the errors these bugs
  actually produce, and entry 3 no longer says Faultbox has no query-based
  healthcheck.
- `docs/index.md` rewritten — it listed 4 of ~40 pages and described a
  "12-chapter" tutorial that now has 30.
- 36 broken internal links fixed (40 → 4; the remaining four are citations
  in an April design doc to documents that were never in this repo).

### Verification

`go build` + `go vet` + `go test -race ./...` green; cross-compile clean on
linux/amd64, linux/arm64 and darwin/arm64.

`go test -race ./...` ran six consecutive clean full-suite iterations, to
distinguish the fixed flakes from luck.

End-to-end in Lima on kernel 6.8:
`poc/protocol-audit/{postgres,mysql,redis,redis-auth,mongodb,clickhouse,nats,cassandra}.star`
against real containers, plus `poc/example`, `poc/demo-container` (which
exercises the shim path) and `poc/kafka-rfc014`. NATS ran 18 consecutive clean
times to confirm its intermittent failure was gone.

The volume-leak fix was verified by the corpus itself: eleven container-heavy
specs ran back to back and left the VM at **0 volumes and unchanged disk use**.

One `poc/demo-container` test failed once inside a batch and passed 3/3
standalone afterwards; its reason was not captured, so it is recorded as an
unexplained transient rather than diagnosed.

Ten of the thirteen step protocols are now covered by a real-server spec.
**http2, udp and grpc still have unit coverage only** — no server spec exists
for them yet, and given what a real server found in the other four, that is a
gap rather than a formality.

The first pass of this audit could not reach MongoDB, Cassandra, ClickHouse or
NATS because Docker Hub was unreachable from the dev VM. Restoring that
connectivity is what surfaced the dict/list mangling, the NATS framing bug, the
single-attempt readiness checks and the MongoDB `authSource` bug — every one of
which was invisible to unit tests and would have shipped behind a "shares the
same code path" argument.

## [0.16.0] - 2026-07-29

Filesystem observation — and two bugs found by being the first thing to assert
on a database.

`watch()` closes `fs-unmediated`, a determinism category that had emitted no
events since RFC-040: Faultbox could count a service's `write` calls but not
name the file. Building a corpus for it turned out to be the first spec that
ever checked the result of a postgres step, which is how the other two landed
in this release.

### Fixed — postgres steps could never authenticate

Two bugs, both predating this release and confirmed against v0.15.0.
`pg.sql.exec()` has never worked against a realistically configured Postgres.

- **The connection string carried no credentials.** `buildConnStr` emitted
  host/port/sslmode only, so lib/pq fell back to the OS user running Faultbox —
  `root` under sudo, which exists in no Postgres installation. Credentials now
  come from the service's own `env=` (explicit step kwargs win; `connstr=`
  remains the escape hatch).
- **The auth handshake was relayed in one direction.** The proxy pumped
  server→client only, which suffices for trust auth. Every method requiring a
  client answer — **SCRAM-SHA-256, the postgres:14+ default**, plus MD5 and
  cleartext — deadlocked until the client's 60-second read deadline produced
  `connection reset by peer`.

It survived because no spec asserted on the result of a postgres step. The
RFC-056 corpus initially did not either: it observed 125 file paths and passed
while every one of its SQL statements was failing — the paths came from
Postgres's own boot.

### Added — `ready(timeout=)`, protocol-aware readiness

`tcp()` asks whether something is listening. For a container that is true the
moment Docker's port proxy binds, before the service starts: measured at
**0 ms** against a Postgres needing ~10 s. `ready()` asks the service instead,
using its interface's protocol plugin and the declared credentials — for
Postgres a real `SELECT 1`, retried to the timeout.

On the RFC-056 corpus: `tcp()` plus a hand-tuned `sleep("25s")` took 25 s per
test and was correct by guesswork; `ready()` takes ~2.4 s and is correct by
construction.

### Added — filesystem observation (RFC-056, v0.16.0)

- **`watch(service, files=, ops=, run=)`** reports a service's file I/O with
  **resolved paths**. Faultbox could already count `write` calls; it could not
  reliably say *which file*, because a syscall carries a descriptor and
  resolving it meant reading `/proc` out of band, racing the SUT. This closes
  `fs-unmediated`, a determinism category that had emitted no events since
  RFC-040 — file I/O outside declared paths was **silently undetected**.

  Built in v0.14.0 and withdrawn before release: `runsc trace create`
  instruments only tasks created *after* it attaches, so a network-driven
  query against a running Postgres produced **2** trace points where the same
  SQL from a freshly spawned process produced 1054. A watch that observes
  nothing still runs, and every assertion under it still passes.

  `--pod-init-config` installs the session at sandbox boot instead, so every
  task is traced from its first instruction — **236** points on that same
  workload, and 11,295 before any query at all.

- **`faultbox setup-trace`** — one-time host registration, since the flag lives
  in `daemon.json` rather than per container. Idempotent, reports every change
  *and what it left alone*, and prints the Docker restart rather than
  performing it: that restart stops every container on the host, so it belongs
  to the operator. A test run never edits Docker configuration.

- **Completeness is enforced, not assumed.** The canonical assertion is
  negative — *"never wrote outside its data directory"* — and is only as strong
  as the trace behind it. A run **fails** when no sandbox connected, when
  points were dropped during the window, when points matched no launched
  service, or on a decode error. Measured: the sink drops between roughly 17k
  and 47k points/second. Drops from before the window — a database's own boot
  — do not count against it.

- **`read`, `close` and `connect` are opt-in** (`setup-trace --with-read`
  etc.). Measured on a read-heavy workload, enabling reads took a run from
  25,015 points and **zero** drops to 48,576 and **1,488** — which, under the
  rule above, would fail the test. Asking for an op the host does not send is a
  spec-load error naming the flag, not a silent empty result.

Observation only: a trace point fires *after* the syscall, so short writes,
torn writes and `fsync` lies need a datapath that can change what the SUT sees.
gVisor has no `fsync` point at all, so write *ordering* is provable and
durability is not — `ops=["fsync"]` is rejected rather than quietly returning
nothing.

## [0.15.0] - 2026-07-29

Contract-driven clients. Faultbox already read your OpenAPI and protobuf
contracts to build the dependencies you can't run; this release reads them
from the other side, to build the callers that drive your service — and
makes each caller a named actor in the trace rather than one anonymous
`test` lane.

The half that isn't ergonomics: with the contract loaded caller-side,
"under fault, the service still returned what it published" becomes a
one-kwarg check. Degraded-but-schema-invalid responses and undeclared
status codes surface as findings without anyone guessing which field to
assert on.

### Added

- **Contract-driven clients — `client()`
  ([RFC-055](docs/rfcs/0055-clients.md), [#149](https://github.com/faultbox/Faultbox/issues/149)).**
  A new topology entity that turns an OpenAPI 3.x document or a protobuf
  `FileDescriptorSet` into a **named caller** bound to a service interface,
  with its operations generated as callable attributes:

  ```python
  orders = service("orders", interface("public", "http", 8080), image = "orders:2.1")
  mobile = client("mobile-app", target = orders.public, openapi = "./orders.yaml",
                  validate = "response")

  def test_order_flow():
      r = mobile.get_order(order_id = 42)     # no path, no verb, no field names
  ```

  This is the caller-side inversion of the loaders RFC-021 and RFC-023 already
  shipped for mocks — the same document can drive a `mock_service()` (callee)
  and a `client()` (caller).

  - **Clients are first-class trace actors.** Calls emit `client_call` /
    `client_return` on the *client's* own swim lane with its own vector-clock
    participant, so three named callers against one API render as three lanes
    instead of one anonymous `test` driver. The events work as
    [temporal anchors](docs/temporal.md) with no new matcher syntax:
    `match.event(type="client_return", client="gRPC-Orders", success="false")`.
  - **Contract conformance is assertable.** `validate="response"` checks each
    response against the schema declared for its status code and records the
    verdict on `resp.contract_ok` / `resp.contract_error`, emitting a
    `contract_violation` event. It deliberately does **not** raise — a contract
    violation under fault is usually the finding, not a harness error. An
    undeclared status code counts as a violation, which is how the undocumented
    degraded path surfaces.
  - **Composes with everything.** Client calls dial through the same proxy
    resolution as step methods, so `fault(iface, ...)` applies unchanged; TLS
    (RFC-038) and remote targets (RFC-036) are inherited from the interface.
    Clients are trace actors, not processes — they take no seccomp filter and
    are never a fault target.
  - `faultbox inspect --clients <spec.star>` prints each client's generated
    operation table.
  - `Response` gains `.client`, `.operation`, `.contract_ok`, `.contract_error`.

  v1 is unary-only (streaming gRPC methods are skipped, the rest of the
  descriptor set still loads), stateless per call, and does not synthesize
  request data. See [spec-language.md](docs/spec-language.md#contract-driven-clients-rfc-055).

### Changed

- `interface(name, protocol, port, spec=)` is now **read**. The kwarg has been
  parsed and stored since early versions with nothing consuming it; a `client()`
  that declares no contract of its own inherits it, choosing the loader by
  extension (`.yaml`/`.yml`/`.json` → OpenAPI, `.pb`/`.desc`/`.protoset` →
  descriptor set).
- The **dict-filter** form of `events()` (`events(service=…)`) now returns
  `client_call`, `client_return`, and `contract_violation` events.
  `step_send` / `step_recv` remain excluded there — admitting them would
  silently change what existing specs see. The `where=` form has had no type
  gate since v0.14.1 and already returns every family.
- HTTP `step_recv` events carry a `content_type` field, and the HTTP protocol
  plugin accepts `HEAD` / `OPTIONS` (an OpenAPI document may declare them).

### Fixed

- `mock_service(openapi=)` and `mock_service(descriptors=)` resolved relative
  paths against the process working directory rather than the spec's own
  directory, so `"./api.yaml"` only loaded when `faultbox` happened to run from
  the directory containing the spec. Both now resolve spec-relative, matching
  `load_file()`, `build=`, and `client()`.
- `service()` and `client()` now reject a name already taken by the other.
  Both are trace actors keyed by name, so sharing one silently folded two
  participants into a single lane and vector clock.

## [0.14.1] - 2026-07-29

Searching fault *timing*, and fixing what that search exposed.

v0.14.0 shipped packet faults and a Raft harness that could express a partition
but not vary **when** it lands. Closing that gap took one new primitive — and
turned up four defects, one of them a documented fault that silently did
something else. So this release is one feature, one deferral delivered, and a
set of corrections that matter more than either.

### Added
- **`sleep(duration, clock="wall")`** — a wall-clock wait that is indifferent
  to event traffic. Faultbox had two ways to wait and both were conditional on
  the SUT: `await_stable()` returns on quiescence, `await_event()` on a
  matching event. Neither can hold a fault open for a fixed time, and
  `await_stable` fails at it in exactly the situation the wait is for — **an
  active fault emits the events that prevent quiescence.** Measured on a 3-node
  `hashicorp/raft` cluster under partition: 6681 events in three minutes,
  longest quiet gap 338 ms, so every window above ~340 ms blocked until the
  per-test deadline and reported INCONCLUSIVE. A fault-timing search that
  should have run 18 configurations ran 6 and hung on 12, spending 36 minutes
  to produce no signal; with `sleep()` the same search runs all 18 in about two
  minutes. See
  [the experiment](docs/design/2026-07-28-timing-exploration-experiment.md).

  `sleep("0ms")` is a no-op rather than an error — deliberately unlike
  `await_stable`, whose window must be positive — because the primary use is as
  a `choose()` axis and "no delay" is that axis's natural baseline. A sleep
  longer than the test's remaining budget is refused up front, naming both
  numbers, instead of running into the deadline and reporting a bare timeout.
  It emits no event (a supervisor-side wait is not something the SUT did, and
  emitting one would reset the quiescence timer of an `await_stable` in a
  `parallel()` branch) and is rejected inside `monitor()` / `assume()`
  predicates like the `await_*` family.

- **`bandwidth(rate, dir="both", queue="250ms")` and `mtu(size)`** — the two
  link shapers deferred from v0.14.0. They take no matcher: a `packet_*` rule
  says what happens to packets that look like a certain way, a shaper says what
  kind of link this is.

  `rate` accepts bit units (`"1mbit"`, `"512kbps"`) or byte units, which must
  say so explicitly (`"2MB/s"`). A bare `"1000000"` is **rejected** — it could
  be bits or bytes, and guessing would be a silent factor-of-eight error in the
  one number the fault depends on. `queue=` bounds the backlog in *time*, so
  the shaper drops when saturated the way a real bottleneck does; a rate
  limiter with an unbounded queue is a memory leak with latency, and the sender
  never observes the congestion a congestion-control bug needs to surface.

  `mtu()` overrides the link MTU, so netstack advertises a smaller TCP MSS and
  fragments — a real small-MTU path. v0.14.0 had to approximate this with
  `packet_drop(len_gt=576)`, which drops oversized packets: that looks like a
  black hole and behaves like nothing real. Sizes below the IPv4 minimum of 68
  are rejected.

  Both are gateway-wide (one TUN link, one NIC — a per-target knob would be one
  that lies), both are cleared at test end, and both **error** on
  `runtime="default"` rather than silently no-opping. Each run records what the
  shaper did — `bandwidth_stats dir=c2s admitted=76 dropped=0
  peak_backlog=38ms` — so "the link was configured slow" can be distinguished
  from "the link was the bottleneck".

### Fixed
- **`packet_delay(dir="s2c")` was a silent drop, not a delay.** Egress builds a
  batch list, hands `release` a closure that appends to it, writes the batch
  when the loop ends and frees it on return — so a packet released *later*, by
  the defer queue's timer, was appended to a list nobody would ever write.
  `packet_reorder` on egress had the same fate, and bandwidth pacing would have
  inherited it. Both existing delay tests drove the ingress path, where release
  goes straight to the dispatcher, so nothing caught it. A fault that silently
  becomes a *different* fault is the exact failure this release line exists to
  prevent. Late releases now take a direct path to the wire.
- **The packet gateway's TUN device no longer bricks a host when a run is
  interrupted.** The device was the shared constant `faultbox0` and it is
  *persistent* — it survives the process. Two consequences, both live in
  v0.14.0: concurrent runs on one machine collided, and any run that died
  without teardown left a device that failed **every later packet-fault run**
  with `TUNSETIFF faultbox0: device or resource busy`, recoverable only by
  `sudo ip link delete faultbox0`, documented nowhere. Three layers now:
  devices are named per-process (`fbox<pid>`), so a leak is clutter rather than
  an outage; `SIGTERM` is handled alongside `SIGINT` (`timeout`, CI runners and
  Docker all send SIGTERM — the most common way to stop a run was the one that
  leaked); and every run sweeps orphaned `fbox<pid>` devices whose owning
  process is gone, which is what recovers a machine after a `SIGKILL` no signal
  handler can catch. Devices owned by live processes are never touched.
  New [troubleshooting §13](docs/troubleshooting.md).
- **An interrupted suite no longer invents verdicts for tests it never ran.**
  `RunAll` had no cancellation check anywhere in its loop, so Ctrl-C cancelled
  the context and the walk continued: each remaining test started its services,
  inherited an already-dead context, failed instantly, and was recorded as
  INCONCLUSIVE. An 18-leaf search interrupted after one leaf reported
  `1 passed, 17 inconclusive`. It now stops and says what did not happen —
  `2 passed, 0 failed, 1 inconclusive, 15 not run (interrupted)` — with the new
  `SuiteResult.Aborted` counter kept **distinct** from `Inconclusive`, since
  "the test was indeterminate" and "the test never started" call for different
  responses.
- Data race in `netfault`'s test `fakeClock`: the defer-queue goroutine called
  `Stop()` while the test goroutine read `stopped` under the clock mutex. It
  did not fire during the v0.14.0 release gate and did fire here.
- **`faultbox plan` was blind to `choose()`**, the construct most likely to
  blow a budget. The runtime discovers axes by *executing* a test body once;
  `plan` is static and executes nothing, so every `choose()`-driven test counted
  as a single instance — the exploration spec that runs 24 leaves reported
  `Total: 2 plan instances`. `--check-cost --max-instances N` exists to catch
  fan-out blowups before they run and was under-reporting by 12×, which is
  worse than having no gate, because a gate is trusted. Axes are now read
  statically from the spec AST and multiplied into `Instances`, and the tree
  shows where the cost comes from:

  ```
  ├── test "test_transfer_timing"  [def]
  │   ├── 18 instances
  │   └── choose
  │       ├── warmup: [0, 10, 40]
  │       ├── gap: [0ms, 400ms, 1200ms]
  │       └── hold: [100ms, 900ms]
  ```

  Where an option list is computed rather than literal, its size is not
  knowable without running the spec. That axis is reported as
  `(computed — size unknown)` and the total becomes `at least N` — an estimate
  that admits its gaps, rather than one that quietly rounds down. A `choose()`
  reached through a helper function is likewise not attributed to the calling
  test: static analysis cannot resolve the call graph, and inventing axes would
  be worse than missing them.
- **Inconclusive verdicts now print their reason.** `TestResult.Reason` has
  always carried the explanation — `test timeout: body did not complete within
  3m0.1s` — and the summary discarded it, leaving a bare `12 inconclusive`.
  Diagnosing one such run took 36 minutes of log archaeology to establish that
  every leaf had blocked in the same `await_stable`; the answer was in the
  struct the whole time. Identical reasons are grouped with a count.

## [0.14.0] - 2026-07-28

Packet-level network faults. Faultbox mediated at two layers — individual
syscalls and parsed L7 protocol messages — with nothing in between. A packet
was not an object anywhere in the codebase, so "drop this TCP segment",
"delay every ACK from the server" and "advertise a zero receive window" were
inexpressible. RFC-054 adds that layer using gVisor's userspace TCP/IP stack
(`gvisor.dev/gvisor/pkg/tcpip`) as a plain Go dependency — no fork, no runsc.

The capability that motivates it: **a dropped packet sends no RST.** Today's
`drop()` closes the connection, and well-written clients handle `ECONNRESET`
correctly on the first try. A socket stuck in `ESTABLISHED` writing into a void
until a keepalive fires — the failure that actually takes production down —
was unreachable. It is now one line.

### Added
- **Packet faults** (RFC-054): `packet_drop`, `packet_delay`, `packet_reorder`,
  `packet_duplicate`, `packet_corrupt`, `packet_reset`, `packet_window`,
  `packet_pass`. Opt in with `determinism(runtime = "gvisor")`; Linux with
  `CAP_NET_ADMIN` required (use the Lima VM on macOS).
- **Packet matcher**: `dir`, `proto`, `flags` (`"PSH,ACK"`, `"!RST"`), `port`,
  `len`/`len_gt`/`len_lt`, `payload_prefix`, `payload_contains`, plus the
  occurrence selectors `nth`/`after`/`every` and the RFC-042 §8.9
  `probability`/`max_fires`/`mode` semantics, identical to syscall faults.
- **`where=` lambda escape hatch** with a read-only `Packet` value (`proto`,
  `dir`, `src_ip`, `dst_ip`, `src_port`, `dst_port`, `len`, `payload`,
  `payload_bytes`, `flags`, `seq`, `ack`, `window`, `index`, `flow`).
  Declarative kwargs are evaluated first, so a lambda refines a cheap filter
  rather than replacing it.
- `packet` trace events carrying `action`, `direction`, `protocol`, `src`,
  `dst`, `len`, `flags`, `flow`, `label`.
- Scenario corpus at `poc/gvisor-rfc054/faultbox.star` — 12 runnable scenarios,
  each asserting its fault actually fired.
- Tutorial chapter: [Packet-Level Faults](docs/tutorial/03-protocol-level/27-packet-faults.md).
- `internal/gvisor/seccheck`: decoder for gVisor's trace-point protocol,
  shipped and tested ahead of the `watch()` primitive it will serve in v0.14.1.

### Fixed
- **`**` path globs now cross directories.** `op(path = "/data/**")` matched
  *nothing*: `MatchPath` used `filepath.Match`, which cannot cross a path
  separator, so a rule targeting a database that nests its files never fired —
  no fault, no diagnostic, test passed. New `internal/pathmatch` supports `**`,
  `?`, `[a-z]` classes and escapes. The change is a widening: every pattern
  that matched before still matches, cross-checked by two back-compat tables.
- **`events(where = ...)` was blind to most event types.** It filtered to
  `{syscall, stdout, topic, wal}` *before* invoking the lambda, so a predicate
  naming any other family silently matched nothing — including
  `events(where = lambda e: e.type == "proxy" ...)`, which
  `docs/spec-language.md` has always documented and which could never have
  worked. `unmediated_io` (RFC-040) had the same problem. The lambda is now the
  filter; the dict-filter path keeps its allow-list.
- A path-filtered rule that matched nothing now says *why*: whether the service
  did no matching I/O, or whether path recovery from `/proc` failed (it races
  the SUT and truncates at 256 bytes). Those were indistinguishable and need
  different fixes.

### Changed
- `determinism(runtime = "gvisor")` is accepted; it was reserved syntax that
  errored. **It does not raise the determinism ceiling** — both runtimes cap at
  L1, and `level = "L2"` still errors at spec load. What widens is the mediated
  surface, not the promise.
- Container launches accept an OCI runtime override. The Docker default stays
  `runc`; nothing changes for specs that did not ask for gVisor.

### Fixed — peer-mesh topologies
- **Packet faults now reach a peer mesh.** Gateway address allocation was gated
  on a *proxy* address existing, and `preStartProxies` only gives an
  interface's proxy to services launched afterwards. A mesh is a cycle, so for
  at least one link the proxy is always absent when the consumer's env is
  built — that link went unmediated and packet rules installed into a link no
  traffic crossed. Measured on a 3-node `hashicorp/raft` cluster: a leader cut
  off from both followers committed **88** applies before, **0** after.
- **`source=` reaches rule installation.** It was parsed, stored and emitted
  into the trace but dropped before the rule was installed, so
  `fault(kafka.main, source=worker, drop(...))` — the documented example — fired
  for every consumer. Rules now scope to the `(consumer, service, interface)`
  triple, which is what makes pairwise and one-way partitions expressible.
- **`partition()` rebuilt on the packet gateway**, with `direction=` and
  `partition_start()` / `partition_stop()`. The old implementation denied
  `connect()`, which only blocks connection *setup* — against any service that
  pools connections it silently did nothing. It is **not** downgraded under
  `runtime="default"`; it errors.
- **`EventVal` gained `.data` / `.fields`.** The documented `monitor()` example
  (`event.data.get("level", "")`) could not run: only `StarlarkEvent`
  auto-decoded, so monitors failed with *"string has no .get field or method"*.
- A service's own bind addresses are no longer rewritten by the dial-address
  substitution table. `RAFT_BIND="127.0.0.1:8301"` became a gateway address and
  the service tried to bind an address it does not own.
- The documented `source=` example did not parse — it put `source=` ahead of
  the positional fault rules, which Starlark forbids.

### Known limitations
- **`watch()` (filesystem observation) is deferred to v0.14.1.** The sink and
  DSL are complete, but `runsc trace create` instruments only tasks created
  *after* the session starts, and Faultbox attaches one after the service is
  healthy. Measured: 2 trace points for a network-driven workload against a
  running Postgres backend, versus 1054 for the same work from a newly-spawned
  process. A `watch()` would observe almost nothing while every assertion under
  it still passed, so it fails at spec load rather than shipping with a caveat.
  The fix is runsc's `-pod-init-config`. See RFC-054 decision record M5.
- **`bandwidth()` and `mtu()` are deferred to v0.14.1** — link-scoped shapers
  needing a token bucket and real fragmentation handling, not per-packet rules.
- Packet faults act below TLS, so corrupting an encrypted stream yields a MAC
  failure rather than a semantic corruption.
- Packet faults do not yet participate in `fault_matrix()` fan-out.
- `gvisor.dev/gvisor` is **pinned**. Its HEAD does not build as a Go module
  dependency (`pkg/tcpip/stack` carries two different external test package
  names in one directory); CI asserts the pin still builds on all targets.

## [0.13.3] - 2026-07-14

Bug fixes and stricter spec loading, from the July 2026 documentation audit.
Several previously silent no-ops (mis-scoped drops, a dead Kafka `duplicate`,
unknown errnos/kwargs) now either work correctly or fail loudly at spec load.

### Fixed
- Five bugs from the July 2026 doc audit (#137-#141): `drop(query=/command=)`
  no longer drops all traffic; Kafka `duplicate(topic=)` actually re-sends the
  produce; unknown errnos and unknown `service()` kwargs fail at spec load
  instead of being silently ignored; proxy events carry `action`/`protocol`.

### Changed
- **Stricter spec loading.** `deny()` rejects errno names outside the supported
  table (widened with the documented ENOSYS/EDEADLK/ELOOP/EDQUOT/ENOLCK);
  `service()` and the proxy fault builtins (`response`/`error`/`drop`/`delay`/
  `duplicate`) reject unknown keyword arguments with migration hints. Specs
  relying on silently-ignored kwargs (`cmd=`, `http=`, `tcp=`, `name=`) must
  switch to the documented forms.
- `subject=` is now an accepted alias for `topic=` in proxy fault matchers
  (NATS), as the protocol-proxy design doc always showed.
- Trace schema: `proxy_conn_close` and `proxy_stall` events now include the
  `protocol` field (previously only lifecycle-open events carried it).

## [0.13.2] - 2026-07-06

Temporal verdict semantics are grounded in finite-trace logic
([RFC-049](docs/rfcs/0049-finite-trace-verdict-semantics.md), D4 of the
[RFC-047](https://github.com/faultbox/Faultbox/issues/132) research
agenda), plus the documentation and site restructure around the six
bug classes.

### Changed

- **Timeout is always INCONCLUSIVE, never PASS (RFC-049 D4).** A `test()`
  that ends by hitting its `timeout` now finalizes INCONCLUSIVE even if
  every `eventually()` it declared was satisfied before the deadline. The
  body did not reach a declared completion (natural return or
  `terminate_when=`), so the run is a *truncated prefix* and a green
  verdict would over-claim. Long-running / `reuse=True` specs that want a
  definite PASS must declare `terminate_when=` rather than lean on the
  deadline. Legacy synchronous `def test_*()` functions are unaffected.
- **Unbounded `always(p)` under timeout is INCONCLUSIVE.** A never-violated
  unbounded safety property (no `between=` window) finalizes INCONCLUSIVE
  on timeout — a longer trace could still violate it (LTL₃ prefix). At
  natural completion or `terminate_when=` it remains a definitive PASS
  (LTL_f end-of-trace). Bounded `always(p, between=(a,b))` is unchanged.

### Added

- **`vacuous_property` warning event (RFC-049 vacuity resolution).** When an
  `always(p, between=)` start anchor never fires, the window never opens and
  the predicate is never evaluated. The verdict stays PASS (the window may be
  legitimately untriggered), but the runtime now emits a `vacuous_property`
  event into the trace so a typo'd or misnamed anchor surfaces instead of
  hiding as a silent green.

### Docs

- Documentation and site restructured around the six bug classes: Part 1 of
  the tutorial reframed to the bug-class + boundary story, new
  [Seeding Data & Initial State](docs/guides/seeding-data.md) guide, new
  [checkout Kafka outage](docs/use-cases/checkout-kafka-outage.md) use case,
  and fault-level guidance in [choosing-fault-levels](docs/guides/choosing-fault-levels.md).
- RFC-048 (causal-guided exploration) and RFC-050 (gray/metastable faults)
  added as drafts; RFC-049 marked Accepted.

## [0.13.1] - 2026-06-18

Fixes from the first field evaluation of v0.13.0 against a real
service (order-service). The evaluation surfaced five rough edges; four
are fixed here (the fifth, the `monitor()` signature change, is an
accepted breaking change and keeps its hard error).

### Fixed

- **`--test` no longer fails silent (F-6).** A `--test` pattern that
  matches no tests now exits **1** (was 0 — a typo read as a green
  suite in CI) and prints the available names. A collapsed
  `fault_matrix` name (`test_matrix_create_order`, the name `faultbox
  plan` prints) now selects every expanded cell under it, instead of
  matching nothing. The CLI exit-code table (incl. the previously
  undocumented INCONCLUSIVE exit 3) is now in
  [docs/cli-reference.md](docs/cli-reference.md).
- **Missing/unstartable target fails fast with the cause (F-2).** A
  service whose binary can't be exec'd now fails immediately with
  `exited before becoming ready: exec <path>: no such file or
  directory`, naming the path. Previously the exec failure was hidden
  behind a generic `context deadline exceeded` a full healthcheck
  timeout later, with a misleading `exit_code=0` in the session log.
  Both the binary-mode launch (`internal/seccomp`) and the container
  shim now carry the target path in the error, and the healthcheck
  races against session exit so a dead service surfaces at once.

### Added

- **Non-2xx response bodies on the trace (F-3).** `step_recv` events
  now record the response `body` (truncated to 2 KB) for non-2xx HTTP
  responses, so debugging a 400/500 reads off the trace or HTML report
  instead of an edit-assert-rerun loop. 2xx bodies are omitted to keep
  bundles small; the full body stays on the in-test `Response` object.
- **`make install-lima` (F-4).** Cross-compiles **both** `faultbox`
  and `faultbox-shim` and installs them into the Lima VM's
  `/usr/local/bin` — container mode needs both side by side, which
  `make build` alone didn't provide. Documented under
  [README → Build from source](README.md#build-from-source).

### Changed

- `findShimPath()` no longer probes a hardcoded developer path that
  could silently win over the alongside-the-binary fallback; it logs
  the chosen shim path at debug level (F-4).

## [0.13.0] - 2026-05-29

Five RFCs ship in v0.13.0:

**RFC-040 — determinism levels.** Makes **L1 (mediated-event
determinism) a contract**: every spec runs at L1 with strict mode on
by default, and the runtime emits `unmediated_io` events when the
service-under-test does I/O Faultbox can observe but isn't mediating
(`clock_gettime`, `getrandom`, DNS to a non-Faultbox resolver,
`connect()` to an undeclared address). Strict mode fails the test on
the first untolerated leak with a precise error pointing at the call
site and the two escape hatches.

**RFC-041 — temporal properties.** Five new primitives for asserting
on *what must be true* about a distributed system rather than *how
long to wait* before checking: `eventually(p)`, `always(p, between=)`,
`await_event(matcher)`, `await_stable(quiescence_window=)`, and a
rewritten `monitor(name, on=, state_init=, update=, check=)` that
keeps per-test memory. The test lifecycle gains a three-valued
verdict (PASS / FAIL / **INCONCLUSIVE**) plus a declarative
`test(name, body=, timeout=, expect=, terminate_when=)` builtin.
User guide: [docs/temporal.md](docs/temporal.md).

**RFC-042 — exploration plan (rc1 + rc2).** rc1 shipped the static
plan-tree enumeration surface: `faultbox plan`, `plan.json` in every
bundle, the HTML report's Plan tab, coverage analysis, `--suggest`,
and the `--check-cost` gate. **rc2** adds the body-re-execution
engine plus three fan-out axes: (1) named `choose("name", [opts])`
(RFC-043 §5.2) — one execution per option. (2) **§8.9 syscall-level
probability fan-out** — `delay()` / `deny()` accept `max_fires=N`
and `mode="exhaustive"|"stochastic"`; exhaustive mode fans out to
2^N leaves per rule with a per-leaf fire/no-fire vector consulted
via `SessionConfig.ProbabilityDecider`. (3) **§8.8
`parallel(interleavings=)`** — `1` (default), `"all"`, `"critical"`,
integer `N`; reserved values (`"dpor"`, `"sut-internal"`) keep
producing explicit "future release" errors. Each interleaving runs
as a separate test execution; **scope limit:** the rc2 engine ships
*launch ordering* (sequential per-leaf branch order), not
mediated-event-level interleaving — the kwarg surface + leaf
descriptors are the substrate the latter plugs into. Every fan-out
axis attributes via `TestResult.LeafID` → `bundle.TestRow.LeafID`
→ HTML report. **Deferred follow-ups:** mediated-event-level
interleaving execution, protocol-level probability fan-out
(`response()`/`error()`/`drop()`), static trigger-count analysis,
the `unmodeled_fanout` plan warning, `wait_all`/`wait_n`/`wait_first`
builtins. User guide: [docs/exploration.md](docs/exploration.md).

**RFC-043 — non-deterministic operators (rc1 + rc2).** rc1 shipped
four small Starlark primitives: `choose([opts])` / `choose("name", [opts])`
for finite N-way choice, `nondet()` for the non-deterministic boolean
(sugar for `choose([True, False])`; the pre-existing `nondet(svc)`
variant for interleaving-control exemption keeps working unchanged),
`halt(reason="")` for plan-tree branch pruning with a new `"halted"`
outcome flowing through `SuiteResult`, bundle manifest, and the HTML
report, and `assume(predicate)` / `test(assume=[...])` for plan-tree
filtering. **rc2** wires the operators into RFC-042's body-
re-execution engine: named `choose()` axes fan out one execution per
option; per-test `assume=` predicates evaluate at body entry against
the current leaf's axis assignment (body-time `choose()` calls
included), with predicate Starlark errors mapping to `Result="error"`
rather than `"fail"` (distinguishing spec-authoring bugs from SUT
behavioral failures); the §8.7 AST denylist for assume() lambda
predicates is enforced at spec load (matching the monitor sandbox
model — named `def` predicates slip past the static walk).
`mode="exhaustive"` on probability faults is now normalized at
parse-time so the internal representation matches the documented
default. **Deferred:** §8.5 plan-walker-time `assume=` pruning
(halt-at-body-entry today; lifting into the enumerator is a
follow-up) and §8.6 cost-guard. User guide:
[docs/nondeterministic-operators.md](docs/nondeterministic-operators.md).

**RFC-044 — spec language simplification (§8.1 + §8.2 + §8.3 +
§8.4 + §8.5).** C1 shipped the docs-and-deprecations bundle: RFC-013
(`param()`) formally **withdrawn** (superseded by RFC-043
`choose("name", [opts])`); RFC-002 (`domain()`) formally
**withdrawn** via a new `docs/rfcs/0002-domain.md` stub; `faultbox
generate` deprecated in favor of `faultbox plan --suggest` with a
one-time stderr warning (removal in v0.14.0); a "Feature
interactions — what caps below L4" subsection in
`docs/spec-language.md` documents the determinism ceilings for
`service(remote=…)` and `service(reuse=True, …)`. **C2** ships the
unified fan-out machinery (§8.2): the three plan-tree axis kinds
(`ChoiceVal`, `ProbFaultSite`, `ParallelSite`) now implement a
single `NonDeterministicChoice` interface in
`internal/star/nondet.go` with `Cardinality()` / `Apply(leaf, digit)`
methods; `enumerateLeaves` is a thin wrapper over a generic
mixed-radix walker `expandLeaves`. Adding a new axis kind in the
future requires only implementing the two-method contract — no
per-kind branching in the enumerator. All pre-existing testops
goldens unchanged. **C3** ships the namespace unifications
(§8.6 + §8.7): a new `observe` Starlark struct exposes
`observe.stdout` and `observe.stderr` as attributes; a unified
`decoder("name", ...)` dispatcher handles `"json"`, `"logfmt"`,
and `"regex"`. The pre-rc2 top-level `stdout()` / `stderr()` /
`json_decoder()` / `logfmt_decoder()` / `regex_decoder()` builtins
remain registered as deprecated aliases that emit a one-time
stderr warning per process and route through the canonical
implementation — DecoderVal construction has a single source of
truth, so a future decoder bug fix lands in one place. Removal
of the aliases is scheduled for v0.14.0.

**Tutorial — RFC-042/043 chapters.** Part 4 (Safety & Verification)
gains two new chapters covering the v0.13.0-rc2 fan-out vocabulary:
**Ch 25 — Non-deterministic Operators (`choose` / `assume` /
`halt` / `nondet`)** walks the four RFC-043 operators end-to-end
from the "one test, 18 leaves" motivating problem through the
sandbox semantics and the discovery-run mental model; **Ch 26 —
Plan-Tree Fan-Out** covers the RFC-042 plan tree, probability
fan-out with `max_fires=`/`mode=`, `parallel(interleavings=)`, the
`faultbox plan --check-cost` gate, and the cross-product
composition rules. Both chapters include exercises and the
`Result="error"` vs `"fail"` distinction for predicate Starlark
errors.

RFC-046 carries the post-L1 roadmap (gVisor Path B/C, L4 hermetic
mode, L5 instruction-boundary research).

### Added

- **`determinism()` top-level builtin** with `level=`, `runtime=`,
  `strict=`, `allow=` kwargs. Defaults: `L1` / `default` / strict on /
  empty allow list. May be called at most once per spec; reserved
  syntax (`L2`–`L5`, `runtime="gvisor"`) parses but errors at spec
  load citing RFC-046, so future migration is non-breaking.
- **`service(nondeterministic_ok=[...])`** kwarg for per-service
  tolerance. Unions with the spec-level `allow=` set when strict
  mode decides whether to fail.
- **L1 detection layer** — five categories (`clock`, `rand`, `dns`,
  `network-unmediated`, `fs-unmediated`) with stable `unmediated_io.<syscall>`
  event types. `fs-unmediated` is reserved in v0.13.0 (accepted in
  lists, no events emitted yet). Detection installs only on services
  that already need a seccomp filter — unfaulted services keep their
  native-speed path.
- **Strict-mode failure surface**. `RunTest` returns Result="fail"
  with a Reason naming the category, service, syscall, and dest.
  New `outcome="strict_determinism_violation"` value flows through
  the bundle manifest and HTML report, parallel to `expectation_violated`
  (refines `failed`) and `fault_bypassed` (refines `passed`).
- **`--strict-determinism[=true|false]`** and **`--no-strict-determinism`**
  CLI overrides on `faultbox test`. Bidirectional and final — beats
  whatever the spec declared. Useful for local iteration on a strict
  CI spec without editing it.
- **`docs/determinism.md`** — full L0–L5 taxonomy, the L1 contract,
  the per-level author manifest, and the post-L1 roadmap pointer.
- Tutorial chapter [24: Determinism & the L1 Contract](docs/tutorial/04-safety/24-determinism.md)
  with worked examples.
- Engine-level `SyscallEvent.DestIP` / `DestPort` fields, populated
  once at the top of `handleNotification` for `connect()` syscalls.
  The rule-loop sockaddr read uses the captured values instead of
  reading process memory twice.
- `proxy.Manager.IsListenPort()` to recognise SUT connections to a
  Faultbox proxy as mediated.

### Added (RFC-041 temporal properties)

- **Five new temporal primitives:**
  - `eventually(predicate, anchor=)` — liveness, evaluated continuously
    and finalized at Termination per the §5.5 verdict table.
  - `always(predicate, between=)` — invariant; fails immediately on
    the first violation in the bounded window.
  - `await_event(matcher_or_predicate)` — blocks the test body until
    a matching event arrives (eager-checks on entry, returns the
    matching `EventVal`).
  - `await_stable(quiescence_window=, ignore=)` — blocks until no
    non-ignored event has fired for the full window.
  - `monitor(name, on=, state_init=, update=, check=)` — state-machine
    monitor with per-test memory and a sandboxed Starlark predicate
    environment.
- **`test(name, body=, setup=, expect=, timeout=, terminate_when=,
  clock=)`** declarative test wrapper. Coexists with legacy
  `def test_*()` functions.
- **Three-valued test verdict** — `TestResult.Result` now takes one of
  `"pass" | "fail" | "error" | "inconclusive"`. `SuiteResult` and
  `TraceOutput` gain an `Inconclusive` counter (omitted from JSON
  when zero so pre-RFC-041 specs serialize identically).
- **CLI exit code 3** for inconclusive-only runs (no failures, at
  least one timeout with pending temporal assertion). Code 2 for
  any-fail stays as it was.
- **Trace API** (`internal/star/trace.go`) — `trace.event/events/
  first/last/count`, `trace.events_between`, `trace.events_within`,
  `trace.causal_chain`. Backed by secondary event-log indexes (by
  type, by service) built incrementally in `EventLog.Emit`.
- **`match` namespace** — `match.event(type=..., **fields)`,
  `match.any(...)`, `match.all(...)`, `match.never()`. Reusable
  matcher values consumed by monitor `on=`, `await_event`,
  `await_stable(ignore=)`.
- **EventVal causal operators** — `happens_before/after`,
  `concurrent_with`, `same_service_as`, `same_correlation_as`,
  `preceded_by/within`, `followed_by/within`, `directly_caused_by`,
  `duration_since`.
- **Reserved kwarg `clock="virtual"`** on `test()`, `await_stable()`,
  and `await_event()` parses but errors with `"requires gVisor (Path
  C); not available in this release"`. Locks the syntax now so the
  L1 → L3 migration is a substrate swap, not a spec rewrite.
- **Monitor sandbox** — `update`/`check` lambdas validated at spec
  load against a denylist of Faultbox builtins that would mutate
  runtime state or recurse into the temporal machinery. Failures cite
  the source line and the per-entry reason.

### Documentation

- README features list now mentions the determinism contract; docs
  table links to `docs/determinism.md`.
- `docs/spec-language.md` gains a Determinism section with the
  builtin reference, escape-hatch workflow, `unmediated_io` event
  schema, and the per-category caveats (Go VDSO blindness for
  `clock`, DoH/DoT for `dns`).
- `docs/feature-manifest.md` rows for the determinism builtin,
  detection layer, and strict-mode outcome.
- **`docs/temporal.md`** — full user guide for the five RFC-041
  primitives, the predicate language, the verdict table, and the
  L1→L3 level-awareness story.
- `docs/spec-language.md` adds a Temporal Primitives section with
  reference entries for `eventually`, `always`, `await_event`,
  `await_stable`, `test()`, the `match` namespace, the trace API,
  and the rewritten `monitor()` signature.
- `docs/feature-manifest.md` rows for every RFC-041 primitive, the
  trace API, and the PASS/FAIL/INCONCLUSIVE lifecycle.

### testops goldens

End-to-end goldens for every L1 detection category, driven by a new
`/tmp/faultbox-leaker` HTTP harness (built by `make testops-prep` on
Linux). Each spec faults the leaker at `write=allow()` (a no-op rule
that installs the seccomp filter), then triggers one leak per request:

- `determinism_clock_read` — raw `clock_gettime` syscall
- `determinism_rand_read` — raw `getrandom` syscall
- `determinism_dns_leak` — `connect()` to `8.8.8.8:53`
- `determinism_raw_socket` — `connect()` to `127.0.0.1:19999`
- `determinism_tolerated` — all four leaks tolerated via `allow=` /
  `nondeterministic_ok=`; verifies the trace still surfaces the
  events even when strict mode is suppressed.

LinuxOnly because seccomp-notify is Linux-kernel-specific. macOS
hosts run them via Lima.

### Filed but not implemented in this cut

- RFC-042 (Exploration Plan) — #111
- RFC-043 (Non-deterministic Operators) — #112
- RFC-044 (Spec Language Simplification) — #113
- RFC-046 (Beyond L1: gVisor Roadmap & L5 Research) — #114

### Behaviour change worth flagging

Tolerated unmediated-I/O categories still emit `unmediated_io` events
into the trace and bundle. Tolerance only suppresses the strict-mode
*failure decision*, not the event itself — customers see what their
service did even when they've explicitly accepted the drift. This
diverges from the original PR-2 design (which skipped seccomp
interception entirely for tolerated categories); the new behaviour
matches the principle that visibility and enforcement are separate
concerns.

## [0.12.29] - 2026-05-02

RFC-036 — **remote services**. The single-keyword path from a local
SUT to a real cluster pod. `service(remote=...)` declares a service
whose process lives in a customer's k8s dev cluster (or any
externally-reachable endpoint); Faultbox stands up its existing
protocol proxy in front of each interface and dials the remote
upstream. Every protocol-level fault — `response()`, `error()`,
`slow()`, gRPC method targeting, SQL matchers — fires unchanged.
The configurations that can't possibly work on a remote (syscall
faults, `seed=`, `reset=`, `volumes=`, etc.) are rejected at spec
load with explicit messages pointing at protocol faults or
`mock_service()`.

Closes the gap that the [2026-04-22 customer-feedback
analysis](docs/design/2026-04-22-customer-feedback-analysis.md)
explicitly deferred ("DevPlatform integration → 1.x"). The
companion design RFC-037 (#94) frames the determinism question
that remote services raise; this release ships the primitive with
a documented best-effort reproducibility caveat and `faultbox
replay` warning, leaving the offline-replay design open.

### Added

- **`service(remote=...)`** kwarg as a fourth source alongside
  `binary` / `image` / `build`. Plain-string form
  (`remote = "config-service.staging.svc.cluster.local"`) applies the host to
  every interface; per-interface override via the typed
  `remotes({"public": "h1", "internal": "h2:9090"})` value.
- **`remotes(dict)`** Starlark builtin returning the typed
  per-interface map. Keys must match declared interface names;
  values are `host` (interface port appended) or `host:port`.
- **`@faultbox/discovery/k8s.star`** stdlib helper exposing
  `k8s.service(name, namespace="default")`,
  `k8s.endpoint(name, port, namespace="default")`, and
  `k8s.local(name, port, namespace="default")` — pure string
  sugar over `<name>.<namespace>.svc.cluster.local`. No runtime
  k8s client; cluster connectivity stays the user's responsibility
  via Telepresence connect / kubectl port-forward / in-cluster
  execution / VPN.
- **`startRemoteService`** runtime path mirrors `startMockService`:
  no process, no seccomp, host-side healthcheck against the
  user-declared remote address. On failure: explicit multi-line
  hint pointing at the supported connectivity workflows.
- **`service_started` event** for remote services carries
  `kind="remote"` plus per-interface upstream addrs in the
  payload — visible in trace and `.fb` bundle.
- **`env.json` `remotes: [...]`** array records every
  (service, interface, host, protocol, resolved_at) tuple from a
  remote-using run. Present means: this bundle is not
  deterministically replayable offline (RFC-037 territory).
  Omitted entirely when no remote services were used.
- **`faultbox replay`** prints a multi-line warning when a bundle's
  `env.json` declares remotes, naming each
  (service, interface) → (host, protocol) pair and pointing at
  RFC-037 for the offline-replay story.
- **`docs/guides/connectivity.md`** new guide covering the four
  supported setups (Telepresence / in-cluster / port-forward /
  VPN) with quick decision tree, walkthroughs, healthcheck
  failure hint, and the TLS-upstream interop notes for RFC-038.
- **`docs/spec-language.md`** new "Remote Services" subsection +
  Primitive Index entries for `remotes()` and the discovery/
  helpers.

### Interop with RFC-038 (TLS-aware proxy)

`interface(..., tls=tls_cert(...))` composes cleanly with
`remote=` — the proxy dials the remote upstream over TLS using
the resolved client config, the SUT speaks TLS to the proxy
listener using the resolved server config. Auto-generated
self-signed certs cover `127.0.0.1`/`localhost` so SUT-side
verification works against the env-rewritten proxy loopback
without extra cert plumbing. Six protocols terminate TLS today
(http, http2, grpc, kafka, redis, tcp); the rest surface
`proxy_tls_pending` until RFC-039 lands them.

### Changed

- **`proxyTargetAddr`** signature is now `(svc, iface)`. For
  remote services the function returns the user-declared upstream
  addr (`<remote>:<iface.port>` for plain-string form,
  `<host:port>` for the per-interface override) instead of
  `127.0.0.1:<port>`. Local services unchanged. All four call
  sites updated (`preStartProxies`,
  `builtinFaultProtocol`, `builtinFaultFromAssumption`,
  `fault_scenario` body).
- **`proxyAddrSubstitutionsFor`** adds substitutions for the
  remote upstream addr so user env values like
  `{"CONFIG_URL": "http://config.staging:8080/"}` get rewritten to the
  proxy listener. Without this the SUT would dial the remote pod
  directly and protocol faults would never fire.
- **Spec-load validation** rejects every kwarg that requires
  process control on a remote service: `seed=`, `reset=`,
  `reuse=`, `volumes=`, `ports=`, `args=`, `seccomp=`,
  `observe=`, `ops=`, the launch sources (`binary`/`image`/
  `build`). `healthcheck=` is **required**. Error messages name
  the offending kwarg and suggest the right alternative.
- **Fault rule registration** rejects syscall-level faults on
  remote services at both `fault_assumption()` time (early
  signal) and `applyFaults()` runtime (safety net). Protocol
  faults route through unchanged.

### Tests

| File | Tests | Surface |
|---|---|---|
| `internal/star/builtins_remote_test.go` | 32 | Spec-load validation, every kwarg accept/reject, `remotes()` typed value, k8s discovery helper, fault rule routing |
| `internal/star/runtime_remote_test.go` | 10 | `startRemoteService` session registration, healthcheck-gated startup with hint, `kind=remote` event payload, `proxyTargetAddr` resolution (3 cases), env-host substitution, full HTTP loop, fault rewrite, local-vs-remote parity, mid-run upstream death, **TLS×remote end-to-end (RFC-038 interop)** |
| `internal/bundle/bundle_test.go` | 2 | `env.json` remotes round-trip + omitempty when unused |
| `cmd/faultbox/replay_test.go` | 2 | Warning printer + no-spurious-warn for non-remote bundles |
| `docs/docs_remote_test.go` | 3 | String-grep gates for spec-language section, connectivity guide, feature-manifest entries |

49 new tests in total. Full repo `go test ./...` green; `go vet
./...` clean; cross-compile linux/arm64 OK; `make demo-container`
4/4 pass on Lima against postgres/redis container demos
(non-regression for the proxy datapath refactor).

### Customer ergonomics

A spec like:

```python
load("@faultbox/discovery/k8s.star", "k8s")

config = service("config-service",
    interface("public", "http", 8080),
    remote      = k8s.service("config-service", namespace = "staging"),
    healthcheck = http(k8s.endpoint("config-service", 8080, namespace = "staging") + "/healthz"),
)

api = service("order-service",
    interface("main", "http", 8000),
    image       = "order-service:dev",
    depends_on  = [config],
    env         = {"CONFIG_URL": "http://%s/" % config.public.addr},
)

fault_assumption("config_unavailable",
    target = config.public,
    rules  = [error(path = "/v1/regions/**", status = 503)],
)
```

with one `telepresence connect` on the host fires real 503s into
the SUT's calls to a real `config-service` pod, no image distribution,
no mock authoring.

Version 0.12.28 → 0.12.29.

## [0.12.28] - 2026-05-02

RFC-038 Phase 3 (5 of 4) — generic TCP plugin TLS migration.
A late addition: TCP is the long-tail escape hatch for any custom-
protocol service that uses TLS but doesn't have a dedicated
Faultbox plugin. Same wrap-and-dial pattern as kafka / redis;
prefix-peek rule predicate (Rule.Method) still fires against the
plaintext bytes between the two TLS legs.

### Changed

- **`tcp` plugin migrated to TLSAware.** `SetTLS(server, client)`:
  - Listener: `proxy.ListenTLS(serverTLS)` when set; plain
    `Listen()` otherwise.
  - Upstream: `proxy.Dial(ctx, target, clientTLS, 5s)` replaces
    `net.DialTimeout`.
  - Plaintext path runs unchanged (existing TestTCPProxyPassThrough,
    TestTCPProxyDropRule, TestTCPProxyRespondRule, TestTCPProxyPrefixMatch
    keep green).
- Added a small ctx-watcher goroutine that closes the listener on
  ctx cancel. The pre-existing `*net.TCPListener` SetDeadline
  trick in `acceptLoop` doesn't apply to the wrapped TLS listener
  (`*tls.listener` is a private type), so without explicit close
  Accept could leak past ctx cancellation. The watcher unblocks
  Accept the moment ctx fires regardless of whether Stop() is also
  called — matches the behavior of the SetDeadline polling loop.

### Tests

4 new tests in `internal/proxy/tcp_tls_test.go`:

| Test | Covers |
|---|---|
| `TestTCPProxy_TLSEndToEnd` | client TLS → proxy → upstream TLS, byte-identity round trip |
| `TestTCPProxy_TLSPrefixRuleStillFires` | prefix-match rule fires on plaintext between the two TLS legs |
| `TestTCPProxy_PlaintextStillWorks` | plaintext regression (parallel guard to existing TestTCPProxyPassThrough) |
| `TestTCPProxy_ImplementsTLSAware` | type-assertion contract |

`TestEnsureProxyTLS_AppliedFlag` (added in #101) was probing
`tcp` as the "plugin not migrated yet" exemplar; updated to use
`amqp` instead so the false-path of the assertion stays exercised.

### RFC-039 deferred set is now smaller

Phase 3 deferred protocols left after this PR: postgres, mysql,
mongodb, cassandra, clickhouse, memcached, nats, amqp, udp.
Postgres/mysql still need the SSLRequest-upgrade design; the rest
cluster around either the wrap-and-dial pattern (we know how) or
no-meaningful-TLS (udp).

Full repo `go test ./... -race` green; cross-compile + Lima
demo-container 4/4 PASS.

Version 0.12.27 → 0.12.28.

## [0.12.27] - 2026-05-02

RFC-038 Phase 3 (4 of 4) — Redis plugin TLS migration. **Phase 3
is now complete** for the four customer-priority plugins (http,
http2, gRPC, Kafka, Redis). The remaining 10 plugins —
postgres, mysql, mongodb, cassandra, clickhouse, memcached, nats,
amqp, tcp, udp — are deferred to RFC-039 (separate follow-up RFC
covering the SSLRequest-upgrade design and the protocols that
need it).

### Changed

- **`redis` plugin migrated to TLSAware.** Redis 6+ supports TLS
  via a separate `tls-port` config entry — no in-band SSL upgrade,
  just "TLS from byte 1" on the configured port. Same wrap-and-dial
  pattern as Kafka:
  - Listener: `proxy.ListenTLS(serverTLS)` when set; plain
    `Listen()` otherwise.
  - Upstream: `proxy.Dial(ctx, target, clientTLS, 5s)` replaces
    `net.DialTimeout`.
  - Plaintext path runs unchanged — `redis_test.go`'s RESP3 corpus
    keeps green.
- **Coverage gate exemption dropped** for `redis.go`. The existing
  `redis_test.go` (RESP3 HELLO map / set / attribute regression
  suite from v0.12.15.x) already satisfied the #84 requirement;
  the exemption was stale. Removed.

### Tests

4 new tests in `internal/proxy/redis_tls_test.go`:

| Test | Covers |
|---|---|
| `TestRedisProxy_TLSEndToEnd` | RESP-over-TLS at both legs |
| `TestRedisProxy_TLSRuleInjection` | key-glob error rule fires inside TLS tunnel |
| `TestRedisProxy_PlaintextStillWorks` | plaintext regression |
| `TestRedisProxy_ImplementsTLSAware` | type-assertion contract |

### Phase 3 wrap-up

Five plugins now terminate TLS at the proxy and / or dial upstream
over TLS, covering the customer's three explicit gaps:
- ✅ HTTPS responses (#2): http plugin
- ✅ gRPC-TLS (#1): gRPC plugin (and http2 for HTTPS HTTP/2)
- ✅ Kafka TLS: Kafka plugin (broker SSL listener)
- ✅ Redis TLS: Redis plugin (`tls-port`)

The customer's fourth implicit ask — TLS-Postgres / TLS-MySQL
(#3) — remains gated on the SSLRequest-upgrade design and
follows in RFC-039. Until then, declarations like
`interface("db", "postgres", 5432, tls=...)` continue to emit
the `proxy_tls_pending` warning.

Full repo `go test ./... -race` green; cross-compile + Lima
demo-container 4/4 PASS.

Version 0.12.26 → 0.12.27.

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
remaining half of the customer's gap #1 (gRPC-TLS) — `order-service →
config-service` over mTLS now flows through the proxy with rules still
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

The customer-facing surface is identical: `interface("config", "grpc",
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
  `target=order-service.svc.cluster.local:443` get a proxy cert that
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

Customer-driven (customer report, 2026-04-28). The v0.12.15.x
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
first-class trace event. Customer-driven (the customer,
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

Hotfix on top of v0.12.15.1. Customer (the customer) verified the
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

Hotfix on top of v0.12.15. Customer (the customer) verified the
MySQL `caching_sha2_password` fast-auth-success fix landed clean
(Finding H closed, smoke test progressed past the MySQL stage). The
failure point moved one step forward to Redis: `order-service` now hangs
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

Hotfix on top of v0.12.14. Customer (the customer) verified that
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

  How the customer hit it: their `seed_db` Starlark hook polls MySQL via
  `db.mysql.exec(sql="SELECT 1", dsn=DB_DSN_POLL)` — `dsn=` overrides
  the proxy address, so seed connects directly to MySQL and populates
  the server's auth cache. By the time `order-service` connected through
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

Hotfix on top of v0.12.13. Customer (the customer) confirmed the
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
`redis.timeout()` matrix against `order-service` (host binary) connecting to a
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
  now read `→ call · order-service.get /orders` and
  `← reply · order-service.get /orders [500]`. Arrows still scan
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
  service cell now shows `laneFor(ev)`, so filtering by `order-service`
  matches both order-service's own lifecycle and the step events
  pointed at it.

## [0.12.6] - 2026-04-25

Three UX fixes from a customer read of the v0.12.5 report:

### Changed

- **Lane markers now color by severity, not just type.** A
  `step_recv` with `success=false`, an `error` field, or
  `status_code ≥ 500` paints with the fault palette (red);
  `status_code` 4xx paints amber. Without this every step rendered
  in the same yellow/warn colour and the eye couldn't find the DB
  invalid-connection or the order-service 500 among a sea of `SELECT 1`
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
  the cell's value (`step_recv` only on `order-service`: two clicks).
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
the customer v0.11.1 report — that the HTML artifact was too
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
  the customer model). Schema unchanged — `Binaries` field
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
    binary  /tmp/order-service    locked sha256:111…   current sha256:222…
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
  through. Customer ask from the customer v0.11.1 feedback #12.

### Added

- **`CHANGELOG.md` + per-release pages on the site**
  ([#81](https://github.com/faultbox/Faultbox/issues/81)) — release
  notes previously lived only on GitHub Releases, which teams reported
  as an adoption blocker ("discovered features from `--help` rather
  than docs"). A root-level changelog mirrors the site.

## [0.11.2] - 2026-04-24

Hotfix for two P0 regressions reported by the customer against v0.11.1. Both
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

Six small primitives addressing customer asks from the the customer feedback
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

[Unreleased]: https://github.com/faultbox/Faultbox/compare/release-0.18.0...HEAD
[0.18.0]: https://github.com/faultbox/Faultbox/compare/release-0.17.0...release-0.18.0
[0.17.0]: https://github.com/faultbox/Faultbox/compare/release-0.16.1...release-0.17.0
[0.16.1]: https://github.com/faultbox/Faultbox/compare/release-0.16.0...release-0.16.1
[0.16.0]: https://github.com/faultbox/Faultbox/compare/release-0.15.0...release-0.16.0
[0.15.0]: https://github.com/faultbox/Faultbox/compare/release-0.14.1...release-0.15.0
[0.14.1]: https://github.com/faultbox/Faultbox/compare/release-0.14.0...release-0.14.1
[0.14.0]: https://github.com/faultbox/Faultbox/compare/release-0.13.3...release-0.14.0
[0.13.3]: https://github.com/faultbox/Faultbox/compare/release-0.13.2...release-0.13.3
[0.13.2]: https://github.com/faultbox/Faultbox/compare/release-0.13.1...release-0.13.2
[0.13.1]: https://github.com/faultbox/Faultbox/compare/release-0.13.0...release-0.13.1
[0.13.0]: https://github.com/faultbox/Faultbox/compare/release-0.12.29...release-0.13.0
[0.12.29]: https://github.com/faultbox/Faultbox/compare/release-0.12.28...release-0.12.29
[0.12.28]: https://github.com/faultbox/Faultbox/compare/release-0.12.16...release-0.12.28
[0.12.27]: https://github.com/faultbox/Faultbox/compare/release-0.12.16...release-0.12.28
[0.12.26]: https://github.com/faultbox/Faultbox/compare/release-0.12.16...release-0.12.28
[0.12.25]: https://github.com/faultbox/Faultbox/compare/release-0.12.16...release-0.12.28
[0.12.24]: https://github.com/faultbox/Faultbox/compare/release-0.12.16...release-0.12.28
[0.12.23]: https://github.com/faultbox/Faultbox/compare/release-0.12.16...release-0.12.28
[0.12.22]: https://github.com/faultbox/Faultbox/compare/release-0.12.16...release-0.12.28
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
