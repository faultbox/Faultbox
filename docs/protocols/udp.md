# UDP Protocol Reference

Interface declaration:

```python
dns = service("dns",
    interface("main", "udp", 53),
    image = "coredns/coredns:1.11",
)

statsd = service("statsd",
    interface("main", "udp", 8125),
    image = "statsd/statsd:latest",
)
```

UDP is connectionless and datagram-based. Faultbox speaks the transport
directly — there is no higher-level wire format to parse, so fault rules
apply uniformly to all datagrams on the interface.

## Methods

### `send(data="", hex="", timeout_ms=5000)`

Send one datagram and wait for a response. Response is returned as hex
(for binary protocols like DNS).

```python
# StatsD text metric
resp = statsd.main.send(data="api.requests:1|c")

# Binary DNS query
resp = dns.main.send(hex="...", timeout_ms=2000)
# resp.data = {"raw": "...", "size": 64}
```

| Parameter | Type | Default | Description |
|-----------|------|---------|-------------|
| `data` | string | — | UTF-8 string payload (exclusive with `hex=`) |
| `hex` | string | — | Hex-encoded binary payload (exclusive with `data=`) |
| `timeout_ms` | int | 5000 | Read timeout for the response |

### `send_no_reply(data="", hex="")`

Fire-and-forget. Does not wait for a response — returns immediately after
the OS accepts the datagram for send.

```python
statsd.main.send_no_reply(data="api.requests:1|c")
```

## Response Object

### For `send()`:

| Field | Type | Description |
|-------|------|-------------|
| `.data.raw` | string | Response payload, hex-encoded |
| `.data.size` | int | Response size in bytes |
| `.ok` | bool | `True` if a response was received |
| `.duration_ms` | int | Roundtrip time |

### For `send_no_reply()`:

| Field | Type | Description |
|-------|------|-------------|
| `.data.size` | int | Bytes sent |
| `.ok` | bool | `True` if the send succeeded locally (does NOT confirm delivery) |

## Fault Rules

### `drop(probability=)`

Silently discard a fraction of datagrams.

```python
lossy = fault_assumption("lossy",
    target = dns.main,
    rules = [drop(probability="30%")],
)

total_loss = fault_assumption("dns_down",
    target = dns.main,
    rules = [drop()],  # 100% loss
)
```

### `delay(delay=, probability=)`

Delay datagram forwarding.

```python
slow = fault_assumption("slow_metrics",
    target = statsd.main,
    rules = [delay(delay="2s")],
)
```

## Future: corrupt and reorder

RFC-016 proposes `corrupt()` (bit-flip) and `reorder()` (buffer+swap)
fault actions unique to UDP. These are NOT yet implemented — they need
new `Action` variants in the proxy engine and corresponding builtins.
Tracked as open questions on [RFC-016](../rfcs/0016-new-protocols.md).

## Recipes

See [recipes/udp.star](../../recipes/udp.star):

- `packet_loss` — probabilistic drops
- `dns_flap` — 50% drop for flappy DNS tests
- `metrics_slow` — delay for metrics pipelines
- `jitter` — fixed delay for congestion simulation
- `blackhole` — total loss

```python
load("./recipes/udp.star", "dns_flap")

broken_dns = fault_assumption("broken_dns",
    target = dns.main,
    rules  = [dns_flap()],
)
```

## Implementation notes

- Proxy listens on a random local UDP port and forwards datagrams to the
  target. Response routing uses per-client upstream sockets so replies
  reach the original sender.
- UDP has no connection state. "Drop" and "delay" apply per-datagram.
- Healthcheck is best-effort: dial the target and return on success.
  UDP has no handshake, so "port open" detection is OS-dependent.
