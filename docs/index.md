# Faultbox Documentation

Fault injection for distributed systems using Linux seccomp-notify.

## Guides

- [Tutorial](tutorial/) -- 12-chapter hands-on guide from first fault to advanced features

## Reference

- [Spec Language](spec-language.md) -- Complete Starlark API: services, faults, assertions, protocols
- [CLI Reference](cli-reference.md) -- All commands and flags
- [Error Codes](errno-reference.md) -- Errno values for syscall fault injection

## Design

- [Protocol Proxy](design/protocol-proxy.md) -- Transparent proxy for protocol-level faults
- [Failure Scenario Generator](design/failure-scenario-generator.md) -- Automatic fault generation
- [Named Operations](design/named-operations.md) -- Grouping syscalls into logical operations
- [VS Code Autocomplete](design/vscode-autocomplete.md) -- IDE integration

## Use Cases

- [Backend Engineer](use-cases/backend-engineer.md) -- Verifying error handling in microservices
- [QA Engineer](use-cases/qa-engineer.md) -- Systematic resilience testing
- [Mobile Engineer](use-cases/mobile-engineer.md) -- Testing offline and degraded network behavior
