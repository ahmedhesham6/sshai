# Durable operations and Restate workflows

## Execution boundary

Restate Cloud is authoritative for workflow journals, durable timers, retries, keyed serialization, and invocation lifecycle. PostgreSQL stores product state and user-facing projections.

## Service types

### Environment virtual object

Key: `environment_id`.

Responsibilities:

- serialize mutating commands for one Environment;
- enforce one current writable Runtime;
- reject or return the active conflicting Operation;
- maintain Auto-stop evaluation timers;
- dispatch uniquely keyed workflows;
- expose read-only signals required by the API.

It stores coordination state only. Product aggregates remain in PostgreSQL.

### Operation workflows

Workflow key: `operation_id`.

Each workflow:

- receives immutable input;
- creates or verifies the Operation projection;
- records semantic steps;
- wraps nondeterministic AWS, PostgreSQL, WorkOS, and Polar calls in durable actions;
- uses deterministic external idempotency keys;
- normalizes transient and terminal failures;
- updates product state transactionally;
- completes the Operation projection.

## Operation types

- `environment.create`
- `environment.delete`
- `environment.update_auto_stop`
- `runtime.start`
- `runtime.stop`
- `runtime.replace`
- `capsule.publish`
- `profile.resolve`
- `profile.prune`
- `project.seed`
- `credential.bind`
- `environment.reconcile`
- `billing.deliver`

Every Environment mutation, including Auto-stop Policy updates, is represented by an Operation and serialized through the Environment virtual object.

## Create workflow

1. Validate User, subscription, credits policy, region, Profile Version, Project Seed, Runtime Preset, and Auto-stop Policy.
2. Reserve Environment slug and immutable region/AZ.
3. Create persistent data volume and inventory it.
4. Create system volume/Runtime from the approved AMI.
5. Attach persistent data.
6. Configure private networking and Environment SSH identity.
7. Wait for guest boot and mount readiness.
8. Apply the Project Seed.
9. Resolve the pinned Profile Version with the reviewed project Capsule, persist the Capsule Lock, and materialize the Lock.
10. Bind required credentials or mark `requires_input`.
11. Validate selected agents and project toolchain.
12. Publish Runtime readiness and complete Environment activation.

No step after data-volume creation may automatically delete the data volume as compensation.

## Start workflow

1. Lock Environment command handling.
2. Return success if current Runtime is already ready.
3. Verify the stopped instance, data volume, image compatibility, and credit policy.
4. Start EC2 with an idempotent provider request.
5. Open the compute usage interval.
6. Wait for current-boot guest readiness and mounted state.
7. Reconcile public SSH keys and safe managed configuration.
8. Mark Runtime ready and return the private route to the regional proxy.

## Stop workflow

1. Record stop reason: manual, auto-stop, billing policy, repair, or resize.
2. Suppress further auto-stop evaluation.
3. Request a current Activity Snapshot for warnings and audit.
4. Ask the guest for graceful shutdown preparation.
5. Stop EC2; do not terminate.
6. Verify stopped provider state and durable-volume presence.
7. Close the compute usage interval and create the credit debit.
8. Mark Runtime stopped and resume policy evaluation only after the next start.

## Replace workflow

1. Mark Runtime replacing and block new proxy connections.
2. Stop the current Runtime if required.
3. Verify persistent data health and ownership.
4. Retire old compute and replaceable system volume.
5. Create a new Runtime in the same AZ using an approved AMI.
6. Attach the existing data volume read-write only after old attachment is gone.
7. Restore Environment SSH host identity.
8. Boot, reconcile, validate, and mark ready.
9. Retain historical Runtime and provider-resource records.

## Profile resolve workflow

The pipeline is `resolve → lock → plan → apply`. Environments materialize only from a Capsule Lock.

1. Validate the target Profile Version, the Environment's reviewed project Capsule, and the requested upgrade policy.
2. Resolve the ordered Capsule Refs. `track` follows the ref's tag and may auto-apply through the safe path; `review` requires explicit approval of the diff since last approval; `pin` never moves.
3. Apply Capsule exclusions and composition rules. Deduplicate identical Component IDs with identical digests, hard-block conflicting Component IDs with different content, merge eligible configuration by Capsule order, and recompute and re-consent permissions when contributors change.
4. Create and persist an immutable, content-addressed Capsule Lock containing exact Capsule digests and the resolved Component map.
5. Compile an adapter-specific plan from the Lock and desired, last-applied, and observed state. The Adapter translates canonical Components; the generic materializer enforces atomic writes, conflict detection, ownership, and rollback.
6. Pull changed layers by digest, verify them, and apply only safe approved operations. Stage and syntax-check format-aware changes. Never execute skill scripts, hooks, or plugins.
7. Stop on drift or conflict without overwriting. Roll back only managed Components modified by the failed operation.
8. Persist Materialization state and pin the Profile Version and Capsule Lock together only after validation succeeds.

## Reconciliation

Periodic Restate workflows compare:

- Environment intent and Runtime provider state;
- Provider Resource inventory and ownership tags;
- persistent data existence and attachment;
- current private address and boot ID;
- Materialization last-applied and observed digests;
- Capsule Lock and resolved Component state versus installed Component state;
- open compute usage intervals and provider state;
- undelivered Polar credit events.

Safe nondestructive divergence may create a repair Operation. Ambiguous, destructive, or data-loss-adjacent divergence sets Environment health to `blocked` and alerts an operator.

## Error taxonomy

- `AUTHORIZATION_FAILED`
- `SUBSCRIPTION_INACTIVE`
- `CREDITS_POLICY_BLOCKED`
- `OPERATION_CONFLICT`
- `REGION_UNAVAILABLE`
- `CAPACITY_UNAVAILABLE`
- `PROFILE_INCOMPATIBLE`
- `PROJECT_SEED_INVALID`
- `STATE_VOLUME_MISSING`
- `STATE_ATTACHMENT_CONFLICT`
- `GUEST_NOT_READY`
- `SSH_NOT_READY`
- `PROFILE_DRIFT`
- `PROFILE_CONFLICT`
- `CREDENTIAL_REQUIRED`
- `PROVIDER_RATE_LIMITED`
- `PROVIDER_AUTH_FAILED`
- `RESOURCE_DIVERGED`
- `BILLING_DELIVERY_FAILED`

Every terminal error states what remains safe and a valid next action.
