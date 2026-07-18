# Plan 002: Extract the per-agent adapter backends from `apps/guest` into `libs/adapters`

> **Executor instructions**: Follow this plan step by step. Run every
> verification command and confirm the expected result before moving to the
> next step. If anything in the "STOP conditions" section occurs, stop and
> report — do not improvise. When done, update the status row for this plan
> in `plans/README.md` — unless a reviewer dispatched you and told you they
> maintain the index.
>
> **Drift check (run first)**: `git diff --stat c2c4e2d..HEAD -- apps/guest libs/profile libs/adapters`
> Changes under `apps/guest` from plans/001 (the vocabulary move and
> `profile_compat.go`) are EXPECTED — plans/001 must be DONE before this plan
> starts. Any other drift: compare the "Current state" excerpts against live
> code; on a mismatch, treat as a STOP condition.

## Status

- **Priority**: P2
- **Effort**: L
- **Risk**: MED (the seam surface is measured in Step 1; freshly security-gated code moves packages)
- **Depends on**: plans/001-move-materialization-vocabulary-to-libs-profile.md
- **Category**: tech-debt
- **Planned at**: commit `c2c4e2d`, 2026-07-17

## Why this matters

`docs/spec/16-implementation-plan.md` ratifies `libs/adapters/` — "Claude,
Codex, and OpenCode compiler backends" — but the three adapters live inside
`apps/guest` as `adapter_claude.go`, `adapter_codex.go`, `adapter_opencode.go`
behind an **unexported** registry in `adapter_registry.go`. `CONTEXT.md`
defines **Adapter** as "a per-agent compiler backend translating canonical
Components into native change plans" — a reusable compiler, not guest
plumbing. Extraction gives the adapters an explicit, importable seam (future
adapters land in `libs/` without touching the guest), shrinks the guest god
package, and makes the tree match the ratified spec.

**Caution that shapes this plan**: the adapter code was recently hardened by
adversarial security gates (destination-based approval, digest-bound
executable review, credential consent). The move must be verbatim — any
"improvement while moving" risks reintroducing a closed vulnerability class.

## Current state

- `apps/guest/adapter_registry.go` (69 lines) — the seam, all unexported:

  ```go
  type capsuleComponentAdapter interface {
      ID() string
      Version() string
      Translate(snapshot domain.CapsuleLockSnapshot, capsuleDigest string, component capsule.Component, files []capsuleFile, installed InstalledMaterialization, hasInstalled bool, batch CapsuleLockMaterializationBatch) (ProfileMaterialization, error)
  }

  var capsuleComponentAdapters = map[string]capsuleComponentAdapter{}

  func registerCapsuleAdapter(a capsuleComponentAdapter) { ... }
  func capsuleAdapterFor(id string) (capsuleComponentAdapter, error) { ... }
  ```

  The same file holds `sensitiveMaterializationSurface`,
  `sensitiveMaterializationApproval`, `materializationSelectorsOverlap` —
  approval helpers the adapters share.

- Adapter implementations: `apps/guest/adapter_claude.go`,
  `adapter_codex.go`, `adapter_opencode.go` (+ `_test.go` siblings; the
  OpenCode test alone is 372 lines). Registration happens inside guest — find
  the exact registration call sites in Step 1.

- The `Translate` signature references guest types whose homes differ:
  - `InstalledMaterialization` — already `libs/profile` after plans/001
    (guest name is an alias).
  - `capsuleFile` (unexported), `ProfileMaterialization`,
    `MaterializationFile`, `CapsuleLockMaterializationBatch` — still guest
    types, defined in `apps/guest/capsule_materialization.go` and
    `apps/guest/profile_materialization.go` (`ProfileMaterialization` around
    line 55, doc comment: "the canonical Component-to-native plan consumed by
    the generic safety engine"). Spec 16 assigns "materializer plans" to
    `libs/profile`, so these move to `libs/profile`, and the adapter interface
    + implementations move to `libs/adapters`.

- Conventions: single root Go module; conventional commits (exemplar:
  `refactor(guest): extract per-agent adapter seam behind capsule
  materialization`); `libs/` packages are flat lowercase (`package adapters`).
  New dirs under `libs/` need the Turborepo package.json shim — copy
  `apps/guest/package.json` and change `"name"` to `"@sshai/adapters"`. Note
  the guest shim's `build` script is `go test -run '^$' ./...` (compile
  check); keep that for the new package.

## Commands you will need

| Purpose | Command | Expected on success |
|---|---|---|
| Locate Go | `which go \|\| export PATH=/tmp/sshai-tools/go/bin:$PATH; go version` | prints go1.26.x |
| Build all | `go build ./...` (repo root) | exit 0 |
| Focused tests | `umask 077 && go test ./apps/guest/... ./libs/adapters/... ./libs/profile/...` | all pass |
| Full Go tests | `umask 077 && go test ./...` | pass except known failures below |
| Format / vet | `gofmt -l apps libs` / `go vet ./...` | no output / exit 0 |

**Guest and adapter tests require `umask 077`** (they assert file modes).

**Known pre-existing failures** (parallel ReserveInitialRuntime WIP — ignore
only if they fail identically on your base commit):
`TestEnvironmentCreateWorkflowDoesNotCompleteAfterInventoryFailure`,
`TestCreateEnvironmentHTTPTracerCompletesFakeWorkflow`,
`TestRuntimeProviderResourceInventoryOwnsProviderIdentity`.

## Scope

**In scope**:
- `libs/adapters/` (create: registry, three adapters, their tests, package.json shim)
- `libs/profile/` (add the moved plan types: `ProfileMaterialization`,
  `MaterializationFile`, `CapsuleFile` (exported form of `capsuleFile`),
  `CapsuleLockMaterializationBatch` — plus whatever Step 1's inventory
  approves)
- `apps/guest/capsule_materialization.go`, `apps/guest/profile_materialization.go`
  (remove moved declarations, re-point call sites), `apps/guest/profile_compat.go`
  (extend aliases), `apps/guest/adapter_*.go` (delete after move)

**Out of scope** (do NOT touch):
- Any behavioral edit to adapter logic, approval rules, TOML selector engine,
  or consent paths — move verbatim. Accepted limitations (quoted dotted TOML
  keys unaddressable; TOML comments lost on managed writes) stay accepted.
- `apps/workflows`, `apps/control-plane`, `libs/domain`, `libs/capsule` —
  no signature or behavior changes land there.
- `docs/spec/*` — plans/004 reconciles docs after this resolves.

## Git workflow

- Branch: `advisor/002-adapters-to-libs` off `main` (after plans/001 merged).
- Commit style: `refactor(adapters): move compiler backends to libs/adapters`.
- Do NOT push or open a PR unless the operator instructed it.

## Steps

### Step 1: Inventory the seam surface (measurement gate)

For each of `adapter_claude.go`, `adapter_codex.go`, `adapter_opencode.go`,
`adapter_registry.go`, list every identifier they reference that is declared
elsewhere in `package guest` (grep each identifier for its declaration site).
Also locate the registration call sites
(`grep -n "registerCapsuleAdapter" apps/guest/*.go`) and every engine call
site of `capsuleAdapterFor`.

**Verify (the gate)**: the out-of-file dependency set is within:
`capsuleComponentAdapter`, `registerCapsuleAdapter`, `capsuleAdapterFor`,
`capsuleFile`, `ProfileMaterialization`, `MaterializationFile`,
`CapsuleLockMaterializationBatch`, `InstalledMaterialization`,
`MaterializationMode`/`MaterializationRoot` consts,
`sensitiveMaterializationSurface`, `sensitiveMaterializationApproval`,
`materializationSelectorsOverlap`, plus `libs/{domain,capsule,profile}` and
stdlib/toml imports — **and the shared helper set confirmed by the first
gate run (2026-07-17, re-scoped by the advisor after verification)**:
`EffectiveCacheKeyFields` (+ its `Digest()` method),
`materializationContentDigest`, `directoryMaterializationDigest`,
`cloneMaterializationFiles`, `toMaterializationFiles`,
`componentRequirementDigest`, `materializationFilePaths`, `filepathExt`.
These are pure functions/types over the moving vocabulary and are used by
both the adapters and the guest engine, so they move to `libs/profile` in
Step 2. **If the true set exceeds this expanded list by more than ~5
additional simple declarations, STOP and report the measured surface with
declaration sites.**

### Step 2: Move the plan types to `libs/profile`

Create `libs/profile/plan.go` (package `profile`). Cut from guest, verbatim
with doc comments: `ProfileMaterialization`, `MaterializationFile`,
`CapsuleLockMaterializationBatch`, and `capsuleFile` — exporting the last as
`CapsuleFile` (field set unchanged). Also cut the shared helper set (all
verified pure over the moving types): `EffectiveCacheKeyFields` + its
`Digest()` method (already exported, moves as-is), and the six unexported
helpers, exported mechanically by capitalizing the first letter only:
`MaterializationContentDigest`, `DirectoryMaterializationDigest`,
`CloneMaterializationFiles`, `ToMaterializationFiles`
(signature becomes `func ToMaterializationFiles(files []CapsuleFile) []MaterializationFile`),
`ComponentRequirementDigest`, `MaterializationFilePaths`, `FilepathExt`.
No body edits. Unqualify any `profile.` references; keep `domain`/`capsule`
imports as needed. Extend `apps/guest/profile_compat.go` with aliases so the
guest engine compiles untouched: type aliases (`type ProfileMaterialization =
profile.ProfileMaterialization`, `type capsuleFile = profile.CapsuleFile`,
`type EffectiveCacheKeyFields = profile.EffectiveCacheKeyFields`, etc.) and
unexported var aliases for the helpers (`var materializationContentDigest =
profile.MaterializationContentDigest`, `var filepathExt = profile.FilepathExt`,
…) so existing unexported call sites in the engine keep resolving.

**Verify**: `go build ./...` → exit 0;
`umask 077 && go test ./apps/guest/... ./libs/profile/...` → all pass.

### Step 3: Create `libs/adapters` with the exported seam

Create `libs/adapters/registry.go` (package `adapters`): move the interface as
`Adapter` (same three methods; `Translate` signature now written in
`profile.*` types), the map, and `Register` / `For` (exported forms of
`registerCapsuleAdapter` / `capsuleAdapterFor`). Move the sensitive-surface
helpers with it, exported only if the adapters need them across files —
otherwise keep them unexported in `package adapters`.

**Verify**: `go build ./libs/adapters/` → exit 0.

### Step 4: Move the three adapters + tests

`git mv` `adapter_claude.go`, `adapter_codex.go`, `adapter_opencode.go` and
their `_test.go` files into `libs/adapters/`, change `package guest` →
`package adapters`, rewrite type references to `profile.*` / local exported
names. Add the `libs/adapters/package.json` shim (Current state gives the
recipe). Registration: keep each adapter's registration mechanism exactly as
found in Step 1, translated to `Register` — if registration was `init()`-based
it stays `init()`-based inside `package adapters`.

**Verify**: `umask 077 && go test ./libs/adapters/...` → all adapter tests
pass unchanged (test *assertions* must not be edited; only package/qualifier
mechanics).

### Step 5: Re-point the guest engine

In `apps/guest` replace `capsuleAdapterFor(...)` calls with `adapters.For(...)`
and ensure the three backends are linked: import `libs/adapters` from the
engine file (a plain import suffices when registration is `init()`-based; use
a blank import only if nothing else is referenced). Delete now-empty guest
declarations. Remove `adapter_registry.go` remnants.

**Verify**: `grep -rn "registerCapsuleAdapter\|capsuleAdapterFor\|capsuleComponentAdapter" apps/guest/` → no output;
`go build ./...` → exit 0;
`umask 077 && go test ./...` → pass except the three known failures.

### Step 6: Format, vet, commit

**Verify**: `gofmt -l apps libs` → no output; `go vet ./...` → exit 0;
`git status --short` shows only in-scope files.

## Test plan

No new tests; the moved adapter test files (Claude/Codex/OpenCode, including
the 372-line OpenCode suite and the security-gate regression tests) are the
protection. The hard requirement: **every moved test passes with unmodified
assertions**. Add exactly one new test:
`libs/adapters/registry_test.go::TestRegisteredAdapters` asserting `For`
resolves the three known adapter IDs and errors on an unknown ID (model the
error-message assertion on the existing `unknown capsule component adapter %q`
text).

## Done criteria

- [ ] `libs/adapters/` exists with registry + 3 adapters + tests + package.json shim
- [ ] `grep -rn "adapter_" apps/guest/ --include='*.go'` → no output (files moved, not copied)
- [ ] `go build ./...` → exit 0
- [ ] `umask 077 && go test ./...` → no failures beyond the three pre-existing
- [ ] `git diff` shows no edits to moved test assertions (mechanical qualifier changes only)
- [ ] `gofmt -l apps libs` → no output
- [ ] `plans/README.md` status row updated

## STOP conditions

Stop and report back (do not improvise) if:

- plans/001 is not DONE (check `plans/README.md`).
- Step 1's measurement gate trips (surface exceeds the listed set by >~5 simple declarations).
- Any moved test fails and the fix would require changing a test assertion —
  these encode security-gate findings; report which test and why.
- The registration mechanism found in Step 1 is not translatable 1:1 (e.g.
  ordering-dependent registration) — report the mechanism.
- A verification fails twice after a reasonable fix attempt.

## Maintenance notes

- After this lands, remove `apps/guest/profile_compat.go` if
  `grep -rn "profile_compat" ` shows the aliases unused inside guest — that
  completes plans/001's transitional shim.
- Future adapters (per spec 16's direction) now land as one file + tests in
  `libs/adapters` plus a `Register` call — reviewers should reject adapter
  logic appearing anywhere under `apps/guest` again.
- Reviewer focus: diff the moved adapter files with `git diff --find-renames`
  — the only intra-file changes should be package clause and qualifiers.
- plans/004 must record the new location in spec 16 once this is DONE.
