# Product and scope

## Product promise

An individual developer runs `devm` inside a repository, chooses or creates a Profile, reviews the initial plan, and receives an agent-ready remote Environment. Standard SSH-compatible clients work through a stable alias. The Runtime stops according to the Environment's selected Auto-stop Policy while work remains durable.

## Canonical first-run journey

1. `devm` authenticates through WorkOS CLI Auth.
2. The CLI discovers the repository, local Git state, candidate personal configuration, an existing SSH public key, and project requirements.
3. The developer selects a Profile or creates one from an explicit allowlist.
4. The CLI constructs a Project Seed and displays included, review-required, and excluded content.
5. The developer selects a region, Runtime Preset, and Auto-stop Policy.
6. The control plane reserves an Environment and begins a durable Restate workflow.
7. AWS resources are created, the Project Seed is applied, the Profile Version is materialized, and readiness is validated.
8. The CLI writes a stable OpenSSH alias and connects.
9. Multiple terminals, IDE connections, Codex processes, and Claude processes may use the same Runtime.
10. The regional guest reports Activity Snapshots. When the selected Auto-stop Policy is satisfied for its grace period, a normal stop Operation closes compute.
11. Running `devm` or using the SSH alias starts the stopped Runtime and reconnects to the same Environment.

## Success definition

A successful working session has:

- an authenticated user with an active subscription;
- an Environment resolved from the current repository;
- a mounted and writable `workspace` State Component;
- the pinned Profile Version materialized without a blocked conflict;
- at least one selected coding agent installed and launchable;
- SSH readiness through the regional proxy;
- billable usage recorded as credits; and
- no manual cloud-console or machine repair.

## MVP scope

### Included

- macOS and Linux CLI clients;
- individual WorkOS AuthKit users;
- one primary repository per Environment;
- dirty and unpushed initial Git state through Project Seeds;
- named Profiles with explicit configuration selection and immutable versions;
- deep Codex and Claude configuration adapters;
- Open Agent Skills, selected native settings, instructions, shell preferences, Git identity, personal tools, MCP references, and Credential Requirements;
- private EC2 Runtimes with standard OpenSSH behind a WebSocket proxy;
- one active AWS region with region-aware contracts;
- stop/start, replacement, and explicit deletion;
- separate system and persistent data EBS volumes;
- Docker Engine and Compose;
- configurable process-aware Auto-stop Policies;
- one subscription Credit Balance integrated with Polar;
- TanStack Start web control surface and a documentation website;
- durable Restate workflows, reconciliation, audit, and operator diagnostics.

### Explicitly excluded

- teams, organizations, RBAC, shared Profiles, and shared Environments;
- background-agent task queues or a proprietary agent UI;
- bidirectional local/remote file synchronization;
- automatic Git pull, reset, rebase, or dirty-tree replacement;
- automatic credential-cache import;
- arbitrary home-directory archives;
- automatic execution of skills, hooks, or plugins during profile synchronization;
- multiple writable Runtimes for one Environment;
- cross-region Environment migration;
- Windows client and Runtime support;
- Kubernetes;
- multiple compute providers in the private alpha;
- full Dev Container conformance;
- custom user images, GPUs, public port management, and browser IDEs;
- user-facing snapshots, forks, and restore UI.

## Product principles

1. **One command, explicit consequences.** Convenience must not hide source, cost, or destructive behavior.
2. **Environment identity outlives compute.** Runtime is replaceable; work is not.
3. **Selected setup, never complete setup.** Portability is an allowlist.
4. **SSH is the client contract.** Existing terminals, IDEs, and agent clients remain usable.
5. **Stop compute, preserve work.** Normal close is not deletion.
6. **No silent overwrite.** Local dirty state, remote drift, and profile conflicts are surfaced.
7. **Credits are product language.** Provider prices are inputs to credit rates, not exposed cloud primitives.

## Product metrics

### North-star metric

Weekly Environments that reach an agent-ready SSH connection without manual infrastructure intervention and later auto-stop successfully without destroying work.

### Activation funnel

`CLI login → repository inspected → Profile selected → plan accepted → first agent launch → first safe auto-stop → successful resume`

### Reliability targets

- Successful start-to-SSH: greater than 99% after a Runtime is healthy.
- Interrupted durable workflows resumed: greater than 99%.
- Silent managed-content overwrites: zero.
- Generic credential synchronization incidents: zero.
- Unrecoverable normal stop operations: zero.
- Duplicate Polar credit debits: zero.
