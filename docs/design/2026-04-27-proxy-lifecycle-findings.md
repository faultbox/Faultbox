# Investigation: Proxy lifecycle gaps for host-binary SUT + Docker upstream

Date: 2026-04-27
Source: customer matrix run on `example-bundle-v0.12.10-dbmatrix.fb` (21 MB), 18/18 cells failed
Status: feeds RFC-033

## Customer-reported findings

> **Finding C (P0)**: `mysql`/`redis` proxy not started when `fault_assumption(target=db.mysql, ...)` is active.
> Comparing the same run:
> | Cell | mysql proxy | redis proxy | gRPC proxies |
> |------|-------------|-------------|--------------|
> | `test_get_client_config` (no fault) | started 127.0.0.1:36643 → 13306 | started 127.0.0.1:33637 → 16379 | 9 started |
> | `test_matrix_*_mysql_deadlock` (fault active) | never started | never started | 9 started |
>
> **Finding D (P1)**: `db.mysql.internal_addr` for Docker-backed services doesn't resolve to the proxy's host-loopback listener. Manual env-rewrite via `internal_addr.rsplit(":")` produces an unroutable address; truck-api healthcheck times out at 60s. For gRPC it works because gRPC upstreams are also host binaries.

## Setup that produced the report

- `db` — Docker service (mysql:8), two interfaces: `db.mysql` (port 3306) + `db.redis` (port 6379), `reuse=True`
- 9 gRPC services — host binaries, default `reuse=False`
- `truck-api` — host-binary SUT
- `fault_matrix(scenarios=[...], faults=[mysql.deadlock(), redis.timeout(), ...])` driving 18 cells
- One baseline cell `test_get_client_config` with no fault

## Code paths reviewed

### 1. Proxy startup — `preStartProxies`

[internal/star/runtime.go:978-1004](internal/star/runtime.go#L978-L1004):

```go
func (rt *Runtime) preStartProxies(ctx context.Context, svcName string, svc *ServiceDef) error {
    for ifaceName, iface := range svc.Interfaces {
        if !proxy.SupportsProxy(iface.Protocol) {
            continue
        }
        if rt.proxyMgr.GetProxyAddr(svcName, ifaceName) != "" {
            continue // already running (container reuse across tests)
        }
        port := iface.Port
        if iface.HostPort > 0 {
            port = iface.HostPort
        }
        targetAddr := fmt.Sprintf("127.0.0.1:%d", port)
        proxyAddr, err := rt.proxyMgr.EnsureProxy(ctx, svcName, ifaceName, iface.Protocol, targetAddr)
        ...
        rt.events.Emit("proxy_started", svcName, map[string]string{...})
    }
    return nil
}
```

Two relevant facts:
- The `GetProxyAddr != ""` short-circuit (line 983) is the **only** thing that gates re-start. It's there for container reuse.
- `proxy_started` emission happens **only** when a fresh proxy is created; the reuse path emits nothing.

### 2. Reuse skip in `startServices`

[internal/star/runtime.go:915-968](internal/star/runtime.go#L915-L968):

```go
for _, svcName := range order {
    svc := rt.services[svcName]

    // Skip services that are already running (reused from previous test).
    if _, running := rt.sessions[svcName]; running {
        rt.log.Info("reusing container from previous test", ...)
        rt.events.Emit("service_started", svcName, nil)
        rt.events.Emit("service_ready", svcName, nil)
        if err := rt.runResetCallback(svcName, svc); err != nil { ... }
        continue                        // ← preStartProxies is never reached
    }
    ...
    if !svc.IsMock() {
        if err := rt.preStartProxies(ctx, svcName, svc); err != nil { ... }
    }
}
```

When a service is reused, `preStartProxies` is skipped entirely — proxies stay alive from the previous test (kept in `stopServices` at [runtime.go:1476-1486](internal/star/runtime.go#L1476-L1486)) but no `proxy_started` event is re-emitted into the new test's trace partition.

### 3. `stopServices` proxy teardown

[internal/star/runtime.go:1471-1486](internal/star/runtime.go#L1471-L1486):

```go
for name, rs := range rt.sessions {
    if reused[name] {
        rs.session.ClearDynamicFaultRules()
        continue                        // proxy stays alive
    }
    rs.cancel()
    if rt.proxyMgr != nil {
        rt.proxyMgr.StopService(name)
    }
}
```

Confirms the asymmetry: non-reused services tear down proxies between tests; reused services keep them.

### 4. `internal_addr` attribute

[internal/star/types.go:188-194](internal/star/types.go#L188-L194):

```go
case "internal_addr":
    if r.Service.IsContainer() {
        return starlark.String(fmt.Sprintf("%s:%d", r.Service.Name, r.Interface.Port)), nil
    }
    return starlark.String(fmt.Sprintf("localhost:%d", r.Interface.Port)), nil
```

For `db.mysql` (container, port 3306) this is `"db:3306"`. From a host-binary SUT, `db` doesn't resolve.

### 5. Auto-substitution at `buildEnv` time

[internal/star/runtime.go:1636-1656](internal/star/runtime.go#L1636-L1656):

```go
func (rt *Runtime) proxyAddrSubstitutionsFor(mode consumerMode) map[string]string {
    out := make(map[string]string)
    for name, s := range rt.services {
        for ifName, iface := range s.Interfaces {
            proxyAddr := rt.proxyMgr.GetProxyAddr(name, ifName)
            if proxyAddr == "" {
                continue
            }
            target := proxyAddr
            if mode == containerConsumer {
                target = fmt.Sprintf("host.docker.internal:%d", pp)
            }
            out[fmt.Sprintf("localhost:%d", iface.Port)] = target
            out[fmt.Sprintf("127.0.0.1:%d", iface.Port)] = target
            out[fmt.Sprintf("%s:%d", name, iface.Port)] = target
        }
    }
    return out
}
```

The substitution table maps three literal forms (`localhost:3306`, `127.0.0.1:3306`, `db:3306`) → proxy addr. **Substitution is purely textual** — applied via `strings.ReplaceAll` at [runtime.go:1660-1668](internal/star/runtime.go#L1660-L1668). It only fires if the env value contains one of these exact substrings.

## Root cause analysis

### Finding C — what actually happened

Most likely **not** a proxy-lifecycle correctness bug. The proxy is almost certainly **still running** in the matrix cells. The customer is reasoning from the absence of `proxy_started` events in the per-cell trace partition.

The reuse path at [runtime.go:919-930](internal/star/runtime.go#L919-L930) skips `preStartProxies` entirely, including its `proxy_started` event emission. Because `db` has `reuse=True` (typical for stateful Docker services) and the gRPC services have `reuse=False` (host binaries), the trace shows:

- `db.mysql` / `db.redis`: emitted once in cell 1 (fresh start), absent in cells 2-18 (reused)
- gRPC: emitted in every cell (restarted each time)

Customer reads "absent" as "didn't run". **The trace is misleading**, not the proxy.

A second possibility is real: in `fault_scenario` body at [runtime.go:1747-1770](internal/star/runtime.go#L1747-L1770), the rule-application loop calls `EnsureProxy` again with `targetAddr := fmt.Sprintf("127.0.0.1:%d", port)`. If the proxy needs to be created here (rare — only if `preStartProxies` was skipped AND there was no prior proxy), the targetAddr formula is identical to the buggy one in `preStartProxies`. So Finding D's root cause applies here too.

### Finding D — real bug in two layers

**Layer 1: targetAddr is wrong for non-host-mapped Docker upstreams.** [runtime.go:990](internal/star/runtime.go#L990) hardcodes `127.0.0.1:port`. This works for binary services, and works for Docker services when Docker auto-mapped the port to the host (HostPort is captured at container launch in [container/launch.go:194](internal/container/launch.go#L194)). But:

- It breaks for Docker services without explicit port publishing
- It silently works for the customer's case (HostPort set), but the customer's *SUT-side connection* still fails for layer 2 reasons

**Layer 2: customer-side env wiring breaks the auto-substitution.** The substitution at [runtime.go:1664-1665](internal/star/runtime.go#L1664-L1665) is purely textual `strings.ReplaceAll`. It fires only when the SUT env value contains a literal `db:3306` / `localhost:3306` / `127.0.0.1:3306`.

The customer's spec does:
```python
host, port = db.mysql.internal_addr.rsplit(":", 1)  # "db", "3306"
env["MYSQL_HOST"] = "127.0.0.1"
env["MYSQL_PORT"] = port                             # "3306"
```

After the rsplit, `MYSQL_HOST` and `MYSQL_PORT` are separate env vars. Neither contains the substring `db:3306`. The substitution map has no entry that matches either. **The truck-api SUT dials `127.0.0.1:3306` — an unbound port on the host** (Docker auto-mapped 3306 → 13306, not → 3306). Connection refused, healthcheck times out.

The auto-injected `FAULTBOX_DB_MYSQL_{HOST,PORT,ADDR}` vars at [runtime.go:1576-1592](internal/star/runtime.go#L1576-L1592) DO point at the proxy correctly, but the customer's spec doesn't use them — they use the documented `db.mysql.internal_addr` attribute and split it. **The documented path is the broken one.**

This is why gRPC works: customer's gRPC spec helper happens to use the auto-injected envs (or composes with proxy-aware addrs) — for host-binary upstreams, `internal_addr` returns `localhost:port` (no rewrite needed because host loopback already works for both proxy and direct paths in their setup).

## Suggested fix shapes

### For Finding C — make the trace honest

Cheapest: in the reuse path at [runtime.go:919-931](internal/star/runtime.go#L919-L931), iterate the proxy manager's known proxies for the service and emit one `proxy_active` event per interface (with `mode: "reused"`). Renderer treats `proxy_active` and `proxy_started` as synonymous for the "wired up at cell start" panel.

This is a one-screen change, no behavior shift, makes the per-cell trace self-describing.

### For Finding D — first-class proxy address

Add three new lazy-resolved `InterfaceRef` attributes:

- `db.mysql.proxy_addr` → `"127.0.0.1:36643"` at runtime (the host-side proxy listener)
- `db.mysql.proxy_host` → `"127.0.0.1"`
- `db.mysql.proxy_port` → `36643`

At spec-load time these return a placeholder string (e.g., `"__FAULTBOX_PROXY_ADDR_db_mysql__"`). At `buildEnv` time, the runtime replaces placeholders with the real proxy addr resolved from `proxyMgr.GetProxyAddr`.

This gives customers a single, correct attribute to use when they need the SUT-facing proxy listener — no rsplit games, no reliance on textual substitution of upstream addresses.

Companion doc change: `internal_addr`'s use case shrinks to "container-to-container DNS only" and gets a doc note pointing host-binary SUT users at `proxy_addr` instead.

Optionally: emit a warning at runtime if a service's user-defined env contains references to a proxied interface's "real" addr that didn't survive substitution (e.g., the env value contains `db:3306` after applyAddrSubstitutions ran — meaning substitution didn't catch it). Helps debug similar issues later.

### Fault-scenario rule-apply path

Same fix as `preStartProxies` for the targetAddr at [runtime.go:1759](internal/star/runtime.go#L1759) — extract to a helper `proxyTargetAddr(svc, iface)` that handles container vs binary correctly.
