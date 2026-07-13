# Open implementation decisions

These items were not accepted during the design session and must not be silently treated as locked.

## Product policy

1. **Zero Credit Balance** — whether to block starts, stop active compute after grace, permit overage, or require top-up.
2. **Auto-stop defaults** — default mode, grace period choices, Activity Snapshot cadence, and stale-report threshold.
3. **Multiple Environments per repository** — MVP currently resolves one; an explicit selector/branching model is deferred.
4. **Private-alpha recovery retention** — snapshot timing, retention days, and operator restore procedure.

## Agent integration

5. **Agent binary version policy** — exact version pinning and disabling agent auto-updates was recommended but not accepted before specification authoring began.
6. **Codex installation/auth adapter** — confirm the current official remote installation and account-login flow after the newly installed OpenAI developer-docs connector becomes available in a restarted Codex task.
7. **Claude installation/auth adapter** — select native versus package installation and confirm updater policy against current official behavior.
8. **OpenCode** — decide whether install-and-validate remains in MVP or moves after private alpha.

## Infrastructure

9. **Initial AWS region and availability zone**.
10. **Runtime Preset mappings** — exact EC2 instance families after Docker/agent benchmarks and capacity tests.
11. **ECS proxy load balancer details** — ALB WebSocket settings, keepalive behavior, maximum connection age, and regional scaling.
12. **NAT cost policy** — exact allocation into credit rates and whether later regions use managed NAT, regional NAT, or another egress architecture.
13. **RDS production topology** — threshold for Multi-AZ promotion.

## Developer experience

14. **Documentation framework and deployment**.
15. **TanStack Start deployment target**.
16. **CLI token storage implementations** — macOS Keychain and Linux Secret Service/fallback behavior.
17. **Default existing-key selection rules** — automatic recommendation when multiple Ed25519 public keys exist.

## Implementation spikes required

- Verify WSS ProxyCommand behavior with OpenSSH, Codex desktop, VS Code/Cursor Remote SSH, SCP, SFTP, and long-lived idle connections.
- Verify EC2 stop/start private-address stability and guest readiness timing in the selected region.
- Verify EBS data-volume bind mounts for `/home/dev`, `/workspace`, and Docker across replacement.
- Verify WorkOS CLI Auth token refresh and revocation in the Go CLI.
- Verify Polar single-meter recurring credit grants, idempotent event ingestion, and customer-meter reconciliation.
- Verify Restate Cloud service versioning and long-running AWS workflow behavior.
