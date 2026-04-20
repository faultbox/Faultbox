# RFC-022: Multi-Process Container Seccomp Acquisition

- **Status:** Draft
- **Target:** v0.9.0 or later
- **Created:** 2026-04-19
- **Discussion:** [#53](https://github.com/faultbox/Faultbox/issues/53)
- **Tracking issue:** [#51](https://github.com/faultbox/Faultbox/issues/51) (customer report + workaround usage)
- **Depends on:** RFC-014 (Unix socket fd passing — v0.6.0)
- **Workaround shipped in:** v0.8.5 (`service(seccomp = False)`)

## Summary

Some container images use a multi-process / fork-heavy entrypoint pattern
(MySQL 8's `mysqld_safe` wrapper, certain JVM-based images, shell-wrapped
`init` scripts). On these images, the RFC-014 SCM_RIGHTS handoff
between the `faultbox-shim` and the host-side runtime can hang, blowing
past the 3-minute test deadline and leaving stale containers behind.

This RFC proposes a real fix. A workaround — `service(seccomp = False)`
— already ships in v0.8.5 to unblock affected users; that workaround
sacrifices syscall-level fault injection on the opted-out service but
preserves everything above it (proxy-level faults, healthchecks, event
log, etc.).

## Motivation

### What RFC-014 fixed

Before RFC-014, Faultbox used `pidfd_getfd` to pull the seccomp listener
fd out of the container's init process. That worked for single-process
entrypoints (the container's init process *was* the target), but broke
for anything that `fork()`s before becoming the real workload — Kafka
in ZooKeeper mode, shell-wrapped entrypoints, etc.

RFC-014 replaced `pidfd_getfd` with SCM_RIGHTS fd passing over a Unix
domain socket. The `faultbox-shim` (container PID 1) installs the
seccomp filter, sends the resulting notification fd back to the host
via SCM_RIGHTS, and only then `exec`s the real entrypoint. This works
regardless of what the real entrypoint does with its own processes —
the fd is already on the host before any fork happens.

This shipped in v0.6.0 and fixed Kafka, Java container demos, and a
number of other images.

### What RFC-014 did not fix

The SCM_RIGHTS handoff assumes the shim itself runs to completion and
successfully connects back to the host's listening Unix socket. In
practice, certain container configurations break that assumption:

- **MySQL 8 (official image):** reported by a customer —
  `mysqld_safe` wrapper reconfigures signal handling + proc mounts in
  ways that appear to interact poorly with the shim's socket connect
  path. The shim never completes the handoff; the host blocks on
  `Accept()` until the 3-minute `testCtx` deadline
  ([runtime.go:481](../../internal/star/runtime.go#L481)).
- **JVM images with custom init scripts:** similar pattern — the shell
  wrapper may re-exec or daemonize in ways that drop the shim's
  socket connection.
- **Images that force `tini` / `dumb-init` as PID 1:** when the image
  author hard-codes a different entrypoint wrapper that we can't
  bypass without breaking signal handling.

Common thread: the shim is no longer guaranteed to be the process that
completes the handoff before the real workload starts fighting for
resources.

### Secondary bug: stale-container retry

When seccomp acquisition fails, the host falls back to `launchSimple`
(no-seccomp mode). That fallback was creating a container with the
same name as the failed attempt, which sometimes raced against Docker's
removal of the stale one — producing a name conflict that masked the
real root cause.

**Fixed in v0.8.5** at
[internal/container/launch.go:222-229](../../internal/container/launch.go#L222-L229):
`launchSimple` now force-removes any pre-existing container with the
target name before creating a fresh one. This is idempotent and safe
to call regardless of the code path that reached it.

## Technical Details

### Current state (v0.8.5)

- `seccomp = True` (default): shim installs filter, SCM_RIGHTS handoff,
  full syscall interception. Works for ~90% of images.
- `seccomp = False` (opt-out, v0.8.5): skip the shim entirely, launch
  container via `launchSimple`. Proxy-level faults still apply;
  syscall-level `fault()` rules on this service are no-ops.

The opt-out is a pragmatic escape hatch — users can still get value
from Faultbox on MySQL/JVM services (recipes, proxy faults, event log,
healthchecks, traces) without the syscall-interception features.

### Proposed approaches for the real fix

Three candidate designs, ordered by implementation cost:

#### Option A: Retry shim handoff with exponential backoff

**Idea:** If the first SCM_RIGHTS handoff attempt fails, retry 2–3
times with increasing timeouts before falling back.

**Why it might work:** Some failures are transient — the shim's
socket-connect races with the image's own init-time namespace setup.

**Why it might not:** Customer reports a 3-minute hang, not a fast
failure — suggests the shim never reaches the socket connect at all,
so retrying will just multiply the wait.

**Effort:** Small (~50 LOC).

**Risk:** Low. Strict superset of current behavior.

#### Option B: Inject seccomp via Docker's native profile support

**Idea:** Use Docker's `--security-opt seccomp=<profile.json>` to apply
the filter at container creation time, then wire up the notification
socket separately via the shim (or skip notification entirely for
simple deny-cases).

**Why it might work:** Docker's seccomp support predates the shim and
is battle-tested on every image, including multi-process ones. The
filter gets applied by the kernel regardless of what the entrypoint
does.

**Why it might not:** Docker's seccomp profile format supports
`SCMP_ACT_ALLOW` / `SCMP_ACT_ERRNO` / `SCMP_ACT_KILL` — but not
`SCMP_ACT_NOTIFY` without a user-listener socket. Getting notifications
out of a Docker-installed filter requires a separate handshake to
obtain the kernel-created notify fd, which is the problem we started
with.

**Effort:** Medium (need to verify `SCMP_ACT_NOTIFY` viability and
bind to the notify fd from outside the container).

**Risk:** Medium. May hit kernel/Docker API limits we don't fully
understand yet.

#### Option C: Pre-shim capture stage

**Idea:** Launch the container with a two-stage entrypoint — a tiny
static capture binary that does nothing except install the seccomp
filter and hand off the notification fd, then `exec`s the original
entrypoint immediately. No shell, no re-exec, no signal handler
reconfiguration.

**Why it might work:** The capture binary is minimal (<200 LOC Go or
hand-written C), statically linked, and does exactly one thing before
handing control to the image's entrypoint. The failure modes of the
current `faultbox-shim` (which does more than just capture) are
eliminated by scope reduction.

**Why it might not:** Still relies on SCM_RIGHTS, so any image that
interferes with Unix socket connect paths before our capture binary
runs is still broken.

**Effort:** Medium-to-large. New binary, new build target,
cross-platform concerns.

**Risk:** Medium. Need to verify in practice that a minimal capture
binary survives where the current shim doesn't.

### Preferred direction

**Option C + Option A as complementary.** Ship a minimal pre-shim
capture binary AND retry the handoff 2–3 times before falling back.
Keep `seccomp = False` opt-out as a permanent escape hatch — some
images (exotic init systems, custom container runtimes) will always
be outside what we can fix without customer-specific workarounds.

The opt-out is not a failure mode to be eliminated; it's a documented
behavior for a documented edge case.

## Open Questions

1. **Diagnostic data from failing images.** We know MySQL 8 hangs, but
   we don't have a packet-level trace of what the shim is (or isn't)
   doing. Getting one would tell us whether Option A alone is enough
   or whether we need Option C. First implementation step should be
   instrumentation, not a fix.

2. **Windows / Podman / containerd.** Faultbox is Linux-only today,
   but the multi-process seccomp fix should not assume Docker-specific
   socket semantics if we can avoid it.

3. **Interaction with `reuse = True` + `seccomp = False`.** Does
   container reuse cleanly when seccomp is disabled? Needs a
   deliberate test; RFC-015 lifecycle didn't anticipate the opt-out.

4. **Which syscall families benefit most from re-enabling seccomp on
   affected services?** If customers on MySQL almost exclusively want
   proxy-level faults anyway, Option C may not be worth the cost.

## Implementation Plan

### Phase 0 — Instrumentation (no behavior change)

1. Add shim-side structured logging for every phase of the handoff
   (filter install, socket dial, SCM send, exec).
2. Add host-side logging for every phase of `waitForListenerFd`
   (listen, accept start, accept done, SCM receive, ack send).
3. Reproduce the MySQL 8 failure with debug logging and capture the
   exact point at which the handoff stalls.

### Phase 1 — Quick win (Option A)

4. Add retry-with-backoff in `waitForListenerFd` — 3 attempts at
   10s / 30s / 90s. If all fail, fall back to `launchSimple` with a
   loud warning.

### Phase 2 — Real fix (Option C)

5. Spec the capture binary (`faultbox-capture`) — inputs, outputs,
   wire format, error handling.
6. Implement + build + ship alongside `faultbox-shim`.
7. Add runtime decision: if image matches a known-problematic pattern
   (or always, if Option C proves strictly better), use capture.
8. Deprecate `faultbox-shim` once capture is proven on the full
   integration-test matrix.

### Phase 3 — Verification

9. Integration tests against MySQL 8, a JVM image, and a tini-wrapped
   image covering before/after states.
10. Customer validation (the original report).

## Impact

- **Breaking changes:** None. `seccomp = False` is already a workaround;
  a real fix just makes it unnecessary for the affected images.
- **Migration:** None required. Existing specs continue to work.
- **Performance:** No measurable change for the common path.
- **Security:** New capture binary must be audited like the shim —
  runs as PID 1 with capabilities.

## Dependencies

- RFC-014 (Unix socket fd passing) — the mechanism we're sharpening.
- `service(seccomp = False)` opt-out in v0.8.5 — the escape hatch that
  gives us time to get this right.

## Alternatives Considered

- **Remove seccomp entirely, rely on proxy faults only.** Rejected —
  syscall-level injection is a differentiator, and many scenarios
  (disk fsync failures, network ENOBUFS, per-fd EBADF) are only
  expressible at the syscall level.
- **Require users to rewrite Dockerfiles for multi-process images.**
  Rejected — unacceptable customer friction; defeats the
  "works on your existing containers" value proposition.
- **Use eBPF instead of seccomp-notify.** Rejected for now — eBPF
  attach-to-process requires CAP_BPF and kernel 5.7+, narrower than
  seccomp-notify's kernel 5.6+ requirement. Would be a separate
  exploration (RFC TBD).
