# Use private regional data-plane cells behind an SSH proxy

Developer Runtimes have private addresses only and are accessed through a regional authenticated WebSocket proxy. Each enabled region contains its own VPC, egress, proxy, compute, and storage resources while the control plane remains global. This avoids public SSH exposure and per-environment public IPv4 resources while preserving standard OpenSSH through `ProxyCommand`.
