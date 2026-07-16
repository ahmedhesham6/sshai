# System architecture and technology

## Architectural shape

```text
Local machine
  devm CLI
    ├── WorkOS CLI Auth
    ├── local discovery/profile compiler
    ├── OpenAPI control-plane client
    └── OpenSSH ProxyCommand bridge
             │ HTTPS / WSS
             ▼
Global product plane
  TanStack Start web ───────► Go control-plane API
                                  │
                                  ├── RDS PostgreSQL
                                  ├── Polar
                                  ├── WorkOS JWT/JWKS
                                  └── Restate Cloud ingress
                                           │
                                           ▼
                               Go Restate workflow service
                                           │ AWS SDK / regional config
                                           ▼
Regional data-plane cell
  public ALB/NLB ─► WebSocket SSH proxy ─► private Runtime:22
                             │
  private subnets ───────────┼── EC2 Runtime
                             ├── system EBS
                             ├── persistent data EBS
                             ├── capsule store (S3, OCI layout)
                             └── NAT egress
```

## Monorepo

- Turborepo orchestrates Go and TypeScript build, test, lint, code generation, and release tasks.
- A single root Go module avoids internal module versioning.
- pnpm workspaces manage web and documentation dependencies.
- OpenAPI and SQL are source artifacts; generated code is reproducible and checked in only when the chosen generators require it for consumer builds.

## Technology baseline

| Concern | Technology | Contract |
|---|---|---|
| CLI, API, workflows, proxy, guest | Go | One root module |
| Web application | TanStack Start + React | Product control surface |
| Documentation site | Undecided; see open decisions | Static public docs |
| Durable execution | Restate Cloud + Go SDK | Workflow authority |
| Product database | Amazon RDS PostgreSQL | Product-state authority |
| Capsule packaging | oras-go v2 (≥2.6.2) | OCI Capsule manifests and layers |
| Capsule registry | S3-backed OCI image layout with short-lived presigned access | MVP content-addressed Capsule storage; hosted OCI Distribution registry deferred to sharing milestone |
| Database access | pgx + sqlc | Explicit SQL |
| Migrations | goose | Ordered SQL migrations |
| Public API | OpenAPI 3.0, chi, oapi-codegen | Contract-first REST |
| Authentication | WorkOS AuthKit | Hosted web + CLI device flow |
| Billing | Polar | Subscription credit meter |
| Managed compute | Amazon EC2 | One provider in alpha |
| Persistent storage | Amazon EBS gp3 | Separate system/data volumes |
| Images | Packer-built Ubuntu 24.04 AMI | Versioned image |
| Service deployment | ECS Fargate | API, workflow endpoint, proxy |
| Infrastructure as code | Terraform | Platform and regional cells |
| Observability | OpenTelemetry | Vendor-neutral telemetry |

## Service boundaries

### `devm` CLI

Owns local authentication tokens, repository inspection, Profile selection and compilation, Project Seed packaging, local SSH identity selection, OpenSSH config generation, user confirmations, and connection UX. It never holds AWS credentials.

### Control-plane API

Owns authorization, validation, idempotency, read models, command acceptance, and Restate workflow invocation. It does not perform AWS mutations inside HTTP request handlers.

### Restate workflow service

Owns Environment operations, provider mutations, durable timers, reconciliation, Polar delivery, and recovery logic. Workflows call PostgreSQL through durable actions and must use deterministic operation IDs and idempotency tokens for external calls.

### Regional SSH proxy

Authenticates a WorkOS access token, authorizes Environment ownership, starts a stopped Runtime through the control plane, waits for SSH readiness, and bridges a secure WebSocket byte stream to the Runtime's private port 22. It does not terminate the SSH protocol or see decrypted SSH contents.

### Guest supervisor

Runs as a systemd service. It mounts and validates state paths, pulls Capsule layers by digest through the guest's mTLS channel, materializes an approved Capsule Lock, reports readiness and Activity Snapshots, tracks user/agent processes, and exposes a mutually authenticated control channel. It cannot mutate billing or Environment ownership.

### Capsule store

The MVP capsule store is content-addressed S3 using the OCI image-layout format. The
control plane mints short-lived presigned GETs scoped per owner prefix, and the guest
pulls Capsule layers by digest through those grants. The oras-go client uses its `Target`
abstraction so the client is identical across OCI image-layout and remote registry
backends. The hosted OCI Distribution registry is deferred to the sharing milestone,
alongside external registries and signing. Profile composition and Capsule Locks remain
PostgreSQL state.

### Provider adapter

Translates Environment and Runtime intent into EC2, EBS, VPC, security-group, and image operations. Provider-specific types and errors do not leak into public API contracts.

### Billing package

Converts measured resource quantities through a versioned credit-rate table, appends Credit Transactions, and reliably delivers one abstract-credit event stream to Polar.

## Authority and consistency

| State | Authority | Reconciliation target |
|---|---|---|
| User identity/session | WorkOS | Local User projection |
| Subscription and external meter | Polar | Local subscription/balance projection |
| Product intent and ledgers | PostgreSQL | None |
| Workflow journal/timers | Restate | Operation projection |
| Provider resources | AWS observed state | PostgreSQL intent/resource inventory |
| Environment filesystem | Persistent data volume | State Component health |
| Capsule content | S3 capsule store | Content-addressed |
| Profile composition and Capsule Locks | PostgreSQL | Materialization observation |

## Availability boundary

- The global API may be unavailable while already-running SSH streams continue.
- The regional proxy must remain available for new connections, but an existing OpenSSH connection survives control-plane read outages after authorization and routing are complete.
- Restate unavailability pauses mutations and timers; it must not cause duplicate AWS or billing actions.
- RDS unavailability blocks new commands and billing projection but must not destroy running Runtimes.
