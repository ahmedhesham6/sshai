#!/usr/bin/env bash
set -euo pipefail

# ADR 0013 makes /home/dev a durable mount. These defaults keep user-installed
# tools off the replaceable system volume after that mount is attached.
sudo tee /etc/profile.d/sshai-home-first.sh >/dev/null <<'EOF'
export NPM_CONFIG_PREFIX=/home/dev/.local/npm
export PIPX_HOME=/home/dev/.local/share/pipx
export PIPX_BIN_DIR=/home/dev/.local/bin
export CARGO_HOME=/home/dev/.local/share/cargo
export RUSTUP_HOME=/home/dev/.local/share/rustup
export GOPATH=/home/dev/.local/share/go
export GOBIN=/home/dev/.local/bin
export PATH=/home/dev/.local/bin:/home/dev/.local/npm/bin:/home/dev/.local/share/cargo/bin:$PATH
EOF
sudo chmod 0644 /etc/profile.d/sshai-home-first.sh

sudo install -d -m 0755 /etc/skel/.config/go
sudo tee /etc/skel/.npmrc >/dev/null <<'EOF'
prefix=/home/dev/.local/npm
update-notifier=false
EOF
sudo tee /etc/skel/.config/go/env >/dev/null <<'EOF'
GOPATH=/home/dev/.local/share/go
GOBIN=/home/dev/.local/bin
EOF
sudo chmod 0644 /etc/skel/.npmrc /etc/skel/.config/go/env
