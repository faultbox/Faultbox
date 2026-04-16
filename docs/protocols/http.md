# HTTP Protocol Reference

Interface declaration:

```python
api = service("api",
    interface("public", "http", 8080),
    ...
)
```

## Methods

### `get(path="", headers={})`

Send an HTTP GET request.

```python
resp = api.public.get(path="/users/1")
resp = api.get(path="/users/1")  # shorthand when service has one HTTP interface

resp = api.get(path="/admin", headers={"Authorization": "Bearer token123"})
```

### `post(path="", body="", headers={})`

Send an HTTP POST request.

```python
resp = api.post(path="/users", body='{"name":"alice"}')
resp = api.post(path="/upload", body="raw data", headers={"Content-Type": "text/plain"})
```

### `put(path="", body="", headers={})`

Send an HTTP PUT request.

```python
resp = api.put(path="/users/1", body='{"name":"bob"}')
```

### `delete(path="", headers={})`

Send an HTTP DELETE request.

```python
resp = api.delete(path="/users/1")
```

### `patch(path="", body="", headers={})`

Send an HTTP PATCH request.

```python
resp = api.patch(path="/users/1", body='{"name":"charlie"}')
```

## Response Object

All methods return a response with:

| Field | Type | Description |
|-------|------|-------------|
| `.status` | int | HTTP status code (200, 404, 500, etc.) |
| `.body` | string | Raw response body |
| `.data` | dict/list | Auto-decoded JSON (if body is valid JSON) |
| `.ok` | bool | `True` if status is 2xx |
| `.duration_ms` | int | Request duration in milliseconds |
| `.headers` | dict | Response headers |

```python
resp = api.get(path="/users")
assert_eq(resp.status, 200)
assert_true(resp.ok)
print(resp.data)          # [{"id": 1, "name": "alice"}, ...]
print(resp.data[0]["name"])  # "alice"
print(resp.duration_ms)   # 12
```

## Fault Rules

Protocol-level faults use `fault_assumption()` with the interface as target:

### `response(method=, path=, status=, body=)`

Return a custom HTTP response without forwarding to the service.

```python
rate_limited = fault_assumption("rate_limited",
    target = api.public,
    rules = [response(method="POST", path="/orders*", status=429,
                      body='{"error":"too many requests"}')],
)
```

| Parameter | Type | Description |
|-----------|------|-------------|
| `method` | string | HTTP method glob (`"POST"`, `"GET"`, `"*"`) |
| `path` | string | Path glob (`"/orders*"`, `"/api/v1/*"`) |
| `status` | int | HTTP status code to return |
| `body` | string | Response body to return |

### `error(method=, path=, status=, message=)`

Same as `response()` — returns an error response.

```python
server_error = fault_assumption("server_error",
    target = api.public,
    rules = [error(path="/health", status=500, message="internal error")],
)
```

### `delay(method=, path=, delay=)`

Delay matching requests, then forward normally.

```python
slow_search = fault_assumption("slow_search",
    target = api.public,
    rules = [delay(path="/search*", delay="2s")],
)
```

### `drop(method=, path=)`

Close the connection without responding.

```python
drop_uploads = fault_assumption("drop_uploads",
    target = api.public,
    rules = [drop(method="POST", path="/upload*")],
)
```

## Seed / Reset Patterns

HTTP services are typically stateless — seed and reset are usually not
needed. For services that cache state in memory:

```python
api = service("api",
    interface("public", "http", 8080),
    build = "./api",
    reuse = True,
    reset = lambda: api.post(path="/admin/reset"),  # app-specific reset endpoint
)
```

## Event Sources

HTTP services don't have a native event source. Use `stdout` to capture
application logs:

```python
api = service("api",
    interface("public", "http", 8080),
    build = "./api",
    observe = [stdout(decoder=json_decoder())],
)
```
