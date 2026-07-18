# Control-plane API

The public HTTP API is contract-first and resource-oriented. The CLI and TanStack Start web application use generated clients from [api/openapi.yaml](../../api/openapi.yaml).

## Conventions

- Base path `/v1`.
- JSON request and response bodies.
- WorkOS bearer access tokens.
- `Idempotency-Key` required on mutating commands.
- `202 Accepted` for commands that start Restate workflows.
- RFC 3339 UTC timestamps.
- Opaque string identifiers with stable prefixes.
- Cursor pagination for collections.
- Not-yet-populated references are `null`, never empty-string sentinels (`capsuleLockId` is nullable until a lock is pinned; 2026-07-18).
- Every response includes `X-Request-ID`.
- Product-level errors never use raw AWS errors as the primary message.

## Resource groups

### Identity and billing

- `GET /v1/me`
- `GET /v1/billing`
- `POST /v1/billing/portal`
- `POST /v1/capsule-access`

The capsule-access endpoint mints short-lived, owner-scoped presigned pull/push grants
for the S3 capsule store from the authenticated WorkOS session. The registry-token
endpoint arrives with the hosted OCI Distribution registry at the sharing milestone.

### SSH keys

- `GET /v1/ssh-keys`
- `POST /v1/ssh-keys`
- `DELETE /v1/ssh-keys/{keyId}`

### Profiles

- `GET /v1/profiles`
- `POST /v1/profiles`
- `GET /v1/profiles/{profileId}`
- `POST /v1/profiles/{profileId}/versions`
- `GET /v1/profile-versions/{versionId}`

Profile publication includes the expected current head and an ordered list of Capsule Refs. Each Capsule Ref contains a registry reference (tag or digest), freshness policy, and component exclusions. Publication payloads reference Capsules rather than embedding content arrays.

MVP Capsules are served from the S3 capsule store through short-lived owner-scoped
presigned grants minted by the control plane; the control plane does not proxy Capsule
object traffic through the public API. The hosted OCI Distribution registry arrives at
the sharing milestone, alongside its registry-token endpoint.

### Uploads and Project Seeds

- `POST /v1/uploads` creates a scoped, short-lived object upload intent.
- `POST /v1/project-seeds` registers a completed immutable Project Seed manifest.

The control plane validates object digests, ownership, size, and type before accepting references.

### Environments

- `GET /v1/environments`
- `POST /v1/environments`
- `GET /v1/environments/{environmentId}`
- `POST /v1/environments/{environmentId}/start`
- `POST /v1/environments/{environmentId}/stop`
- `POST /v1/environments/{environmentId}/replace-runtime`
- `POST /v1/environments/{environmentId}/apply-profile`
- `PUT /v1/environments/{environmentId}/auto-stop-policy`
- `DELETE /v1/environments/{environmentId}`

Environment detail includes the pinned `ProfileVersionID`, `LockID`, pending Capsule updates, and drift/conflicts. The Capsule Lock contains exact Capsule digests and the resolved component map; Environments materialize only from a Lock.

### Connection

- `POST /v1/environments/{environmentId}/connection-intents`

A Connection Intent authorizes one short-lived attempt, returns the regional WSS endpoint, stable logical host name, and Runtime/start Operation state. It is not an SSH credential.

Updating the Auto-stop Policy is an asynchronous Environment Operation because it must durably cancel or replace pending Restate timers.

### Operations and events

- `GET /v1/operations/{operationId}`
- `GET /v1/environments/{environmentId}/events`

Polling is sufficient for the first CLI. Server-Sent Events may be added for web progress without changing operation semantics.

## Authorization

Every resource is owned by the authenticated WorkOS user projection. A missing foreign resource returns `404` to avoid ownership disclosure. No team or organization scope is accepted.

## Idempotency

- Scope: authenticated user plus `Idempotency-Key`.
- Same key and identical canonical input returns the existing Operation.
- Same key and different input returns `409 IDEMPOTENCY_CONFLICT`.
- Connection Intent creation may use a short deterministic key derived from Environment and CLI attempt.

## Errors

```json
{
  "requestId": "req_01J...",
  "error": {
    "code": "OPERATION_CONFLICT",
    "message": "api-dev is currently stopping.",
    "operationId": "op_01J...",
    "safeState": "Persistent state remains attached and intact.",
    "nextAction": "Wait for the current operation to finish."
  }
}
```

Status mapping:

- `400`: malformed or invalid input;
- `401`: invalid/expired authentication;
- `403`: active subscription or policy denial where existence is already known;
- `404`: resource absent or not owned;
- `409`: idempotency, version, or active-operation conflict;
- `422`: a valid request cannot be applied to current domain state;
- `429`: user or platform quota/rate limit;
- `503`: regional or dependency unavailability.

## Internal APIs

Guest readiness/activity, WorkOS webhooks, Polar webhooks, and Restate service endpoints are not part of the public `/v1` API. They use separate hosts or paths, authentication policies, rate limits, and generated contracts.
