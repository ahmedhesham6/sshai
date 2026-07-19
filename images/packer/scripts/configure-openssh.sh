#!/usr/bin/env bash
set -euo pipefail

# OpenSSH uses the first obtained value, so load the platform policy before
# cloud-image drop-ins that may otherwise retain a weaker setting.
sudo tee /etc/ssh/sshd_config.d/00-sshai-hardening.conf >/dev/null <<'EOF'
PermitRootLogin no
PasswordAuthentication no
KbdInteractiveAuthentication no
PermitEmptyPasswords no
PubkeyAuthentication yes
X11Forwarding no
AllowTcpForwarding yes
EOF
sudo chmod 0644 /etc/ssh/sshd_config.d/00-sshai-hardening.conf
sudo sshd -t
