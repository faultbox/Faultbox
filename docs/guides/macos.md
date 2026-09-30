# Run Faultbox on macOS

The macOS CLI can inspect bundles, render reports and edit/check specs. Native
service execution uses Linux. Use the supported Lima runtime profile for local
execution; use a Linux runner directly in CI. No Go installation is required to
run a prebuilt service.

## Install the CLI and start the runner

Install Lima with `brew install lima`. Install Faultbox on the host:

```sh
curl -fsSL https://faultbox.io/install.sh -o /tmp/faultbox-install.sh
sh /tmp/faultbox-install.sh
faultbox --version
```

Download `lima/faultbox.yaml` from the **same release tag** as the host CLI, then
create the VM. From a source checkout, use the checked-in file:

```sh
limactl start --name=faultbox lima/faultbox.yaml
```

The profile supports Apple Silicon and Intel Macs, installs Docker and basic
runtime tools, and mounts the host home at `/host-home` with write access. Adjust
the mount in the YAML before creating the VM if your team requires a narrower
project directory. CPU, memory and disk settings are also explicit in that file.
It does not install Go, cgo toolchains, runsc or language SDKs.

Install the **same Faultbox version** in the VM. Replace `X.Y.Z` with the host's
release version; install both Faultbox and the shim using the official installer:

```sh
limactl shell --workdir /tmp faultbox sh -c 'curl -fsSL https://faultbox.io/install.sh -o /tmp/faultbox-install.sh'
limactl shell --workdir /tmp faultbox sudo env FAULTBOX_VERSION=X.Y.Z FAULTBOX_DIR=/usr/local/bin sh /tmp/faultbox-install.sh
limactl shell --workdir /tmp faultbox faultbox --version
```

For a checkout containing an unreleased `doctor`, build host and guest binaries
from that same checkout with the same explicit version label. An older guest
without `doctor` is reported as incompatible, not silently considered healthy.

## Check the environment

```sh
faultbox doctor --lima=faultbox
limactl shell --workdir /tmp faultbox sudo faultbox doctor --docker
```

`doctor` is read-only: it does not create a VM, install software, start Docker,
modify permissions or restart daemons. Its exit codes are 0 (requested checks
pass, possibly with warnings), 1 (invalid command), and 2 (failed prerequisite).
`--format=json` exposes stable check codes and remediation text for CI/agents.
Each subprocess and the complete diagnostic run have deadlines.

The host-to-Lima check uses the normal guest user. Docker access may legitimately
fail there when the daemon is root-only. Check it under the same user that will
run your specs, as in the explicit `sudo` command above; do not treat a root check
as proof that an unprivileged CI user has the same access.

| Mode | Check | What it establishes |
|---|---|---|
| Native binary services | `faultbox doctor` on Linux | Kernel exposes seccomp notification; platform/version/path are visible. Permissions for a real launch still require a small representative run. |
| Docker services | `faultbox doctor --docker` | Linux Docker daemon is reachable and an executable shim is alongside Faultbox. Clean source revisions and shim architecture are compared when available. |
| Packet faults | `faultbox doctor --packet` | TUN device exists and the current process has CAP_NET_ADMIN. |
| Filesystem observation | `faultbox doctor --trace` | Docker/shim, runsc and trace host registration checks. No daemon changes. |
| Host/VM pairing | `faultbox doctor --lima=faultbox` | Host and guest version labels agree; guest diagnostics are reported. |

Passing prerequisite checks is not a guarantee that every sandbox/fault mode is
permitted. The report retains that distinction. Packet faults and filesystem
observation have different prerequisites; see [gVisor requirements](../gvisor-requirements.md).

## Run a project

Host paths under your home are available beneath `/host-home`. For a project at
`~/git/my-service`, with prebuilt Linux binaries matching the VM architecture:

```sh
limactl shell --workdir /host-home/git/my-service faultbox sudo faultbox test faultbox.star
```

Use `sudo` only when required by your execution mode and local Docker access.
Faultbox reports missing capabilities instead of silently installing them.

If a service needs cgo or another native toolchain, install that toolchain in a
project build image or your own development VM profile. Keep service compilation
separate from Faultbox runtime provisioning. The existing `faultbox-dev.yaml`
profile and Makefile `env-*` commands are intended for developing Faultbox itself.

After upgrading the host and guest with the official installer or `self-update`,
run `doctor` again. It reports mismatched versions and paths rather than updating
either environment automatically.
