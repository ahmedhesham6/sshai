#!/usr/bin/env bash
set -euo pipefail

: "${CLAUDE_CODE_VERSION:?CLAUDE_CODE_VERSION is required}"
: "${CODEX_VERSION:?CODEX_VERSION is required}"
: "${OPENCODE_VERSION:?OPENCODE_VERSION is required}"

sudo apt-get update
sudo DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends \
  busybox-static \
  ca-certificates \
  curl \
  docker-compose-v2 \
  docker.io \
  git \
  golang-go \
  jq \
  nodejs \
  npm \
  pipx \
  python3-pip \
  shellcheck \
  unzip
sudo rm -rf /var/lib/apt/lists/*

# Agent package names and versions are exact. npm verifies each registry
# tarball against its published integrity digest during installation.
sudo env NPM_CONFIG_PREFIX=/usr/local npm install --global --ignore-scripts=false \
  "@anthropic-ai/claude-code@${CLAUDE_CODE_VERSION}" \
  "@openai/codex@${CODEX_VERSION}" \
  "opencode-ai@${OPENCODE_VERSION}"
sudo npm config --global set update-notifier false

# Record the exact pins in a root-owned manifest consumed by the guest's
# runtime validation. The validator still executes each binary with --version;
# this file is the expected side of that comparison.
sudo install -d -o root -g root -m 0755 /etc/sshai
printf '%s\t%s\t%s\n' \
  claude /usr/local/bin/claude "$CLAUDE_CODE_VERSION" \
  codex /usr/local/bin/codex "$CODEX_VERSION" \
  opencode /usr/local/bin/opencode "$OPENCODE_VERSION" \
  | sudo tee /etc/sshai/agent-versions >/dev/null
sudo chmod 0644 /etc/sshai/agent-versions

# Claude Code and OpenCode support explicit updater disable switches. Codex's
# system npm package does not self-update; the immutable AMI owns its version.
sudo tee /etc/profile.d/sshai-agent-updates.sh >/dev/null <<'EOF'
export DISABLE_AUTOUPDATER=1
export OPENCODE_DISABLE_AUTOUPDATE=1
EOF
sudo chmod 0644 /etc/profile.d/sshai-agent-updates.sh

sudo tee -a /etc/environment >/dev/null <<'EOF'
DISABLE_AUTOUPDATER=1
OPENCODE_DISABLE_AUTOUPDATE=1
EOF
