# Open implementation decisions

These items were not accepted during the design session and must not be silently treated as locked.

## Product policy

1. **Zero Credit Balance** — RESOLVED for private alpha (2026-07-18): starts refuse with `CREDITS_POLICY_BLOCKED` at balance ≤ 0; running compute is never force-stopped and may drive the balance slightly negative. Must be revisited before paid launch.
2. **Auto-stop defaults** — RESOLVED (2026-07-18): default mode `when_fully_idle`, 300-second grace period, 60-second Activity Snapshot cadence, 300-second stale-report threshold. See [Activity](./06-activity-and-autostop.md).
3. **Multiple Environments per repository** — MVP currently resolves one; an explicit selector/branching model is deferred.
4. **Private-alpha recovery retention** — snapshot timing, retention days, and operator restore procedure.

## Agent integration

5. **Agent binary version policy** — RESOLVED (2026-07-18): agents are pinned at AMI build time with auto-update disabled; the AMI is rebuilt weekly and adopted at the next start boundary. See ADR 0013.
6. **Codex installation/auth adapter** — confirm the current official remote installation and account-login flow after the newly installed OpenAI developer-docs connector becomes available in a restarted Codex task.
7. **Claude installation/auth adapter** — select native versus package installation and confirm updater policy against current official behavior.
8. **OpenCode** — decide whether install-and-validate remains in MVP or moves after private alpha.

## Infrastructure

9. **Initial AWS region and availability zone** — RESOLVED (2026-07-18): `eu-central-1` / `eu-central-1a` for the private alpha (EMEA-latency-driven).
10. **Runtime Preset mappings** — RESOLVED for alpha (2026-07-18): a preset ladder (`cpu2-mem8`, `cpu4-mem16`, `cpu8-mem32`, `cpu16-mem64`) mapped to the m7i-flex family in `eu-central-1`; exact families remain re-benchmarkable regional configuration.
11. **ECS proxy load balancer details** — ALB WebSocket settings, keepalive behavior, maximum connection age, and regional scaling.
12. **NAT cost policy** — exact allocation into credit rates and whether later regions use managed NAT, regional NAT, or another egress architecture.
13. **RDS production topology** — threshold for Multi-AZ promotion.

## Developer experience

14. **Documentation framework and deployment**.
15. **TanStack Start deployment target**.
16. **CLI token storage implementations** — RESOLVED for alpha (2026-07-18): lock-protected file storage (mode 0600) as implemented; macOS Keychain and Linux Secret Service remain post-alpha upgrades.
17. **Default existing-key selection rules** — RESOLVED (2026-07-18): exactly one Ed25519 key → use it silently; none → generate a dedicated `devm` Ed25519 key; multiple → most-recently-used with a printed note and an override flag.

## Capsules and packaging

18. **External-registry support timing** — decide when ghcr and ECR publication and consumption move beyond a power-user path.
19. **Signing phase** — decide when Capsule signing ships and whether cosign keyless or Notation with bring-your-own-PKI is supported first.
20. **OpenCode adapter in MVP or after** — decide whether the OpenCode Adapter ships in MVP or after private alpha.
21. **Drift-adoption defaults** — decide whether guest-observed drift is proposed for adoption by default and how consent and executable-content review apply.
22. **Hosted-registry multi-tenancy isolation model** — resolved for MVP by per-owner S3 prefixes; multi-tenant registry isolation becomes a sharing-milestone decision.
23. **Tag→digest resolution** — RESOLVED (2026-07-18): Postgres tag index written at `capsule publish`, queried by resolvers. See ADR 0012.
24. **Deterministic user toolset** — NEW (2026-07-18): a replicable, Nix-style mechanism for user-installed tools so an Environment's full toolchain becomes reproducible; until then user installs are home-first and system-path installs are ephemeral per ADR 0013.

## Implementation spikes required

- Verify WSS ProxyCommand behavior with OpenSSH, Codex desktop, VS Code/Cursor Remote SSH, SCP, SFTP, and long-lived idle connections.
- Verify EC2 stop/start private-address stability and guest readiness timing in the selected region.
- Verify EBS data-volume bind mounts for `/home/dev`, `/workspace`, and Docker across replacement.
- Verify WorkOS CLI Auth token refresh and revocation in the Go CLI.
- Verify Polar single-meter recurring credit grants, idempotent event ingestion, and customer-meter reconciliation.
- Verify Restate Cloud service versioning and long-running AWS workflow behavior.
- Verify two-machine determinism for capsule packaging.
- Verify ECR/GHCR artifact push-pull-fallback conformance.
- Verify TOML key-level merge ownership in the guest materializer.
