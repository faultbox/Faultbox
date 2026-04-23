# Starlark in Faultbox — Dialect Reference

Faultbox specs are written in [Starlark](https://github.com/google/starlark-go),
a Python-subset designed for hermetic configuration. Starlark looks
like Python, but the dialect is intentionally narrower. This page
collects the gotchas every customer hits in their first week so you
can skip the rediscovery.

> **Why this page exists.** The inDrive PoC lost ~2 hours to dialect
> surprises (FB §2.1 #8). One reference document up front would have
> caught most of them.

## What works (Python-like)

- `def name(args, *, kw=default):` — function definitions, including
  keyword-only args after `*`.
- `lambda x: x + 1` — anonymous functions.
- `if`/`elif`/`else`, `for`/`else`, `while`. (Caveat: `for`/`else`
  semantics match Python; rarely used.)
- `dict`, `list`, `tuple`, `set` literals; comprehensions:
  `[x for x in xs if cond]`, `{k: v for k, v in items}`.
- `+`, `-`, `*`, `/`, `//`, `%`, `**` — standard arithmetic.
- `and`, `or`, `not`; truthiness identical to Python.
- `print(...)` — output goes to stderr.
- `fail("message")` — abort spec load with an error.
- `assert_*` family for runtime checks (see
  [spec-language.md](spec-language.md)).
- `load("path", "name1", "name2")` — module imports (Faultbox-resolved
  paths, see below).
- `struct(field1=…, field2=…)` — namespace objects, used by every
  `@faultbox/mocks/*.star` to expose multiple attrs from one
  constructor.

## What does NOT work (and what to use instead)

### File I/O — use the loaders

Standard Python `open()`, `os.walk`, etc. are all unavailable.
Use the v0.9.8 spec-load-time readers:

```python
seed_sql = load_file("./seed.sql")           # raw bytes → string
fixture  = load_yaml("./fixtures/users.yaml") # → dict / list / scalar
config   = load_json("./config/rates.json")
```

Paths resolve relative to the spec's directory, **not** cwd. See
[RFC-026](https://github.com/faultbox/Faultbox/issues/66) for the
security model.

### String formatting — use `%` or `+`

`str.format()` and f-strings (`f"…"`) don't exist. Two patterns:

```python
# C-style %, available on str:
url = "http://%s:%d/api" % (host, port)
url = "%(scheme)s://%(host)s/" % {"scheme": "https", "host": "x"}

# Plain concatenation:
url = "http://" + host + ":" + str(port) + "/api"
```

### No regex, no JSON parser, no datetime

If your spec needs a regex, parse JSON, or do date arithmetic, do it
**at fault-injection time** in your test scenario via a `dynamic(fn)`
mock callback (which gets the request body as a Starlark string), or
preprocess outside the spec and feed the result via `load_json`.

### Keyword-only arguments are common in builtins

Most Faultbox builtins reject positional argument soup. Use keywords:

```python
# Wrong — positional args won't bind to (name=, target=, ...).
fault_assumption("crash", db, deny("EIO"))

# Right.
fault_assumption("crash", target = db, write = deny("EIO"))
```

`UnpackArgs` in starlark-go enforces this; the error message names
the missing keyword.

### No mutable globals across modules

Each `load("./helpers.star", …)` exposes the listed names but
**doesn't share mutable state**. If `helpers.star` defines
`STATE = {}` and the caller mutates it, the change is visible inside
helpers.star — but only because Starlark dicts are reference types.
A cleaner pattern: have helpers expose a constructor function and
have the caller hold the state.

### No Python stdlib

`json`, `re`, `os`, `sys`, `math`, `random` — none of these are
importable. Faultbox surfaces what it needs as builtins (see the
table in [spec-language.md](spec-language.md)). `struct(...)` for
namespacing; that's the closest thing to a module system.

### Lambdas can't have statements

```python
# Wrong — Starlark lambdas are expressions only.
sign = lambda c: token = jwt_sign(kp, c); print(token); return token

# Right — wrap statements in a `def`.
def sign(claims):
    token = jwt_sign(keypair = kp, claims = claims)
    print(token)
    return token
```

The `default_expect=`/`overrides=` slots in `fault_matrix` accept
either lambdas (for one-line predicates) or `def`-functions (for
anything multi-line).

### `range()` returns a sequence, not an iterator

`range(1000000)` materialises a list of one million ints. Starlark
doesn't have generators or lazy iteration — keep ranges small or
use a `for` loop without `range()` if you can.

## Faultbox-specific affordances

### `@faultbox/...` stdlib loads

`load("@faultbox/recipes/mongodb.star", "mongodb")` resolves into the
embedded recipe library shipped with the consuming binary. You don't
need to vendor or download these. Different Faultbox versions ship
different recipe sets — see [recipes.md](recipes.md).

### `struct(**kwargs)` for namespaces

Stdlib mocks return structs so a single import exposes multiple
helpers without polluting the global namespace:

```python
load("@faultbox/mocks/jwt.star", "jwt")

auth = jwt.server(...)        # constructor on the struct
token = auth.sign(claims=...) # method-like attribute on the result
```

You can build your own with `struct(name=…, fn=…)` — useful for
shared helpers in your own `.star` files.

### Print debugging prints to stderr

`print("debug: %s" % x)` shows up on the terminal. The runtime
captures all spec-load-time output for the `.fb` bundle's
`services/<name>.stderr` so post-mortem inspection works.

## Common pitfalls and their fixes

| Symptom | Likely cause | Fix |
|---|---|---|
| `error: undefined: open` | Trying Python `open()` | Use `load_file(path)` |
| `error: undefined: f` | Trying f-string `f"{x}"` | Use `"%s" % x` or `"a" + x` |
| `error: positional arg may not follow named` | Argument order in builtin | Pass everything as kwargs |
| Spec loads, scenario references undefined name | `load()` didn't import the symbol | Add to `load(...)` second arg list |
| `attribute 'foo' of 'NoneType' is not callable` | Used `.field` on a None scenario result | Add `assert_true(result != None)` first |
| Mutated dict in `default_expect` between runs | Closure capture sharing state | Build dict inside `def`, not at module level |

## See also

- [`spec-language.md`](spec-language.md) — the full primitive
  reference (services, faults, assertions, mocks).
- [`bundles.md`](bundles.md) — what `.fb` bundles capture about
  spec loads (every `load_file` lands under `spec/` automatically).
- [Starlark spec](https://github.com/google/starlark-go/blob/master/doc/spec.md)
  — the upstream language reference from Google.
