# AGENTS.md

## What this repo is

`sshai` builds `devm`: run one command in a repository to enter an
agent-ready remote development Environment with a chosen profile. Start at
`docs/spec/README.md` for the implementation specification, and use the
vocabulary defined in `CONTEXT.md` for domain terms — each term there has an
*Avoid* list; don't substitute a near-synonym.

## Layout

- `apps/` — deployable services. Binaries build from
  `apps/<svc>/cmd/<binary-name>/` (exemplar:
  `apps/control-plane/cmd/control-plane/`; the CLI's binary lives at
  `apps/cli/cmd/devm/` once plan 003 lands). `apps/guest`, `apps/ssh-proxy`,
  and `apps/workflows` are currently library packages without their own
  `cmd/` entrypoints, consumed by those binaries.
- `libs/` — shared Go packages (`domain`, `application`, `capsule`, `db`,
  `auth`, `billing`, `profile`, `provider`, `provideraws`, `projectseed`,
  `testfixtures`) plus `libs/contracts`, the generated TS/Go API contracts
  package.
- `api/` — `api/openapi.yaml`, the OpenAPI source of truth (linted by
  `pnpm contract:lint`).
- `docs/spec/` — the canonical implementation specification; `docs/adr/` —
  architecture decision records.
- `infra/terraform/` — Terraform modules, one per infra concern, each with
  its own `tests/*.tftest.hcl`.
- `images/packer/` — Packer image definitions.
- `plans/` — implementation plans for agent-driven work; read
  `plans/README.md` before any structural change.

## Build model

The repo is a single root Go module (`github.com/ahmedhesham6/sshai`,
`go 1.26.5`) wrapped in a pnpm workspace (`apps/*`, `libs/*`). Turborepo
drives Go builds/tests through a `package.json` shim in every Go package
directory whose scripts wrap `go build`/`go vet`/`go test` (exemplar:
`apps/guest/package.json`, see its `build`/`lint`/`test`/`generate`
scripts). The root gate is `pnpm check`, which expands to
`pnpm contract:lint && pnpm format:check && pnpm lint && pnpm test`. Run
`pnpm generate` for codegen (`go generate ./...` per package plus
`tsr generate` for TanStack Start routes in `apps/web`).

## Running the surfaces

- CLI: `go run ./apps/cli/cmd/devm <command>`. Reads two env vars
  (`apps/cli/cmd/devm/main.go`): `DEVM_WORKOS_CLIENT_ID` and
  `DEVM_CONTROL_PLANE_URL`.
- Web: `cd apps/web && cp .env.example .env`, then `pnpm dev` (Vite).
  `.env.example` names the four WorkOS vars — `WORKOS_REDIRECT_URI`,
  `WORKOS_API_KEY`, `WORKOS_CLIENT_ID`, `WORKOS_COOKIE_PASSWORD` — with
  placeholder values; substitute real ones.

## Testing gotchas

- Guest tests require `umask 077` before running (`apps/guest`'s
  materialization state touches file permissions the tests assert on).
- Terraform modules carry `tests/*.tftest.hcl`, run per-module with
  `terraform init -backend=false && terraform test`.
- The Go race suite runs via `pnpm test:race` (`turbo run test:race`).

## Conventions

- Conventional commits scoped to the touched package, e.g.
  `feat(guest): OpenCode adapter` or `fix(guest): close adversarial-gate
  findings on adapter approval and TOML engine` — check `git log` for more
  examples before writing a new one.
- Generated files are edited only via `pnpm generate`, never by hand:
  `*.gen.go` (e.g. `libs/contracts/api.gen.go`), `libs/db/internal/dbsql`,
  and `libs/contracts/src/generated`.
