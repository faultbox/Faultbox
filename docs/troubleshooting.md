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
the first request against the service fails with `connection
refused` or `protocol error`.

Cause: `tcp()` only proves a port is bound. Postgres/MySQL etc.
have to handshake the protocol layer before accepting queries —
Docker's port-forwarder accepts TCP connections **before** the app
behind it is ready.

Fix: use `ready()` (v0.16.0), which asks the service through its protocol
plugin rather than asking whether a port is open:

```python
healthcheck = ready(timeout = "60s")
```

For Postgres and MySQL that is a real `SELECT 1` using the credentials the
spec declared in `env=`; for Redis a `PING`; for MongoDB a `Ping`. It retries
to the timeout, so a slow cold start is waited out rather than guessed at.

Measured on a `postgres:16-alpine` container: `tcp()` reported ready in
**0 ms**, `ready()` in ~2.4 s — which is when the database could actually
answer.

An app with its own `/healthz` that checks its DB connection is still the
most meaningful gate when there is an app in front:

```python
healthcheck = http("http://localhost:8080/healthz")
```

Before v0.16.0 there was no query-based builtin, and the usual workaround was
`tcp()` plus a generous `sleep()`. That is no longer necessary.

This was the #5 hour-burner in an early field evaluation.

## 4. "Container starts but order-service can't reach it"

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
  `user_id` (this was 8h in an early field evaluation). Check what the SUT
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

## 10. "Container DNS works from the test driver but not from the SUT"

Symptom: `db.main.query(...)` from the test body works, but the SUT
inside its container errors on `dial tcp: lookup db: no such host`.

Cause: test-body requests run from the test driver (host process) which
uses `localhost:<HostPort>` to reach the container. The SUT inside its
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

## 11. "Host-binary SUT can't connect to a Docker DB upstream"

Symptom: `order-service` (a host binary) times out at the healthcheck stage
while trying to connect to a Docker `db` service. Trace shows the proxy
started cleanly. Spec wires SUT env from `db.main.internal_addr.rsplit(":")`.

Cause: `internal_addr` returns the container DNS name (`"db:3306"`) which
the host-binary process can't resolve. Worse, the auto-substitution that
rewrites real addrs to proxy addrs only matches the literal substring
`db:3306` in env values — so `rsplit(":")` decomposition silently breaks
it. The SUT ends up dialing `db:3306` or `127.0.0.1:3306` (the unmapped
container-internal port) and times out.

Fix: use `iface.proxy_addr` / `proxy_host` / `proxy_port` instead. These
are late-bound — they return placeholders at spec-load and resolve to the
real proxy listener at test-execution:

```python
api = service("order-service", "/usr/local/bin/order-service",
    interface("public", "http", 9000),
    env = {
        "MYSQL_HOST": db.main.proxy_host,
        "MYSQL_PORT": db.main.proxy_port,
        "MYSQL_DSN":  "user:pass@tcp(" + db.main.proxy_addr + ")/appdb",
    },
)
```

Don't `.split()` or `.rsplit()` `proxy_addr` — operations run at spec-load
where it's still a placeholder. Use the separate `proxy_host` /
`proxy_port` attributes when you need the parts.

See [recipes.md → Wiring SUTs to the proxy](recipes.md#wiring-suts-to-the-proxy)
for more context. Fixed in v0.12.12 (RFC-033).

## 12. "Service exited before becoming ready" / missing-binary launch

Symptom (binary mode): the test fails fast with
`service "order-service" exited before becoming ready: exec /tmp/order-service: no such file or directory`.

Cause: the target binary path in your spec doesn't exist, isn't
executable, or wasn't built for the VM's architecture. Before v0.13.0
this surfaced as a misleading `context deadline exceeded` a full
healthcheck-timeout later (often 60s), with `exit_code=0` in the
session log — the exec failure was invisible. v0.13.0 resolves and
verifies the target before signaling readiness, so the launch now fails
immediately and names the path that couldn't be exec'd.

Fix:

1. Confirm the path in your `service(...)` declaration exists in the VM
   and is executable: `make env-exec CMD='ls -l /tmp/order-service'`.
2. Rebuild for the VM arch if it's a cross-compile
   (`GOOS=linux GOARCH=arm64`). A host-built (darwin) binary copied into
   the VM produces an exec format error here.

Related, for **container mode**: if the `faultbox-shim` binary is
missing alongside `faultbox`, container services never reach
`service_ready`. Build and install both with `make install-lima` (see
[README → Build from source](../README.md#build-from-source)). The shim
is the container entrypoint; without it the container can't start.

## 13. `TUNSETIFF ...: device or resource busy` on a packet-fault spec

The packet gateway (`determinism(runtime = "gvisor")`) needs a TUN device, and
a TUN device created with `ip tuntap add` is **persistent** — it outlives the
process that made it. If a run dies without tearing down (SIGKILL, an OOM kill,
a panic), the device stays behind.

**In v0.14.1 this is self-healing.** Devices are named per-process
(`fbox<pid>`), so a leftover cannot collide with a new run, and each run sweeps
orphans on startup — any `fbox<pid>` whose owning process is gone is removed:

```
packet gateway: removed orphaned TUN device left by an earlier run
  device=fbox999999 owner_pid=999999
```

Devices belonging to *live* processes are never touched, so concurrent runs on
one host are fine.

**If you are on v0.14.0**, the device was the shared constant `faultbox0` and
neither property held: two concurrent runs collided, and one leaked device
broke every later run on that host. Recover with:

```bash
sudo ip link delete faultbox0
```

Worth knowing because of how the failure presents: the run **continues**,
installs its packet rules into a gateway that is not there, and every fault
silently affects nothing. Faultbox fails the test rather than reporting that as
a pass —

```
packet faults were installed 8 time(s) but no netstack gateway was attached,
so no packet was affected; the result below would be meaningless
```

— but if you see that message, this is why.

## 14. Database steps fail with `invalid connection`, `connection reset by peer`, or `role "root" does not exist`

Symptom: a `query()` / `exec()` step against a real database returns
`ok = False` with one of:

| Message | Protocol | What it actually means |
|---|---|---|
| `invalid connection` | MySQL | Access denied — no password was sent |
| `Error 1046: No database selected` | MySQL | The DSN named no schema |
| `read: connection reset by peer` after ~60 s | Postgres | The auth handshake deadlocked |
| `pq: role "root" does not exist` | Postgres | Fell back to the OS user (`root` under sudo) |
| `NOAUTH Authentication required` | Redis | No `AUTH` sent to a `--requirepass` server |
| `command requires authentication` | MongoDB | No credentials sent |

Cause: the step client was not sending the credentials the spec declared.
Fixed for Postgres in **v0.16.0** and for MySQL, Redis, MongoDB, ClickHouse
and Cassandra in **v0.16.1**.

Fix: upgrade. Then declare credentials once, in `env=`, using the image's own
convention — see [protocols/README.md](protocols/README.md#credentials-come-from-the-service-v0160-extended-v0161)
for the per-protocol table. Steps, healthchecks and `ready()` all pick them
up; an explicit `user=` / `password=` / `database=` on a step overrides them.

**Why this went unnoticed for so long, and what to change in your specs:** a
failing step returns `ok = False` rather than raising, so a spec that never
checks the result passes whether or not anything worked. If your database
steps have never been asserted on, add the check before assuming they work:

```python
r = db.main.exec(sql = "INSERT INTO t VALUES (1)")
assert_true(r.ok, "insert failed: %s" % r.error)
```

See [Pattern 0](guides/spec-patterns.md#pattern-0-assert-on-every-step).

## 15. `ready()` times out against a service that is clearly running

Symptom: `healthcheck = ready(timeout = "60s")` burns its whole budget and
reports `not ready: context deadline exceeded`, while `docker exec` into the
container shows the service answering fine.

Cause, if you are on **v0.16.0**: `ready()` resolved to
`<protocol>://host:port` and handed the whole string to the protocol plugin,
but only Postgres, HTTP and HTTP/2 could parse a URL. Every other plugin
dialled it verbatim — attempting to reach a host literally named
`redis://localhost` — so the check could never succeed. Affected Cassandra,
ClickHouse, gRPC, MongoDB, MySQL, NATS, Redis and UDP.

Fix: upgrade to **v0.16.1**. As a workaround on v0.16.0, use `tcp()` and
accept that it only proves the port is bound.

## 16. In the Lima VM, DNS resolves but every connection times out (`docker pull` hangs)

Symptom: inside `limactl shell faultbox-dev`, name resolution works but nothing
connects.

```
$ getent hosts registry-1.docker.io      # resolves fine
98.87.178.151   registry-1.docker.io
$ curl https://github.com                # times out
$ docker pull mongo:7
... dial tcp 44.205.146.148:443: i/o timeout
```

Cause: the VM has **two** default routes, and the one Lima prefers is dead.

```
$ ip route show default
default via 192.168.64.1 dev lima0 ... metric 100    ← preferred, unreachable
default via 192.168.5.2  dev eth0  ... metric 200    ← works
```

`lima0` is the vmnet/vzNAT interface; `eth0` is Lima's user-mode NAT. Lima gives
`lima0` the better metric because in a healthy setup it is the better path. But
on the host, a VPN (Tailscale, corporate clients — anything owning a `utun`
default route) commonly marks the vmnet bridge's routes **`RTF_REJECT`**:

```
$ netstat -rn -f inet | grep 192.168.64
192.168.64         link#30            UC     bridge100  !     ← ! = reject
```

Everything outbound then blackholes. DNS still works because Lima intercepts it
with an iptables `LIMADNS` rule and answers locally — which is exactly what makes
this confusing: resolution succeeds, so it does not look like a network problem.

Diagnose by testing each interface directly:

```bash
curl -m 8 --interface lima0 https://github.com    # hangs
curl -m 8 --interface eth0  https://github.com    # 200
ping -c 2 192.168.64.1                            # 100% loss
```

**Fix** — prefer `eth0`, persistently:

```bash
sudo tee /etc/netplan/99-faultbox-prefer-eth0.yaml >/dev/null <<'EOF'
network:
  version: 2
  ethernets:
    lima0:
      dhcp4-overrides:
        route-metric: 900
EOF
sudo chmod 600 /etc/netplan/99-faultbox-prefer-eth0.yaml
sudo netplan generate && sudo netplan apply
```

A VM restart is **not** enough on its own — the interface comes back with the
same metric and the host-side reject is unchanged.

Reverting is a file deletion, so it costs nothing to undo once `lima0` is
healthy again:

```bash
sudo rm /etc/netplan/99-faultbox-prefer-eth0.yaml && sudo netplan apply
```

Trade-off: `eth0` is userspace-NATted, so throughput is lower than a working
vmnet path, and the VM keeps its `192.168.64.2` address for host→VM access
(only the *default route* is deprioritised, not the subnet route). If you would
rather fix the host, the VPN is where to look.

## 17. "My suite is green but I don't trust it"

Two v0.17.0 diagnostics answer exactly that, and both are warnings printed after
the summary:

```
WARNING: [NO_POSITIVE_CONTROL] no test asserts that a step on 'pg.main' succeeds
WARNING: [TEST_NO_ASSERTIONS] test_smoke: passed without evaluating any assertion
```

`NO_POSITIVE_CONTROL` is the one worth acting on. It means every assertion about
that interface is satisfied by a client that cannot connect at all — so the suite
would not fail if it broke. That is not hypothetical: a CI spec here exercised a
completely broken Postgres client on every pull request for three releases and
passed, because its only assertion was that a query *fails* under an injected
fault.

**Fix:** add one test that runs a step with no fault injected and asserts `r.ok`.
See [`poc/protocol-audit/`](../poc/protocol-audit/) — each spec there pairs a
statement that must succeed with one that must fail.

To catch spec problems *before* running anything, use
[`faultbox check`](cli-reference.md#faultbox-check-v0170). Note it cannot report
these two: deciding whether an assertion is positive or negative from source
alone is unreliable, so they need a run.

Every code and its remedy: [diagnostic-codes.md](diagnostic-codes.md).

## 18. Every socket in the SUT times out at once, and never recovers

```
read tcp 127.0.0.1:54012->127.0.0.1:41863: i/o timeout
dial tcp 127.0.0.1:41863: i/o timeout
```

Established connections *and* new dials both fail, from one moment onward, with
no recovery. The dependency is healthy, and steps from the spec to the same
service still succeed - only the SUT is affected. Services with a connection
pool are hit hardest; a quiet single-connection client may keep working.

This was a Faultbox bug, fixed in v0.18.0. The seccomp supervisor stopped
answering while the service kept its filter, so every intercepted syscall
blocked forever. It was silent - no log line, no failed test, just a service
that went quiet.

**If you see it on v0.18.0 or later**, the run now says so:

```
ERROR seccomp supervisor stopped while the target is still running
  impact=intercepted syscalls will now block indefinitely
```

and the test fails with that reason rather than timing out. Attach the bundle to
an issue - the supervisor is not supposed to stop.

**On v0.17.0 and earlier** there is no workaround beyond upgrading. The
`dropped_notifications` counter that would have shown it did not exist yet.

## 19. A fault is declared but nothing is intercepted

```
WARNING: [FAULT_NOT_FIRED] write fault on 'api' was installed but never fired
```

with `rule_count=0` and `seccomp=false` in the session log, meaning no filter was
installed at all.

Before v0.18.0, Faultbox decided which services needed a filter by searching spec
source for the exact text `fault(<var>,` and `write=deny(`. Spaces around the `=`
or a call split across lines matched neither, so the fault silently did nothing:

```python
fault(db, write = delay("1ms"), run = s)   # spaces - was not seen
fault(                                     # multi-line - was not seen
    db,
    write = deny("EIO"),
    run = scenario,
)
```

Both work from v0.18.0. The scan is still static, so two shapes remain invisible
to it on any version:

```python
rules = {"write": deny("EIO")}     # built in a variable
fault(db, **rules, run = scenario)

def hit_the_db(scenario):          # wrapped in a helper
    fault(db, write = deny("EIO"), run = scenario)
```

**Fix:** spell the fault inline at the call site. If you need the indirection,
check the session log for `seccomp=true` on the target service before trusting
the result.

## See also

- [gvisor-requirements.md](gvisor-requirements.md) — what packet faults
  and `watch()` each need. Most "it did not attach" failures are a
  missing prerequisite, and the two features do not need the same ones.
- [bundles.md](bundles.md) — bundle inspection (`faultbox inspect`)
  is the single best diagnostic tool when something goes wrong.
- [seccomp-cheatsheet.md](seccomp-cheatsheet.md) — Go-op → syscall
  mapping for "which family do I fault" questions.
- [starlark-dialect.md](starlark-dialect.md) — Starlark gotchas that
  cause spec-load failures.
- [GitHub issues](https://github.com/faultbox/Faultbox/issues) —
  if your case isn't here, file it; we add to this page.
