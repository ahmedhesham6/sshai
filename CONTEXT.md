# Dev Environments

This context defines the language for creating, operating, and closing agent-ready remote development environments while preserving a developer's work and selected configuration.

## Configuration

**Profile**:
A named, reusable selection of a developer's portable personal configuration across projects.
_Avoid_: Machine profile, compute profile, dotfiles bundle

**Profile Version**:
An immutable snapshot of a Profile. An Environment pins one version until an explicit upgrade.
_Avoid_: Current profile, live profile

**Profile Artifact**:
One selected instruction, skill, setting, tool declaration, or other portable configuration item contained in a Profile Version.
_Avoid_: Synced file

**Materialization**:
The recorded application of a Profile Artifact to an Environment, including its target, mode, adapter, and last-applied digest.
_Avoid_: Copy, installation

**Materialization Mode**:
The ownership rule for a Materialization: `managed`, `seeded`, or `referenced`.
_Avoid_: Sync mode

## Project

**Project Binding**:
The identity of the one primary repository managed by an Environment.
_Avoid_: Workspace repository, repo link

**Project Spec**:
The repository-derived declaration of runtimes, package managers, services, instructions, and setup intent.
_Avoid_: Profile, environment config

**Project Seed**:
The immutable initial transfer of a repository revision plus selected unpushed commits, tracked changes, and untracked files.
_Avoid_: Repository sync, home archive

## Environment and compute

**Environment**:
A durable logical development workspace that owns one Project Binding, one pinned Profile Version, state components, lifecycle policy, and at most one current Runtime.
_Avoid_: Machine, VM, devbox

**State Component**:
A logical category of durable or disposable Environment state: `workspace`, `home`, `services`, or `cache`.
_Avoid_: Disk, mount

**Runtime**:
A provider compute allocation that temporarily runs an Environment and mounts its State Components.
_Avoid_: Environment, session, machine

**Runtime Preset**:
A product-level compute size that maps to a region-specific provider instance type.
_Avoid_: Compute profile, instance type

**Activity Snapshot**:
A periodic summary of connections, recognized agents, protected processes, and selected containers observed in a Runtime.
_Avoid_: Session

**Auto-stop Policy**:
The Environment rule that decides when a Runtime may stop after a grace period.
_Avoid_: Idle timeout

## Operations

**Operation**:
A user-visible durable mutation of an Environment, orchestrated by Restate and projected into PostgreSQL.
_Avoid_: Job, task, request

**Operation Step**:
A semantic milestone projected from a durable Restate workflow for progress and support visibility.
_Avoid_: Queue item

**Provider Resource**:
An inventoried cloud resource owned by the platform and associated with an Environment, Runtime, State Component, or Operation.
_Avoid_: AWS ID field

## Access and billing

**Credential Requirement**:
A declaration that an Environment needs an external authorization without containing the credential itself.
_Avoid_: Secret

**Credential Binding**:
An Environment-specific authorization that satisfies a Credential Requirement.
_Avoid_: Synced credential

**Credit Balance**:
The single subscription-funded pool of abstract credits used by all billable resource types.
_Avoid_: Compute wallet, storage wallet

**Credit Transaction**:
An immutable grant, debit, adjustment, or refund against the Credit Balance.
_Avoid_: Usage row, invoice line
