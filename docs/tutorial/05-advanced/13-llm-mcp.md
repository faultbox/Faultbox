---
title: "Chapter 13: LLM Agents & MCP Integration"
---

# Chapter 13: LLM Agents & MCP Integration

**Duration:** 15 minutes
**Prerequisites:** [Chapter 0 (Setup)](../00-prelude/00-setup.md) completed

## Goals & Purpose

Faultbox is designed for two types of users: **human engineers** and
**LLM agents**. Both write specs, run tests, and fix code — but LLM agents
need structured output and tool integration instead of human-readable text.

This chapter teaches you to:
- **Set up Claude Code integration** in one command
- **Use custom slash commands** for fault injection workflows
- **Connect via MCP** for native tool-use in any LLM agent
- **Parse structured JSON output** for automated code-test-fix loops

After this chapter, your LLM agent can autonomously: generate specs from
docker-compose, run fault tests, analyze failures, and suggest fixes.

## Quick setup

One command sets up everything:

```bash
faultbox init --claude
```

This creates:

```
.claude/commands/
├── fault-test.md         # /fault-test slash command
├── fault-generate.md     # /fault-generate slash command
└── fault-diagnose.md     # /fault-diagnose slash command
.mcp.json                 # MCP server auto-config
```

Open Claude Code in your project. The slash commands and MCP tools are
available immediately.

## Custom slash commands

### `/fault-test` — Run tests

Type `/fault-test` in Claude Code. It finds your `.star` spec, runs all
tests with `--format json`, and reports:
- Pass/fail count
- Failure reasons with replay commands
- Diagnostics with fix suggestions

### `/fault-generate` — Generate specs

Type `/fault-generate`. It detects your project setup:
- Has `docker-compose.yml`? Generates spec from compose
- Has a Go/Node/Python service? Generates a starter spec
- Has an existing spec? Runs `faultbox generate` for failure scenarios

### `/fault-diagnose` — Analyze failures

Type `/fault-diagnose` after a test failure. It reads the JSON output,
finds the relevant source code, and suggests specific fixes based on
the diagnostic codes:

| Diagnostic | What it means |
|-----------|---------------|
| `FAULT_FIRED_BUT_SUCCESS` | Fault hit but service didn't return error — missing error handling |
| `FAULT_NOT_FIRED` | Wrong syscall variant or path filter |
| `SERVICE_CRASHED` | Unhandled error caused panic |
| `TIMEOUT_DURING_FAULT` | Possible infinite retry loop |

## MCP server

The MCP (Model Context Protocol) server exposes Faultbox as native tools
for any compatible LLM agent.

### How it works

```
LLM Agent ──(JSON-RPC)──→ faultbox mcp ──→ run tests, parse results
                                         ──→ generate specs
                                         ──→ analyze topology
```

The `.mcp.json` file (created by `faultbox init --claude`) auto-configures
the connection:

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

### Available tools

| Tool | Description |
|------|-------------|
| `run_test` | Run all tests, return structured JSON results |
| `run_single_test` | Run a specific test by name |
| `list_tests` | Discover test functions in a .star file |
| `generate_faults` | Run failure scenario generator |
| `init_from_compose` | Generate spec from docker-compose.yml |
| `init_spec` | Generate starter spec for a binary |

### Example: agent workflow

An LLM agent building a microservice uses Faultbox like this:

```
1. Agent writes service code (Go, Rust, Node, etc.)
2. Agent writes docker-compose.yml
3. Agent calls init_from_compose → gets faultbox.star
4. Agent calls run_test → gets structured results
5. Test fails: FAULT_FIRED_BUT_SUCCESS on write path
6. Agent reads diagnostic → missing error handling in /data endpoint
7. Agent fixes the code → adds proper error return
8. Agent calls run_test again → all pass
9. Agent commits the fix with confidence
```

No human intervention. The structured JSON output and diagnostics give the
agent enough context to fix issues autonomously.

## Structured JSON output

For CI pipelines and programmatic consumption:

```bash
faultbox test faultbox.star --format json
```

JSON goes to stdout, human output goes to stderr. Parse with `jq`:

```bash
# Check if all tests passed
faultbox test spec.star --format json | jq '.fail == 0'

# Get failed test names
faultbox test spec.star --format json | jq '.tests[] | select(.result=="fail") | .name'

# Get diagnostics
faultbox test spec.star --format json | jq '.tests[].diagnostics[]'
```

### JSON structure

```json
{
  "version": 2,
  "pass": 3,
  "fail": 1,
  "tests": [
    {
      "name": "test_write_failure",
      "result": "fail",
      "reason": "assert_eq failed: 200 != 503",
      "failure_type": "assertion",
      "seed": 42,
      "replay_command": "faultbox test spec.star --test write_failure --seed 42",
      "faults": [
        {
          "service": "db",
          "syscall": "write",
          "action": "deny",
          "errno": "EIO",
          "hits": 3,
          "label": "disk failure"
        }
      ],
      "syscall_summary": {
        "db": {"total": 45, "faulted": 3, "breakdown": {"write": 20, "read": 15}},
        "api": {"total": 30, "faulted": 0, "breakdown": {"write": 10, "connect": 5}}
      },
      "diagnostics": [
        {
          "level": "error",
          "code": "ASSERTION_MISMATCH",
          "message": "assert_eq failed: 200 != 503",
          "suggestion": "Check the service's error handling logic."
        }
      ]
    }
  ]
}
```

## Docker (for CI agents)

LLM agents in CI environments (GitHub Actions, GitLab CI) can use the
Docker image:

```bash
docker run --privileged -v $(pwd):/workspace -w /workspace \
  ghcr.io/faultbox/faultbox test faultbox.star --format json
```

Or use the GitHub Action:

```yaml
- uses: faultbox/faultbox/.github/actions/test@main
  with:
    spec: faultbox.star
```

The action installs Faultbox, runs tests, posts a summary, and uploads
JSON results as an artifact.

## Manual MCP setup

For editors other than Claude Code, configure the MCP server manually.

**Cursor:** Add to `.cursor/mcp.json`:
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

**Claude Desktop:** Add to `claude_desktop_config.json`:
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

## What you learned

- `faultbox init --claude` sets up Claude Code integration in one command
- `/fault-test`, `/fault-generate`, `/fault-diagnose` slash commands
- MCP server exposes 6 tools for native LLM agent integration
- `--format json` provides structured output for automated workflows
- Diagnostics give agents enough context to fix code autonomously
- Docker image and GitHub Action for CI integration

## What's next

You've completed the Faultbox tutorial. You now know how to:
- Inject syscall-level and protocol-level faults
- Write temporal assertions on internal behavior
- Explore concurrent interleavings
- Monitor invariants across all tests
- Use containers with real infrastructure
- Generate failure scenarios automatically
- Integrate with LLM agents for automated testing

**Next steps:**
- Add Faultbox to your CI pipeline
- Write scenario() functions for your critical paths
- Run `faultbox generate` to discover untested failure modes
- Use `/fault-diagnose` to fix issues found by the generator
