# Does `-pod-init-config` fix `watch()`?

**Date:** 2026-07-29
**Status:** Spike complete. Answer: **yes.**
**Context:** [RFC-054 decision record M5](../rfcs/0054-gvisor-packet-and-file-mediation.md)
**Environment:** Lima `faultbox-dev`, kernel 6.8, runsc `release-20260721.0`, `postgres:16-alpine`

---

## The question

v0.14.0 withdrew `watch()` because `runsc trace create` instruments only tasks
created *after* the session starts. Faultbox attaches once a service is healthy,
by which point every worker thread already exists — so a network-driven workload
was observed almost not at all:

| Workload | Trace points (v0.14.0) |
|---|---|
| Network query to the running backend | **2** |
| `docker exec psql` — newly spawned process | 1054 |

M5 hypothesised that runsc's `-pod-init-config`, which installs trace sessions
at sandbox boot, would fix it. That was a hypothesis. This is the measurement.

## Result

**It works.** Same workload shape, same image, same host:

```
after sandbox boot, before any query:  11295 points  (2815 writes, 8480 opens)
after one network-driven query:        11531 points

NETWORK-QUERY DELTA: 236        v0.14.0 measured 2
```

Two things are established, and the first matters more than the delta:

1. **Boot itself is instrumented.** 11,295 points arrived before a single query
   — every task in the sandbox is traced from its first instruction, which is
   exactly the property `runsc trace create` lacks.
2. **A network query against the already-running backend is observed.** 236
   points, versus 2. This is the shape `watch()` exists to serve.

Attribution is clean: the `psql` client ran under the **default** runtime
(runc), not the traced one, so it contributed nothing. Every point came from
the Postgres sandbox reacting to network input.

Zero decode errors, zero drops, across 11,531 points through the existing
`internal/gvisor/seccheck` decoder — the sink shipped in v0.14.0 needs no
changes.

## The flag is real but undocumented

`--pod-init-config` does not appear in `runsc --help` or `runsc help`. It is
nonetheless accepted, which a control confirms:

```
$ runsc --definitely-not-a-flag list
flag provided but not defined: -definitely-not-a-flag

$ runsc --pod-init-config=/tmp/pod-init.json list
ID          PID         STATUS      BUNDLE      CREATED     OWNER
```

Config shape (the trace session moves under a `trace_session` key; points and
sinks are otherwise identical to what `StartTrace` already builds):

```json
{
  "trace_session": {
    "name": "Default",
    "points": [ … ],
    "sinks": [{"name": "remote", "config": {"endpoint": "/path/to.sock"},
               "ignore_setup_error": false}]
  }
}
```

## What still has to be designed

The spike registered a **second runtime** rather than modifying the existing
one:

```json
"runsc-trace": {
  "path": "/usr/local/bin/runsc",
  "runtimeArgs": ["--pod-init-config=/tmp/pod-init.json"]
}
```

This is the crux M5 already identified, now with the shape confirmed:
`-pod-init-config` is a **runtime-level** flag in `daemon.json`, not a
per-container option. That is host-wide state, and it brings three problems the
implementation must answer:

1. **The sink path is baked into host config.** Every Faultbox run would have
   to agree on one socket path, or rewrite `daemon.json` and restart the Docker
   daemon per run — which is both slow and hostile on a shared machine.
2. **Concurrent runs collide.** Two runs pointing the same runtime at different
   sockets cannot coexist. This is the `faultbox0` problem again, one layer up:
   shared global state named without regard to who owns it.
3. **`ignore_setup_error: false` means the sandbox refuses to boot** if the sink
   is absent. Good for honesty — a silently untraced run is exactly what v0.14.0
   refused to ship — but it means a stale runtime registration breaks every
   gVisor container on the host, including ones that have nothing to do with
   Faultbox.

A dedicated runtime name (`faultbox-trace`) registered once at install time is
the obvious direction; the daemon restart is only tolerable if it happens then
and never per run.

**Follow-up measurement, same day.** Problem 3 turns out to be avoidable.
Setting `ignore_setup_error: true` was measured both ways:

| Sink | Result |
|---|---|
| absent | container **boots normally**, untraced |
| present | tracing **works** — points arrive as usual |

So a registration left on an idle machine is inert rather than a landmine, and
unrelated gVisor containers are never affected. The honesty that setting gives
up is recovered on the Faultbox side by the guard that already exists for
packet faults (`unwiredInstalls`): *`watch()` was installed, zero trace points
arrived → fail the test.* That check turned an 18-leaf run reporting all-success
into a loud failure twice during v0.14.1, so it is not a hypothetical.

This removes the need for a resident multiplexer process entirely. See
[RFC-056](../rfcs/0056-filesystem-observation.md).

## Verdict

`watch()` is **unblocked**. The decoder, the DSL, the event schema and the path
matching all shipped in v0.14.0 and are unchanged by any of this.

What remains is host-config lifecycle rather than tracing, and the follow-up
measurement shrank it further: with `ignore_setup_error: true` the registration
is inert when idle, so there is no resident process to build and no landmine to
defuse. The residue is a one-time `daemon.json` entry, a Faultbox-side guard
that already exists in another form, and a concurrency story that can be blunt
in v1.

Still its own release rather than a patch line — it introduces a host setup
step, and that deserves a version boundary and release notes rather than
arriving inside a patch. But the uncertainty M5 deferred is gone.

## Reproducing

The spike harness is `.claude/spike-podinit/main.go` (not committed to the
build; it is a measurement tool). It starts the v0.14.0 seccheck sink and
prints running counts.

```bash
# in the Lima VM
/tmp/spike-sink /tmp/spike-seccheck.sock &
docker run -d --name spike-pg --runtime=runsc-trace \
  -e POSTGRES_PASSWORD=x -p 15432:5432 postgres:16-alpine
docker run --rm --network host -e PGPASSWORD=x postgres:16-alpine \
  psql -h 127.0.0.1 -p 15432 -U postgres -c "INSERT INTO t SELECT generate_series(1,500); CHECKPOINT;"
```

`daemon.json` was backed up and restored; the VM is left as it was found.

## A note on the first measurement

The first run of this spike reported `opens=0` across 11,295 points, which
would have meant `openat` was not being delivered. It was a bug in the spike's
own counter — it matched `Op == "openat"`, and the decoder emits `Op == "open"`.
Corrected, the totals account exactly: 2,815 writes + 8,480 opens = 11,295.

Recorded because the failure mode is the one this project keeps meeting: a
number that looks like a finding about the system under test and is really a
finding about the instrument.
