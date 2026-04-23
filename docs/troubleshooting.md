# Troubleshooting Playbook

Ten failure modes that consistently consume a half-day of engineering
time on every Faultbox onboarding, with the diagnostic shortcut for
each. Pulled from the v0.9.x customer reports — most of these were
hours-of-Slack-DM situations.

## 1. "My fault rule installed but nothing fired"

Symptom: `faultbox test` reports green; `--format json` shows
`"hits": 0` for the rule. Since v0.9.7 the terminal also surfaces
`Zero-traffic faults (N): rule installed, matched no syscalls`.

Most-likely causes (ranked):

1. **SUT cached the connection** — the upstream connect happened
   before your fault window opened, and the SUT reused the open
   socket. Fault `write` or `read` instead of `connect`.
2. **Wrong syscall name** — you faulted `sendto` but the SUT uses
   plain `write` (stream socket). Use the family canonical:
   `write=deny(…)` expands to write/writev/pwrite64.
3. **Fault window too narrow** — the scenario didn't drive any
   upstream traffic during the lambda body. Use
   `fault_start`/`fault_stop` to span more of the test, or move the
   inducing call inside the `run=` lambda.

Diagnostic: see [seccomp-cheatsheet.md](seccomp-cheatsheet.md) for the
Go-op-to-syscall table; the v0.9.4 `fault_zero_traffic` event
generally points at one of (1)/(2)/(3) directly.

## 2. "Test passes locally but fails on Lima/CI"

Symptom: `faultbox test` is green on the host but red in CI or in a
fresh Lima VM.

Most-likely causes:

- **Stale binary** — Faultbox uses cached binaries in `bin/faultbox`
  if they exist. Delete and rebuild: `rm bin/faultbox && make
  build`.
- **Different Go toolchain** — the version that compiled the SUT
  affects which syscalls it emits. Check
  `faultbox inspect run-*.fb env.json` for `go_toolchain` on both
  hosts.
- **Different image digests** — `mysql:8.0.32` resolves to different
  bytes over time. Pin via `faultbox.lock` (v0.10.0+); meanwhile,
  pin in your spec: `image="mysql@sha256:…"`.
- **Different kernel** — seccomp behaviour can shift across major
  kernel versions (rare, but real). Check `env.json` for `kernel`.

Diagnostic: `faultbox inspect <bundle>.fb` shows everything
environment-related the run captured. Diff two bundles' `env.json`
to spot the drift.

## 3. "TCP healthcheck says ready but app rejects requests"

Symptom: `healthcheck=tcp("localhost:5432")` returns success, then
the first `step()` against the service fails with `connection
refused` or `protocol error`.

Cause: `tcp()` only proves a port is bound. Postgres/MySQL etc.
have to handshake the protocol layer before accepting queries —
Docker's port-forwarder accepts TCP connections **before** the app
behind it is ready.

Fix: replace `tcp(...)` with a protocol-aware check:

```python
healthcheck = http("http://localhost:8080/healthz", expect_status = 200)
# Or for SQL services, poll with a real connection (write a custom
# wait loop in the test driver):
healthcheck = wait_until(lambda: step(db.main, "query", sql = "SELECT 1").success)
```

This was the #5 hour-burner on the inDrive PoC.

## 4. "Container starts but truck-api can't reach it"

Symptom: `service` container is running (you can see it in
`docker ps`), but the SUT container's request to it times out.

Cause: until v0.9.6 the proxy ran on the host's loopback only — a
container couldn't reach `127.0.0.1:<port>` on the host from inside
its netns. v0.9.6 added `host.docker.internal` via Docker's
`ExtraHosts`, plus auto-rewriting in `buildContainerEnv`.

Fix: upgrade to v0.9.6+. If you're on v0.9.5, set
`env={"OTHER_SVC_ADDR": other.main.internal_addr}` and use the
container DNS name (matching container-mode's pre-RFC-024 behaviour).

## 5. "JWT-protected request returns 401 even with my mock token"

Symptom: SUT rejects every token your `jwt.server()` mints.

Most-likely causes:

- **Wrong claim name** — your middleware expects `uid` but you sent
  `user_id` (this was 8h on the inDrive PoC). Check what the SUT
  actually validates.
- **Audience mismatch** — middleware demands `aud="api.example.com"`
  but you didn't set it. Add `aud` to the claims dict.
- **Token expired** — `iat`/`exp` claims are seconds-since-epoch.
  If you set them once and re-ran a day later, they're stale.
  Either omit `exp` (some middlewares don't enforce it) or compute
  `now + 3600` in your driver.
- **JWKS endpoint unreachable from the SUT** — the SUT fetches the
  JWKS over HTTP. If the issuer URL doesn't resolve from inside the
  SUT's netns, no signature verifies. Check
  `OIDC_JWKS_URL` actually works from the SUT.

Diagnostic: enable the SUT's auth-middleware DEBUG logs and look for
the actual rejection reason. Most middlewares log "claim X missing"
or "kid not found in JWKS."

## 6. "fault_matrix runs but my expectation never fires"

Symptom: every cell of a fault_matrix passes, even ones that
shouldn't.

Cause: `default_expect=` accepts callables but **doesn't fail unless
the callable raises**. A returning-truthy lambda is read as "pass."

Fix: use the v0.9.8 explicit predicates:

```python
fault_matrix(
    scenarios      = [...],
    faults         = [...],
    default_expect = expect_success(),  # explicit, raises on violation
)
```

Or for ad-hoc cases, use `assert_true` inside the lambda:

```python
default_expect = lambda r: assert_true(r != None, "scenario hung"),
```

## 7. "Lima VM hangs on `make demo`"

Symptom: `make demo` runs the binary in the Lima VM but the test
never completes.

Most-likely causes:

- **VM out of resources** — check `limactl shell faultbox-dev free
  -h`. seccomp-notify is memory-light but Docker daemon + your
  containers add up.
- **Stale Docker network** — `docker network rm faultbox-net` then
  re-run.
- **Stale containers from a previous test** — `docker ps -a |
  grep faultbox-` and `docker rm -f` anything reusable. The runtime
  cleans up on success but a panic mid-test can orphan containers.

If the hang is reproducible: kill it with `Ctrl-C`, then
`faultbox inspect run-*.fb` — the partial bundle usually shows
which service hadn't reached `service_ready` yet.

## 8. "Spec loads then immediately errors with `unknown keyword 'X'`"

Symptom:
`error: load test.star: fault_assumption() unexpected keyword 'foo'`

Cause: undocumented kwargs are a parse error since v0.9.7. You
either typed a kwarg name wrong or you're on a version that
predates a feature you saw in docs/examples.

Fix:

1. Double-check the kwarg name in [spec-language.md](spec-language.md).
2. Check your version: `faultbox --version`. Compare against the
   feature's "shipped in vX.Y.Z" callout in the docs.
3. Bump if needed: `brew upgrade faultbox` or download from GitHub
   releases.

## 9. "Bundle says faultbox 0.9.7 but I have 0.9.8 installed"

Symptom: `faultbox inspect run-*.fb` warns
`bundle was produced by faultbox 0.9.7; current is 0.9.8`.

Cause: bundle was generated by an older binary; you've since
upgraded. Reading the bundle still works (`inspect`/`report` never
refuse on minor-drift). For byte-identical replay, install the
producer version.

`faultbox replay` (v0.10.0+) refuses on **major** version drift only
(0.x → 1.x) — minor/patch drift warns and proceeds. See the
[`bundles.md`](bundles.md) version-compat table for the full matrix.

## 10. "Container DNS works in step() but not from the SUT"

Symptom: `step(db.main, "query", ...)` works, but the SUT inside its
container errors on `dial tcp: lookup db: no such host`.

Cause: `step()` runs from the test driver (host process) which uses
`localhost:<HostPort>` to reach the container. The SUT inside its
container needs to use the Docker DNS name (`db`) over the
`faultbox-net` bridge.

Fix: pass the right address into the SUT's env:

```python
api = service("api", image = "myapi:latest",
    env = {
        "DB_HOST": db.main.internal_addr,  # → "db:5432" inside container
    },
)
```

Use `.internal_addr` for service-to-service references in container
mode. `.addr` returns the host-port form, which only the test
driver can reach.

## See also

- [bundles.md](bundles.md) — bundle inspection (`faultbox inspect`)
  is the single best diagnostic tool when something goes wrong.
- [seccomp-cheatsheet.md](seccomp-cheatsheet.md) — Go-op → syscall
  mapping for "which family do I fault" questions.
- [starlark-dialect.md](starlark-dialect.md) — Starlark gotchas that
  cause spec-load failures.
- [GitHub issues](https://github.com/faultbox/Faultbox/issues) —
  if your case isn't here, file it; we add to this page.
