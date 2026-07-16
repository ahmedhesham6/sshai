# Dev Environments implementation specification

Status: **implementation baseline**
Product stage: **private alpha**
Primary customer: **individual developers**

This directory is the canonical implementation specification for the product. It supersedes the older machine-centric MVP document while retaining its proven foundations: standard SSH, durable asynchronous operations, provider reconciliation, explicit resource ownership, failure recovery, and strong persistence tests.

## North star

Run one command in a repository to enter an agent-ready remote development environment with a chosen profile. When work ends, compute stops automatically, durable work remains, and the user spends subscription credits only for the resources they consume.

## Specification map

1. [Product and scope](./01-product.md)
2. [Domain model](./02-domain-model.md)
3. [System architecture and technology](./03-architecture.md)
4. [Profiles and project seeding](./04-profiles-and-projects.md)
5. [Environment, runtime, and storage](./05-runtime-and-storage.md)
6. [Activity detection and auto-stop](./06-activity-and-autostop.md)
7. [SSH access, proxy, and networking](./07-ssh-and-networking.md)
8. [Durable operations and Restate workflows](./08-workflows.md)
9. [Control-plane API](./09-api.md) and [OpenAPI contract](../../api/openapi.yaml)
10. [Billing and Polar credits](./10-billing.md)
11. [Security and trust](./11-security.md)
12. [Product surfaces and UX](./12-product-surfaces.md)
13. [Infrastructure and regional cells](./13-infrastructure.md)
14. [Observability and support](./14-operations.md)
15. [Testing and acceptance](./15-testing.md)
16. [Repository layout and implementation plan](./16-implementation-plan.md)
17. [Open decisions](./17-open-decisions.md)
18. [Decision register](./18-decision-register.md)
19. [Capsule packaging and distribution](./19-capsule-packaging.md)

The root [CONTEXT.md](../../CONTEXT.md) is the canonical glossary. Hard-to-reverse architectural decisions are recorded in [docs/adr](../adr/).

## Authority rules

- Product and domain terminology in this specification overrides older planning documents.
- OpenAPI is authoritative for the public HTTP interface.
- PostgreSQL is authoritative for product state and ledgers.
- Restate is authoritative for workflow execution and durable timers.
- Cloud-provider state is observed state and must be reconciled against product intent.
- Files inside a running Environment are authoritative for ongoing project work after initial creation.
- An item in [open decisions](./17-open-decisions.md) is not implementation authorization.

## Locked product decisions

- Individual users only; no shared environments, profiles, or team policy in the MVP.
- One primary project per Environment.
- Profiles are named, reusable, ordered groups of Capsule Refs (proposed).
- Environments materialize only from Capsule Locks and upgrade only through explicit operations (proposed).
- The remote workspace becomes authoritative after creation; there is no automatic bidirectional file synchronization.
- A Runtime may serve many concurrent connections and processes, but an Environment has at most one current writable Runtime.
- Auto-stop behavior is selected per Environment, with an optional Profile default.
- Provider compute is stopped, not terminated, during normal automatic close.
- Runtimes are private-only and reached through an authenticated WebSocket SSH proxy.
- State and system disks are separate.
- One AWS region is enabled for private alpha; regional cells are part of every contract from day one.
- Billing uses one shared subscription credit pool for compute, storage, and future billable resource types.
