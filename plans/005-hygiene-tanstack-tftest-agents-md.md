# Plan 005: Structure hygiene — ignore `.tanstack/`, add the missing capsule-store tftest, write root `AGENTS.md`

> **Executor instructions**: Follow this plan step by step. Run every
> verification command and confirm the expected result before moving to the
> next step. If anything in the "STOP conditions" section occurs, stop and
> report — do not improvise. When done, update the status row for this plan
> in `plans/README.md` — unless a reviewer dispatched you and told you they
> maintain the index.
>
> **Drift check (run first)**: `git diff --stat c2c4e2d..HEAD -- .gitignore infra/terraform/modules/capsule-store AGENTS.md`
> If any in-scope file changed since this plan was written, compare the
> "Current state" facts against the live tree before proceeding; on a
> mismatch, treat it as a STOP condition.

## Status

- **Priority**: P1 (cheap, independent, closes a latent commit accident and a test-convention hole)
- **Effort**: S
- **Risk**: LOW
- **Depends on**: none
- **Category**: dx
- **Planned at**: commit `c2c4e2d`, 2026-07-17

## Why this matters

Three small, unrelated-looking gaps share one theme: the repo's conventions
have holes exactly where a newcomer (human or agent) falls in. (1) TanStack
Start's scratch dir `apps/web/.tanstack/` is the only tool scratch dir not
gitignored — verified: `git check-ignore apps/web/.tanstack/tmp` exits 1 — so
it can be accidentally committed. (2) Every Terraform module has a
`tests/*.tftest.hcl` except `capsule-store`, the module implementing the
ratified S3 Capsule store (ADR 0009) — the convention has a hole at the most
security-relevant module. (3) There is no root `AGENTS.md`/`CLAUDE.md`, and
the build model is genuinely non-obvious (Go driven through per-package
`package.json` shims by Turborepo; guest tests require `umask 077`), so every
agent session re-derives it.

## Current state

- Root `.gitignore` (verbatim, complete, at `c2c4e2d`):

  ```
  .turbo/
  node_modules/
  dist/
  coverage/
  *.out
  apps/cli/cli
  .env
  .env.local
  ```

  (If plans/003 already landed, the `apps/cli/cli` line reads
  `apps/cli/cmd/devm/devm` — either way, leave that line alone.)

- `infra/terraform/modules/capsule-store/` contains `main.tf` (100 lines),
  `outputs.tf`, `variables.tf` (`bucket_name`, `tags`), `versions.tf` — and no
  `tests/`. Resources in `main.tf` (verified names):
  - `aws_s3_bucket.capsules` (line 1)
  - `aws_s3_bucket_versioning.capsules` (line 8)
  - `aws_s3_bucket_server_side_encryption_configuration.capsules` (line 19)
  - `aws_s3_bucket_public_access_block.capsules` (line 29)
  - `aws_s3_bucket_lifecycle_configuration.capsules` (line 38)
  - `aws_iam_policy.control_plane_presigning` (line 73)
  - `aws_s3_bucket_policy.secure_transport` (line 80)

- Sibling exemplar `infra/terraform/modules/object-storage/tests/artifacts.tftest.hcl`
  starts:

  ```hcl
  mock_provider "aws" {}

  variables {
    bucket_name = "sshai-development-us-east-1-artifacts"
    tags = {
      managed-by = "terraform"
      project    = "sshai"
    }
  }

  run "protects_immutable_artifacts" {
    command = apply

    assert {
      condition     = aws_s3_bucket.artifacts.bucket == var.bucket_name && !aws_s3_bucket.artifacts.force_destroy
  ```

- Repo build model facts for AGENTS.md (all verified): single root Go module
  `github.com/ahmedhesham6/sshai` (go 1.26.5); pnpm workspace globs `apps/*`,
  `libs/*`; every Go package dir carries a `package.json` shim whose scripts
  wrap `go build/vet/test` so Turborepo can orchestrate (exemplar:
  `apps/guest/package.json`); root gate is `pnpm check` = `contract:lint`
  (redocly on `api/openapi.yaml`) + `format:check` + `lint` + `test`;
  `pnpm generate` runs codegen (`go generate`, `tsr generate`); guest tests
  need `umask 077`; domain vocabulary lives in `CONTEXT.md`; specs in
  `docs/spec/`, ADRs in `docs/adr/`; implementation plans in `plans/`.

## Commands you will need

| Purpose | Command | Expected on success |
|---|---|---|
| Ignore check | `git check-ignore apps/web/.tanstack/tmp` | prints the path, exit 0 (after fix) |
| TF init | `cd infra/terraform/modules/capsule-store && terraform init -backend=false` | exit 0 |
| TF test | `cd infra/terraform/modules/capsule-store && terraform test` | all runs pass |
| Root gate (unaffected) | `pnpm format:check` | exit 0 |

## Scope

**In scope**:
- `.gitignore` (add one line)
- `infra/terraform/modules/capsule-store/tests/store.tftest.hcl` (create)
- `AGENTS.md` at repo root (create)

**Out of scope** (do NOT touch):
- `main.tf` or any other Terraform source — if an assertion can't be written
  against the existing resources, that's a STOP, not a resource edit.
- `CONTEXT.md`, `docs/spec/*` — AGENTS.md links to them, never duplicates
  their content.
- `CLAUDE.md` — do not create a second file; `AGENTS.md` is the single
  cross-agent convention file.

## Git workflow

- Branch: `advisor/005-structure-hygiene`.
- One commit, style: `chore: ignore .tanstack, add capsule-store tftest, add AGENTS.md`
  (or three scoped commits if the operator prefers granular history).
- Do NOT push or open a PR unless the operator instructed it.

## Steps

### Step 1: Ignore `.tanstack/`

Add the line `.tanstack/` to root `.gitignore` (alongside `.turbo/`).

**Verify**: `git check-ignore apps/web/.tanstack/tmp` → prints the path,
exit 0.

### Step 2: Add `capsule-store/tests/store.tftest.hcl`

Model directly on the exemplar excerpt above: `mock_provider "aws" {}`, a
`variables` block (`bucket_name = "sshai-development-us-east-1-capsules"`,
same `tags` map), then `run` blocks with `command = apply`. Write assertions
against the actual resources listed in Current state; read
`infra/terraform/modules/capsule-store/main.tf` first and assert on the
attributes it really sets. Minimum coverage (one `run` block each, or
grouped):

1. Bucket name matches `var.bucket_name` and `force_destroy` is false (if the
   attribute is set; otherwise assert the name only).
2. Versioning configuration is `Enabled`.
3. Server-side encryption configuration exists with the algorithm `main.tf`
   configures.
4. Public access block: all four flags true (assert the flags `main.tf`
   actually sets).
5. The secure-transport bucket policy's JSON contains a `Deny` with
   `aws:SecureTransport` = `false` (use `strcontains(...)` on
   `aws_s3_bucket_policy.secure_transport.policy` as the assertion).

**Verify**: `cd infra/terraform/modules/capsule-store && terraform init
-backend=false && terraform test` → all runs pass.

### Step 3: Write root `AGENTS.md`

Create `AGENTS.md` (target 40–70 lines) with exactly these sections, drawing
only on the verified facts in Current state (do not invent commands):

1. **What this repo is** — two sentences: `devm`, agent-ready remote dev
   Environments; pointer to `docs/spec/README.md` and `CONTEXT.md` ("use the
   vocabulary defined there; each term has an *Avoid* list").
2. **Layout** — one line each for `apps/` (deployable services; binaries
   build from `apps/<svc>/cmd/<name>/`), `libs/` (shared Go + the generated
   TS contracts package), `api/` (OpenAPI source of truth), `docs/spec` +
   `docs/adr`, `infra/terraform`, `images/packer`, `plans/` (implementation
   plans — read `plans/README.md` before structural work).
3. **Build model** — the non-obvious part: single root Go module; Turborepo
   drives Go through per-package `package.json` shims (point at
   `apps/guest/package.json` as the exemplar); root gate `pnpm check` and
   what it expands to; `pnpm generate` for codegen.
4. **Testing gotchas** — guest tests require `umask 077`; Terraform modules
   carry `tests/*.tftest.hcl` run with `terraform test`; Go race suite via
   `pnpm test:race`.
5. **Conventions** — conventional commits with package scope (give one real
   example from `git log`); generated files (`*.gen.go`, `libs/db/internal/dbsql`,
   `libs/contracts/src/generated`) are edited only via `pnpm generate`.

**Verify**: `test -f AGENTS.md && wc -l AGENTS.md` → file exists, 30–100
lines; every command named in it appears verbatim in either root
`package.json` scripts or this plan's Current state facts.

### Step 4: Commit

**Verify**: `git status --short` shows exactly the three in-scope paths;
`pnpm format:check` → exit 0 (confirms no formatter complaint introduced).

## Test plan

Step 2 *is* new test coverage (the module's first tftest — five assertions
listed there, modeled on `object-storage/tests/artifacts.tftest.hcl`).
Steps 1 and 3 are verified by their step checks; no further tests apply.

## Done criteria

- [ ] `git check-ignore apps/web/.tanstack/tmp` → exit 0
- [ ] `infra/terraform/modules/capsule-store/tests/store.tftest.hcl` exists and `terraform test` passes in that module
- [ ] `AGENTS.md` exists at root, 30–100 lines, all five sections present
- [ ] `git status --short` shows only the three in-scope paths
- [ ] `plans/README.md` status row updated

## STOP conditions

Stop and report back (do not improvise) if:

- `terraform` is not installed or `terraform init -backend=false` cannot run
  offline in this environment — report; do not commit an unexecuted test file.
- An assertion from Step 2's minimum list has no corresponding attribute in
  `main.tf` — report the gap (it may be a real infra finding) instead of
  editing `main.tf`.
- A `CLAUDE.md` or `AGENTS.md` appears at root after the drift check — merge
  intent conflict; report instead of overwriting.

## Maintenance notes

- AGENTS.md is a map, not a mirror: it links to `CONTEXT.md`/specs rather than
  restating them, so it only goes stale when commands change — reviewers
  should require an AGENTS.md touch in any PR that renames a root script.
- When staging/production terraform environments are created (spec 13), their
  modules should follow the same `tests/` convention this plan completes.
- The `.tanstack/` ignore assumes TanStack keeps its scratch dir name; if
  `apps/web` upgrades and a new scratch dir appears untracked, extend
  `.gitignore` the same way.
