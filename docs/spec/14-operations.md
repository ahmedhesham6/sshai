# Observability, reconciliation, and support

## Telemetry

Use OpenTelemetry across Go services and the guest supervisor.

Required correlation fields:

- `request_id`
- `user_id_hash`
- `environment_id`
- `runtime_id`
- `operation_id`
- `restate_invocation_id`
- `region`
- `provider_resource_id`
- `step_key`
- `attempt`
- `error_code`
- `rate_version`
- `credit_transaction_id`

Do not place secrets, SSH payload, arbitrary process command lines, or Profile contents in telemetry.

## Metrics

### Product

- `environment_creations_total`
- `environment_time_to_first_ssh_seconds`
- `environment_agent_ready_seconds`
- `runtime_starts_total`
- `runtime_start_to_ssh_seconds`
- `runtime_auto_stops_total`
- `runtime_resume_success_total`
- `profile_materializations_total`
- `profile_conflicts_total`

### Reliability

- `restate_workflow_failures_total`
- `operation_duration_seconds`
- `provider_api_errors_total`
- `provider_divergence_total`
- `guest_heartbeat_age_seconds`
- `proxy_connections_active`
- `proxy_connection_failures_total`
- `state_component_health`

### Billing

- `credit_debits_total`
- `polar_delivery_lag_seconds`
- `polar_delivery_failures_total`
- `open_compute_intervals`
- `billing_reconciliation_difference`

Avoid high-cardinality user, repository, path, or process labels.

## Reconciliation loops

### Provider reconciliation

Compare desired Runtime power, observed EC2 state, data-volume existence/attachment, root volume, security group, private address, and tags.

### Guest reconciliation

Compare latest boot ID, image version, mounts, SSH readiness, Docker health, and Activity Snapshot freshness.

### Profile reconciliation

Compare Materialization last-applied and observed digests/selectors. Report drift; do not overwrite.

### Billing reconciliation

Compare Runtime intervals, provider state, local Credit Transactions, Polar delivery status, and Polar meter totals.

## Operator view

Show:

- Environment desired and observed state;
- Runtime and State Component history;
- Restate invocation and Operation timeline;
- Provider Resource inventory and ownership tags;
- guest readiness/activity freshness;
- Profile drift/conflicts without secret contents;
- compute intervals and credit delivery status;
- normalized and raw provider errors;
- safe repair commands.

No direct data-volume deletion action exists outside the normal Environment delete workflow.

## Alerts

Page or urgently notify on:

- missing persistent data volume;
- multiple writable attachments or multiple current Runtimes;
- duplicate or inconsistent credit debit;
- provider resource without inventory ownership;
- Environment deletion without required safety steps;
- proxy authentication bypass or unexpected public port exposure;
- RDS or Restate durability risk.

Ticket-level alerts:

- stale guest report;
- start-to-SSH SLO breach;
- repeated capacity failure;
- Polar delivery lag;
- orphaned stopped compute or volume;
- profile adapter validation failures.

## Required runbooks

- Environment creation stuck.
- Runtime running but proxy or SSH unavailable.
- Runtime stopped but compute usage interval remains open.
- Guest heartbeat stale.
- Data volume missing or attached to the wrong Runtime.
- Runtime replacement partially completed.
- Profile application blocked by drift/conflict.
- Restate workflow retrying indefinitely.
- PostgreSQL/Restate authority mismatch.
- Polar delivery failure or balance mismatch.
- Environment deletion partially completed.
- AWS capacity, IAM, NAT, or regional outage.
