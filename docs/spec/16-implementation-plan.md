# Repository layout and implementation plan

## Repository layout

```text
apps/
  cli/                    Go devm binary, building from cmd/devm/
  control-plane/          Go chi/OpenAPI HTTP API
  workflows/              Go Restate services and workflows
  ssh-proxy/              Go regional WSS-to-SSH bridge
  guest/                  Go Runtime supervisor
  web/                    TanStack Start product application
  docs/                   Public documentation site (planned — not yet created)

libs/
  domain/                 aggregates, value types, invariants
  application/            commands, queries, authorization policies
  contracts/              generated OpenAPI and internal protocol types
  db/                     pgx/sqlc queries, transactions, migrations
  capsule/                Capsule/Component model, capture, deterministic packaging, local build, locks
  profile/                Profile refs, publication, composition, and materializer plans
  adapters/               Claude, Codex, and OpenCode compiler backends
  projectseed/            Git bundle/patch/archive creation and validation
  provider/               provider interfaces and conformance suite
  provideraws/            EC2/EBS/VPC implementation
  billing/                rates, credit ledger, Polar integration
  auth/                   WorkOS verification and claims mapping
  observability/          OpenTelemetry conventions (planned — not yet created)
  testfixtures/           fake provider, fake guest, deterministic fixtures

api/
  openapi.yaml

infra/
  terraform/

images/
  packer/

docs/
  spec/
  adr/

go.mod
go.sum
go.work                  optional only if tooling later requires it
package.json
pnpm-workspace.yaml
turbo.json
```

`apps/guest`, `apps/workflows`, and `apps/ssh-proxy` are library packages today —
they have no `cmd/` entrypoint yet, and building one for each is phase-pending.
As an interim arrangement, the control-plane binary
(`apps/control-plane/cmd/control-plane/main.go`) embeds the workflows service
directly. The binary convention, once each service gets its own entrypoint, is
`apps/<svc>/cmd/<binary-name>/main.go` — as already followed by
`apps/cli/cmd/devm/` and `apps/control-plane/cmd/control-plane/`.

Use one root Go module unless a concrete tooling limitation requires `go.work`.

## Dependency direction

```text
apps → application → domain
apps → infrastructure libraries
infrastructure libraries → domain interfaces
domain → standard library only
```

Provider, billing, WorkOS, database, Restate, and transport types do not enter core domain types.

## Milestones

### M0 — Foundations

Scope:

- Turborepo/pnpm/root Go module;
- CI, formatting, linting, generation checks;
- domain types and PostgreSQL schema;
- OpenAPI generation for Go and TypeScript;
- local Restate server and fake provider;
- TanStack Start and WorkOS skeleton.

Exit: authenticated API command creates an Environment projection and completes a fake Restate workflow.

### M1 — Capsule model and local build

Scope:

- WorkOS device login;
- SSH public-key discovery/generation;
- local repository inspection and explicit capture allowlist;
- Capsule and Component model, including stable IDs, scopes, trust classes, and requirements;
- capture-to-Components;
- deterministic packaging with one OCI layer per Component;
- local Capsule build with stable digests;
- dirty Git Project Seed;
- plan UX.

Exit: a local CLI captures approved content into Components, builds a deterministic Capsule with stable digests, and produces a Project Seed without uploading Capsule content.

### M1.5 — Capsule store and Profile publication

Scope:

- regional S3 capsule store using the OCI image-layout format and short-lived presigned access;
- Capsule upload and pull with deterministic Capsule manifests and one layer per Component;
- hosted OCI Distribution registry deferred to the sharing milestone;
- Profile Version publication with ordered Capsule Refs, freshness policies, and component exclusions;
- single-capsule Capsule Lock resolution from a published Profile Version.

Exit: the local CLI publishes a Capsule, the control plane records the expected-head Profile Version with its Capsule Ref, and the Environment can resolve a single-capsule Lock.

### M2 — Regional AWS cell and private SSH

Scope:

- Terraform cell, RDS, S3, ECS services, NAT;
- Packer AMI and guest supervisor;
- AWS provider adapter;
- private EC2 Runtime with separate data volume;
- WSS regional SSH proxy and OpenSSH alias.

Exit: `ssh <alias>` reaches a private Runtime with no public IPv4 and survives stop/start.

### M3 — Environment assembly from Locks

Scope:

- Project Seed application;
- State Component layout;
- Capsule Lock consumption: pull changed Capsule layers by digest, verify them, and feed the existing plan/apply engine;
- Capsule materialization and three-way state;
- Claude adapter first, Codex adapter second;
- credential requirement UX;
- Docker/service readiness.

Exit: `devm` creates a real agent-ready Environment from a dirty repository and the selected Profile Version's Capsule Lock.

### M4 — Activity, auto-stop, and credits

Scope:

- guest process/connection observation;
- Activity Snapshots;
- Auto-stop Policy UI and Restate timers;
- Runtime usage intervals;
- credit-rate table and Credit Transactions;
- Polar subscription, credits, webhooks, outbox delivery.

Exit: agent/process conditions safely stop compute and debit the shared Credit Balance once.

### M5 — Replacement, reconciliation, and hardening

Scope:

- Runtime/system-volume replacement;
- provider, guest, Profile, and billing reconciliation;
- delete safety and private-alpha snapshot procedure;
- operator view and runbooks;
- failure injection, load, compatibility, and security testing.

Exit: canonical acceptance demo and launch gates pass.

## CI

Required jobs:

- Go format, vet, static analysis, unit tests, race tests for relevant packages;
- TypeScript lint, typecheck, unit tests, and TanStack build;
- OpenAPI lint and generated-code diff;
- sqlc generation and migration verification;
- Terraform format, validate, lint, and plan for nonproduction;
- Packer validate;
- secret scanning, dependency scanning, SBOM, and container/image scanning;
- Restate workflow replay/failure tests;
- fake-provider end-to-end test.

Real AWS tests run on explicit protected workflows with strict budgets and cleanup verification.

## Delivery strategy

- Trunk-based development with small vertical changes.
- Every milestone ships one end-to-end path before breadth.
- Feature flags gate AWS mutations, billing debit, auto-stop, and deletion independently.
- Database migrations are expand/contract and safe for rolling ECS deploys.
- Restate service deployments are versioned so in-flight workflows complete on compatible code.
- CLI maintains an API compatibility range and prompts for upgrade when unsupported.
