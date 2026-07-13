# Testing and acceptance

## Test layers

### Domain unit tests

- Environment lifecycle and health transitions.
- One-current-Runtime invariant.
- Profile head optimistic concurrency.
- Materialization three-way comparison.
- Project Seed manifest and path safety.
- Auto-stop policy evaluation.
- Runtime Preset and credit-rate conversion.
- Credit Balance projection from immutable transactions.
- authorization and idempotency behavior.

### Contract tests

- OpenAPI request/response generation compiles for Go and TypeScript.
- WorkOS JWT validation fixtures.
- Polar webhook signature and event fixtures.
- Provider adapter conformance suite.
- Guest protocol version compatibility.
- WebSocket proxy control and binary-frame contract.

### Restate workflow tests

For every durable action boundary:

- terminate the handler and verify resume;
- replay the same invocation and verify no duplicate provider or billing mutation;
- inject transient and terminal failures;
- verify Operation projections remain monotonic;
- verify stale signals and timers do not mutate a newer Runtime.

### Scanner/profile fixtures

- supported Codex configuration versions;
- supported Claude configuration versions;
- instruction-only and executable skills;
- credential and token fixtures;
- symlink escapes;
- JSON, JSONC, TOML, YAML, and Markdown selectors;
- comments and unknown fields preserved where promised;
- remote drift and three-way conflict.

### Project Seed fixtures

- clean pushed branch;
- detached HEAD;
- unpushed commits;
- modified tracked files;
- selected untracked files;
- ignored files;
- submodules and Git LFS;
- malicious paths, symlinks, and secret-containing content;
- seed replay is idempotent.

### Real AWS smoke tests

- create private Runtime and connect through regional WSS proxy;
- verify no public IPv4 and no public SSH ingress;
- stop/start with the same Environment host identity;
- preserve workspace, home, services, and Docker data;
- replace system volume/Runtime and reattach persistent data;
- NAT egress reaches Git, package registries, containers, Codex, and Claude endpoints;
- provider reconciliation repairs safe drift and blocks destructive ambiguity;
- delete removes owned resources only.

### SSH compatibility matrix

- interactive shell;
- remote command with exit status;
- SCP;
- SFTP;
- rsync;
- local forwarding;
- at least three concurrent connections;
- idle connection with keepalive;
- Codex desktop;
- one mainstream Remote SSH IDE.

### Auto-stop tests

- each policy predicate and grace cancellation;
- live Codex process blocks `when_agents_finish`;
- live Claude process blocks `when_agents_finish`;
- stale snapshot blocks stop;
- unknown process blocks `when_fully_idle`;
- manual policy never stops;
- auto-stop closes compute usage exactly once;
- activity arriving at timer expiry prevents stop.

### Billing tests

- subscription credit grant;
- compute conversion for every regional Runtime Preset;
- storage conversion across stopped time;
- rate version changes do not rewrite history;
- transaction/outbox atomicity;
- Restate and Polar retries do not duplicate events;
- Polar webhook replay is idempotent;
- local and Polar meter reconciliation.

### Web and CLI end to end

- WorkOS hosted login and CLI device flow;
- existing SSH key selection and dedicated-key generation;
- first Profile allowlist flow;
- dirty repository plan and Project Seed upload;
- create progress survives CLI termination;
- direct `ssh alias` starts a stopped Runtime through ProxyCommand;
- Profile upgrade blocks on remote drift;
- web Auto-stop Policy change resets evaluation;
- credit balance and Polar portal display.

## Canonical acceptance demo

1. Sign in through `devm login` using WorkOS device authorization.
2. Run `devm` in a dirty repository with unpushed commits.
3. Select existing or new Profile configuration; observe credentials and unknown files excluded.
4. Create an Environment in the enabled region.
5. Connect through `ssh <alias>` with no Runtime public IPv4.
6. Launch Codex and Claude and open a second SSH/IDE connection.
7. Create files, commits, a Docker volume, and database data.
8. Exit the agents and satisfy the chosen Auto-stop Policy.
9. Verify EC2 compute stops and a credit debit is recorded once.
10. Reconnect through the alias; verify automatic start and preserved state.
11. Replace the Runtime/system disk and verify the same state and SSH host identity.
12. Publish and apply a Profile update.
13. Modify the same managed target remotely and verify conflict protection.
14. Delete through the exact-name flow and verify no leaked owned resources.

## Launch gates

- 50 automated staging create → stop → start → replace → delete cycles without leaked resources.
- 20 cycles with workflow termination at random durable boundaries.
- Zero lost workspace markers, local commits, Docker volumes, or SSH host keys.
- Zero duplicate credit debits under retry and replay.
- Security review of proxy authorization, IAM, data-volume deletion, scanner boundaries, WorkOS validation, and Polar webhooks.
- All required runbooks exercised by someone other than their author.
