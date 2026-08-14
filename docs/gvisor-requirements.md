# gVisor: what each feature needs

Two Faultbox features run on gVisor rather than seccomp-notify: **packet
faults** (RFC-054) and **filesystem observation** (RFC-056). They have
different requirements, and neither is the default — a spec opts in.

This page is the checklist. If a run fails with "needs CAP_NET_ADMIN",
"no netstack gateway was attached", or "trace host not registered", the
answer is here.

## At a glance

| | Packet faults | `watch()` |
|---|---|---|
| Opt in with | `determinism(runtime = "gvisor")` | `watch(...)` in the test body |
| Platform | Linux only | Linux only |
| Needs `runsc` installed | No | **Yes** |
| Needs `CAP_NET_ADMIN` (sudo) | **Yes** | No |
| Needs `/dev/net/tun` | **Yes** | No |
| Needs one-time host setup | No | **Yes** — `faultbox setup-trace` |
| Needs a Docker restart to enable | No | **Yes**, once |

The two are independent. A spec can use packet faults without `runsc`
installed at all, because the packet gateway links gVisor's netstack as a
Go library rather than running a sandbox.

## Packet faults

`packet_drop`, `packet_delay`, `packet_reorder`, `packet_duplicate`,
`packet_corrupt`, `packet_reset`, `packet_window`, `packet_pass`,
`bandwidth()`, `mtu()`, and `partition()`.

### Requirements

1. **Linux.** The gateway creates a TUN device; there is no macOS or
   Windows equivalent. On macOS use the Lima VM (`make env-start`).
2. **`/dev/net/tun` present and writable.**
3. **`CAP_NET_ADMIN`.** Creating and configuring a TUN device is a
   privileged operation.

### This means running as root

```sh
sudo faultbox test faultbox.star
```

or, in a container, `docker run --cap-add=NET_ADMIN`.

There is no way around it: the capability is what the kernel requires to
create a TUN device, and the gateway needs a TUN device to see packets at
all. A spec that declares packet faults and runs unprivileged fails with
the reason rather than silently injecting nothing.

If `sudo` is awkward — a shared CI runner, a developer machine with a
locked-down sudoers — grant the capability to the binary once instead:

```sh
sudo setcap cap_net_admin+ep $(which faultbox)
```

Note this grants the capability to *every* invocation of that binary, so
it is a decision about the machine, not about one run.

### Verifying

```sh
ls -l /dev/net/tun          # exists
sudo faultbox test spec.star
```

A working run logs the gateway attaching. A run that could not attach
fails the test and names the cause, rather than reporting a green result
no packet ever reached — see
[troubleshooting](troubleshooting.md).

### What packet faults cannot reach

The gateway sees traffic that crosses the Faultbox container network.
Two consequences worth knowing before you design a topology around it:

- A SUT reaching a dependency over a **pinned host port** or
  `host.docker.internal` bypasses the gateway; those packets never
  traverse the mediated link. Address dependencies by their
  container-network name.
- A protocol that **advertises its own address** takes the client off the
  mediated path after the handshake — Kafka's `Metadata` response, Redis
  Cluster's `CLUSTER SLOTS` / MOVED, MongoDB's `hello.hosts`. The
  bootstrap connection is mediated; everything after it is not.

## Filesystem observation — `watch()`

`watch()` reports a service's file I/O with resolved paths. It runs the
service under `runsc` and reads gVisor's trace points.

### Requirements

1. **Linux.**
2. **`runsc` installed** and registered with Docker as a runtime.
3. **One-time host registration**, because the trace session is installed
   at sandbox boot via `--pod-init-config`, which lives in
   `/etc/docker/daemon.json` rather than per container.

`CAP_NET_ADMIN` is *not* required — `watch()` does not create a TUN
device. It does need permission to write `/etc/docker/daemon.json`.

### Setup

```sh
sudo faultbox setup-trace          # writes daemon.json, reports every change
faultbox setup-trace --check       # confirm it took
```

`setup-trace` is idempotent, reports what it left alone as well as what
it changed, and **prints the Docker restart rather than performing it** —
that restart stops every container on the host, so it belongs to whoever
owns the machine.

`read`, `close` and `connect` are off by default. Enable them with
`--with-read` and friends, and know the cost: on a read-heavy workload,
enabling reads took a run from 25,015 trace points and zero drops to
48,576 and 1,488 — and dropped points **fail** the test, because a
negative assertion is only as good as the trace behind it.

### Why the setup is a separate command

`runsc trace create` instruments only tasks created *after* it attaches.
Attaching after a service is healthy produced **2** trace points for a
network-driven query against a running Postgres, where the same SQL from
a freshly spawned process produced 1054 — a `watch()` that observes
almost nothing while every assertion under it still passes. Installing
the session at sandbox boot is what makes the observation trustworthy,
and that is a host-level setting.

## Common failures

| Message | Cause |
|---|---|
| `packet faults need /dev/net/tun, which is missing` | Not Linux, or the tun module is not loaded (`modprobe tun`) |
| `packet faults need read/write access to /dev/net/tun` | Running unprivileged — use `sudo` or `--cap-add=NET_ADMIN` |
| `create TUN fbox<pid> (needs CAP_NET_ADMIN)` | Same |
| `stale TUN ... could not be removed` | A previous run was killed; the sweep needs the same capability |
| `no netstack gateway was attached` | The gateway could not attach, or the traffic never crossed the mediated link — the failure names which |
| `runsc not found` | `watch()` without runsc installed |
| trace host not registered | `watch()` before `sudo faultbox setup-trace` |

## See also

- [Packet-Level Faults](tutorial/03-protocol-level/27-packet-faults.md) —
  the tutorial chapter.
- [Watching the filesystem](tutorial/05-advanced/29-filesystem-observation.md).
- [determinism.md](determinism.md) — `runtime = "gvisor"` widens the
  mediated surface; it does **not** raise the determinism level. Both
  runtimes cap at L1.
- [troubleshooting.md](troubleshooting.md).
