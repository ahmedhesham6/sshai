#!/usr/bin/env bash
set -euo pipefail

: "${CLAUDE_CODE_VERSION:?CLAUDE_CODE_VERSION is required}"
: "${CODEX_VERSION:?CODEX_VERSION is required}"
: "${OPENCODE_VERSION:?OPENCODE_VERSION is required}"

os_version=$(sed -n 's/^VERSION_ID="\{0,1\}\([^"[:space:]]*\)"\{0,1\}$/\1/p' /etc/os-release)
test "$os_version" = "24.04"
test "$(dpkg --print-architecture)" = "amd64"

npm list --global --depth=0 "@anthropic-ai/claude-code@${CLAUDE_CODE_VERSION}"
npm list --global --depth=0 "@openai/codex@${CODEX_VERSION}"
npm list --global --depth=0 "opencode-ai@${OPENCODE_VERSION}"
export DISABLE_AUTOUPDATER=1
export OPENCODE_DISABLE_AUTOUPDATE=1
claude --version | grep -F "$CLAUDE_CODE_VERSION"
codex --version | grep -F "$CODEX_VERSION"
opencode --version | grep -F "$OPENCODE_VERSION"

sshd_config=$(sudo sshd -T)
grep -qx 'permitrootlogin no' <<<"$sshd_config"
grep -qx 'passwordauthentication no' <<<"$sshd_config"
grep -qx 'kbdinteractiveauthentication no' <<<"$sshd_config"
grep -qx 'x11forwarding no' <<<"$sshd_config"

sudo systemctl enable --now docker.service
sudo docker info >/dev/null
sudo docker compose version

smoke_dir=$(mktemp -d)
trap 'rm -rf "$smoke_dir"' EXIT
tar -C /bin -cf "$smoke_dir/rootfs.tar" busybox
image_id=$(sudo docker import "$smoke_dir/rootfs.tar")
sudo docker run --rm "$image_id" /busybox true
sudo docker image rm "$image_id" >/dev/null

test "$(sed -n 's/^prefix=//p' /etc/skel/.npmrc)" = "/home/dev/.local/npm"
grep -qx 'export PIPX_HOME=/home/dev/.local/share/pipx' /etc/profile.d/sshai-home-first.sh
grep -qx 'export CARGO_HOME=/home/dev/.local/share/cargo' /etc/profile.d/sshai-home-first.sh
grep -qx 'export RUSTUP_HOME=/home/dev/.local/share/rustup' /etc/profile.d/sshai-home-first.sh
grep -qx 'export GOPATH=/home/dev/.local/share/go' /etc/profile.d/sshai-home-first.sh
test "$(GOPATH=/home/dev/.local/share/go GOBIN=/home/dev/.local/bin go env GOPATH)" = "/home/dev/.local/share/go"
test "$(GOPATH=/home/dev/.local/share/go GOBIN=/home/dev/.local/bin go env GOBIN)" = "/home/dev/.local/bin"
