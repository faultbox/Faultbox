# Publishing Faultbox to the Smithery MCP Registry

## Overview

[Smithery](https://smithery.ai) is the registry for MCP (Model Context Protocol)
servers. Publishing Faultbox there makes it discoverable by developers searching
for "fault injection", "testing", "chaos engineering", or "distributed systems".

## Prerequisites

1. Faultbox `mcp` command working (`faultbox mcp`)
2. `smithery.yaml` in repo root (already done)
3. Smithery account linked to GitHub

## Steps

### 1. Sign up at smithery.ai

Go to https://smithery.ai and sign in with GitHub.

### 2. Create a new server

Click "Submit Server" and enter the GitHub repo URL:
```
https://github.com/faultbox/faultbox
```

Smithery reads the `smithery.yaml` from the repo root. Our config uses
`stdio` transport — Smithery will detect the command function and build
the server page automatically.

### 3. Fill in metadata

- **Name:** Faultbox
- **Description:** Fault injection for distributed systems. Intercept syscalls
  and protocol messages to test how your services behave under failure.
- **Categories:** Testing, DevOps, Infrastructure
- **Tags:** fault-injection, testing, distributed-systems, chaos-engineering,
  seccomp, starlark, microservices

### 4. Test the installation

Once published, users install via:
```bash
npx @smithery/cli install faultbox --client claude
```

Or configure manually in Claude Desktop (`claude_desktop_config.json`):
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

### 5. Verify tools appear

After installation, verify in Claude Desktop or Claude Code that 6 tools
are available:

| Tool | Description |
|------|-------------|
| `run_test` | Run all tests in a .star spec file |
| `run_single_test` | Run a specific test by name |
| `list_tests` | Discover test functions |
| `generate_faults` | Run failure scenario generator |
| `init_from_compose` | Generate spec from docker-compose.yml |
| `init_spec` | Generate starter spec for a binary |

## smithery.yaml format

Our current config:

```yaml
startCommand:
  type: stdio
  configSchema:
    type: object
    properties: {}
  commandFunction: |-
    (config) => ({ command: 'faultbox', args: ['mcp'] })
```

- `type: stdio` — server communicates over stdin/stdout (standard MCP transport)
- `configSchema` — no user configuration needed (could add `workdir` later)
- `commandFunction` — JavaScript function that returns the command to run

## Future enhancements

- Add `configSchema` property for `workdir` to set the working directory
- Add a Docker-based alternative for environments without Go/Linux
- Auto-run tests on PR comments via the MCP connection

## References

- [Smithery docs: smithery.yaml](https://smithery.ai/docs/build/project-config/smithery-yaml)
- [Smithery docs: stdio transport](https://smithery.ai/docs/build/transports/stdio)
- [MCP specification](https://spec.modelcontextprotocol.io)
