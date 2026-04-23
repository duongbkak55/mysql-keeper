#!/usr/bin/env bash
# install-prereqs.sh — install the tools needed to run Tier C (e2e/kind) and
# the light Tier D drills locally on a Linux host. Idempotent: re-running
# after a partial install is safe.
#
# Expected environment: Ubuntu / Debian. On Windows, run this inside WSL2
# (e.g. `wsl -d Ubuntu -e bash scripts/local/install-prereqs.sh`).
set -euo pipefail

log() { printf "\033[1;34m>>\033[0m %s\n" "$*"; }

# --- Docker ---
if ! command -v docker >/dev/null 2>&1; then
  log "Installing Docker Engine"
  curl -fsSL https://get.docker.com | sh
  sudo usermod -aG docker "$USER"
  log "Added $USER to docker group. Log out/in (or run 'newgrp docker') for group change to take effect."
else
  log "Docker already installed: $(docker --version)"
fi

if ! docker info >/dev/null 2>&1; then
  log "Docker daemon not reachable. On WSL, try: sudo service docker start"
  log "On systemd hosts: sudo systemctl enable --now docker"
  exit 1
fi

# --- kubectl ---
if ! command -v kubectl >/dev/null 2>&1; then
  log "Installing kubectl"
  KUBE_VER=$(curl -sL https://dl.k8s.io/release/stable.txt)
  curl -fsSL "https://dl.k8s.io/release/${KUBE_VER}/bin/linux/amd64/kubectl" -o /tmp/kubectl
  sudo install -m 0755 /tmp/kubectl /usr/local/bin/kubectl
  rm /tmp/kubectl
else
  log "kubectl already installed: $(kubectl version --client --output=yaml | grep gitVersion | head -1)"
fi

# --- kind ---
if ! command -v kind >/dev/null 2>&1; then
  log "Installing kind"
  curl -fsSL https://kind.sigs.k8s.io/dl/v0.23.0/kind-linux-amd64 -o /tmp/kind
  sudo install -m 0755 /tmp/kind /usr/local/bin/kind
  rm /tmp/kind
else
  log "kind already installed: $(kind version)"
fi

# --- go ---
if ! command -v go >/dev/null 2>&1; then
  log "Installing Go 1.25 (required by go.mod)"
  GO_VER="1.25.0"
  curl -fsSL "https://go.dev/dl/go${GO_VER}.linux-amd64.tar.gz" -o /tmp/go.tar.gz
  sudo rm -rf /usr/local/go
  sudo tar -C /usr/local -xzf /tmp/go.tar.gz
  rm /tmp/go.tar.gz
  if ! grep -q '/usr/local/go/bin' "$HOME/.bashrc"; then
    echo 'export PATH=$PATH:/usr/local/go/bin' >> "$HOME/.bashrc"
  fi
  export PATH=$PATH:/usr/local/go/bin
  log "Go installed: $(/usr/local/go/bin/go version). Run 'source ~/.bashrc' in new shells."
else
  go_ver=$(go env GOVERSION)
  log "Go already installed: $go_ver"
fi

log "Done. Verify:"
log "  docker ps"
log "  kind version"
log "  kubectl version --client"
log "  go version"
