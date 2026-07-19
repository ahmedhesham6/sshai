#!/usr/bin/env bash
set -euo pipefail

sudo tee /etc/ssh/sshd_config.d/60-sshai-hardening.conf >/dev/null <<'EOF'
PermitRootLogin no
PasswordAuthentication no
KbdInteractiveAuthentication no
PermitEmptyPasswords no
PubkeyAuthentication yes
X11Forwarding no
AllowTcpForwarding no
EOF
sudo chmod 0644 /etc/ssh/sshd_config.d/60-sshai-hardening.conf
sudo sshd -t
