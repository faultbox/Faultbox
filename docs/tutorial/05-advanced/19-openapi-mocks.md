# Chapter 19: OpenAPI Mocks — Auto-Generate HTTP Stubs From Your Contract

**Duration:** 15 minutes
**Prerequisites:** [Chapter 17 (Mock Services)](17-mock-services.md), an OpenAPI 3.0 document (YAML or JSON)

## Goals & purpose

Your SUT talks to five HTTP upstreams. Each has an OpenAPI spec. You
want to mock all five in tests, and you don't want to hand-copy every
path/method/response into a `routes={}` dict that will drift from the
real spec the moment someone ships a new endpoint.

**OpenAPI mocks** (shipped in v0.9.3) point Faultbox at the spec file.
Routes auto-generate from the `paths:` tree. Responses come from the
`example:` declared in each operation. Overrides let you swap specific
responses when you need custom behaviour. Request validation fails the
SUT fast when it sends a malformed body.

This chapter teaches you to:

- **Load an OpenAPI document** into a mock via `http.server(openapi=…)`.
- **Pick response examples** by strategy — `first`, named, random,
  or schema synthesis.
- **Override specific operations** with custom responses or dynamic
  handlers.
- **Enforce request schemas** with `validate="strict"` so the SUT can't
  send a malformed body and have it silently accepted.

## 1 · The minimum working mock

Start with a Petstore-ish spec:

```yaml
# specs/petstore.yaml
openapi: 3.0.3
info: {title: Petstore, version: "1.0"}
paths:
  /pets:
    get:
      responses:
        "200":
          description: list pets
          content:
            application/json:
              example:
                - {id: 1, name: "fluffy"}
                - {id: 2, name: "scruffy"}
  /pets/{id}:
    get:
      parameters:
        - {name: id, in: path, required: true, schema: {type: integer}}
      responses:
        "200":
          description: single pet
          content:
            application/json:
              example: {id: 1, name: "fluffy"}
```

Point Faultbox at it:

```python
load("@faultbox/mocks/http.star", "http")

petstore = http.server(
    name      = "petstore",
    interface = interface("main", "http", 8090),
    openapi   = "./specs/petstore.yaml",
)

def test_list_pets():
    result = step(petstore.main, "get", path = "/pets")
    assert_true(result.status_code == 200)
    assert_true(result.body.startswith("[{"))

def test_get_pet():
    result = step(petstore.main, "get", path = "/pets/42")
    assert_true(result.status_code == 200)
    assert_true("fluffy" in result.body)
```

Two lines of Starlark, every `example:` in the spec becomes a live
response. No route table to maintain.

## 2 · Choosing which example to serve

Real specs rarely have one example per operation. Use `examples=` to
pick a strategy:

```python
# Deterministic default — first inline example, else first entry in
# examples: map (sorted alphabetically). Same response every time.
petstore = http.server(
    ...,
    examples = "first",
)

# Select a named variant across all ops. Falls back to "first" for ops
# that don't declare that variant — useful when your spec has mixed
# coverage.
error_ride = http.server(
    ...,
    examples = "error",
)

# Seeded random per op — reproducible between runs, different examples
# per op. Good for exercising client code paths that branch on response
# shape.
fuzz_ride = http.server(
    ...,
    examples = "random",
)

# Synthesise minimal type-correct values for ops that declare only a
# schema (no example). Keeps the spec usable even when spec authors
# haven't filled in examples yet.
partial = http.server(
    ...,
    examples = "synthesize",
)
```

## 3 · Overriding specific operations

Sometimes you want the generated routes for everything *except* two
critical ops. `overrides={}` replaces generated entries by pattern:

```python
petstore = http.server(
    name      = "petstore",
    interface = interface("main", "http", 8090),
    openapi   = "./specs/petstore.yaml",
    overrides = {
        # OpenAPI-style `{id}` — normalised to a glob internally, so
        # you can paste paths directly from your spec.
        "GET /pets/{id}":     json_response(status = 404, body = {"error": "not found"}),

        # Dynamic responses still work.
        "POST /pets":         dynamic(lambda req: json_response(
                                  status = 201,
                                  body   = {"echoed": req["body"]},
                              )),
    },
)
```

Overrides take priority over generated routes. Anything you don't
override is served from the spec.

## 4 · Validating requests

If your SUT sends a malformed POST body, the real upstream would
return a 400. The mock should too — otherwise bugs hide until
production. Add `validate="strict"`:

```python
auth = http.server(
    name      = "auth",
    interface = interface("main", "http", 8090),
    openapi   = "./specs/auth.openapi.yaml",
    validate  = "strict",  # reject malformed bodies with HTTP 400
)

def test_login_requires_password():
    # Missing required "password" field — mock returns 400.
    result = step(auth.main, "post",
                  path = "/login",
                  body = '{"email": "x@y.z"}')
    assert_true(result.status_code == 400)
```

Alternatives:

- `validate="warn"` — log the mismatch as an event (`mock.POST /login`
  with a `validate_error` field) but serve the generated response
  anyway. Good for passively detecting contract drift in CI.
- `validate="off"` (default) — no validation.

Only JSON request bodies are validated in v0.9.3. Non-JSON content
types pass through unchecked.

## 5 · Combining with faults

OpenAPI mocks are just `mock_service` instances — every other
Faultbox primitive works against them. Inject faults against the
mock's upstream to simulate a flapping producer:

```python
def test_auth_upstream_flake():
    # First call: the mock responds normally (generated from spec).
    # Second call: the mock's own HTTP write syscall is denied,
    # simulating a socket failure mid-response.
    fault(auth,
          write = deny("ECONNRESET", after = 1),
          run   = lambda: my_app_handles_auth_flake(),
    )
```

## Takeaways

- `http.server(openapi=…)` auto-generates routes from an OpenAPI 3.0 document.
- Example-selection strategies — `first`, `<name>`, `random`,
  `synthesize` — let you pick what responses look like without
  rewriting the spec.
- `overrides={}` replaces generated routes by pattern. OpenAPI-style
  paths (`{id}`) work verbatim.
- `validate="strict"` makes the mock behave like a real
  OpenAPI-enforcing gateway.
- Malformed spec files fail at `faultbox test` load, not mid-run.

Next: [Chapter 20 — Trace-level assertions →](../06-verification/20-trace-assertions.md)
