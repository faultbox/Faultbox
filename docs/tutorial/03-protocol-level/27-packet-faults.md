# Packet-Level Faults

**Duration:** 25 min
**You'll learn:** why a dropped packet is not a closed connection, and how to
reproduce the failures that only exist below the protocol layer.

## The bug you cannot currently write

Here is a fault you have already used:

```python
fault(db.main, drop(query = "SELECT*"), run = scenario)
```

It closes the connection. Your service sees `ECONNRESET`, its retry logic runs,
the test passes, and you learn something real about your error handling.

Now consider the incident that actually pages people at 3am: the database stops
responding, but **nothing tells the client**. No reset, no refusal. The socket
sits in `ESTABLISHED`, the connection pool believes it is healthy, requests
queue behind it, and the service degrades for two minutes until a TCP keepalive
finally fires.

That failure is unreachable with `drop()`, because closing a connection sends a
RST — and a RST is *information*. Well-written clients handle it correctly on
the first try. To reproduce the outage you have to make packets **vanish**.

That is what packet faults are for.

## Turning them on

Packet faults run on a userspace TCP/IP stack that Faultbox puts on the data
path. Opt in at the top of the spec:

```python
determinism(runtime = "gvisor")
```

They need Linux with `CAP_NET_ADMIN`. On macOS, run inside the Lima VM:

```bash
make env-start
limactl shell faultbox-dev
sudo faultbox test faultbox.star
```

If a prerequisite is missing, the preflight check names it — you will not get a
mysterious connection timeout thirty seconds into a test.

## Your first blackhole

```python
determinism(runtime = "gvisor")

db = service("db", "/tmp/mock-db",
    interface("main", "tcp", 5432),
    env = {"PORT": "5432"},
    healthcheck = tcp("localhost:5432"),
)

api = service("api", "/tmp/mock-api",
    interface("public", "http", 8080),
    env = {"PORT": "8080", "DB_ADDR": db.main.addr},
    depends_on = [db],
    healthcheck = http("localhost:8080/health"),
)

def hit():
    api.post(path = "/data/k", body = "v")

def test_half_open_blackhole():
    fault(db.main,
        packet_drop(dir = "s2c", flags = "!SYN", label = "blackhole"),
        run = hit,
    )
```

`dir = "s2c"` is server-to-client — replies from the database. `flags = "!SYN"`
excludes the handshake, so the connection *establishes* and then goes silent.

Run it and watch the duration. A `drop()` test finishes in milliseconds. This
one takes as long as your client's timeout, because from its point of view the
database simply stopped existing.

## Proving the fault fired

A packet fault that matches nothing is worse than no fault at all: the test
passes, and you conclude your service is resilient. Always assert:

```python
def test_half_open_blackhole():
    fault(db.main,
        packet_drop(dir = "s2c", flags = "!SYN", label = "blackhole"),
        run = hit,
    )
    dropped = events(where = lambda e: e.type == "packet" and
        e.fields.get("action") == "drop")
    assert_true(len(dropped) > 0, "no packet was dropped; this test proves nothing")
```

Use `events()` — a scan of what already happened. `assert_eventually()` waits
for *future* events, so it cannot see inside a window that has closed.

## The matcher

Every `packet_*` builtin takes the same optional filters, ANDed:

```python
packet_drop(dir = "c2s")                      # direction
packet_drop(proto = "udp")                    # transport
packet_drop(flags = "PSH,ACK")                # TCP flags set
packet_drop(flags = "!RST")                   # TCP flags clear
packet_drop(port = 5432)                      # destination port
packet_drop(len_gt = 1400)                    # payload length
packet_drop(payload_prefix = "GET ")          # bytes
packet_drop(every = 3)                        # every third match, per flow
packet_drop(probability = "30%")              # partial loss
```

Rules are first-match-wins, so a narrow allow above a broad drop carves out an
exception:

```python
fault(db.main,
    packet_pass(payload_prefix = "PING"),   # keep health checks alive
    packet_drop(dir = "c2s"),               # black-hole everything else
    run = scenario,
)
```

## Four failures worth reproducing

**Gray partition** — the metastable-failure trigger. Not dead, just bad:

```python
fault(db.main, packet_drop(dir = "c2s", probability = "30%"), run = scenario)
```

**Asymmetric latency** — breaks every timeout tuned by measuring RTT once:

```python
fault(db.main,
    packet_delay("400ms", dir = "s2c"),
    packet_delay("5ms",   dir = "c2s"),
    run = scenario)
```

**Connection-pool poisoning** — does the pool notice a dead connection, or keep
handing it out?

```python
fault(db.main, packet_reset(after = 100), run = scenario)
```

**Backpressure** — the server advertises a full receive buffer. Does your
service apply backpressure, or buffer until it OOMs?

```python
fault(db.main, packet_window(size = 0, dir = "s2c"), run = scenario)
```

## Matching on payload

When the declarative filters are not enough — a custom binary protocol, say —
drop to a lambda:

```python
fault(db.main,
    packet_delay("2s", where = lambda p: p.payload.startswith("\x00\x00\x00")),
    run = scenario)
```

The `Packet` value exposes `proto`, `dir`, `src_ip`, `dst_ip`, `src_port`,
`dst_port`, `len`, `payload`, `flags`, `seq`, `ack`, `window`, `index`, `flow`.

Two things to know:

- `payload` is a **string**, not bytes, so `startswith` / `endswith` / `in` all
  work. Use `payload_bytes` if you want slicing.
- The declarative filters run first and the lambda only sees what survives them,
  so `where=` refines a cheap filter rather than replacing it. Keep the cheap
  part in kwargs.

A lambda that raises **fails the test**. It cannot quietly mean "no match" —
that would inject nothing while every assertion still passed.

## Composing with protocol faults

They act at different layers of the same path, so they stack:

```python
fault(db.main,
    packet_delay("50ms", dir = "c2s"),          # every segment is slow
    error(query = "INSERT*", message = "disk full"),  # and INSERTs fail
    run = scenario)
```

## What this layer will not do

- **It is below TLS.** Corrupting an encrypted stream gives you a MAC failure,
  not a semantic corruption.
- **`bandwidth()` and `mtu()` are not here yet** — they need a token bucket and
  real fragmentation handling. v0.14.1.
- **Netstack's timers are wall-clock**, so a test that hinges on a TCP
  retransmit deadline is timing-sensitive.

## Try it

The full scenario corpus is runnable:

```bash
sudo faultbox test poc/gvisor-rfc054/faultbox.star
```

Twelve scenarios, each asserting that its fault actually fired. Read them —
they are the fastest way to see what this layer makes possible.

---

**Next:** [Part 4 — Safety & Determinism](../04-safety/index.md)
