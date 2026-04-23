# Running Faultbox in CI (Linux)

Faultbox uses Linux's seccomp-notify primitive. CI hosts that aren't
Linux (and Linux hosts with restrictive defaults) need a small recipe
to get fault-injection working. This page is that recipe — concrete
GitHub Actions and BuildKite templates plus the privilege requirements
each one satisfies.

> **Customer ask A7 + D3** from the inDrive feedback analysis. Not
> being able to run Faultbox in CI was a hard payment blocker
> (FB §6.1 #2). This page closes that gap; v0.9.10+ may add a
> first-class action wrapper but the manual recipe always works.

## What Faultbox needs from a Linux host

Three primitives have to be available in the kernel and accessible
to the test process:

| Capability | Why | Default on hosted runners |
|---|---|---|
| `seccomp(2)` syscall + `SECCOMP_FILTER_FLAG_NEW_LISTENER` | The interception mechanism | **Available** on Linux 5.6+, which covers Ubuntu 22.04 / 24.04 runners |
| `ptrace_scope` permits per-process attach | The shim attaches via Unix-socket fd passing (RFC-014) | `ptrace_scope=1` (parent-only) is fine; `0` (any) also works; `2` and `3` require a workaround |
| Docker daemon (only for container-mode tests) | Real Postgres/Redis/Kafka faults | Available on `ubuntu-*` GitHub runners; needs `dockerd` install on self-hosted |

You **do not** need root, you **do** need `CAP_SYS_PTRACE` for
container-mode tests (granted automatically when running as the
container's PID 1 in most CI setups).

### `ptrace_scope` quick reference

```sh
$ cat /proc/sys/kernel/yama/ptrace_scope
1   # parent-only — Faultbox works
0   # permissive  — Faultbox works
2   # admin-only — Faultbox needs CAP_SYS_PTRACE OR temp lower
3   # disabled    — needs reboot to lower (no override at runtime)
```

If you see `2` or `3`, either:

- Run the test process as root (containerised CI usually does this
  by default), or
- Lower the scope at the start of CI via:
  `echo 0 | sudo tee /proc/sys/kernel/yama/ptrace_scope`
  (requires `sudo` in the runner; documented as "elevated mode" by
  most CI providers).

## GitHub Actions template

Drop this in `.github/workflows/faultbox.yml`. Comments mark the
parts you'll customise per-project.

```yaml
name: Faultbox

on:
  push:
    branches: [main]
  pull_request:

jobs:
  faultbox-test:
    # Hosted ubuntu-latest runners satisfy all three primitives above
    # (kernel 6.x, ptrace_scope=1, dockerd available). Self-hosted
    # runners may need additional setup; see "Self-hosted" below.
    runs-on: ubuntu-latest

    steps:
      - uses: actions/checkout@v4

      - name: Set up Go
        uses: actions/setup-go@v5
        with:
          go-version: '1.26'  # match your project's go.mod
          cache: true

      - name: Install Faultbox
        # Pin a specific version — don't auto-track latest on a
        # paid CI minute. Update intentionally with a Renovate/
        # Dependabot config or scheduled "bump faultbox" PR.
        run: |
          curl -L https://github.com/faultbox/Faultbox/releases/download/release-0.9.9/faultbox-0.9.9-linux-amd64.tar.gz | tar xz
          sudo install faultbox /usr/local/bin/

      # Build whatever Go binaries your spec references via
      # service(binary=...). Skip if you only use container or mock
      # services.
      - name: Build SUT binaries
        run: go build -o ./bin/ ./...

      # Pre-pull container images so faultbox doesn't spend test
      # time on a cold pull. Skip if you only use binary/mock services.
      - name: Pre-pull container images
        run: |
          docker pull mysql:8.0.32
          docker pull redis:7-alpine

      - name: Run Faultbox tests
        run: |
          faultbox test faultbox.star --seed 1 --output traces/run.json

      # Upload the .fb bundle so failures are debuggable from the
      # PR. Bundle filename is run-<ts>-<seed>.fb in cwd.
      - name: Upload Faultbox bundle
        if: always()  # upload even on failure
        uses: actions/upload-artifact@v4
        with:
          name: faultbox-bundle
          path: run-*.fb
          retention-days: 7
```

### What this gives you

- **Reproducible runs** via the pinned `--seed 1` and the v0.9.7
  `.fb` bundle (every run drops one, see [bundles.md](../bundles.md)).
- **Failure artifacts** uploaded to the GitHub Actions run page —
  download the `.fb`, run `faultbox inspect run.fb` locally to triage.
- **Cache hit** on the Go module download via `cache: true` on
  `setup-go`.

### When to opt the bundle out

For purely-passing branches (post-merge `main` runs) you may want to
skip the bundle to save artifact storage. Add `--no-bundle` to the
`faultbox test` command — bundles are only really useful when
something failed.

## BuildKite template

```yaml
steps:
  - label: ":boom: Faultbox"
    agents:
      # Buildkite agents need a Linux host with kernel 5.6+ and
      # Docker installed. ptrace_scope is whatever the agent host
      # has — common managed agents (CodeBuild, EC2-with-Docker)
      # default to 1, which works.
      queue: linux-docker
    plugins:
      - docker#v5:
          image: golang:1.26
          # CAP_SYS_PTRACE so the shim can attach across a netns.
          # Required only for container-mode tests; binary mode
          # works without it.
          cap-add:
            - SYS_PTRACE
          security-opt:
            - seccomp=unconfined  # let our seccomp filter take over
          environment:
            - FAULTBOX_BUNDLE_DIR=$$BUILDKITE_BUILD_PATH/bundles
    commands:
      - |
        curl -L https://github.com/faultbox/Faultbox/releases/download/release-0.9.9/faultbox-0.9.9-linux-amd64.tar.gz | tar xz
        sudo install faultbox /usr/local/bin/
      - go build -o ./bin/ ./...
      - faultbox test faultbox.star --seed 1
    artifact_paths:
      - "bundles/*.fb"
```

The `seccomp=unconfined` directive **doesn't disable Faultbox's
filter** — it disables Docker's *default* filter so Faultbox's
custom one can be installed. This is a Docker-specific quirk;
similar flags exist for `containerd`, `nerdctl`, etc.

## Self-hosted runners

If GitHub-hosted or Buildkite-managed runners aren't an option,
provision your own. Required setup, in roughly the order the
runner needs it:

1. **Linux kernel 5.6+.** Check with `uname -r`. Ubuntu 22.04 and
   Debian 12 both ship with this. Anything older needs a HWE
   kernel.
2. **Docker daemon.** Standard `docker.io` package; rootless Docker
   works too as long as the runner can `docker run`.
3. **`ptrace_scope=0` or `=1`.** Persist with
   `/etc/sysctl.d/10-faultbox.conf` containing
   `kernel.yama.ptrace_scope = 1`.
4. **Faultbox binary** in `/usr/local/bin/` (or anywhere on `PATH`).

A minimal `cloud-init` snippet for AWS/GCP self-hosted runners:

```yaml
#cloud-config
packages:
  - docker.io
  - golang-1.26-go
write_files:
  - path: /etc/sysctl.d/10-faultbox.conf
    content: |
      kernel.yama.ptrace_scope = 1
runcmd:
  - sysctl --system
  - systemctl enable --now docker
  - curl -L https://github.com/faultbox/Faultbox/releases/download/release-0.9.9/faultbox-0.9.9-linux-amd64.tar.gz | tar -xz -C /usr/local/bin/
```

## Container-in-container (Docker-in-Docker)

If your CI itself runs inside a container (most managed CI does),
container-mode Faultbox tests need either:

1. **Mount the host Docker socket** into the runner container —
   easiest, what `docker:dind` and most "Docker workflow" base
   images already do. Check by running `docker info` in your CI.
2. **Real DinD** with privileged mode — works but slower because
   you're running a full daemon. Avoid unless option 1 isn't
   available.

For GitHub Actions specifically, `services:` containers connect to
the runner's Docker socket automatically — the template above
"just works" for both binary-mode and container-mode tests.

## Performance expectations

Rough timings on `ubuntu-latest` (4-core hosted runner):

| Workload | First run | Cached run |
|---|---|---|
| 50-test mock-only suite | ~5 s | ~5 s |
| 10-test suite with one container service | ~25 s | ~25 s (no caching today) |
| 100-test fault_matrix with two containers | ~70 s | ~70 s |

Container startup dominates. The v0.9.x roadmap doesn't include
container reuse across CI jobs, but `reuse=True` within a single
job already works.

## When Faultbox refuses to start

A handful of failure modes you'll see on day one of a CI integration:

| Error | Cause | Fix |
|---|---|---|
| `seccomp_listen: operation not permitted` | `ptrace_scope=2` or `=3`, no `CAP_SYS_PTRACE` | Run as root or lower `ptrace_scope` (see top of page) |
| `seccomp filter installation failed: function not implemented` | Kernel < 5.6 | Upgrade kernel or use Faultbox's mock-only mode (no seccomp needed) |
| `cannot connect to Docker daemon` | dockerd not running, or socket not accessible | Start dockerd, or mount `/var/run/docker.sock` into the runner |
| `bundle write: permission denied` | Workdir not writable by the runner user | Set `$FAULTBOX_BUNDLE_DIR` to a writable path |

## See also

- [Bundle format](../bundles.md) — what the CI uploads as an artifact.
- [Troubleshooting playbook](../troubleshooting.md) — covers
  development-time debugging too.
- [GitHub issue tracker](https://github.com/faultbox/Faultbox/issues)
  — open an issue with your CI provider name if you hit something
  not in this recipe.
