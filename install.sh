#!/bin/sh
# Faultbox installer
# Usage: curl -fsSL https://faultbox.io/install.sh | sh
#
# Environment variables:
#   FAULTBOX_VERSION  - specific version (default: latest)
#   FAULTBOX_DIR      - install directory (default: ~/.faultbox/bin)

set -e

REPO="faultbox/faultbox"
INSTALL_DIR="${FAULTBOX_DIR:-$HOME/.faultbox/bin}"

# --- Helpers ---

say() {
  printf "  %s\n" "$@"
}

err() {
  printf "error: %s\n" "$@" >&2
  exit 1
}

need() {
  if ! command -v "$1" > /dev/null 2>&1; then
    err "need '$1' (command not found)"
  fi
}

# --- Detect platform ---

detect_platform() {
  local os arch

  os=$(uname -s | tr '[:upper:]' '[:lower:]')
  case "$os" in
    linux)  os="linux" ;;
    darwin) os="darwin" ;;
    *)      err "unsupported OS: $os" ;;
  esac

  arch=$(uname -m)
  case "$arch" in
    x86_64|amd64)   arch="amd64" ;;
    aarch64|arm64)   arch="arm64" ;;
    *)               err "unsupported architecture: $arch" ;;
  esac

  echo "${os}-${arch}"
}

# --- Get latest version ---

get_latest_version() {
  need curl
  local url="https://api.github.com/repos/${REPO}/releases/latest"
  local tag
  tag=$(curl -fsSL "$url" | grep '"tag_name"' | head -1 | sed 's/.*"tag_name": *"//;s/".*//')
  if [ -z "$tag" ]; then
    err "could not determine latest version"
  fi
  # tag is "release-X.Y.Z", extract version
  echo "${tag#release-}"
}

# --- Main ---

main() {
  need curl
  need tar

  local platform version archive url checksum_url

  platform=$(detect_platform)
  say "detected platform: ${platform}"

  if [ -n "$FAULTBOX_VERSION" ]; then
    version="$FAULTBOX_VERSION"
  else
    say "fetching latest version..."
    version=$(get_latest_version)
  fi
  say "version: ${version}"

  archive="faultbox-${version}-${platform}.tar.gz"
  url="https://github.com/${REPO}/releases/download/release-${version}/${archive}"
  checksum_url="${url}.sha256"

  # Create install directory
  mkdir -p "$INSTALL_DIR"

  # Download
  local tmpdir
  tmpdir=$(mktemp -d)
  trap 'rm -rf "$tmpdir"' EXIT

  say "downloading ${archive}..."
  curl -fsSL -o "${tmpdir}/${archive}" "$url" || err "download failed — check that version ${version} exists"

  # Verify checksum
  say "verifying checksum..."
  curl -fsSL -o "${tmpdir}/checksum" "$checksum_url" || err "checksum download failed"
  local expected actual
  expected=$(awk '{print $1}' "${tmpdir}/checksum")
  if command -v sha256sum > /dev/null 2>&1; then
    actual=$(sha256sum "${tmpdir}/${archive}" | awk '{print $1}')
  else
    actual=$(shasum -a 256 "${tmpdir}/${archive}" | awk '{print $1}')
  fi
  if [ "$expected" != "$actual" ]; then
    err "checksum mismatch: expected ${expected}, got ${actual}"
  fi
  say "checksum ok"

  # Extract
  say "installing to ${INSTALL_DIR}..."
  tar xzf "${tmpdir}/${archive}" -C "$INSTALL_DIR"
  chmod +x "${INSTALL_DIR}/faultbox"
  if [ -f "${INSTALL_DIR}/faultbox-shim" ]; then
    chmod +x "${INSTALL_DIR}/faultbox-shim"
  fi

  # Verify
  local installed_version
  installed_version=$("${INSTALL_DIR}/faultbox" --version 2>/dev/null || echo "unknown")
  say "installed: ${installed_version}"

  # PATH instructions
  echo
  if echo "$PATH" | tr ':' '\n' | grep -qx "$INSTALL_DIR"; then
    say "faultbox is ready! Run: faultbox --help"
  else
    say "add faultbox to your PATH:"
    echo
    case "$SHELL" in
      */zsh)
        say "  echo 'export PATH=\"${INSTALL_DIR}:\$PATH\"' >> ~/.zshrc"
        say "  source ~/.zshrc"
        ;;
      */fish)
        say "  fish_add_path ${INSTALL_DIR}"
        ;;
      *)
        say "  echo 'export PATH=\"${INSTALL_DIR}:\$PATH\"' >> ~/.bashrc"
        say "  source ~/.bashrc"
        ;;
    esac
    echo
    say "then: faultbox --help"
  fi

  # Platform note
  if [ "$(uname -s)" = "Darwin" ]; then
    echo
    say "note: faultbox uses Linux seccomp-notify."
    say "on macOS, run inside a Lima VM:"
    say "  brew install lima"
    say "  see: https://faultbox.io/docs/tutorial/00-prelude/00-setup"
  fi
}

main
