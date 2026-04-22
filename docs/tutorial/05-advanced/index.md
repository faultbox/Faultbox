# Part 5: Advanced Features

Test real infrastructure, auto-generate failures, capture structured events,
define high-level operations, and integrate with LLM agents.

| Chapter | Duration | What you'll learn |
|---------|----------|------------------|
| [Containers](09-containers.md) | 30 min | Test real Postgres, Redis with Docker containers |
| [Scenarios & Generation](10-scenarios.md) | 20 min | scenario(), faultbox generate, load(), per-scenario fault files |
| [Event Sources](11-event-sources.md) | 25 min | observe=, stdout/WAL/Kafka events, decoders, .data |
| [Named Operations](12-named-ops.md) | 15 min | ops=, op(), operation-level faults, trace output |
| [LLM Agents & MCP](13-llm-mcp.md) | 15 min | Claude Code setup, MCP server, --format json, CI integration |
| [Mock Services](17-mock-services.md) | 25 min | mock_service(), @faultbox/mocks/ stdlib, TLS, faulting mocks, when to use vs real services |
| [Typed gRPC Mocks](18-typed-grpc-mocks.md) | 20 min | grpc.server(descriptors=...), FileDescriptorSet ingestion, typed responses for compiled-stub clients, reflection + grpcurl |
| [OpenAPI Mocks](19-openapi-mocks.md) | 15 min | http.server(openapi=...), example strategies, overrides, strict validation |
| [`.fb` Bundles](20-bundles.md) | 10 min | Reproducibility-by-default, `faultbox inspect`, sharing runs, zero-traffic hints |
