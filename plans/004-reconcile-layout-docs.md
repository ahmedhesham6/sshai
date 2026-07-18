# Plan 004: Reconcile the repository-layout docs (spec 16, spec 13) with the actual tree

> **Executor instructions**: Follow this plan step by step. Run every
> verification command and confirm the expected result before moving to the
> next step. If anything in the "STOP conditions" section occurs, stop and
> report — do not improvise. When done, update the status row for this plan
> in `plans/README.md` — unless a reviewer dispatched you and told you they
> maintain the index.
>
> **Drift check (run first)**: `git diff --stat c2c4e2d..HEAD -- docs/spec/16-implementation-plan.md docs/spec/13-infrastructure.md`
> Also run the tree checks in "Current state" — this plan documents the tree
> AS IT IS when you execute, so re-verify each claimed presence/absence and
> the status of plans 001–003 in `plans/README.md` before writing.

## Status

- **Priority**: P2
- **Effort**: S
- **Risk**: LOW (docs only)
- **Depends on**: plans/001, plans/003 (hard); plans/002 must be DONE or REJECTED (its outcome decides one sentence — see Step 2)
- **Category**: docs
- **Planned at**: commit `c2c4e2d`, 2026-07-17

## Why this matters

Both layout docs present aspirational structure as fact. Spec 16's
"Repository layout" section lists `apps/docs/`, `libs/adapters/`, and
`libs/observability/` — none exist at `c2c4e2d` — and spec 13 lists eight
Terraform modules and three environments where four modules and one
environment exist, while omitting `capsule-store`, the module that actually
implements the ratified S3 capsule store (ADR 0009). A contributor or agent
using these sections as a map is sent to phantom packages, and — because the
pnpm/turbo workspace globs are `apps/*` and `libs/*` — a directory created to
"match the doc" would silently join the build graph. This plan makes the docs
state what exists, and marks what is planned as planned.

## Current state

- `docs/spec/16-implementation-plan.md` — the "Repository layout" listing
  includes (verbatim excerpts at `c2c4e2d`):

  ```
  apps/
    ...
    guest/                  Go Runtime supervisor
    web/                    TanStack Start product application
    docs/                   Public documentation site
  libs/
    ...
    profile/                Profile refs, publication, composition, and materializer plans
    adapters/               Claude, Codex, and OpenCode compiler backends
    ...
    provideraws/            EC2/EBS/VPC implementation
    ...
    observability/          OpenTelemetry conventions
  ```

  Reality at `c2c4e2d`: no `apps/docs`, no `libs/adapters` (adapters live in
  `apps/guest/adapter_*.go` — moved to `libs/adapters` only if plans/002 is
  DONE), no `libs/observability`. Additionally `apps/guest`, `apps/workflows`,
  `apps/ssh-proxy` are library packages with no `main()`; the control-plane
  binary (`apps/control-plane/cmd/control-plane/main.go`) currently embeds the
  workflows service; the CLI entrypoint is `apps/cli/cmd/devm/` after
  plans/003.

- `docs/spec/13-infrastructure.md` (lines ~118–131) lists modules
  `global-networking, regional-cell, ecs-service, rds, object-storage, iam,
  image-pipeline, observability` and environments
  `development/staging/production`. Reality:
  `infra/terraform/modules/{capsule-store,object-storage,rds,regional-cell}`
  and `infra/terraform/environments/development` only. `capsule-store` is
  absent from the doc.

- Convention: spec files are plain prose/markdown, no front-matter; ADRs are
  short single-decision files. Domain vocabulary per `CONTEXT.md` (use
  "Capsule", "Adapter", "Environment", "Runtime" with these exact meanings).

## Commands you will need

| Purpose | Command | Expected on success |
|---|---|---|
| Presence checks | `ls apps libs infra/terraform/modules infra/terraform/environments` | matches what you document |
| Entrypoint check | `grep -rn "func main()" apps --include='*.go'` | the set you document |
| Link check | `grep -n "](" docs/spec/16-implementation-plan.md docs/spec/13-infrastructure.md` | every relative link target exists |

## Scope

**In scope**:
- `docs/spec/16-implementation-plan.md` — the repository-layout section only
- `docs/spec/13-infrastructure.md` — the terraform layout section only

**Out of scope** (do NOT touch):
- Creating any directory the docs mention — this plan changes prose, never the tree.
- Other sections of either spec, all other specs, all ADRs, `CONTEXT.md`.
- `docs/spec/README.md` (its index links are verified intact; no change needed).

## Git workflow

- Branch: `advisor/004-layout-doc-reconcile`.
- Commit style: `docs(spec): reconcile repository and terraform layout with the tree`.
- Do NOT push or open a PR unless the operator instructed it.

## Steps

### Step 1: Re-verify reality

Run the presence and entrypoint checks from the commands table and the status
of plans 001–003 in `plans/README.md`. Write down the actual list — it is the
source of truth for Steps 2–3.

**Verify**: you can state, for every directory named in both doc sections,
whether it exists right now.

### Step 2: Rewrite spec 16's repository layout

Edit the layout listing so that:
- Every entry that exists stays, with its real path (CLI as
  `cli/` with a note that the binary builds from `cmd/devm/`).
- Every entry that does not exist is kept but marked `(planned)` — e.g.
  `docs/                   Public documentation site (planned — not yet created)`.
- `adapters/`: if plans/002 is DONE, list `libs/adapters/` as existing; if
  REJECTED/not done, mark it
  `(planned — currently implemented in apps/guest/adapter_*.go)`.
- Add a short status note under the listing (3–5 lines): `apps/guest`,
  `apps/workflows`, `apps/ssh-proxy` are library packages whose `cmd/`
  entrypoints are phase-pending; the control-plane binary currently embeds the
  workflows service as an interim arrangement; the binary convention is
  `apps/<svc>/cmd/<binary-name>/main.go`.

**Verify**: `grep -n "planned" docs/spec/16-implementation-plan.md` → every
directory you confirmed absent in Step 1 carries a `(planned…)` marker; no
directory you confirmed present carries one.

### Step 3: Rewrite spec 13's terraform layout

- Add `capsule-store/` to the module listing with a one-line description
  ("S3 OCI image-layout Capsule store; owner-scoped presigned access — ADR
  0009").
- Mark absent modules (`global-networking`, `ecs-service`, `iam`,
  `image-pipeline`, `observability`) and absent environments (`staging`,
  `production`) as `(planned)`.

**Verify**: `grep -n "capsule-store" docs/spec/13-infrastructure.md` → at
least one hit in the layout section;
`grep -c "planned" docs/spec/13-infrastructure.md` → ≥ 7 (five modules + two
environments).

### Step 4: Link check and commit

**Verify**: every relative markdown link in both edited files resolves
(`for f in $(grep -oh '](\./[^)]*)' docs/spec/16-implementation-plan.md docs/spec/13-infrastructure.md | tr -d ']()' ); do test -e docs/spec/$f || echo MISSING $f; done`
→ no `MISSING` lines); `git status --short` shows only the two spec files.

## Test plan

Docs-only change; the verification greps in Steps 2–4 are the test. No code
or CI impact (`pnpm check`'s doc-adjacent gate is `contract:lint`, which does
not read these files).

## Done criteria

- [ ] Every directory named in spec 16's layout either exists or is marked planned
- [ ] Spec 16 records the phase-pending entrypoint status of guest/workflows/ssh-proxy
- [ ] Spec 13's module list includes `capsule-store` and marks the five absent modules + two absent environments planned
- [ ] `git status --short` shows only the two spec files
- [ ] `plans/README.md` status row updated

## STOP conditions

Stop and report back (do not improvise) if:

- A directory this plan calls absent now exists (or vice versa) and the
  discrepancy isn't explained by plans 001–003's status — the tree moved
  under you; re-verify everything before writing.
- You find yourself wanting to create a directory to make the doc true —
  that is explicitly the wrong direction for this plan.

## Maintenance notes

- Whenever a `(planned)` directory is actually created, the same commit should
  drop the marker — reviewers should ask for that in any PR creating a new
  `apps/*` or `libs/*` package.
- If plans/002 lands *after* this plan, its executor updates the one
  `adapters/` line (that hand-off is noted in plans/002's maintenance notes).
