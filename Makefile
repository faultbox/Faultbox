.PHONY: build test clean lint fmt vet run \
       env-create env-start env-stop env-destroy env-shell env-exec env-status env-verify

APP_NAME := faultbox
BIN_DIR  := bin
LIMA_VM  := faultbox-dev
LIMA_CFG := lima/faultbox-dev.yaml
# Project path inside VM (~ is mounted at /host-home via lima/faultbox-dev.yaml)
VM_PROJECT := /host-home/git/Faultbox

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
