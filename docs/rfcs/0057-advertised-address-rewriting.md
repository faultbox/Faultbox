# RFC-057: Advertised-Address Rewriting — keeping cluster-aware clients on the mediated path

- **Status:** Draft
- **Target:** v0.19.0 (candidate)
- **Created:** 2026-08-14
- **Origin:** Field report, first large-service evaluation — the last capability gap after v0.18.0
- **Depends on:** RFC-024 (proxy datapath), RFC-054 (packet gateway)
- **Relates to:** RFC-035 (container-consumer reachability), RFC-039 (TLS for deferred plugins)

## Summary

Some protocols answer "where am I?" during the handshake, and the client
believes them. Kafka's `Metadata` response carries
`advertised.listeners`; Redis Cluster's `CLUSTER SLOTS` and `MOVED` carry
node addresses; MongoDB's `hello` carries `hosts`. In each case the
client opens **new connections to the address the server named**, and
those connections do not go through Faultbox.

The bootstrap connection is mediated. Everything after it is not. So no
fault — proxy-level or packet-level — reaches the traffic that matters,
and the ones that do fire cover a connection the application barely uses.

This RFC proposes rewriting those advertised addresses in the protocol
proxy so the client's later connections come back to us.

## The problem, concretely

A spec faults Kafka:

```python
kafka = service("kafka", interface("main", "kafka", 9092), image = "apache/kafka:3.7.0")

def test_broker_outage():
    def scenario():
        api.post(path = "/orders", body = "…")
    fault(kafka.main, drop(topic = "orders"), run = scenario)
```

What happens:

1. The SUT's client connects to the proxy. Mediated.
2. It sends `Metadata`. The proxy forwards it; the broker replies with
   its own `advertised.listeners`.
3. The client reads that, and **dials the advertised address directly**
   for every produce and fetch.
4. The proxy sees the bootstrap exchange and nothing else. `drop(topic=)`
   never matches a produce, because no produce crosses it.

Confirmed in code: `internal/proxy/kafka.go` inspects only
`kafkaAPIProduce` and `kafkaAPIFetch`. `Metadata` (apiKey 3) is forwarded
byte-for-byte, so whatever the broker advertises reaches the client
untouched.

The same shape, different call:

| Protocol | Where the address is advertised |
|---|---|
| Kafka | `Metadata` response — `brokers[].host` / `.port` |
| Redis Cluster | `CLUSTER SLOTS`, `CLUSTER SHARDS`, `MOVED` / `ASK` redirects |
| MongoDB | `hello` / `isMaster` — `hosts[]`, `primary`, `me` |
| Cassandra | `system.peers`, and the topology-change event stream |

The packet gateway does not save us. It mediates traffic crossing the
Faultbox container network, and a client that has been handed a host-port
address leaves that network. This is what the field evaluation ran
into: the proxy workaround forced them onto host ports, and host ports
put them outside the gateway. Both layers blind at once.

## Why this matters more than it looks

These are exactly the dependencies people most want to fault. A broker,
a cluster cache, a replica set — the things whose partial failure produces
the interesting behaviour. A user can fault Postgres and Redis-standalone
today and reasonably conclude the tool works, then find it silently does
nothing against the clustered half of their infrastructure.

"Silently" is the operative word. Before v0.18.0 a fault that matched
nothing was a passing test; now `FAULT_NOT_FIRED` warns and packet faults
refuse to report an unmediated run. So the failure is visible — but it is
visible as "your fault did not fire", with no hint that the cause is
three layers down in a protocol handshake.

## Proposal

Rewrite advertised addresses in the proxy, so the address the client
receives is the proxy's listener rather than the server's own.

The proxy already parses these protocols to match fault rules. Rewriting
is an extension of that parsing, not new machinery.

### Sketch — Kafka

```
client → proxy: Metadata request
proxy  → broker: (forwarded unchanged)
broker → proxy: Metadata response { brokers: [{host: "kafka", port: 9092, node_id: 1}] }
proxy  → client: Metadata response { brokers: [{host: "127.0.0.1", port: <proxy>, node_id: 1}] }
```

The client then dials the proxy for every subsequent connection, and
`drop(topic=)` matches.

Multi-broker clusters need one proxy listener per broker, keyed by node
ID, so the client's partition-to-broker mapping stays coherent. That is
the main piece of new work: today a proxy is one listener per
`(service, interface)`.

### Alternatives considered

**A. Make the server advertise the proxy.** Set
`KAFKA_ADVERTISED_LISTENERS` to the proxy's address. Faultbox already has
late-bound `iface.proxy_addr` (RFC-033) resolved at `buildEnv` time, so
this is expressible today:

```python
kafka = service("kafka",
    interface("main", "kafka", 9092),
    image = "apache/kafka:3.7.0",
    env = {"KAFKA_ADVERTISED_LISTENERS": "PLAINTEXT://" + kafka.main.proxy_addr},
)
```

Zero code, works now, and worth documenting immediately whatever we
decide here. Limits: it is per-protocol configuration the user has to
know about, it does not generalise (Redis Cluster and MongoDB advertise
from runtime state, not a config knob), and it is single-broker only.

**Recommendation: document A as the interim answer for Kafka** while the
rewriting lands.

**B. Rewrite at the packet gateway.** Deep-inspect and rewrite in the
netstack path. Works below the protocol, so one mechanism covers all
protocols — but it means parsing protocol payloads out of a TCP stream
without the framing context the proxy already has, and it inherits
`CAP_NET_ADMIN`. Worse trade.

**C. DNS / hosts interception.** Map the advertised hostname to the
proxy. Fails whenever the advertisement carries an IP, which Redis
Cluster and MongoDB routinely do.

**D. Do nothing; document the limit.** Honest, and cheap. But it leaves
the clustered case permanently unreachable, which is a large fraction of
the infrastructure people run in anger.

## Open questions

1. **Per-broker listeners.** How does a proxy grow from one listener to
   one per cluster member, and what owns their lifecycle? This is the
   bulk of the design.
2. **Which protocols first.** Kafka is the clearest demand and the
   best-understood parser. Redis Cluster and MongoDB are more invasive —
   `MOVED` is a per-command redirect, not a handshake.
3. **Correctness under rewrite.** A client that compares the advertised
   address against what it dialled, or that uses node identity for
   partition assignment, may behave differently. Needs a real-cluster
   test, not a unit test — the same lesson as the v0.16.1 protocol audit.
4. **Interaction with TLS (RFC-038).** Rewriting inside a TLS session
   requires terminating it, which the migrated plugins already do; the
   nine deferred ones (RFC-039) would not benefit until they migrate.
5. **Opt-in or default?** Rewriting changes what the SUT observes about
   its own infrastructure. Silent by default is convenient and slightly
   dishonest; explicit is safer and one more thing to know.

## What would make this shippable

- A real-cluster spec per protocol, in `poc/protocol-audit/` — a
  three-broker Kafka, not a single node. The single-broker case hides
  exactly the mapping bugs this design can introduce.
- An assertion that a fault **fires** against a cluster member, not
  merely that the spec loads.
- A documented statement of what remains unreachable, so the next
  evaluation does not have to discover it.

## Interim guidance (ship regardless)

Independent of this RFC, document today:

- Packet faults and proxy faults both mediate the *bootstrap* connection
  only, for protocols that advertise their own address.
- The `proxy_addr` workaround above for single-broker Kafka.
- That `FAULT_NOT_FIRED` against a clustered dependency most likely means
  this, not a bad matcher.
