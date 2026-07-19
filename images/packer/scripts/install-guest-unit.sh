#!/usr/bin/env bash
set -euo pipefail

: "${VALIDATE_ONLY:=false}"

if [[ ! -f /tmp/sshai-guest ]]; then
  if [[ "$VALIDATE_ONLY" == "true" ]]; then
    exit 0
  fi
  printf 'guest supervisor source is absent\n' >&2
  exit 1
fi

sudo install -o root -g root -m 0755 /tmp/sshai-guest /usr/local/bin/sshai-guest
sudo install -o root -g root -m 0644 /tmp/sshai-guest.service /etc/systemd/system/sshai-guest.service
sudo systemctl daemon-reload
sudo systemctl enable sshai-guest.service
