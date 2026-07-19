#!/usr/bin/env bash
set -euo pipefail

: "${SOURCE_REVISION:?SOURCE_REVISION is required}"

# Re-run the cheap static/runtime checks after the reboot/reconnect gate.
sudo systemctl is-active --quiet ssh.service
sudo systemctl is-active --quiet docker.service

# Produce a deterministic inventory as the initial SBOM artifact. A formal
# SPDX/CycloneDX SBOM and vulnerability-policy scan remain credentialed-pipeline
# gates because no scanner or severity policy is ratified yet.
sudo install -d -m 0755 /opt/sshai/build
{
  printf 'source-revision\t%s\n' "$SOURCE_REVISION"
  dpkg-query -W -f='deb\t${binary:Package}\t${Version}\n' | LC_ALL=C sort
  npm list --global --depth=0 --json
} | sudo tee /opt/sshai/build/software-inventory.txt >/dev/null
sudo chmod 0444 /opt/sshai/build/software-inventory.txt
