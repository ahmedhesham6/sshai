# Decision register

This register concludes the design-grilling session. `Accepted` entries are implementation constraints. `Open` entries link to [open decisions](./17-open-decisions.md) and must not be guessed.

## Product and scope

| Decision | Status | Specification |
|---|---|---|
| Target individual developers; no shared/team model in MVP | Accepted | [Product](./01-product.md) |
| North star is one-command agent-ready Environment with automatic compute stop and durable work | Accepted | [Product](./01-product.md) |
| User spends credits on consumed resources; stopped compute does not consume compute credits | Accepted | [Billing](./10-billing.md) |
| One primary project per Environment | Accepted | [Domain model](./02-domain-model.md) |
| Remote workspace becomes authoritative after creation | Accepted | [Profiles and projects](./04-profiles-and-projects.md) |
| No automatic bidirectional filesystem synchronization | Accepted | [Profiles and projects](./04-profiles-and-projects.md) |

## Profiles and projects

| Decision | Status | Specification |
|---|---|---|
| Profile creation is CLI-driven and explicit | Accepted | [Profiles and projects](./04-profiles-and-projects.md) |
| Local scanner proposes candidates; the user selects every included configuration item | Accepted | [Profiles and projects](./04-profiles-and-projects.md) |
| Store selectors, not blanket home-directory snapshots | Accepted | [Profiles and projects](./04-profiles-and-projects.md) |
| New CLI installation can read an existing Profile, fork it, or create another | Accepted | [Domain model](./02-domain-model.md) |
| No Device aggregate in MVP | Accepted | [Domain model](./02-domain-model.md) |
| Profiles have immutable versions; Environments pin and explicitly upgrade | Superseded | [Capsules and packaging](#capsules-and-packaging): Profile composition and Capsule Lock materialization |
| Personal Profile and repository-derived Project Spec are separate | Accepted | [Profiles and projects](./04-profiles-and-projects.md) |
| Dirty/unpushed local Git state is supported through Project Seed | Accepted | [Profiles and projects](./04-profiles-and-projects.md) |
| Materialization modes are managed, seeded, and referenced | Accepted | [Profiles and projects](./04-profiles-and-projects.md) |
| Selecting executable content does not authorize its execution | Accepted | [Security](./11-security.md) |
| `devm` does not execute skills, hooks, or plugins during sync | Accepted | [Profiles and projects](./04-profiles-and-projects.md) |
| Managed targets are drift-protected and never silently overwritten | Accepted | [Profiles and projects](./04-profiles-and-projects.md) |
| Credentials are Environment-specific bindings; Capsules and Project Specs contain requirements/references only | Accepted | [Security](./11-security.md) |
| Agent version pinning and updater policy | Open | [Open decisions](./17-open-decisions.md) |

## Capsules and packaging

| Decision | Status | Specification |
|---|---|---|
| Capsules are OCI artifacts with one layer per Component | Accepted | [Capsule packaging and distribution](./19-capsule-packaging.md) |
| Profile is redefined as an ordered group of Capsule Refs (ops objection on migration cost recorded and overridden) | Accepted | [Capsules, profiles, and project seeding](./04-profiles-and-projects.md) |
| Environments materialize only from Capsule Locks | Accepted | [Domain model](./02-domain-model.md) |
| MVP serves Capsules from content-addressed S3 using the OCI image-layout format behind short-lived presigned GETs minted by the control plane and scoped per owner prefix; hosted OCI Distribution (Zot), external registries, and signing are deferred to the sharing milestone | Accepted | [Capsule packaging and distribution](./19-capsule-packaging.md) |
| Capsule distribution has no Referrers API dependency | Accepted | [Capsule packaging and distribution](./19-capsule-packaging.md) |
| Use oras-go v2 | Accepted | [Capsule packaging and distribution](./19-capsule-packaging.md) |
| Deterministic packaging is a CI gate | Accepted | [Capsule packaging and distribution](./19-capsule-packaging.md) |
| Permission Components are always re-consented | Accepted | [Security](./11-security.md) |
| Project-scope Components are seeded only | Accepted | [Capsules, profiles, and project seeding](./04-profiles-and-projects.md) |
| Signing is deferred | Accepted | [Capsule packaging and distribution](./19-capsule-packaging.md) |
| Component trust class is digest-bound | Accepted | [Security](./11-security.md) |
| Integration and Credential Requirement changes are never auto_safe | Accepted | [Profiles and projects](./04-profiles-and-projects.md) |
| Determinism gate pins the Go toolchain and strips xattrs | Accepted | [Capsule packaging and distribution](./19-capsule-packaging.md) |
| Environments reference only Capsules owned by the authenticated user until signing ships | Accepted | [Security](./11-security.md) |

## Environment, Runtime, and state

| Decision | Status | Specification |
|---|---|---|
| Environment, Runtime, and State Component are separate models | Accepted | [Domain model](./02-domain-model.md) |
| State components are workspace, home, services, and cache | Accepted | [Runtime and storage](./05-runtime-and-storage.md) |
| System root is replaceable | Accepted | [Runtime and storage](./05-runtime-and-storage.md) |
| One Environment has at most one current writable Runtime | Accepted | [Domain model](./02-domain-model.md) |
| Multiple connections and agent processes may share one Runtime | Accepted | [Activity](./06-activity-and-autostop.md) |
| No first-class Session aggregate | Accepted | [Domain model](./02-domain-model.md) |
| Environment lifecycle, Environment health, Runtime status, and Operation progress are orthogonal | Accepted | [Domain model](./02-domain-model.md) |
| Normal close stops EC2 rather than terminating it | Accepted | [Runtime and storage](./05-runtime-and-storage.md) |
| Storage remains billable while compute is stopped | Accepted | [Billing](./10-billing.md) |
| Separate replaceable system and persistent data EBS volumes | Accepted | [Runtime and storage](./05-runtime-and-storage.md) |
| Packer-built versioned AMI from hosted alpha | Accepted | [Infrastructure](./13-infrastructure.md) |
| RecoveryPoint is not a core MVP aggregate; backup is an operational alpha capability | Accepted | [Open decisions](./17-open-decisions.md) |

## Automatic stop

| Decision | Status | Specification |
|---|---|---|
| Auto-stop Policy is stored per Environment with optional Profile default | Accepted | [Activity](./06-activity-and-autostop.md) |
| User chooses the closing condition | Accepted | [Activity](./06-activity-and-autostop.md) |
| Conditions may use connections, Codex/Claude processes, protected processes, and containers | Accepted | [Activity](./06-activity-and-autostop.md) |
| Process observation feeds policy evaluation; CPU alone is insufficient | Accepted | [Activity](./06-activity-and-autostop.md) |
| Live Codex/Claude process counts as active even while waiting for input | Accepted | [Activity](./06-activity-and-autostop.md) |
| Exact defaults, cadence, and grace periods | Open | [Open decisions](./17-open-decisions.md) |

## Access and networking

| Decision | Status | Specification |
|---|---|---|
| Standard OpenSSH remains the client contract | Accepted | [SSH and networking](./07-ssh-and-networking.md) |
| Runtimes have no public IPv4 | Accepted | [SSH and networking](./07-ssh-and-networking.md) |
| Regional proxy bridges authenticated WSS to private Runtime SSH | Accepted | [SSH and networking](./07-ssh-and-networking.md) |
| CLI may use an existing Ed25519 public key or create a dedicated key | Accepted | [SSH and networking](./07-ssh-and-networking.md) |
| One managed NAT gateway supplies MVP regional egress | Accepted | [Infrastructure](./13-infrastructure.md) |
| Architecture supports regional cells; private alpha operates one region | Accepted | [Infrastructure](./13-infrastructure.md) |
| Environment region is immutable in MVP | Accepted | [Domain model](./02-domain-model.md) |
| Initial region, AZ, and Runtime Preset mappings | Open | [Open decisions](./17-open-decisions.md) |

## Control plane and technology

| Decision | Status | Specification |
|---|---|---|
| Turborepo monorepo | Accepted | [Implementation plan](./16-implementation-plan.md) |
| Go for CLI, API, workflows, proxy, guest, providers, and billing | Accepted | [Architecture](./03-architecture.md) |
| TanStack Start for product web application | Accepted | [Architecture](./03-architecture.md) |
| Documentation website exists; framework undecided | Partially accepted | [Product surfaces](./12-product-surfaces.md) |
| PostgreSQL owns product data; Restate owns durable execution | Accepted | [Workflows](./08-workflows.md) |
| Restate Cloud for MVP | Accepted | [Architecture](./03-architecture.md) |
| RDS PostgreSQL | Accepted | [Infrastructure](./13-infrastructure.md) |
| pgx + sqlc + goose | Accepted | [Architecture](./03-architecture.md) |
| Contract-first OpenAPI with chi and oapi-codegen | Accepted | [API](./09-api.md) |
| Fiber rejected for the control-plane API | Accepted | [Architecture](./03-architecture.md) |
| ECS Fargate for Go control-plane services | Accepted | [Infrastructure](./13-infrastructure.md) |
| WorkOS AuthKit with hosted UI and CLI Auth | Accepted | [Security](./11-security.md) |

## Billing

| Decision | Status | Specification |
|---|---|---|
| Polar handles subscriptions and external credit meter | Accepted | [Billing](./10-billing.md) |
| Subscription grants one shared Credit Balance | Accepted | [Billing](./10-billing.md) |
| Compute and storage convert through type-specific rates then debit the same pool | Accepted | [Billing](./10-billing.md) |
| Do not model separate compute/storage balances | Accepted | [Billing](./10-billing.md) |
| Zero-balance behavior | Open | [Open decisions](./17-open-decisions.md) |
