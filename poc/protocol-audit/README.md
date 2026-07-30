# Protocol step-client audit

Specs that run real operations against real servers and **assert on every
result**. That is the whole idea.

```bash
faultbox test poc/protocol-audit/postgres.star
faultbox test poc/protocol-audit/mysql.star
faultbox test poc/protocol-audit/redis.star
faultbox test poc/protocol-audit/redis-auth.star
```

Requires Linux + Docker. On macOS: `limactl shell faultbox-dev`.

## Why these exist

A protocol step that fails returns `ok = False`. It does not raise — in a
fault-injection tool, a failing dependency is frequently the thing the spec is
deliberately provoking, so raising would make the common case unwritable.

The cost is that **an ignored result is indistinguishable from a successful
one**, and for a long time no spec in this repo checked one. Two credential
bugs lived behind that gap for as long as the plugins had existed:

| Bug | Symptom the spec saw | Fixed |
|---|---|---|
| Postgres steps sent no credentials, and the proxy relayed auth in one direction only | `connection reset by peer` after 60 s | v0.16.0 |
| MySQL steps sent no password and selected no database | `invalid connection` | v0.16.1 |

Both were found the same way: by writing the first spec that asserted on the
result of a step. This directory is that spec, kept.

## What the audit turned up beyond the credentials

Fixing the MySQL credentials let a step reach the command phase for the first
time — and immediately exposed a second bug hiding behind the first:

- **The MySQL proxy hung forever on every result set.** `forwardResponse`
  forwarded a packet, then peeked with a short deadline to decide whether more
  was coming; the peek consumed the terminator, so the next iteration issued a
  deadline-free read for a packet that would never arrive. `exec()` was fine
  (single OK packet); every `query()` wedged. Because `Stop()` waited on the
  connection WaitGroup, one stuck handler hung the entire run **in teardown**,
  after the test body had finished — so no per-test timeout could fire.
- **`ready()` could never succeed for 8 protocols.** It resolved to
  `<protocol>://host:port` and handed the whole string to the plugin, but only
  Postgres, HTTP and HTTP/2 parsed a URL. The rest dialled it verbatim.
- **`args=` was silently ignored for container services.** Accepted by the spec
  loader, then dropped — it only ever reached binary mode. A spec configuring a
  server through its command line got a default server and no warning.

## The pattern, if you are adding a protocol

1. Real server, declared the way its image documents.
2. `healthcheck = ready(...)`, never `tcp()`. A port check reports ready the
   moment Docker's proxy binds, which is what lets a broken client look healthy.
3. Assert `r.ok` on **every** step, with `r.error` in the message.
4. Include one step that is *supposed* to fail, and assert it reports
   `ok = False` with a non-empty error — otherwise a client that always claims
   success passes too.
5. Assert that the workload ran at all. Completeness of observation and
   occurrence of the workload are different claims; see
   [Pattern 0](../../docs/guides/spec-patterns.md#pattern-0-assert-on-every-step).

## Coverage

Ten of the thirteen step protocols are covered by a real-server spec:
**postgres, mysql, redis** (with and without a password), **mongodb,
clickhouse, nats, cassandra** here, plus kafka, http and tcp via the existing
`poc/` specs.

**http2, udp and grpc have unit coverage only.** Given that reaching four
protocols with a real server found four bugs no unit test saw, that is a gap
worth closing, not a formality. Adding one means writing the spec below against
a real server — the pattern is the same for every protocol.

Not gated in CI: the corpus needs Docker and roughly five minutes of container
starts (Cassandra alone takes ~60 s per test to serve). Run it before a release,
and after touching anything in `internal/protocol` or `internal/proxy`.

### When a batch fails at the tail, check the disk first

The first time this corpus was run end to end, everything from MongoDB onward
failed. It looked like resource contention. It was `no space left on device`:
Faultbox removed its containers but not the anonymous volumes the database
images declare, so a session of runs had left **290 orphaned volumes / 18.7 GB**
on a 30 GB disk. Fixed in v0.16.1 (`RemoveVolumes: true`), but if you are on an
older build:

```bash
docker volume prune -f
df -h /
```

The failures name whichever spec ran next, not the leak, so the shape is a
"flaky test" that is really a resource exhausted three specs earlier.

If `poc/example` or `poc/demo-container` fail wholesale with launch errors,
their binaries are staged in `/tmp` and a VM reboot wipes it — re-run
`make demo-build` and reinstall them.

## What reaching the last four protocols cost

The first pass of this audit could not reach MongoDB, Cassandra, ClickHouse or
NATS: Docker Hub was unreachable from the dev VM, because the host had marked
the `lima0` gateway's routes `RTF_REJECT` (a VPN on the host does this) while
Lima's netplan gave that dead interface the winning route metric. DNS still
resolved via LIMADNS, so the symptom was every TCP connect timing out with name
resolution working perfectly.

It would have been easy to call those four "covered by the same code path" and
move on. Restoring connectivity instead found:

- dict and list step arguments silently mangled — every non-string value in a
  document became `""`, and `insert_many` had never run
- the NATS proxy corrupting **every** line it forwarded, by stripping CRLF
- `ready()` making a single attempt for MongoDB and Cassandra, so Cassandra
  could never have passed
- NATS `publish` reporting success without confirming the server received it

None of those were visible to a unit test. Sharing a code path is not evidence
that a path works.
