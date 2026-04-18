# HTTP/2 Protocol Reference

Interface declaration:

```python
api = service("api",
    interface("public", "http2", 8080),
    image = "myapp:latest",
)
```

Faultbox speaks HTTP/2 over cleartext (h2c) — the dominant service-mesh
deployment. TLS-terminated HTTP/2 is transparent to most clients because
ALPN negotiates it automatically; specs that need TLS can run the real
service behind a sidecar proxy.

## When to use `http2` vs `http`

Use `"http2"` when the service under test **requires** HTTP/2 — streaming,
server push, or specific stream-level behavior you want to assert on. Use
`"http"` (HTTP/1.1) for everything else. Most APIs work with either; the
wire protocol choice is rarely load-bearing for fault injection.

## Methods

HTTP/2 exposes the same method names as HTTP/1.1. The wire protocol
changes; the spec-level API does not.

| Method | Description |
|--------|-------------|
| `get(path="", headers={})` | HTTP/2 GET |
| `post(path="", body="", headers={})` | HTTP/2 POST |
| `put(path="", body="", headers={})` | HTTP/2 PUT |
| `delete(path="", headers={})` | HTTP/2 DELETE |
| `patch(path="", body="", headers={})` | HTTP/2 PATCH |

```python
resp = api.public.get(path="/users/1")
assert_eq(resp.status, 200)
assert_eq(resp.fields["proto"], "HTTP/2.0")
```

## Response Object

Identical to HTTP/1.1, with one additional field:

| Field | Type | Description |
|-------|------|-------------|
| `.status` | int | HTTP status code |
| `.body` | string | Response body (truncated at 64KB) |
| `.ok` | bool | `True` on any HTTP response |
| `.duration_ms` | int | Request time |
| `.fields["proto"]` | string | Negotiated protocol — expect `"HTTP/2.0"` |

## Fault Rules

All HTTP fault rules work identically on HTTP/2:

### `response(path=, status=, body=)`

Return a custom response for matching requests.

```python
rate_limit = fault_assumption("rate_limit",
    target = api.public,
    rules = [response(path="/api/**", status=429, body='{"error":"rate limited"}')],
)
```

### `error(path=, status=, message=)`

Return a protocol-level error.

```python
maintenance = fault_assumption("maintenance",
    target = api.public,
    rules = [error(path="/api/*", status=503, message="upgrade in progress")],
)
```

### `delay(path=, delay=)`

Delay matching requests.

```python
slow_api = fault_assumption("slow_api",
    target = api.public,
    rules = [delay(path="/slow/*", delay="2s")],
)
```

### `drop(path=)`

Close the stream without a response (HTTP/2 stream reset semantics).

```python
stream_reset = fault_assumption("stream_reset",
    target = api.public,
    rules = [drop(path="/broken")],
)
```

## Stream-level fault caveats

Faultbox's HTTP/2 proxy runs as a standard `httputil.ReverseProxy` on top
of `golang.org/x/net/http2`. This gives real HTTP/2 framing, HPACK, and
stream multiplexing out of the box, but the following HTTP/2-specific
faults are **not yet supported**:

- `goaway()` — sending GOAWAY frames
- `window_exhaustion()` — stalling flow control windows
- Connection-level faults (vs stream-level)

These are tracked in [RFC-016](../rfcs/0016-new-protocols.md) as open
questions and will need a lower-level proxy (frame-aware, not
request-aware) to implement correctly.

## Recipes

See [recipes/http2.star](../../recipes/http2.star) for curated wrappers:

- `rate_limited` — 429 with Retry-After
- `server_error` — 500 internal error
- `service_unavailable` — 503 retryable
- `gateway_timeout` — 504
- `slow_endpoint` — latency injection
- `maintenance_window` — 503 with Retry-After
- `stream_reset` — RST_STREAM via drop
- `flaky` — probabilistic 500s
- `unauthorized` / `forbidden` — 401 / 403

```python
load("@faultbox/recipes/http2.star", "http2")

faulty = fault_assumption("faulty_api",
    target = api.public,
    rules  = [http2.rate_limited(path="/api/**"), http2.flaky(probability="10%")],
)
```

## Implementation notes

- Inbound listener uses `h2c.NewHandler` so clients connecting with
  HTTP/1.1 or prior-knowledge HTTP/2 both work.
- Upstream transport uses `http2.Transport` with `AllowHTTP=true` —
  cleartext connections only. TLS upstream is future work.
- gRPC rides on HTTP/2 but has its own protocol plugin (`"grpc"`) because
  gRPC's semantics (methods named by path, trailers, status codes in
  headers) deserve first-class support.
