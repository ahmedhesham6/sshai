# Profiles and project seeding

## Profile creation

Profile creation is an explicit CLI allowlist workflow.

1. Scan only known agent/configuration roots and project files locally.
2. Classify candidates without uploading content.
3. Display type, path, evidence, sensitivity, trust, and executable-content flags.
4. Require selection for every included candidate, including instruction-only content.
5. Persist selectors—not blanket directories—as Profile source intent.
6. Compile selected content into an immutable Profile Version.
7. Upload only compiled artifacts and metadata.

Unknown content is excluded. Private-key contents are never read. Symlinks escaping selected roots are rejected. Credentials found inside otherwise portable files block the artifact until removed or replaced with a reference.

## Initial artifact classes

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
- agent project/session history as Profile content;
- arbitrary hooks and plugins;
- caches and machine-local state;
- unknown files;
- hardware- or OS-bound credentials.

## Executable-content boundary

Selecting executable content for a Profile does not authorize execution.

- `devm` never executes skill scripts, hooks, or plugins during synchronization.
- It may materialize an explicitly selected digest after review.
- Explicit setup commands such as dotfile installers or lifecycle commands require separate review and run in a constrained setup runner with no secrets and no network by default.
- Later execution by Codex, Claude, or the user occurs with normal Environment permissions and is outside `devm` enforcement.
- A changed executable digest requires renewed review.

## Profile publication and devices

There is no Device aggregate. Local CLI state records the selected authoring Profile and its last observed head version.

On a fresh machine, the CLI offers:

- use an existing Profile read-only;
- fork an existing Profile into a new named Profile; or
- create a new blank Profile.

Publishing a Profile Version requires the expected current head. Stale publication is rejected; the CLI shows the intervening changes and offers refresh or fork. Automatic Profile merging is out of scope.

## Materialization planning

For each artifact, planning resolves:

```text
source artifact
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

Deletion requires an explicit prune operation.

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

Ignored files are excluded unless a later explicit transfer surface supports them. Secret scanning runs before packaging. The Project Seed is immutable and content-addressed.

## Post-creation authority

After the Project Seed is applied, the Environment's `workspace` component is authoritative. Subsequent `devm` runs attach to it. They do not automatically upload local changes, pull Git, reset the worktree, or compare filesystem trees.

Moving work uses explicit Git commands or a future explicit export/import operation.
