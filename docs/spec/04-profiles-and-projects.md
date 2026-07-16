# Capsules, profiles, and project seeding

## Capsule capture

Capsule capture is an explicit allowlist workflow. Selecting or packaging executable
content never authorizes execution.

1. Scan only known agent/configuration roots and project files locally.
2. Classify candidates without uploading content.
3. Display type, path, evidence, sensitivity, trust, and executable-content flags.
4. Require selection for every included candidate, including instruction-only content.
5. Persist selectors—not blanket directories—as capture intent.
6. Compile selected content into typed Components and an immutable Capsule.
7. Upload only the compiled Capsule and metadata.

Unknown content is excluded. Private-key contents are never read. Symlinks escaping
selected roots are rejected. Secret scanning runs before packaging. Credentials found
inside otherwise portable files block the Capsule until removed or replaced with a
reference.

## Initial component sources

- Agent instructions: `AGENTS.md`, `CLAUDE.md`, and selected personal instructions.
- Open Agent Skills and native skill directories.
- Selected Codex and Claude settings fields.
- Shell preferences and selected dotfile fragments.
- Git identity and non-secret preferences.
- Personal tool declarations.
- MCP server references.
- Credential Requirements and secret references.

Excluded by default:

- access tokens, auth caches, and private keys;
- agent project/session history as Capsule content;
- arbitrary hooks and plugins;
- caches and machine-local state;
- unknown files;
- hardware- or OS-bound credentials.

## Component model

A Capsule contains independently updateable Components. Each Component has a stable id,
its own OCI layer, a digest, a scope, a trust class, and declared requirements.
The Capsule is the unit of distribution and sharing.

Component types are `config`, `skill`, `command`, `subagent`, `hook`, `integration`
(MCP), `permission-policy`, `template`, and `extension` (the agent-native escape
hatch). Component scope is `user | project`. A stable id is type-qualified, such as
`skill:fix-ci`.

Trust classes are:

| Trust class | Rule |
|---|---|
| `declarative` | Declarative content. |
| `executable` | A changed executable digest requires renewed review. |
| `permission` | Always requires explicit itemized consent at apply time. |

`permission-policy` Components are never applied by `auto_safe` and are never trusted
transitively through a signature or `track` policy. `devm` never executes skill
scripts, hooks, or plugins during synchronization. Explicit setup commands require
separate review and run in a constrained setup runner with no secrets and no network
by default.

Components declare requirements, including commands and Credential Requirements.
Credential Requirements contain names or references only. Values never enter Capsule
layers, registry metadata, plans, logs, or diffs. MCP `env` secrets become Credential
Requirements during capture and are never copied.

## Profile and Capsule Ref

A Profile is a named, reusable, ordered group of Capsule Refs plus its composition
rules. A Profile contains no configuration content. A Profile Version is immutable and
stores the ordered group and its history.

A Capsule Ref points to a Capsule by registry reference, tag, or digest. It carries a
freshness policy and component exclusions.

Until signing ships, Environments may reference only Capsules owned by the authenticated
user; the control plane enforces this constraint.

There are no inter-Capsule dependencies, profiles-of-profiles, or implicit ordering.
The ordered Capsule Ref list is the flat composition set. Forking a Profile copies the
ref list.

Publishing a Profile Version requires the expected current head. Stale publication is
rejected; the CLI shows the intervening changes and offers refresh or fork. Automatic
Profile merging is out of scope.

There is no Device aggregate. Local CLI state records the selected authoring Profile
and its last observed head version. On a fresh machine, the CLI offers:

- use an existing Profile read-only;
- fork an existing Profile into a new named Profile; or
- create a new blank Profile.

## Composition and conflicts

Resolution applies the Capsule Refs in Profile order.

| Case | Resolution |
|---|---|
| Same component id and identical digest | Deduplicate silently. |
| Same component id and different content | Hard conflict at resolve time. Resolve with exclusions or reordering recorded in the Profile Version. |
| Mergeable JSON/TOML config | Capsule order defines merge precedence. The plan shows effective config with per-key provenance. |
| Permissions | Never merge silently. Recompute effective policy and re-consent when any contributing Capsule changes. |

## Capsule Lock resolution

A Capsule Lock is the immutable, content-addressed result of resolving a Profile
Version together with the Environment's project capsule. It contains exact Capsule
digests and the resolved component map.

```text
Profile Version
  + ordered Capsule Refs
  + Environment project capsule
  → resolve exclusions, order, merges, and conflicts
  → Capsule Lock
  → materialize only from the Lock
```

Environments pin `(ProfileVersionID, LockID)`. The Lock is the reproducibility anchor,
replacing a pinned Profile Version alone. A new Capsule digest or project capsule
changes the Lock; materialization does not consume an unresolved tag.

## Freshness and upgrades

Freshness is per Capsule Ref:

| Policy | Rule |
|---|---|
| `track` | Follows the ref's tag and may auto-apply through the safe path. |
| `review` | Requires explicit approval of the diff since last approval. |
| `pin` | Never moves. |

Upgrade policy is per Environment: `manual | notify | auto_safe`.

`auto_safe` applies a new Lock automatically only when the plan has managed targets
clean, no executable digest changes, no permission changes, no integration (MCP)
component changes, no Credential Requirement changes, and no conflicts.

## Materialization and drift

An Adapter is a per-agent compiler backend. It translates canonical Components into
native change plans. The generic materializer runtime enforces atomic writes, conflict
detection, ownership, and rollback.

For each Component in the Lock, planning resolves:

```text
component
→ native adapter
→ target path or configuration selector
→ materialization mode
→ desired digest/value
→ last-applied digest/value
→ observed digest/value
→ risk classification
```

Plan operations are `create | update | skip | drift | conflict | orphan | remove | requires_input`.

Three-way rule for managed content:

```text
observed == lastApplied
  apply desired safely

observed != lastApplied && desired == lastApplied
  remote drift; report and preserve

observed != lastApplied && desired != lastApplied
  conflict; require explicit resolution

source removed
  mark orphaned; do not delete automatically
```

Deletion requires an explicit prune operation. Guest-observed drift on a managed
Component may be proposed back into its owning Capsule as a new version: "adopt into
Capsule X?". Adoption is one-keystroke consent, never fully automatic, and executable
content always requires review.

Project-scope Components are `seeded` only: applied at creation, then owned by the
workspace. They are never `managed` inside `/workspace`.

## Project capsule and ProjectSpec

The project capsule is compiled from repository detection, reviewed, and attached to
the Environment, not placed inside the Profile. It is the reviewed authority for
project-scope Components. `ProjectSpec` remains detected evidence.

## Project discovery

Project discovery records evidence for:

- Git remote, branch, detached HEAD, ahead/behind state, submodules, and LFS;
- package-manager and language runtime declarations;
- Dockerfile and Compose services;
- project instructions;
- Dev Container configuration as intent only;
- setup and lifecycle commands;
- port and service declarations.

`ProjectSpec` is a detected snapshot, not a live authority over a working Environment.

## Project Seed

### Clean and pushed

Seed the exact commit from the canonical repository.

### Unpushed commits

Create and verify a Git bundle containing selected refs. Do not copy `.git` directly.

### Uncommitted state

Create:

- a patch for modified tracked files;
- an archive containing explicitly selected, nonignored untracked files;
- a manifest with paths, modes, sizes, and content digests.

Ignored files are excluded unless a later explicit transfer surface supports them. Secret
scanning runs before packaging. The Project Seed is immutable and content-addressed.

## Post-creation authority

After the Project Seed is applied, the Environment's `workspace` component is
authoritative. Subsequent `devm` runs attach to it. They do not automatically upload
local changes, pull Git, reset the worktree, or compare filesystem trees.

Moving work uses explicit Git commands or a future explicit export/import operation.
