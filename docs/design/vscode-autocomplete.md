# Design Document: VS Code Autocomplete for Starlark Specs

## Problem

Writing `.star` files without autocomplete is slow — users must reference
docs for builtin names, parameter names, service attributes, and protocol
methods. IDE support makes adoption faster and reduces errors.

## Goal

Provide autocomplete for Faultbox Starlark specs in VS Code. Three phases
of increasing sophistication:

| Phase | Approach | Effort | Value |
|-------|----------|--------|-------|
| **1** | VS Code snippets + Python type stubs | Days | 80% of value |
| **2** | Starlark syntax highlighting extension | Week | Better DX |
| **3** | LSP server with context-aware completions | Months | Full IDE |

**This document covers Phase 1** — the fastest path to useful autocomplete.

## Phase 1: Snippets + Type Stubs

### How it works

1. **Python type stubs** (`.pyi` file) — VS Code's Python extension provides
   autocomplete for `.star` files when a `.pyi` stub declares the available
   functions and types. Since Starlark is Python-like, the Python extension's
   autocomplete works well enough.

2. **VS Code snippets** (`.code-snippets` file) — predefined code templates
   triggered by typing a prefix (e.g., `svc` → full service declaration).

### Setup for users

```bash
# One-time: copy stubs to project
faultbox init --vscode

# Creates:
#   .vscode/settings.json          — associates .star with Python
#   .vscode/faultbox.code-snippets — snippet templates
#   faultbox.pyi                   — type stubs for autocomplete
```

Or manual: add `faultbox.pyi` to the project root and configure VS Code
to treat `.star` files as Python.

### .vscode/settings.json

```json
{
    "files.associations": {
        "*.star": "python"
    },
    "python.analysis.extraPaths": ["."],
    "python.analysis.stubPath": "."
}
```

This tells VS Code:
- `.star` files are Python (for syntax highlighting and autocomplete)
- Look for type stubs in the project root

### faultbox.pyi — Type Stubs

```python
"""Faultbox Starlark type stubs for VS Code autocomplete."""

from typing import Any, Callable, Dict, List, Optional, Union

# ---------------------------------------------------------------------------
# Types
# ---------------------------------------------------------------------------

class service:
    """A service declaration."""
    name: str
    def __getattr__(self, name: str) -> 'interface_ref': ...

class interface:
    """An interface declaration."""
    def __init__(self, name: str, protocol: str, port: int, *, spec: str = ...) -> None: ...

class interface_ref:
    """Reference to a service interface. Access via service.interface_name."""
    addr: str
    host: str
    port: int
    internal_addr: str
    # HTTP protocol methods
    def get(self, *, path: str = "/", headers: Dict[str, str] = ...) -> 'response': ...
    def post(self, *, path: str = "/", body: str = "", headers: Dict[str, str] = ...) -> 'response': ...
    def put(self, *, path: str = "/", body: str = "", headers: Dict[str, str] = ...) -> 'response': ...
    def delete(self, *, path: str = "/", headers: Dict[str, str] = ...) -> 'response': ...
    def patch(self, *, path: str = "/", body: str = "", headers: Dict[str, str] = ...) -> 'response': ...
    # TCP protocol methods
    def send(self, *, data: str) -> str: ...
    # Postgres/MySQL protocol methods
    def query(self, *, sql: str) -> 'response': ...
    def exec(self, *, sql: str) -> 'response': ...
    # Redis protocol methods
    def set(self, *, key: str, value: str) -> 'response': ...
    # (see full list below)

class response:
    """Response from a protocol step method."""
    status: int
    body: str
    data: Any          # auto-decoded JSON (dict or list)
    ok: bool
    error: str
    duration_ms: int

class event:
    """Event in the trace log. Passed to where= lambdas."""
    seq: int
    service: str
    type: str          # "syscall", "stdout", "wal", "topic"
    event_type: str    # "syscall.write", "lifecycle.started"
    data: Any          # auto-decoded payload (dict)
    fields: Dict[str, str]
    first: Optional['event']  # in assert_before: matched first event
    op: str            # operation name (if using named ops)

class fault:
    """Fault definition returned by deny()/delay()/allow()."""
    ...

class healthcheck:
    """Healthcheck definition returned by tcp()/http()."""
    ...

class op:
    """Operation definition for named operations."""
    def __init__(self, *, syscalls: List[str], path: str = ...) -> None: ...

class decoder:
    """Decoder for event sources."""
    ...

class observe_source:
    """Event source for service observation."""
    ...

# ---------------------------------------------------------------------------
# Service & Interface
# ---------------------------------------------------------------------------

def service(
    name: str,
    binary: str = ...,
    *interfaces: interface,
    image: str = ...,
    build: str = ...,
    args: List[str] = ...,
    env: Dict[str, str] = ...,
    depends_on: List[service] = ...,
    volumes: Dict[str, str] = ...,
    healthcheck: healthcheck = ...,
    observe: List[observe_source] = ...,
    ops: Dict[str, op] = ...,
) -> service: ...

# ---------------------------------------------------------------------------
# Healthchecks
# ---------------------------------------------------------------------------

def tcp(addr: str, *, timeout: str = "10s") -> healthcheck: ...
def http(url: str, *, timeout: str = "10s") -> healthcheck: ...

# ---------------------------------------------------------------------------
# Fault Builders
# ---------------------------------------------------------------------------

def deny(errno: str, *, probability: str = "100%", label: str = ...) -> fault: ...
def delay(duration: str, *, probability: str = "100%", label: str = ...) -> fault: ...
def allow() -> fault: ...

# ---------------------------------------------------------------------------
# Fault Injection
# ---------------------------------------------------------------------------

def fault(svc: service, *, run: Callable, **syscall_faults: fault) -> Any: ...
def fault_start(svc: service, **syscall_faults: fault) -> None: ...
def fault_stop(svc: service) -> None: ...

# ---------------------------------------------------------------------------
# Assertions
# ---------------------------------------------------------------------------

def assert_true(condition: bool, msg: str = ...) -> None: ...
def assert_eq(a: Any, b: Any, msg: str = ...) -> None: ...

def assert_eventually(
    *,
    service: str = ...,
    syscall: str = ...,
    path: str = ...,
    decision: str = ...,
    where: Callable[[event], bool] = ...,
) -> None: ...

def assert_never(
    *,
    service: str = ...,
    syscall: str = ...,
    path: str = ...,
    decision: str = ...,
    where: Callable[[event], bool] = ...,
) -> None: ...

def assert_before(
    *,
    first: Union[Dict[str, str], Callable[[event], bool]],
    then: Union[Dict[str, str], Callable[[event], bool]],
) -> None: ...

# ---------------------------------------------------------------------------
# Events & Monitoring
# ---------------------------------------------------------------------------

def events(
    *,
    service: str = ...,
    syscall: str = ...,
    path: str = ...,
    decision: str = ...,
    where: Callable[[event], bool] = ...,
) -> List[event]: ...

def monitor(
    callback: Callable[[event], None],
    *,
    service: str = ...,
    syscall: str = ...,
    path: str = ...,
    decision: str = ...,
) -> None: ...

# ---------------------------------------------------------------------------
# Concurrency
# ---------------------------------------------------------------------------

def parallel(*callables: Callable) -> List[Any]: ...
def nondet(*services: service) -> None: ...

# ---------------------------------------------------------------------------
# Tracing
# ---------------------------------------------------------------------------

def trace(svc: service, *, syscalls: List[str], run: Callable) -> Any: ...
def trace_start(svc: service, *, syscalls: List[str]) -> None: ...
def trace_stop(svc: service) -> None: ...

# ---------------------------------------------------------------------------
# Network
# ---------------------------------------------------------------------------

def partition(svc_a: service, svc_b: service, *, run: Callable) -> Any: ...

# ---------------------------------------------------------------------------
# Scenarios
# ---------------------------------------------------------------------------

def scenario(fn: Callable) -> None: ...

# ---------------------------------------------------------------------------
# Event Sources & Decoders
# ---------------------------------------------------------------------------

def stdout(*, decoder: decoder = ...) -> observe_source: ...
def json_decoder() -> decoder: ...
def logfmt_decoder() -> decoder: ...
def regex_decoder(*, pattern: str) -> decoder: ...

# ---------------------------------------------------------------------------
# Starlark builtins
# ---------------------------------------------------------------------------

def print(*args: Any) -> None: ...
def fail(msg: str) -> None: ...
def load(module: str, *symbols: str) -> None: ...
```

### .vscode/faultbox.code-snippets

```json
{
    "Faultbox Service": {
        "prefix": "svc",
        "scope": "python",
        "body": [
            "${1:name} = service(\"${1:name}\",",
            "    interface(\"main\", \"${2|http,tcp,postgres,redis,kafka,mysql,nats,grpc|}\", ${3:8080}),",
            "    ${4|binary =,image =,build =|} \"${5:path}\",",
            "    healthcheck = ${6|tcp,http|}(\"localhost:${3:8080}\"),",
            ")"
        ],
        "description": "Faultbox service declaration"
    },
    "Faultbox Test": {
        "prefix": "test",
        "scope": "python",
        "body": [
            "def test_${1:name}():",
            "    \"\"\"${2:description}\"\"\"",
            "    resp = ${3:api}.${4|get,post,put,delete|}(path=\"${5:/}\")",
            "    assert_eq(resp.status, ${6:200})"
        ],
        "description": "Faultbox test function"
    },
    "Faultbox Scenario": {
        "prefix": "scenario",
        "scope": "python",
        "body": [
            "def ${1:name}():",
            "    \"\"\"${2:Happy path description}\"\"\"",
            "    ${3:resp = api.post(path=\"/\", body=\"\")}",
            "    ${4:assert_eq(resp.status, 200)}",
            "",
            "scenario(${1:name})"
        ],
        "description": "Faultbox scenario (happy path for generator)"
    },
    "Faultbox Fault": {
        "prefix": "fault",
        "scope": "python",
        "body": [
            "def test_${1:name}():",
            "    \"\"\"${2:description}\"\"\"",
            "    def scenario():",
            "        resp = ${3:api}.${4|post,get|}(path=\"${5:/}\")",
            "        assert_true(resp.status >= 500, \"${6:expected error}\")",
            "    fault(${7:db}, ${8|write,connect,read,fsync|}=${9|deny,delay|}(\"${10:EIO}\", label=\"${11:label}\"), run=scenario)"
        ],
        "description": "Faultbox fault injection test"
    },
    "Faultbox Monitor": {
        "prefix": "monitor",
        "scope": "python",
        "body": [
            "monitor(lambda e: fail(\"${1:violation}\") if ${2:condition},",
            "    service=\"${3:service}\",",
            "    ${4|syscall,decision|}=\"${5:value}\",",
            ")"
        ],
        "description": "Faultbox event monitor"
    },
    "Faultbox Observe Stdout": {
        "prefix": "observe",
        "scope": "python",
        "body": [
            "observe = [stdout(decoder=${1|json_decoder(),logfmt_decoder(),regex_decoder(pattern=\"\")|})]"
        ],
        "description": "Faultbox stdout observation"
    },
    "Faultbox Assert Eventually": {
        "prefix": "assert_ev",
        "scope": "python",
        "body": [
            "assert_eventually(where=lambda e: e.${1|service,type|} == \"${2:value}\" and e.data[\"${3:key}\"] == \"${4:value}\")"
        ],
        "description": "Faultbox temporal assertion with lambda"
    }
}
```

### `faultbox init --vscode` command

Adds a `--vscode` flag to the existing `init` command (or a standalone
subcommand) that copies the stub files into the project:

```bash
faultbox init --vscode
# Creates:
#   .vscode/settings.json
#   .vscode/faultbox.code-snippets
#   faultbox.pyi
```

If files already exist, only updates `faultbox.pyi` (regenerated from
the current builtin registry).

## What the User Gets

After setup, typing in a `.star` file shows:

- **Function autocomplete**: type `fault` → shows `fault()`, `fault_start()`, `fault_stop()` with signatures
- **Parameter hints**: inside `deny(` → shows `errno, probability=, label=`
- **Attribute completion**: after `resp.` → shows `status`, `body`, `data`, `ok`, `error`, `duration_ms`
- **Snippet expansion**: type `svc` + Tab → full service declaration template
- **Protocol methods**: after `db.main.` → shows `query`, `exec` (from postgres protocol)

## Limitations of Phase 1

- No **context-aware** completions (e.g., can't suggest only services
  defined in the current file)
- No **Starlark-specific** syntax (Python mode doesn't know about `load()`)
- Protocol methods are **statically listed** in the stub, not dynamically
  resolved from the actual interface protocol
- No **error checking** (e.g., won't warn about wrong parameter types)

These limitations are addressed by Phase 2 (syntax extension) and
Phase 3 (LSP server).

## Key Files

| File | Purpose |
|------|---------|
| `faultbox.pyi` | Type stubs — generated, lives in project root |
| `.vscode/settings.json` | Associates .star with Python, sets stub path |
| `.vscode/faultbox.code-snippets` | Code templates for common patterns |
| `cmd/faultbox/main.go` | `init --vscode` flag to generate files |

## Rollout

1. Create `faultbox.pyi` stub content (hardcoded in Go)
2. Create `.code-snippets` content (hardcoded in Go)
3. Add `--vscode` flag to `init` command
4. Test with VS Code + Python extension
5. Docs: setup guide in tutorial or CLI reference
