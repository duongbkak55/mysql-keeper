#!/usr/bin/env bash
# install-prereqs.sh — install tools for Tier C (e2e/kind) locally. Installs
# without sudo when possible by placing binaries under $HOME/.local/bin and
# Go under $HOME/go. Docker is expected to be pre-installed (use
# Docker Desktop with WSL integration, or install Docker Engine via
# https://get.docker.com with sudo once — after that, the rest of this
# script needs no elevation).
#
# Usage (Ubuntu/WSL2 Ubuntu):
#   bash scripts/local/install-prereqs.sh
set -euo pipefail

log() { printf "\033[1;34m>>\033[0m %s\n" "$*"; }

BINDIR="${HOME}/.local/bin"
GODIR="${HOME}/go-toolchain"
mkdir -p "${BINDIR}"

# --- Docker (check-only) ---
if ! command -v docker >/dev/null 2>&1; then
  log "Docker is not installed. On Ubuntu:"
  log "  curl -fsSL https://get.docker.com | sudo sh"
  log "  sudo usermod -aG docker \$USER && newgrp docker"
  log "On Windows: install Docker Desktop + enable WSL integration."
  exit 1
fi
if ! docker info >/dev/null 2>&1; then
  log "Docker daemon unreachable. On WSL: sudo service docker start"
  log "Or enable Docker Desktop's WSL integration."
  exit 1
fi
log "Docker OK: $(docker --version)"

# --- kubectl ---
if [[ -x "${BINDIR}/kubectl" ]] || command -v kubectl >/dev/null 2>&1; then
  log "kubectl already on PATH"
else
  log "Installing kubectl into ${BINDIR}"
  KUBE_VER=$(curl -sL https://dl.k8s.io/release/stable.txt)
  curl -fsSL "https://dl.k8s.io/release/${KUBE_VER}/bin/linux/amd64/kubectl" -o "${BINDIR}/kubectl"
  chmod +x "${BINDIR}/kubectl"
fi

# --- kind ---
if [[ -x "${BINDIR}/kind" ]] || command -v kind >/dev/null 2>&1; then
  log "kind already on PATH"
else
  log "Installing kind into ${BINDIR}"
  curl -fsSL https://kind.sigs.k8s.io/dl/v0.23.0/kind-linux-amd64 -o "${BINDIR}/kind"
  chmod +x "${BINDIR}/kind"
fi

# --- Go ---
if command -v go >/dev/null 2>&1 && go version | grep -Eq 'go1\.(2[5-9]|[3-9])'; then
  log "Go already on PATH: $(go version)"
else
  log "Installing Go 1.25 into ${GODIR}"
  GO_VER="1.25.0"
  rm -rf "${GODIR}"
  mkdir -p "${GODIR}"
  curl -fsSL "https://go.dev/dl/go${GO_VER}.linux-amd64.tar.gz" | tar -C "${GODIR}" -xz
fi

# --- PATH export ---
PATH_FRAGMENT="export PATH=\"\${HOME}/.local/bin:\${HOME}/go-toolchain/go/bin:\${HOME}/go/bin:\${PATH}\""
if ! grep -qF "${PATH_FRAGMENT}" "${HOME}/.bashrc" 2>/dev/null; then
  echo "" >> "${HOME}/.bashrc"
  echo "# Added by mysql-keeper install-prereqs.sh" >> "${HOME}/.bashrc"
  echo "${PATH_FRAGMENT}" >> "${HOME}/.bashrc"
  log "Appended PATH export to ${HOME}/.bashrc — reload with 'source ~/.bashrc' or reopen the shell."
fi

# Also set for the remainder of THIS script's environment so the verify
# commands below work immediately.
export PATH="${HOME}/.local/bin:${HOME}/go-toolchain/go/bin:${HOME}/go/bin:${PATH}"

log "Done. Versions:"
docker --version
kubectl version --client --output=yaml 2>/dev/null | grep gitVersion | head -1 || true
kind version
go version

log ""
log "Reload your shell ('source ~/.bashrc' or new terminal) so PATH sticks,"
log "then run: bash scripts/e2e-setup.sh && go test -tags=e2e -v ./test/e2e/..."
