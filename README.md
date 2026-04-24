# Faultbox

Fault injection for distributed systems. Intercept syscalls and protocol messages
to test how your services behave under failure.

Faultbox uses Linux's **seccomp-notify** to pause processes on specific syscalls
and decide — allow, deny, or delay — without modifying application code.
Tests are written in **Starlark** (Python-like): declare topology, start services,
inject faults, assert on behavior.

```python
# faultbox.star
api = service("api", binary="./api", http="localhost:8080")
db  = service("db",  binary="./db",  tcp="localhost:5432")

def test_write_failure(t):
    fault(db, write=deny("EIO"))
    resp = api.http.post("/orders", json={"item": "widget"})
    assert_eq(resp.status, 503, "API should return 503 when DB fails")
```

```bash
faultbox test faultbox.star
```

## Features

**Syscall-level fault injection** -- deny, delay, or hold any syscall.
Faultbox automatically expands families (`write` covers `write`, `writev`, `pwrite64`).

**Protocol-level fault injection** -- inject faults at HTTP, HTTP/2, gRPC,
Postgres, MySQL, Redis, Kafka, NATS, MongoDB, Cassandra, ClickHouse, AMQP,
Memcached, UDP, and TCP protocol level via transparent proxy.

**Recipe library** -- curated failure wrappers ship embedded in the binary.
`load("@faultbox/recipes/mongodb.star", "mongodb")` gets you `mongodb.disk_full()`,
`mongodb.replica_unavailable()`, and more — no filesystem setup needed.
Discover with `faultbox recipes list`.

**Deterministic exploration** -- `hold()` + `release()` control syscall ordering
across services. `--explore` mode walks all interleavings automatically.

**Two modes** -- run local binaries with `binary=` or real infrastructure
(Postgres, Redis, Kafka) in Docker containers with `image=`.

**Starlark specs** -- topology, faults, and assertions in one file. No YAML.
No separate config language. The spec is code.

**Reproducibility bundles + HTML reports** -- every run writes a `.fb` bundle
with trace, env, spec, and a replay command. Run `faultbox report <bundle.fb>`
for a single self-contained HTML — fault matrix, swim-lane trace viewer, full
drill-down. Email it, Slack it, commit it. Offline forever.
[Live example ›](https://faultbox.io/reports/sample.html)

## Install

```bash
curl -fsSL https://faultbox.io/install.sh | sh
```

This detects your platform (linux/darwin, amd64/arm64), downloads the latest
release, verifies the checksum, and installs to `~/.faultbox/bin`.

Or install a specific version:
```bash
FAULTBOX_VERSION=0.1.0 curl -fsSL https://faultbox.io/install.sh | sh
```

### Build from source

Requirements: Go 1.24+, Linux kernel 5.6+ (macOS via [Lima](https://lima-vm.io/))

```bash
git clone https://github.com/faultbox/faultbox.git
cd faultbox
make build          # Build bin/faultbox
```

For macOS (cross-compile for Lima VM):

```bash
make env-create     # Create Lima VM (first time)
make env-start      # Start VM
make demo-build     # Cross-compile faultbox + demo binaries
```

### Run the Demo

```bash
# Linux (native)
bin/faultbox test poc/demo/demo.star

# macOS (via Lima)
make demo
```

## Documentation

| Document | Description |
|----------|-------------|
| [Tutorial](docs/tutorial/) | 12-chapter hands-on guide (beginner to advanced) |
| [Spec Language Reference](docs/spec-language.md) | Complete Starlark API reference |
| [CLI Reference](docs/cli-reference.md) | All commands and flags |
| [Error Code Reference](docs/errno-reference.md) | Errno values for fault injection |

### Tutorial Structure

| Part | Chapters | Topics |
|------|----------|--------|
| [0: Prelude](docs/tutorial/00-prelude/) | Setup | Environment, Lima VM |
| [1: First Taste](docs/tutorial/01-first-taste/) | 1-2 | First fault, first test |
| [2: Syscall-Level](docs/tutorial/02-syscall-level/) | 3-6 | Fault injection, traces, concurrency, monitors |
| [3: Protocol-Level](docs/tutorial/03-protocol-level/) | 7-8 | HTTP/Redis faults, database faults |
| [4: Advanced](docs/tutorial/04-advanced/) | 9-12 | Containers, scenarios, event sources, named ops |

## How It Works

```
Starlark Spec (.star)
    |
    v
Runtime -- parses topology, discovers tests
    |
    +-- For each test:
        1. Start services (binary or Docker container)
        2. Install seccomp filter (via shim for containers)
        3. Wait for healthchecks
        4. Run test function (HTTP/TCP steps, fault injection)
        5. Notification loop processes intercepted syscalls
        6. Stop services, report trace
```

**seccomp-notify** pauses a process when it makes a specific syscall and asks
Faultbox (the supervisor) what to do. This happens in the kernel — no `ptrace`,
no eBPF, no code instrumentation.

For protocol-level faults, Faultbox injects a transparent proxy between services
and rewrites address wiring automatically. The proxy intercepts protocol messages
(e.g., specific SQL queries, HTTP paths, Kafka topics) and applies fault rules.

## Project Structure

```
faultbox/
├── cmd/
│   ├── faultbox/             # CLI: test, run, generate commands
│   └── faultbox-shim/        # Container entrypoint shim (Linux-only)
├── internal/
│   ├── engine/               # Session lifecycle, fault rules, notification loop
│   ├── seccomp/              # BPF filter generation, seccomp-notify API
│   ├── star/                 # Starlark runtime, builtins, event log
│   ├── container/            # Docker API wrapper, container launch
│   ├── protocol/             # Protocol plugins (http, tcp, postgres, redis, etc.)
│   ├── proxy/                # Transparent proxy for protocol-level faults
│   ├── eventsource/          # Event source plugins + decoders
│   └── generate/             # Failure scenario generator
├── poc/                      # Demo services and example specs
├── docs/                     # Documentation and tutorials
├── lima/                     # Lima VM configuration for macOS
└── Makefile
```

## LLM Agent Integration

Faultbox is designed to work with LLM agents (Claude, Cursor, etc.) for
automated code → test → fix workflows.

### Claude Code setup

```bash
faultbox init --claude
```

This creates:
- `.claude/commands/fault-test.md` — `/fault-test` slash command
- `.claude/commands/fault-generate.md` — `/fault-generate` slash command
- `.claude/commands/fault-diagnose.md` — `/fault-diagnose` slash command
- `.mcp.json` — MCP server config (auto-connects `faultbox mcp`)

### MCP server (manual)

Add to your Claude Code or Claude Desktop config:
```json
{
  "mcpServers": {
    "faultbox": {
      "command": "faultbox",
      "args": ["mcp"]
    }
  }
}
```

Available tools: `run_test`, `run_single_test`, `list_tests`, `generate_faults`,
`init_from_compose`, `init_spec`.

### Structured output

```bash
faultbox test spec.star --format json
```

Machine-parseable JSON with test results, fault info, syscall summary, and
actionable diagnostics.

### Docker (CI / agents)

```bash
docker run --privileged ghcr.io/faultbox/faultbox test /workspace/spec.star
```

## Contributing

```bash
make build          # Build
make test           # Run all tests
make lint           # Format + vet
```

Branch naming: `feature/`, `bugfix/`, `docs/`
Commit messages: [Conventional Commits](https://www.conventionalcommits.org/) (`feat:`, `fix:`, `docs:`)

## License

Apache License 2.0. See [LICENSE](LICENSE).
