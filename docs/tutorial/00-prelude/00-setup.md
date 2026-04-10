# Chapter 0: Setup

**Duration:** 10 minutes

## Why a setup chapter?

Faultbox uses Linux's seccomp-notify, which only works on Linux kernel 5.6+.
If you're on macOS, you'll run everything inside a lightweight Linux VM (Lima).
This chapter gets your environment ready so every subsequent chapter "just works."

## Step 1: Install Faultbox

```bash
curl -fsSL https://faultbox.io/install.sh | sh
```

This installs the `faultbox` binary to `~/.faultbox/bin/`. Add it to your PATH
if the installer suggests it.

Verify:
```bash
faultbox --version
```

> **Note:** On macOS the binary installs but can't run tests directly —
> seccomp-notify requires Linux. Step 2 sets up a Lima VM for that.

## Step 2: Clone the demo

The tutorial uses demo services from a separate repo:

```bash
git clone https://github.com/faultbox/demo.git
cd demo
```

This contains:

| Service | Description | Chapters |
|---------|-------------|----------|
| `target/` | Minimal binary (write + HTTP) | 1 |
| `mock-db/` | TCP key-value store | 2-3 |
| `mock-api/` | HTTP API wrapping mock-db | 2-3 |
| `inventory-svc/` | TCP service with WAL | 4-6 |
| `order-svc/` | HTTP API calling inventory | 4-6 |

## Step 3: Choose your platform

### Linux (native)

Build the demo services:
```bash
make build
```

This creates binaries in `bin/`. Edit the `BIN` variable in `.star` files
to point to `"bin"`:

```python
BIN = "bin"   # Linux native
```

Verify:
```bash
faultbox test first-test.star
```

### macOS (via Lima VM)

Lima creates a lightweight Linux VM that mounts your Mac's filesystem.

**One-time setup:**
```bash
brew install lima                # if not installed
make lima-create                 # creates VM with kernel 5.6+ (~3 min)
```

**Every session:**
```bash
make lima-start                  # start VM (if stopped)
make lima-build                  # cross-compile demos + install faultbox in VM
```

This cross-compiles demo binaries to `bin/linux/` and installs `faultbox`
inside the VM.

**Running commands inside the VM:**

```bash
make lima-test                   # run demo.star
make lima-run CMD="faultbox test first-test.star"
make lima-run CMD="faultbox run --fault 'write=EIO:100%' bin/linux/target"
```

Or create an alias for direct access:
```bash
alias vm="limactl shell --workdir /host-home/${PWD#$HOME/} faultbox-dev --"
vm faultbox --help
vm faultbox test first-test.star
```

**Binary paths on macOS:** The `.star` files default to `BIN = "bin/linux"`.
This works for Lima. On Linux native, change to `BIN = "bin"`.

## Step 4: Verify it works

**Linux:**
```bash
faultbox test first-test.star
```

**macOS:**
```bash
make lima-run CMD="faultbox test first-test.star"
```

You should see:
```
--- PASS: test_ping (200ms, seed=0) ---
--- PASS: test_set_and_get (200ms, seed=0) ---
--- PASS: test_happy_path (210ms, seed=0) ---
--- PASS: test_write_failure (5200ms, seed=0) ---

4 passed, 0 failed
```

If you see this, you're ready for Chapter 1.

## Step 5: VS Code autocomplete (optional)

```bash
faultbox init --vscode
```

This creates type stubs and snippets for `.star` files. Works on both
macOS and Linux (doesn't need seccomp).

| Prefix | Expands to |
|--------|-----------|
| `svc` | Full service declaration |
| `test` | Test function skeleton |
| `scenario` | Scenario with `scenario()` registration |
| `fault` | Fault injection test |

> **Requires** the Python extension for VS Code (ms-python.python).

## Step 6: Claude Code integration (optional)

```bash
faultbox init --claude
```

Creates `/fault-test`, `/fault-generate`, `/fault-diagnose` slash commands
and auto-configures the MCP server. See [Chapter 13](../04-advanced/13-llm-mcp.md).

## Step 7: Docker (Chapter 9 only)

Docker is only needed for Chapter 9 (containers). Skip for now.

**macOS:** Docker is available inside the Lima VM:
```bash
make lima-run CMD="docker version"
```

## Quick reference

| What | Linux | macOS (Lima) |
|------|-------|-------------|
| Install faultbox | `curl -fsSL https://faultbox.io/install.sh \| sh` | Same (runs on host, installs macOS binary) |
| Build demos | `make build` | `make lima-build` |
| BIN path in .star | `BIN = "bin"` | `BIN = "bin/linux"` (default) |
| Run tests | `faultbox test first-test.star` | `make lima-run CMD="faultbox test first-test.star"` |
| Run faultbox | `faultbox run ...` | `make lima-run CMD="faultbox run ..."` |
| Container tests | `sudo faultbox test ...` | `make lima-run CMD="sudo faultbox test ..."` |
