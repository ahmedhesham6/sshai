# Product surfaces and UX

## CLI

The CLI is the primary product interface. Commands are noun-light and repository-contextual.

### Core commands

```text
devm                         inspect, create or attach, start if needed, then connect
devm inspect                 local-only project and Profile candidate discovery
devm plan                    compute a create or update plan without applying
devm status                  Environment, Runtime, activity, operation, and credits
devm stop                    stop the current Runtime
devm delete                  delete an Environment after safety inventory
devm doctor                  diagnose local, control-plane, proxy, and guest state

devm profile list
devm profile show <name>
devm profile create <name>
devm profile fork <source> <name>
devm profile diff <from> <to>
devm profile publish
devm profile apply <version>

devm ssh-proxy               internal OpenSSH ProxyCommand entrypoint
devm login
devm logout
```

### Repository resolution

From a repository, `devm` resolves an existing Environment by canonical Project Binding. If none exists, it starts creation. Multiple Environments for the same repository are out of scope for the first CLI unless a later explicit selector is added.

### Plan presentation

Every plan groups actions into:

- `safe`;
- `review`;
- `requires_authorization`;
- `excluded`;
- `conflict`.

Every item includes evidence and source. Unknown content is excluded, not silently treated as safe.

### Progress

The CLI maps internal workflow steps to product language and remains resumable after interruption. Re-running the command attaches to the existing Operation using its idempotency identity.

### Local state

```text
~/.config/devm/
  config.toml
  auth/                 OS credential-store references, never raw refresh tokens when avoidable
  ssh/
    config
  known_hosts
  profiles/
    <profile-id>.toml   local authoring/head metadata
  projects/
    <project-id>.toml   repository-to-Environment link
```

## Web application

TanStack Start provides:

- WorkOS AuthKit integration and callback handling;
- Environment list and detail;
- Runtime state and start/stop controls;
- current activity and Auto-stop Policy configuration;
- Profile list, versions, and diffs;
- Operation progress and error recovery;
- SSH setup and key management;
- Credit Balance, usage breakdown, and Polar portal link;
- delete safety flow;
- operator-only diagnostic surface behind separate authorization.

The web application does not contain an editor, terminal, or proprietary agent orchestration UI.

## Environment detail information architecture

1. Identity: name, repository, region, health.
2. Runtime: status, Runtime Preset, active duration, primary action.
3. Connection: stable alias, proxy region, SSH-key state.
4. Activity: connections, Codex/Claude counts, current auto-stop decision.
5. Profile: pinned version, pending version, drift/conflicts.
6. State: workspace/home/services/cache health and allocated storage.
7. Billing: current credit consumption and remaining balance.
8. Operations: current and recent workflows.
9. Danger zone: replace Runtime and delete Environment.

## Documentation website

Public documentation must cover:

- installation and WorkOS login;
- first Environment creation;
- Profiles and selection safety;
- dirty Git-state transfer;
- SSH alias and supported clients;
- Auto-stop Policies;
- credit model and persistent storage cost;
- agent authentication;
- troubleshooting and `doctor`;
- deletion and private-alpha recovery limitations;
- security and privacy boundaries.

The documentation framework remains an open technology choice.

## Copy rules

- Say “Environment” for the durable product resource and “Runtime” for compute.
- Say “stop” for ending compute and “delete” for destroying the Environment.
- Never claim the complete local setup is imported.
- Distinguish credits spent on active compute from credits spent on persistent storage.
- State what remains safe before presenting a repair action.
