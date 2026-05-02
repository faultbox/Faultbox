# Cluster connectivity for `service(remote=...)`

Faultbox's `remote=` primitive (RFC-036) points at an externally-running
endpoint instead of launching a process. Faultbox itself is
**cluster-agnostic** — the `remote=` value is just a hostname that the
host resolves and the proxy dials. How that hostname becomes reachable
is your choice. This page covers the four supported workflows in order
of preference.

> Use `remote=` when your dependency's Docker image isn't available on
> the developer machine — common in shared k8s dev platforms where
> images are built centrally and only deployed to the cluster.

## Quick decision tree

| Situation | Use |
|---|---|
| Developer laptop, want cluster Service DNS to work | **Telepresence connect** |
| CI / dev container running inside the cluster | **In-cluster execution** |
| One or two specific services, light setup | **kubectl port-forward** |
| Already have a corporate VPN to the cluster | **VPN** |

All four end up the same to Faultbox: `remote = "geo.staging.svc.cluster.local"`
resolves and dials successfully. Differences are in setup overhead and
what else they affect on your machine.

---

## 1. Telepresence connect (recommended)

`telepresence connect` proxies your laptop into a k8s namespace's network.
After connecting, `*.svc.cluster.local` resolves on the host as if you
were inside the cluster, and outbound TCP traffic is routed through a
Telepresence pod in the cluster.

```sh
$ telepresence connect
$ kubectl config use-context dev-cluster
$ telepresence intercept geo-config --port 0  # not needed; we just want connect

# Verify connectivity
$ curl http://geo-config.staging.svc.cluster.local:8080/healthz
ok
```

Then run Faultbox normally:

```sh
$ faultbox test truck-api.star
```

The pre-started proxies dial via the host's resolver, which Telepresence
has rewritten to point at the cluster.

**Why we recommend this:** zero per-service config in your spec, works
for hostnames you don't enumerate up front, no port-forwarding to keep
running. Trade-off: a Telepresence agent runs in the cluster and a
local daemon runs on your machine while connected.

---

## 2. In-cluster execution

If you run `faultbox test` inside a pod in the same cluster (CI on
self-hosted runners scheduled in-cluster, dev containers, GitHub Actions
self-hosted runners deployed as k8s Pods), Service DNS works natively
with no Faultbox configuration.

Pod manifest checklist:

- `dnsPolicy: ClusterFirst` (default) so `*.svc.cluster.local`
  resolves to cluster IPs.
- A ServiceAccount with read access to the namespaces you reference.
- Egress NetworkPolicies (if any) permitting traffic to the
  dependency Services.

The bundle's `env.json` will record `runtime_hints: ["k8s"]` (you set
this via `FAULTBOX_RUNTIME_HINT=k8s` if your CI image doesn't autodetect
it) so reports flag in-cluster runs.

**Why we recommend this for CI:** no Telepresence dependency, simpler
permissions, the connectivity question moves into your Pod manifest
where it belongs. Trade-off: you can't run the same spec from your
laptop without a separate setup.

---

## 3. kubectl port-forward (or kubefwd)

For specs that depend on a small number of cluster services, raw
port-forwards are the lightest setup:

```sh
$ kubectl port-forward -n staging svc/geo-config 8080:8080 &
$ kubectl port-forward -n staging svc/auth-server 50051:50051 &
$ kubectl port-forward -n staging svc/feature-flags 8082:8080 &
$ faultbox test truck-api.star
```

In your spec, point `remote=` at the local forwarded address:

```python
geo = service("geo-config",
    interface("public", "http", 8080),
    remote      = "127.0.0.1",
    healthcheck = http("127.0.0.1:8080/healthz"),
)
```

`kubefwd` (https://github.com/txn2/kubefwd) is a useful wrapper if you
want all Services in a namespace forwarded at once with their cluster
hostnames preserved on the host:

```sh
$ sudo kubefwd svc -n staging
# Now geo-config.staging:8080 resolves on your host
```

**Why we recommend this for ad-hoc work:** zero cluster install, easy
to reason about. Trade-off: doesn't scale to dozens of dependencies
without becoming a process-management chore.

---

## 4. VPN

Functionally identical to Telepresence connect from Faultbox's
perspective: the host resolver and routing are configured to reach the
cluster. Use whatever VPN your platform team blesses; Faultbox doesn't
care.

---

## Healthcheck failure hint

If a remote service is unreachable on session start, the failure
message looks like:

```
remote service "geo-config" not reachable at http://geo-config.staging.svc.cluster.local:8080/healthz: dial tcp: lookup geo-config.staging.svc.cluster.local: no such host

Faultbox does not manage cluster connectivity. Verify one of:
  - `telepresence connect` is running and the namespace is in scope
  - `kubectl port-forward` covers this service
  - You are running faultbox from inside the target cluster
  - The host is reachable from your network (VPN, direct route)

The remote= value was "geo-config.staging.svc.cluster.local".
```

The hint is intentional — if you're seeing it, the issue is *always*
on the connectivity side, not the spec side. Verify with `curl` or
`nc` that the address resolves and the port answers before re-running
Faultbox.

---

## TLS upstreams (RFC-038)

Most production-shaped cluster services speak TLS — Postgres-with-`sslmode=require`,
HTTPS REST, gRPC-over-TLS. `remote=` composes with `interface(..., tls=tls_cert(...))`
so the proxy terminates TLS at the listener and dials the upstream over TLS:

```python
geo = service("geo-config",
    interface("public", "http", 8080,
        tls = tls_cert(insecure = True),  # accept self-signed cluster certs
    ),
    remote      = "geo-config.staging.svc.cluster.local",
    healthcheck = tcp("geo-config.staging.svc.cluster.local:8080"),
)
```

Practicalities:

- Six protocols terminate TLS today: http, http2, grpc, kafka,
  redis, tcp. Postgres / MySQL / MongoDB / Cassandra / ClickHouse /
  memcached / NATS / amqp ship in RFC-039 — declaring `tls=` on
  those produces a `proxy_tls_pending` event so the gap is visible.
- The auto-generated proxy cert covers `127.0.0.1` and `localhost`
  by default, so SUT-side TLS verification works without supplying
  your own cert. Supply `tls_cert(cert=..., key=...)` if you need
  a stable fingerprint.
- Use `tcp()` healthchecks for TLS upstreams. The runtime's `http()`
  healthcheck client verifies certs and doesn't accept a CA / insecure
  flag yet.
- For mTLS to the upstream, use `tls_cert(cert=..., key=..., ca=...)`
  — the same value drives both legs (server config the SUT sees;
  client config the proxy presents to the upstream).

## Reproducibility

Bundles from runs that used `remote=` are flagged in `env.json` under
`remotes: [...]`. `faultbox replay` warns and re-dials those endpoints,
so replay requires the same connectivity that the original run had.
Fully offline replay (against recorded remote responses) is tracked in
**RFC-037**.

For tests where deterministic replay is non-negotiable today, swap
`remote=` for `mock_service()` and check the mock contract into git.
The DSL surface is otherwise identical.

---

## Related

- [RFC-036 — Remote Services](../rfcs/0036-remote-services.md)
- [RFC-037 — Determinism for Remote Services](../rfcs/0037-remote-determinism.md)
- [Spec language reference: Remote Services](../spec-language.md#remote-services)
