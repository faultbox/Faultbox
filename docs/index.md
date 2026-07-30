# Faultbox Documentation

Fault injection for distributed systems. Declare a topology in Starlark,
inject faults at the syscall, protocol, or packet level, and assert on what
the system actually did.

## Start here

- [Tutorial](tutorial/) — 30 chapters in 6 parts, from your first fault to
  filesystem observation
- [Setup](tutorial/00-prelude/00-setup.md) — install, and the Lima VM if you
  are on macOS
- [Positioning](positioning.md) — what Faultbox is for, and what it is not

## Reference

- [Spec Language](spec-language.md) — the complete Starlark API: services,
  clients, faults, assertions, temporal operators
- [CLI Reference](cli-reference.md) — every command and flag
- [Protocols](protocols/README.md) — 13 protocol plugins: methods, fault
  rules, credentials, readiness
- [Error Codes](errno-reference.md) — errno values for syscall faults
- [Starlark Dialect](starlark-dialect.md) — how this differs from Python
- [Feature Manifest](feature-manifest.md) — what is supported, and the test
  that proves each claim

## Capabilities

| Level | What it does | Reference |
|---|---|---|
| Syscall | `deny`, `delay`, `hold` on intercepted syscalls | [tutorial ch. 3](tutorial/02-syscall-level/03-fault-injection.md) |
| Protocol | Rewrite wire responses per query, path, key, topic | [protocols/](protocols/README.md) |
| Packet | Loss, delay, reorder, bandwidth, MTU per segment | [tutorial ch. 27](tutorial/03-protocol-level/27-packet-faults.md) |
| Filesystem | Observe which files a service touches (`watch()`) | [tutorial ch. 29](tutorial/05-advanced/29-filesystem-observation.md) |

- [Determinism](determinism.md) — L0–L5 levels, `unmediated_io`, strict mode
- [Temporal Properties](temporal.md) — `eventually`, `always`, `monitor`
- [Exploration](exploration.md) — `choose()`, `assume()`, `faultbox plan`
- [Nondeterministic Operators](nondeterministic-operators.md)
- [Mock Services](mock-services.md) — in-process stand-ins for trivial
  dependencies
- [Recipes](recipes.md) — the embedded `@faultbox/` stdlib

## Guides

- [Methodology](guides/methodology.md) — how to choose what to test
- [Spec Patterns](guides/spec-patterns.md) — idioms that hold up
- [Choosing Fault Levels](guides/choosing-fault-levels.md) — syscall vs
  protocol vs packet
- [Invariants](guides/invariants.md) — writing assertions that mean something
- [Seeding Data](guides/seeding-data.md) — `seed`, `reset`, `reuse`
- [Connectivity](guides/connectivity.md) — who can reach whom, and how
  addresses are rewritten
- [CI on Linux](guides/ci-on-linux.md)
- [In-Cluster](guides/in-cluster.md) — remote services against real pods

## Outputs

- [Reports](reports.md) — the self-contained HTML report
- [Bundles](bundles.md) — the `.fb` archive and `faultbox replay`
- [Troubleshooting](troubleshooting.md)
- [Seccomp Cheatsheet](seccomp-cheatsheet.md)

## Design

- [Protocol Proxy](design/protocol-proxy.md) — transparent proxy for
  protocol-level faults
- [Failure Scenario Generator](design/failure-scenario-generator.md)
- [Named Operations](design/named-operations.md) — grouping syscalls into
  logical operations
- [VS Code Autocomplete](design/vscode-autocomplete.md)
- [RFCs](rfcs/README.md) — the design record

## Use Cases

- [Backend Engineer](use-cases/backend-engineer.md) — verifying error
  handling in microservices
- [QA Engineer](use-cases/qa-engineer.md) — systematic resilience testing
- [Mobile Engineer](use-cases/mobile-engineer.md) — offline and degraded
  network behavior
- [Game Developer](use-cases/gamedev.md)
- [Checkout / Kafka outage](use-cases/checkout-kafka-outage.md) — a worked
  example
