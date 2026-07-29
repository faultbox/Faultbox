# Running Faultbox inside a k8s dev namespace

This page is written to be handed to the people who have to approve it.
It covers what Faultbox does at runtime, what it needs from the cluster,
and — the part that matters most — **how to ask for the least privilege
that still covers your tests.**

Three audiences:

- **Developers** — pick a privilege tier, write the spec.
- **Ops** — the pod manifest, network reachability, image distribution.
- **Security** — the privilege review, blast radius, data handling.

Related: [connectivity.md](connectivity.md) (laptop workflows, Telepresence),
[ci-on-linux.md](ci-on-linux.md) (host requirements outside k8s).

---

## 1. What Faultbox does, for a reviewer who has never seen it

Faultbox is a **test runner**. It runs as one pod in your own dev
namespace and does three things:

1. Starts your **service-under-test (SUT)** as a child process inside
   that same pod.
2. Puts a **local TCP proxy** in front of each dependency the test spec
   declares, and dials the dependency's ordinary k8s Service.
3. **Injects failures into that proxy leg** — HTTP 503s, added latency,
   connection resets, SQL errors — and asserts on how the SUT behaves.

The SUT talks to `127.0.0.1:<proxy port>` instead of
`geo-config.my-ns.svc.cluster.local:8080`. Faultbox rewrites the
dependency address in the SUT's environment
([runtime.go:3385](../../internal/star/runtime.go#L3385)). Everything
else about the dependency call is normal client traffic.

### What it does *not* do

This list is the substance of the security review. Each item is a
design property, not a configuration choice:

| Claim | Why it holds |
|---|---|
| **No Kubernetes API access.** No ServiceAccount permissions, no RBAC rules, no CRDs, no operator, no admission webhook, no kubeconfig. | Faultbox has no k8s client. The `@faultbox/discovery/k8s.star` helper is string formatting — see [discovery/k8s.star](../../discovery/k8s.star). RFC-036 took "cluster-agnostic core" as an explicit non-goal boundary. |
| **No mutation of the cluster.** Does not deploy, patch, restart, scale, or annotate anything. | There is no code path that writes to the cluster. |
| **Does not intercept other pods' traffic.** Not a service mesh, not a sidecar injector, no iptables rules outside its own pod. | Faults apply to connections the SUT originates through Faultbox's own in-pod proxy listener. |
| **Does not touch dependency processes.** Cannot pause, signal, or filter syscalls in a pod it doesn't own. | Syscall-level faults against a remote dependency are rejected at spec load with a hard error — [runtime.go:4122](../../internal/star/runtime.go#L4122). This is enforced, not documented. |
| **`telepresence intercept` is not used.** No inbound traffic to real Services is redirected. | Explicit non-goal in RFC-036. Faultbox only makes outbound calls. |

### The honest part of the blast radius

Two things reviewers should be told plainly rather than discovering later:

- **Faultbox's SUT is a real client.** If your service writes to a shared
  dev Postgres, a Faultbox run writes to that shared dev Postgres —
  exactly as running your service by hand would. Fault injection does not
  make those writes safer or more contained. If a dependency is shared and
  stateful, the usual dev-namespace hygiene applies.
- **A fault can mean the dependency sees *less* traffic, never malformed
  traffic.** When a rule short-circuits a request with a 503, the proxy
  answers the SUT and never forwards to the upstream. Faultbox does not
  send crafted or hostile payloads to dependency pods.

---

## 2. Privilege tiers — ask for the lowest one that works

This is the section to negotiate from. The privilege ask scales with the
*kind* of fault you want, and **most of the value sits in Tier 0, which
needs nothing unusual.** Do not open with a request for a privileged pod.

| Tier | What you can inject | Pod privileges needed | Expected friction |
|---|---|---|---|
| **T0** | Protocol faults on every dependency: HTTP/gRPC status codes, latency, SQL errors, connection resets, Kafka/Redis/Mongo faults. SUT runs as a plain binary. | **None.** Standard restricted pod. | Approve on sight |
| **T1** | T0 + syscall-level faults (`deny`, `delay`, `hold` on `write`/`connect`/`read`…) **on your own SUT only**. | `seccompProfile: Unconfined`, or a custom profile (§4) | Small, if you propose the custom profile |
| **T2** | T1 + SUT as a real container image, plus `seed=` / `reset=` / `reuse=` fixtures. | A Docker daemon — DinD sidecar (privileged) or a mounted node socket | **High.** Usually not worth it — see §5 |
| **T3** | Packet-level faults: bandwidth shaping, MTU, gray partitions, half-open blackholes (RFC-054). | `CAP_NET_ADMIN` + `/dev/net/tun` | High |
| **T4** | `watch()` filesystem observation via gVisor. | Node-level `daemon.json` edit + runsc install | **Don't ask.** See §7 |

**Start by writing your spec against T0 and see what it actually needs.**
If your tests are "what does my service do when `pricing` returns 503 /
hangs for 30s / drops the connection mid-response", that is T0 and the
conversation is over.

---

## 3. Tier 0 — the manifest to actually ask for

Nothing here is unusual. This is a normal pod that makes outbound HTTP calls.

```yaml
apiVersion: v1
kind: Pod
metadata:
  name: faultbox-run
  namespace: my-ns
spec:
  restartPolicy: Never
  # Default ServiceAccount is fine — Faultbox makes no API calls.
  # Prefer one with automountServiceAccountToken: false; nothing needs it.
  automountServiceAccountToken: false
  containers:
    - name: faultbox
      image: <your-registry>/faultbox-runner:<pinned-tag>
      command: ["faultbox", "test", "/spec/truck-api.star", "--seed", "1"]
      securityContext:
        runAsNonRoot: true
        allowPrivilegeEscalation: false
        readOnlyRootFilesystem: false   # bundles are written to disk
        capabilities:
          drop: ["ALL"]
      resources:
        requests: {cpu: "500m", memory: "512Mi"}
        limits:   {cpu: "2",    memory: "2Gi"}
      volumeMounts:
        - {name: spec, mountPath: /spec, readOnly: true}
        - {name: bundles, mountPath: /out}
      env:
        - name: FAULTBOX_BUNDLE_DIR
          value: /out
  volumes:
    - {name: spec, configMap: {name: faultbox-spec}}
    - {name: bundles, emptyDir: {}}
```

This satisfies a `restricted` Pod Security Standard. It drops all
capabilities, runs as non-root, and forbids privilege escalation.

### What Ops needs to provide for T0

1. **The runner image.** Faultbox binary + your SUT binary in one image,
   built by your existing CI. Pin the tag.
2. **Egress to the dependency Services**, if NetworkPolicies are
   default-deny. The set is: your dependency Services on their declared
   ports, plus `kube-dns`/CoreDNS on 53/UDP+TCP for Service resolution.
   Nothing else — Faultbox itself makes no outbound calls beyond what
   the spec declares.
3. **A writable path for `.fb` bundles** (the `emptyDir` above), and a
   way to retrieve them — `kubectl cp`, an artifact sidecar, or object
   storage. Read §6 before choosing.

Note that the SUT↔proxy leg is `127.0.0.1` inside the pod, so **no
NetworkPolicy is needed for the faulted hop itself.**

---

## 4. Tier 1 — the seccomp ask, and how to make it small

Faultbox's syscall interception calls `seccomp(2)` with
`SECCOMP_FILTER_FLAG_NEW_LISTENER`
([filter_linux.go:80](../../internal/seccomp/filter_linux.go#L80)). The
filtered child sets `PR_SET_NO_NEW_PRIVS` first, which is what makes the
call legal without `CAP_SYS_ADMIN`
([shim_linux.go:74](../../internal/seccomp/shim_linux.go#L74)).

Two facts to put in front of Security:

- **The filter applies only to processes Faultbox forks.** A seccomp
  filter is inherited downward, never sideways. It cannot be attached to
  a process Faultbox did not create, and it cannot escape the pod.
- **`no_new_privs` is a privilege *reduction*.** The filtered SUT can no
  longer gain privileges through setuid binaries. Tier 1 makes the SUT
  strictly less privileged than it is in a normal deployment.

The obstacle is not capabilities — it is that the container runtime's
**default seccomp profile may block the `seccomp` syscall itself.** The
blunt fix is `seccompProfile: {type: Unconfined}` on the pod, which is
the k8s analog of the `--security-opt seccomp=unconfined` that Faultbox
already passes to Docker ([docker.go:163](../../internal/container/docker.go#L163)).

**Do not lead with `Unconfined` — lead with a custom profile.** It is a
much easier approval and it is the technically correct ask:

```json
{
  "defaultAction": "SCMP_ACT_ERRNO",
  "baseProfilePath": "runtime/default",
  "syscalls": [
    { "names": ["seccomp"], "action": "SCMP_ACT_ALLOW" }
  ]
}
```

Ops places this at `<kubelet-seccomp-root>/faultbox.json` on the nodes
(default `/var/lib/kubelet/seccomp/`), and the pod references it:

```yaml
securityContext:
  seccompProfile:
    type: Localhost
    localhostProfile: faultbox.json
```

That is "RuntimeDefault, plus one syscall" — a reviewable diff rather
than a blanket exemption. If distributing a node-level file is harder in
your shop than approving `Unconfined`, take `Unconfined`; just know you
had the better option.

> **Verify, don't assume.** Whether the runtime default actually blocks
> `seccomp` depends on your runtime and its version. Run the preflight in
> §8 before filing either request — you may find T1 needs nothing at all.

---

## 5. Tier 2 — container-mode SUT, and why to avoid it in-cluster

Running the SUT as a real container image gets you `seed=` / `reset=` /
`reuse=` fixtures and image-digest reproducibility (RFC-030). In-cluster
it costs you a Docker daemon, which means either:

- **A privileged DinD sidecar** — `privileged: true`, which most security
  teams treat as equivalent to node root. Expect a hard no, and they are
  not wrong to say it.
- **Mounting the node's container socket** — strictly worse. Container
  socket access is node-level control.

**The recommendation is to not ask.** Instead, split by environment:

| Environment | SUT mode | Dependencies |
|---|---|---|
| Laptop (Lima/Linux) | container — full fixture and syscall surface | `remote=` over Telepresence |
| In-cluster CI | **binary** (T0/T1) | `remote=` via native cluster DNS |

You keep the expensive fault surface where privileges are cheap (your own
machine) and run the protocol-level matrix where privileges are expensive
(the cluster). The same spec covers both if the SUT can be built as a
binary; `service(binary=...)` and `service(image=...)` differ by one kwarg.

---

## 6. Tier 3 — packet faults

Packet-level faults (RFC-054) need a TUN device: `CAP_NET_ADMIN` plus
access to `/dev/net/tun`
([gateway_linux.go:40](../../internal/netfault/gateway_linux.go#L40)).

```yaml
securityContext:
  capabilities:
    add: ["NET_ADMIN"]
```

`NET_ADMIN` is a genuine escalation — it permits arbitrary network
configuration within the pod's network namespace, including manipulating
iptables and routes for anything sharing that namespace. Ask for it only
if you specifically need timeout tuning, backpressure, or gray-partition
tests, and only with `hostNetwork: false` so the namespace is the pod's
own.

---

## 7. Tier 4 — `watch()` / gVisor: don't ask

Filesystem observation needs `runsc` registered as a Docker runtime in the
node's `daemon.json` with a `--pod-init-config` flag, which is host-wide
node state — see the
[pod-init-config spike](../design/2026-07-29-pod-init-config-spike.md).
In a managed or shared cluster this is not a reasonable request. Run
`watch()` tests locally.

---

## 8. Data handling — read this before uploading bundles

**This is the item most likely to be raised late and block you, so raise
it first.**

Every run writes a `.fb` bundle containing the event trace. That trace
records the protocol interactions the proxy observed. Concretely, today:

- HTTP/gRPC: **method, path, status code** — see
  [http.go:164](../../internal/proxy/http.go#L164)
- Postgres/MySQL: **full SQL query text** —
  [postgres.go:211](../../internal/proxy/postgres.go#L211)
- Cassandra/ClickHouse: query text, truncated to 120 characters

**Response bodies are not captured.** That is a meaningful limit on
exposure and worth stating explicitly.

But two things are equally true and must be stated:

1. **SQL query text can carry literal values** — a `WHERE email = '...'`
   predicate puts real data in the trace. URL paths carry identifiers and
   occasionally tokens.
2. **There is no redaction. None.** No allowlist, no denylist, no
   `redact` code path anywhere in the tree. RFC-037 §"cross-cutting
   questions" lists the redaction policy as an *open design problem*, not
   a shipped feature.

Practical consequences for the review:

- Treat a `.fb` bundle from a namespace with real-shaped data at the same
  classification as that data.
- Decide deliberately whether bundles leave the namespace. Uploading them
  to a CI artifact store is an export.
- If that is unacceptable, keep bundles in-namespace and inspect them with
  `faultbox inspect` from a pod, or run the suite against seeded synthetic
  data only.

---

## 9. Preflight — verify before you file the request

Ops can answer most of the review with one throwaway pod. Run this in the
target namespace **before** asking for any privilege:

```sh
kubectl run faultbox-preflight -n my-ns --rm -it --restart=Never \
  --image=<your-registry>/faultbox-runner:<tag> -- sh -c '
    echo "kernel:   $(uname -r)          # need >= 5.6"
    echo "seccomp:  $(grep -c CONFIG_SECCOMP_FILTER=y /boot/config-$(uname -r) 2>/dev/null || echo "check node")"
    echo "tun:      $(test -e /dev/net/tun && echo present || echo absent)"
    echo "docker:   $(docker info >/dev/null 2>&1 && echo reachable || echo absent)"
    getent hosts geo-config.my-ns.svc.cluster.local || echo "DNS: dependency not resolvable"
  '
```

Then the real test — a minimal spec that installs one syscall fault on a
binary service. If it passes under the default seccomp profile, **Tier 1
needs no security exception at all** and §4 becomes moot. If it fails
with `seccomp(SET_MODE_FILTER): operation not permitted`, you have the
exact error string to attach to the request.

Node kernel must be **5.6+** for seccomp-notify. 5.19+ additionally
enables `WAIT_KILLABLE_RECV`, which Faultbox uses when present and falls
back from silently when absent
([filter_linux.go:84](../../internal/seccomp/filter_linux.go#L84)) — a
nice-to-have, not a requirement.

---

## 10. The one-page ask

If you want a single paragraph to paste into a ticket:

> We want to run fault-injection tests for `<service>` in our own dev
> namespace. The tool (Faultbox) runs as a single non-privileged pod that
> starts our service as a child process and puts a loopback proxy in front
> of its declared dependencies, injecting HTTP/gRPC/SQL-level failures into
> that proxy. It needs **no Kubernetes API access, no ServiceAccount
> permissions, and makes no changes to the cluster** — it does not deploy,
> patch, or intercept anything, and it cannot affect other teams' pods. The
> only requirements are: a runner image in our registry, egress from the pod
> to our dependency Services plus CoreDNS, and a writable volume for test
> artifacts. Artifacts contain request paths and SQL query text (not
> response bodies) and are not redacted, so we will keep them
> `<in-namespace | in our artifact store, classified as dev data>`.
> If we later need syscall-level fault injection against our *own* process,
> we will request a `Localhost` seccomp profile that is RuntimeDefault plus
> the `seccomp` syscall — we will confirm with a preflight whether that is
> even necessary.

---

## See also

- [connectivity.md](connectivity.md) — Telepresence and port-forward workflows
- [ci-on-linux.md](ci-on-linux.md) — non-k8s CI host requirements
- [RFC-036](../rfcs/0036-remote-services.md) — the `remote=` primitive
- [RFC-037](../rfcs/0037-remote-determinism.md) — determinism and the open redaction question
