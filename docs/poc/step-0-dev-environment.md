# PoC Step 0: Linux Dev Environment on macOS

**Branch:** `poc/step-0-dev-environment`
**Status:** In Progress
**Date:** 2026-03-22

## Goal

Set up a Linux development environment on macOS that supports all kernel features
needed for the PoC (seccomp-notify, eBPF, FUSE, network namespaces, ptrace).
Must work in both manual (developer SSH) and headless (agent/CI) modes.

## Decision: Lima with Virtualization.framework

**Why Lima over alternatives:**

| Requirement | Lima | OrbStack | Docker | UTM |
|---|---|---|---|---|
| seccomp-notify | YES | maybe | NO | YES |
| eBPF | YES | YES | partial | YES |
| Headless CLI | full | good | good | no create |
| Kernel control | full | locked | locked | full |
| Cost | free | paid | varies | free |

Lima provides a full Linux kernel via Apple's Virtualization.framework (VZ mode),
with virtiofs for fast host directory mounts and `limactl` for complete CLI automation.

## Setup

### Prerequisites

```bash
brew install lima
```

### Create & Start

```bash
make env-create    # Creates VM from lima/faultbox-dev.yaml and starts it
make env-verify    # Checks all kernel features are available
```

### Daily Usage

```bash
make env-start     # Start the VM
make env-shell     # Interactive shell
make env-exec CMD="go test ./..."  # Run a command headlessly
make env-stop      # Stop the VM
```

### Smoke Test

```bash
make env-exec CMD="bash poc/smoke-test.sh"
```

Note: `env-exec` automatically `cd`s into the project directory (`/host-home/git/Faultbox`)
inside the VM. The host home directory (`~`) is mounted at `/host-home` via virtiofs.

This builds and runs the target binary, then checks all kernel capabilities.

## What's Provisioned

The VM (Ubuntu 24.04 ARM64) includes:

- **Go 1.24.1** — matches go.mod
- **eBPF toolchain** — clang, llvm, libbpf-dev, bpftrace, bpfcc-tools
- **FUSE** — fuse3, libfuse3-dev
- **Network tools** — iproute2, iptables, tcpdump, nftables
- **seccomp** — libseccomp-dev
- **Debug tools** — strace, ltrace, htop

## File Layout

```
lima/
└── faultbox-dev.yaml    # VM template (committed to repo)
poc/
├── target/
│   └── main.go          # Test binary exercising FS, network, syscalls
└── smoke-test.sh        # Verifies environment capabilities
```

## Next Step

→ Step 1: Launch Go binary in isolated Linux namespaces (`poc/step-1-linux-launcher`)
