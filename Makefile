.PHONY: build test clean lint fmt vet run \
       demo demo-build demo-container \
       testops-prep install-lima \
       env-create env-start env-stop env-destroy env-shell env-exec env-status env-verify

APP_NAME := faultbox
BIN_DIR  := bin
LIMA_VM  := faultbox-dev
LIMA_CFG := lima/faultbox-dev.yaml
# Project path inside VM (~ is mounted at /host-home via lima/faultbox-dev.yaml).
# Must match the real checkout path, case included — the Linux guest mount is
# case-sensitive even though the macOS host isn't.
VM_PROJECT := /host-home/git/faultbox/faultbox

# ─── Build & Test (host) ───────────────────────────────────────────

build:
	go build -o $(BIN_DIR)/$(APP_NAME) ./cmd/faultbox

run: build
	./$(BIN_DIR)/$(APP_NAME)

test:
	go test ./...

fmt:
	go fmt ./...

vet:
	go vet ./...

lint: fmt vet

clean:
	rm -rf $(BIN_DIR)

# ─── Demo (cross-compile + run in Lima) ───────────────────────────

LINUX_BIN := $(BIN_DIR)/linux-arm64

demo-build:
	@echo "Cross-compiling for linux/arm64..."
	GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -o $(LINUX_BIN)/faultbox       ./cmd/faultbox/
	GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -o $(LINUX_BIN)/faultbox-shim  ./cmd/faultbox-shim/
	GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -o $(LINUX_BIN)/inventory-svc  ./poc/demo/inventory-svc/
	GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -o $(LINUX_BIN)/order-svc      ./poc/demo/order-svc/
	GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -o $(LINUX_BIN)/mock-db        ./poc/mock-db/
	GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -o $(LINUX_BIN)/mock-api       ./poc/mock-api/
	GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -o $(LINUX_BIN)/target         ./poc/target/
	cd poc/demo-container/api && GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -o ../../../$(LINUX_BIN)/api-svc .
	@echo "Binaries: $(LINUX_BIN)/"

demo: demo-build
	limactl shell --workdir $(VM_PROJECT) $(LIMA_VM) -- bash poc/demo/run-demo.sh

demo-container: demo-build
	limactl shell --workdir $(VM_PROJECT) $(LIMA_VM) -- bash poc/demo-container/run-demo.sh

# ─── testops: prep binaries for poc_* corpus cases ─────────────────
#
# Builds the service binaries the poc_example / poc_demo Starlark specs
# reference at hardcoded /tmp paths, native to the host arch. Intended
# for CI and for Lima runs of `go test ./testops/...`. Cross-compilation
# (GOOS/GOARCH) is intentionally absent — whoever runs the testops
# harness is the target OS, and binaries must match.
testops-prep:
	@echo "Building native binaries into /tmp/ for testops poc cases..."
	CGO_ENABLED=0 go build -o /tmp/mock-db        ./poc/mock-db/
	CGO_ENABLED=0 go build -o /tmp/mock-api       ./poc/mock-api/
	CGO_ENABLED=0 go build -o /tmp/inventory-svc  ./poc/demo/inventory-svc/
	CGO_ENABLED=0 go build -o /tmp/order-svc      ./poc/demo/order-svc/
	@# RFC-040 leak harness drives the determinism_* corpus cases.
	@# Linux-only — uses raw clock_gettime / getrandom syscalls via
	@# golang.org/x/sys/unix, which doesn't compile on darwin.
	@if [ "$$(uname -s)" = "Linux" ]; then \
		CGO_ENABLED=0 go build -o /tmp/faultbox-leaker ./poc/leaker/ ; \
		echo "Built /tmp/faultbox-leaker (linux)" ; \
	else \
		echo "Skipping /tmp/faultbox-leaker: not Linux ($$(uname -s)) — RFC-040 goldens require Lima/Linux" ; \
	fi
	@# faultbox-shim is linux-only (seccomp-notify); skip on other hosts.
	@if [ "$$(uname -s)" = "Linux" ]; then \
		CGO_ENABLED=0 go build -o /tmp/faultbox-shim ./cmd/faultbox-shim/ ; \
		echo "Built /tmp/faultbox-shim (linux)" ; \
	else \
		echo "Skipping faultbox-shim: not Linux ($$(uname -s)) — Docker container mode requires Lima/Linux" ; \
	fi

# ─── Install from source into the Lima VM ──────────────────────────
#
# Container mode needs BOTH `faultbox` and `faultbox-shim`, built
# linux-native and living in the same directory: the shim is the
# container entrypoint (bind-mounted in), and the runtime's
# findShimPath() falls back to alongside-the-binary. `make build`
# produces only the host `faultbox`, so a from-source Lima run used to
# mean hand-building the shim and copying it in by hand — the F-4
# discovery loop from the v0.13.0 eval. This target does both in one
# step, installing into the VM's /usr/local/bin (on PATH).
install-lima:
	@echo "Cross-compiling faultbox + faultbox-shim for linux/arm64..."
	GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -o $(LINUX_BIN)/faultbox      ./cmd/faultbox/
	GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -o $(LINUX_BIN)/faultbox-shim ./cmd/faultbox-shim/
	@echo "Installing into $(LIMA_VM):/usr/local/bin (sudo)..."
	limactl shell --workdir $(VM_PROJECT) $(LIMA_VM) -- sudo install -m 0755 $(LINUX_BIN)/faultbox      /usr/local/bin/faultbox
	limactl shell --workdir $(VM_PROJECT) $(LIMA_VM) -- sudo install -m 0755 $(LINUX_BIN)/faultbox-shim /usr/local/bin/faultbox-shim
	@echo "Installed. Verify with: make env-exec CMD='faultbox version'"

# ─── Linux Dev Environment (Lima) ──────────────────────────────────

env-create:
	@echo "Creating Lima VM '$(LIMA_VM)'..."
	limactl create --name=$(LIMA_VM) $(LIMA_CFG) --tty=false
	limactl start $(LIMA_VM)
	@echo "VM created and started. Run 'make env-verify' to check."

env-start:
	limactl start $(LIMA_VM)

env-stop:
	limactl stop $(LIMA_VM)

env-destroy:
	limactl delete $(LIMA_VM) --force

env-shell:
	limactl shell $(LIMA_VM)

# Run a command inside the VM. Usage: make env-exec CMD="uname -a"
env-exec:
	limactl shell --workdir $(VM_PROJECT) $(LIMA_VM) -- $(CMD)

env-status:
	@limactl list $(LIMA_VM) 2>/dev/null || echo "VM '$(LIMA_VM)' does not exist. Run 'make env-create'."

# Verify all required kernel features are available
env-verify:
	limactl shell --workdir $(VM_PROJECT) $(LIMA_VM) -- bash -c '\
		set -e; \
		echo "=== Faultbox Dev Environment Verification ==="; \
		echo; \
		echo "1. Kernel: $$(uname -r)"; \
		echo "2. Go: $$(/usr/local/go/bin/go version)"; \
		echo "3. Clang: $$(clang --version | head -1)"; \
		echo; \
		echo "--- Kernel Features ---"; \
		echo -n "4. seccomp: "; (grep -q CONFIG_SECCOMP_FILTER=y /boot/config-$$(uname -r) 2>/dev/null || zgrep -q CONFIG_SECCOMP_FILTER=y /proc/config.gz 2>/dev/null) && echo OK || echo MISSING; \
		echo -n "5. eBPF: "; sudo bpftool prog list >/dev/null 2>&1 && echo OK || echo MISSING; \
		echo -n "6. FUSE: "; test -e /dev/fuse && echo OK || echo MISSING; \
		echo -n "7. Network NS: "; sudo ip netns add _faultbox_test 2>/dev/null && sudo ip netns delete _faultbox_test && echo OK || echo MISSING; \
		echo -n "8. tc/netem: "; sudo tc qdisc show >/dev/null 2>&1 && echo OK || echo MISSING; \
		echo -n "9. ptrace: "; strace -e trace=write echo test >/dev/null 2>&1 && echo OK || echo MISSING; \
		echo; \
		echo "=== All checks passed ===" \
	'
