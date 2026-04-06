# Chapter 0: Setup

**Duration:** 10 minutes

## Why a setup chapter?

Faultbox uses Linux's seccomp-notify, which only works on Linux kernel 5.6+.
If you're on macOS, you'll run everything inside a lightweight Linux VM (Lima).
This chapter gets your environment ready so every subsequent chapter "just works."

## Step 1: Clone and build

```bash
git clone https://github.com/faultbox/Faultbox.git
cd Faultbox
```

## Step 2: Choose your platform

### Linux (native)

You're running directly on Linux. Build everything:

```bash
make build          # builds bin/faultbox (host binary)
go build -o bin/target     ./poc/target/
go build -o bin/mock-db    ./poc/mock-db/
go build -o bin/mock-api   ./poc/mock-api/
```

Verify:
```bash
bin/faultbox --help
```

For the rest of the tutorial, all commands use `bin/` paths:
```bash
bin/faultbox run ...
bin/target
bin/mock-db
```

### macOS (via Lima VM)

Lima creates a lightweight Linux VM that mounts your Mac's filesystem.
Faultbox binaries are cross-compiled for Linux and run inside the VM.

**One-time setup:**
```bash
brew install lima              # if not installed
make env-create                # creates and starts the 'faultbox-dev' VM (~2 min)
```

**Every session:**
```bash
make env-start                 # start the VM (if stopped)
make demo-build                # cross-compile ALL binaries for linux/arm64
```

This builds into `bin/linux-arm64/`:
```
bin/linux-arm64/
├── faultbox        # CLI
├── faultbox-shim   # container entrypoint shim
├── target          # chapter 1
├── mock-db         # chapters 2-3
├── mock-api        # chapter 3
├── inventory-svc   # chapters 4-6
└── order-svc       # chapters 4-6
```

**Running commands inside the VM:**

Every faultbox command is wrapped with `limactl shell`. Run from the
Faultbox project root:
```bash
limactl shell --workdir /host-home/<path-from-home>/Faultbox faultbox-dev -- <command>
```

For convenience, create an alias that maps your Mac's working directory
into the VM (your home `~` is mounted at `/host-home`):
```bash
# From the Faultbox project root:
alias vm="limactl shell --workdir /host-home/${PWD#$HOME/} faultbox-dev --"
```

> **Example:** if you cloned to `~/git/faultbox-demo/Faultbox`, the alias
> resolves to `--workdir /host-home/git/faultbox-demo/Faultbox`.

Then:
```bash
vm bin/linux-arm64/faultbox --help
vm bin/linux-arm64/target
```

**Binary paths on macOS:** Throughout the tutorial, when you see `bin/faultbox`
or `bin/target`, use `bin/linux-arm64/faultbox` or `bin/linux-arm64/target`
instead, and run inside the VM.

## Step 3: Verify it works

**Linux:**
```bash
bin/faultbox --help
bin/target
```

**macOS:**
```bash
vm bin/linux-arm64/faultbox --help
vm bin/linux-arm64/target
```

You should see:
```
PID: 12345
filesystem: write+read OK (2ms)
network: HTTP 200 OK (150ms)
```

If you see this, you're ready for Chapter 1.

## Step 4: VS Code autocomplete (recommended)

If you use VS Code, set up autocomplete for `.star` files.

**Linux:**
```bash
bin/faultbox init --vscode
```

**macOS:** Build a host-native binary first (init doesn't need Linux):
```bash
go build -o bin/faultbox ./cmd/faultbox/
bin/faultbox init --vscode
```

This creates three files:
- **`faultbox.pyi`** — type stubs so VS Code shows parameter hints and
  attribute completion for all Faultbox builtins
- **`.vscode/settings.json`** — associates `.star` files with Python
  syntax highlighting
- **`.vscode/faultbox.code-snippets`** — code templates triggered by
  typing prefixes:

| Prefix | Expands to |
|--------|-----------|
| `svc` | Full service declaration |
| `test` | Test function skeleton |
| `scenario` | Scenario with `scenario()` registration |
| `fault` | Fault injection test |
| `monitor` | Event monitor |
| `observe` | stdout observation with decoder |
| `assert_ev` | Lambda assertion |

After setup, open any `.star` file in VS Code — you'll get autocomplete
for `fault(`, `deny(`, `assert_eventually(`, `resp.data`, etc.

> **Note:** This requires the Python extension for VS Code (ms-python.python).
> The autocomplete works because Starlark syntax is close enough to Python.

## Step 5: Docker (chapter 7 only)

Docker is only needed for Chapter 7 (containers). Skip this for now.

**Linux:** Install Docker normally.

**macOS:** Docker is already installed inside the Lima VM. Verify:
```bash
vm docker version
```

Container tests require `sudo`:
```bash
vm sudo bin/linux-arm64/faultbox test poc/demo-container/faultbox.star
```

## Quick reference

| What | Linux | macOS (Lima) |
|------|-------|-------------|
| Build | `make build` | `make demo-build` |
| VS Code setup | `bin/faultbox init --vscode` | `bin/faultbox init --vscode` (host binary, see Step 4) |
| Faultbox binary | `bin/faultbox` | `bin/linux-arm64/faultbox` (run inside VM) |
| Target binary | `bin/target` | `bin/linux-arm64/target` (run inside VM) |
| Run command | `bin/faultbox run ...` | `vm bin/linux-arm64/faultbox run ...` |
| Test command | `bin/faultbox test ...` | `vm bin/linux-arm64/faultbox test ...` |
| Generate faults | `bin/faultbox generate ...` | `vm bin/linux-arm64/faultbox generate ...` |
| Container test | `sudo bin/faultbox test ...` | `vm sudo bin/linux-arm64/faultbox test ...` |
