#!/usr/bin/env bash
set -euo pipefail

sudo install -o root -g root -m 0644 /tmp/sshai-guest.service /etc/systemd/system/sshai-guest.service
sudo systemctl daemon-reload
sudo systemctl enable sshai-guest.service
