# Repository layout and implementation plan

## Repository layout

```text
apps/
  cli/                    Go devm binary and local scanner
  control-plane/          Go chi/OpenAPI HTTP API
  workflows/              Go Restate services and workflows
  ssh-proxy/              Go regional WSS-to-SSH bridge
  guest/                  Go Runtime supervisor
  web/                    TanStack Start product application
  docs/                   Public documentation site

libs/
  domain/                 aggregates, value types, invariants
  application/            commands, queries, authorization policies
  contracts/              generated OpenAPI and internal protocol types
  db/                     pgx/sqlc queries, transactions, migrations
  profile/                discovery, compilation, adapters, materializer plans
  projectseed/            Git bundle/patch/archive creation and validation
  provider/               provider interfaces and conformance suite
  provideraws/            EC2/EBS/VPC implementation
  billing/                rates, credit ledger, Polar integration
  auth/                   WorkOS verification and claims mapping
  observability/          OpenTelemetry conventions
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

### M1 — CLI, Profile, and Project Seed

Scope:

- WorkOS device login;
- SSH public-key discovery/generation;
- local repository inspection;
- explicit Profile allowlist compiler;
- immutable artifact upload;
- dirty Git Project Seed;
- plan UX.

Exit: a local CLI produces deterministic Profile and Project Seed digests and registers them through the API.

### M2 — Regional AWS cell and private SSH

Scope:

- Terraform cell, RDS, S3, ECS services, NAT;
- Packer AMI and guest supervisor;
- AWS provider adapter;
- private EC2 Runtime with separate data volume;
- WSS regional SSH proxy and OpenSSH alias.

Exit: `ssh <alias>` reaches a private Runtime with no public IPv4 and survives stop/start.

### M3 — Environment assembly

Scope:

- Project Seed application;
- State Component layout;
- Profile materialization and three-way state;
- selected Codex/Claude installation and validation;
- credential requirement UX;
- Docker/service readiness.

Exit: `devm` creates a real agent-ready Environment from a dirty repository and selected Profile.

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
