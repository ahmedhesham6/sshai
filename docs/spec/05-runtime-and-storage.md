# Environment, Runtime, and storage

## Lifecycle separation

An Environment may exist without a running Runtime. Normal close transitions the current Runtime to `stopped`; it does not delete the Environment or persistent data.

```text
Environment lifecycle
creating → active → deleting → deleted

Runtime lifecycle
absent → provisioning → starting → ready → stopping → stopped
                                      │                    │
                                      └──── replacing ─────┘
```

Environment health is evaluated independently as `healthy`, `degraded`, `blocked`, or `unknown`.

## Runtime invariants

- At most one current writable Runtime per Environment.
- Multiple SSH connections, IDE servers, agent processes, and background processes may share it.
- A stopped EC2 instance incurs no EC2 compute usage charge, while EBS and shared platform resources continue to cost money.
- Runtime start creates billable compute usage; Runtime stop closes it.
- Replacement retires the old Runtime before attaching writable data to the new one.
- Provider instance identity is not Environment identity.

## Volume layout

### System volume

- Small encrypted EBS root volume (platform-owned, 30 GiB in alpha).
- Versioned Ubuntu AMI, guest supervisor, Docker engine, baseline tools, pinned agent binaries, and materializer runtime.
- Replaceable during repair or upgrade; exclusively platform-owned — user system-path installs (`sudo apt …`) are ephemeral by contract and do not survive replacement (ADR 0013).
- Not authoritative for user work. The image preconfigures user-space package managers to install into the durable home component so the ordinary tool-install path never touches the system volume.

### Persistent data volume

- One encrypted gp3 EBS volume per Environment in the MVP.
- Size is user-configurable per Environment at create (default 100 GiB, bounded 20–500 GiB in alpha).
- Survives stop, Runtime replacement, and system-volume replacement.
- `DeleteOnTermination=false`.
- Explicit deletion only through the Environment delete workflow after ownership validation and private-alpha backup policy.

### Logical mounts

```text
/workspace              → workspace component
/home/dev               → home component
/var/lib/docker         → services component
/var/cache/devm         → cache component
/var/lib/devm           → platform metadata on the persistent volume
```

The first implementation may use directories or subvolumes on one filesystem. Each logical component still has its own metadata and policy.

## Durability classes

| Component | Default | Contains | Stop/start | Replacement |
|---|---|---|---|---|
| `workspace` | durable | repository and worktrees | preserved | reattached |
| `home` | durable | user files and agent-native sessions | preserved | reattached |
| `services` | durable | Docker volumes and databases | preserved | reattached |
| `cache` | disposable | build/package/container caches | best effort | may rebuild |

Profile-generated content is reproducible and not the authoritative copy even when materialized under durable paths.

## Availability-zone constraint

EBS volumes attach within an availability zone. The Environment records a region and availability zone at creation. Region is immutable in the MVP; normal replacement stays in the same zone. Capacity failures must not silently create empty state in another zone.

## Start readiness levels

1. Runtime allocated.
2. Persistent data attached and mounted.
3. SSH server ready through the proxy.
4. Project environment ready.
5. Selected agent setup validated.

The user may connect at level 3 while later readiness work continues, provided the CLI displays incomplete steps.

## Deletion

Deletion is the only normal operation that destroys persistent state.

Before deletion, display:

- modified and untracked workspace files;
- unpushed commits;
- durable component sizes;
- running agent/process activity;
- outstanding credential bindings;
- the applicable private-alpha snapshot/grace policy.

Require exact Environment-name confirmation and recent authentication. Never delete a data volume as automatic compensation for another failed step.
