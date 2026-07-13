# Activity detection and automatic stop

## Goal

Stop compute when the developer's selected definition of “finished” remains true, without treating low CPU usage as proof that work is complete.

## Policies

Initial `AutoStopPolicy.mode` values:

| Mode | Stop condition |
|---|---|
| `when_disconnected` | No SSH or recognized IDE connections |
| `when_agents_finish` | No running Codex or Claude process trees |
| `when_fully_idle` | No connections, recognized agents, protected processes, selected containers, or unknown user processes |
| `manual` | Never stop automatically |

Every non-manual policy has a configurable grace period. The web and CLI display the exact predicate in plain language.

## Guest observation

The guest supervisor periodically reports an `ActivitySnapshot` containing counts and bounded metadata, not arbitrary command lines or environment variables.

Sources:

- active sshd sessions;
- known IDE remote-server connections;
- Codex and Claude executable process trees;
- user processes placed in systemd scopes/cgroups;
- explicitly protected background processes;
- selected Docker containers;
- unknown user-owned processes.

Baseline operating-system and platform services are excluded from activity.

## Agent detection

- A live Codex or Claude process counts as active even while waiting for input.
- Descendants belong to the same recognized process tree.
- CPU inactivity does not end agent activity.
- A process that changes executable identity or escapes its tracked cgroup becomes unknown activity.
- Versioned detectors are shipped with the guest supervisor and reported in snapshot metadata.

## Evaluation flow

1. Guest reports an Activity Snapshot with a monotonic sequence.
2. The control plane stores the latest snapshot and appends significant transitions to audit telemetry.
3. A Restate virtual object keyed by Environment evaluates the selected policy.
4. If the condition is false, any pending grace timer is cancelled.
5. If true, Restate starts or continues a durable grace timer.
6. At expiry, the workflow requests a fresh snapshot.
7. If still true and the Runtime has no conflicting Operation, Restate starts `runtime.stop` with reason `auto_stop`.
8. The stop Operation records the policy, qualifying snapshots, and grace interval.

## Safety rules

- A stale or missing guest report blocks automatic stop.
- Unknown user processes block `when_fully_idle`.
- Setup, materialization, start, replacement, and restore workflows suppress automatic stop.
- A manual stop bypasses the policy but still performs graceful shutdown.
- Runtime termination is never an automatic-stop action.
- The user may change policy at any time; changes are audited and reset pending evaluation timers.

## UX

`devm status` and the web app show:

```text
Auto-stop: when Codex and Claude finish
Activity:  1 Claude process, 0 Codex processes
Decision:  runtime remains active
```

Before a pending stop:

```text
No selected agent processes detected.
Runtime will stop in 10 minutes unless activity resumes.
```

Exact default grace periods and polling cadence remain open implementation parameters.
