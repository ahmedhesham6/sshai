# Plan 001: Move the materialization/drift vocabulary from `apps/guest` to `libs/profile`, dissolving the `workflows → guest` cross-app import

> **Executor instructions**: Follow this plan step by step. Run every
> verification command and confirm the expected result before moving to the
> next step. If anything in the "STOP conditions" section occurs, stop and
> report — do not improvise. When done, update the status row for this plan
> in `plans/README.md` — unless a reviewer dispatched you and told you they
> maintain the index.
>
> **Drift check (run first)**: `git diff --stat c2c4e2d..HEAD -- apps/guest apps/workflows libs/profile`
> If any in-scope file changed since this plan was written, compare the
> "Current state" excerpts against the live code before proceeding; on a
> mismatch, treat it as a STOP condition.

## Status

- **Priority**: P1
- **Effort**: M
- **Risk**: MED (mechanical, but `apps/workflows` is under active parallel development — see STOP conditions)
- **Depends on**: none
- **Category**: tech-debt
- **Planned at**: commit `c2c4e2d`, 2026-07-17

## Why this matters

`apps/workflows` (the Restate workflow service) imports `apps/guest` (the guest
supervisor) in six files to reach nine shared types and functions. This makes
`apps/` no longer mean "independent deployable": one app is a library dependency
of another, and the directory taxonomy hides the layering. The shared symbols
are materialization-state and drift-adoption *vocabulary* — exactly what
`docs/spec/16-implementation-plan.md` says belongs in `libs/profile`
("Profile refs, publication, composition, and materializer plans"). After this
plan, `apps/workflows` imports only `libs/*`, and `apps/guest` keeps compiling
unchanged via Go type aliases (a deliberate transitional shim).

## Current state

- `apps/guest/profile_materialization.go` — 1,457-line guest materialization
  engine. Lines 25–38 define the vocabulary to move:

  ```go
  type MaterializationMode string

  const (
      MaterializationManaged    MaterializationMode = "managed"
      MaterializationSeeded     MaterializationMode = "seeded"
      MaterializationReferenced MaterializationMode = "referenced"
  )

  type MaterializationRoot string

  const (
      MaterializationHome      MaterializationRoot = "home"
      MaterializationWorkspace MaterializationRoot = "workspace"
  )
  ```

  Lines 117–146 define `type ProfileMaterializationResult struct` (fields are
  scalars plus `domain.ComponentScope`, `MaterializationMode`,
  `MaterializationRoot`, `profile.PlanOperation`, `profile.RequirementState`).
  Lines 148–173 define `type InstalledMaterialization struct` (same field-type
  families). Lines 175–195 define
  `func InstalledMaterializationsFromResults(results []ProfileMaterializationResult) []InstalledMaterialization`.

- `apps/guest/drift_adoption.go` — ~110-line file, moves in full. Header:

  ```go
  package guest

  import (
      "errors"
      "fmt"
      "regexp"

      "github.com/ahmedhesham6/sshai/libs/contracts"
      "github.com/ahmedhesham6/sshai/libs/domain"
  )

  var driftAdoptionDigestPattern = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)

  type DriftAdoptionProposal struct { ... }
  type DriftAdoptionConsent struct { ... }
  type AcceptedDriftAdoption struct { ... }
  func ProposeDriftAdoption(lock domain.CapsuleLockSnapshot, ownerID string, installed InstalledMaterialization, observedDigest, diffSummary string) (DriftAdoptionProposal, error)
  func AcceptDriftAdoption(lock domain.CapsuleLockSnapshot, ownerID string, proposal DriftAdoptionProposal, consent DriftAdoptionConsent) (AcceptedDriftAdoption, error)
  ```

- `libs/profile` — existing `package profile` (files: `compiler.go`,
  `materialization.go`, `scanner.go`, `selection.go`). It already owns the
  materialization *planning* core (`MaterializationSnapshot`, `DigestState`,
  `PlanMaterialization`, `PlanOperation`, `RequirementState`), and
  `apps/guest/profile_materialization.go:21` already imports it — so moving
  the vocabulary here cannot create an import cycle. Note:
  `libs/profile/materialization.go:26` declares an **unexported**
  `type materializationMode uint8`. The moved exported `MaterializationMode`
  is a distinct identifier and coexists legally; do not merge or rename them.

- Consumers of the moved symbols outside `apps/guest` (verified by grep at
  `c2c4e2d`): only `apps/workflows` — the files `environment_create.go`,
  `environment_create_test.go`, `drift_adoption.go`, `drift_adoption_test.go`,
  `environment_capsule_state.go`, `environment_capsule_state_test.go`, using
  exactly: `guest.AcceptDriftAdoption`, `guest.AcceptedDriftAdoption`,
  `guest.DriftAdoptionConsent`, `guest.DriftAdoptionProposal`,
  `guest.InstalledMaterialization`, `guest.InstalledMaterializationsFromResults`,
  `guest.MaterializationManaged`, `guest.ProfileMaterializationResult`,
  `guest.ProposeDriftAdoption`.

- Conventions: single root Go module `github.com/ahmedhesham6/sshai`
  (go 1.26.5). Commit style is conventional commits with package scope, e.g.
  `refactor(guest): extract per-agent adapter seam behind capsule materialization`
  (from `git log`). Vocabulary: `CONTEXT.md` defines **Materialization** and
  **Materialization Mode** (`managed`/`seeded`/`referenced`) as domain terms —
  keep these exact names in the moved code.

## Commands you will need

| Purpose | Command | Expected on success |
|---|---|---|
| Locate Go | `which go \|\| export PATH=/tmp/sshai-tools/go/bin:$PATH; go version` | prints go1.26.x |
| Build all | `go build ./...` (repo root) | exit 0, no output |
| Guest+profile tests | `umask 077 && go test ./apps/guest/... ./libs/profile/...` | ok, all pass |
| Workflows tests | `go test ./apps/workflows/...` | see known-failures note below |
| Format | `gofmt -l apps libs` | no output |
| Vet | `go vet ./apps/guest/... ./apps/workflows/... ./libs/profile/...` | exit 0 |

**Guest tests require `umask 077`** — they assert on created-file permissions
and fail under the default umask.

**Known pre-existing failures** (parallel ReserveInitialRuntime WIP, NOT caused
by this plan; ignore them if — and only if — they also fail on a clean checkout
of your base commit):
`TestEnvironmentCreateWorkflowDoesNotCompleteAfterInventoryFailure`,
`TestCreateEnvironmentHTTPTracerCompletesFakeWorkflow`,
`TestRuntimeProviderResourceInventoryOwnsProviderIdentity`.

## Scope

**In scope** (the only files you should modify or create):
- `libs/profile/materialization_state.go` (create)
- `libs/profile/drift_adoption.go` (create)
- `apps/guest/profile_materialization.go` (delete moved declarations)
- `apps/guest/drift_adoption.go` (delete)
- `apps/guest/profile_compat.go` (create — alias shim)
- `apps/workflows/environment_create.go`, `apps/workflows/drift_adoption.go`,
  `apps/workflows/environment_capsule_state.go` and their `_test.go` siblings
  (import + qualifier swap only)

**Out of scope** (do NOT touch, even though they look related):
- `apps/guest/adapter_*.go`, `apps/guest/capsule_materialization.go` — the
  adapter/engine extraction is plans/002; do not export or move
  `capsuleComponentAdapter`, `capsuleFile`, `ProfileMaterialization`, or
  `CapsuleLockMaterializationBatch` here.
- `libs/profile/materialization.go` — do not rename its unexported
  `materializationMode uint8`.
- `apps/guest/*_test.go` — the alias shim keeps them compiling; leave them in
  place (relocating tests is deferred, see Maintenance notes).
- Any behavior change. This plan moves declarations verbatim.

## Git workflow

- Branch: `advisor/001-materialization-vocab-to-profile` off `main`.
- One commit per step or one final commit; message style:
  `refactor(profile): move materialization/drift vocabulary out of apps/guest`.
- Do NOT push or open a PR unless the operator instructed it.

## Steps

### Step 1: Preflight — confirm no name collisions in `libs/profile`

Run:
`grep -n "MaterializationMode\|MaterializationRoot\|MaterializationHome\|MaterializationWorkspace\|MaterializationManaged\|MaterializationSeeded\|MaterializationReferenced\|ProfileMaterializationResult\|InstalledMaterialization\|DriftAdoption\|driftAdoptionDigestPattern" libs/profile/*.go`

**Verify**: the only hits are the unexported `materializationMode` (lowercase
m) in `libs/profile/materialization.go` and its uses. Any hit on an *exported*
name from the move set → STOP condition.

### Step 2: Create `libs/profile/materialization_state.go`

New file, `package profile`. Cut (do not copy-and-keep) these declarations from
`apps/guest/profile_materialization.go` verbatim, preserving doc comments:
`MaterializationMode` + its three consts, `MaterializationRoot` + its two
consts, `ProfileMaterializationResult`, `InstalledMaterialization`,
`InstalledMaterializationsFromResults`.

Two mandatory edits while pasting:
1. Field types `profile.PlanOperation` and `profile.RequirementState` become
   unqualified `PlanOperation` / `RequirementState` (they are now
   package-local).
2. Imports for the new file: only
   `"github.com/ahmedhesham6/sshai/libs/domain"` (for
   `domain.ComponentScope`).

**Verify**: `go build ./libs/profile/` → exit 0. (`apps/guest` will not build
yet — that is expected until Step 4.)

### Step 3: Create `libs/profile/drift_adoption.go`

`git mv apps/guest/drift_adoption.go libs/profile/drift_adoption.go`, then in
the moved file change `package guest` to `package profile`. The file's
existing imports (`errors`, `fmt`, `regexp`, `libs/contracts`, `libs/domain`)
stay as-is; its references to `InstalledMaterialization` and
`MaterializationManaged` are already unqualified and now resolve
package-locally.

**Verify**: `go build ./libs/profile/` → exit 0.

### Step 4: Add the alias shim `apps/guest/profile_compat.go`

New file in `package guest`:

```go
package guest

import "github.com/ahmedhesham6/sshai/libs/profile"

// Transitional aliases: this vocabulary moved to libs/profile (plans/001).
// New code should import libs/profile directly. Remove once no guest-internal
// reference remains (see plans/002).
type (
	MaterializationMode          = profile.MaterializationMode
	MaterializationRoot          = profile.MaterializationRoot
	ProfileMaterializationResult = profile.ProfileMaterializationResult
	InstalledMaterialization     = profile.InstalledMaterialization
	DriftAdoptionProposal        = profile.DriftAdoptionProposal
	DriftAdoptionConsent         = profile.DriftAdoptionConsent
	AcceptedDriftAdoption        = profile.AcceptedDriftAdoption
)

const (
	MaterializationManaged    = profile.MaterializationManaged
	MaterializationSeeded     = profile.MaterializationSeeded
	MaterializationReferenced = profile.MaterializationReferenced
	MaterializationHome       = profile.MaterializationHome
	MaterializationWorkspace  = profile.MaterializationWorkspace
)

var (
	InstalledMaterializationsFromResults = profile.InstalledMaterializationsFromResults
	ProposeDriftAdoption                 = profile.ProposeDriftAdoption
	AcceptDriftAdoption                  = profile.AcceptDriftAdoption
)
```

**Verify**: `go build ./apps/guest/` → exit 0, then
`umask 077 && go test ./apps/guest/... ./libs/profile/...` → all pass
(including the drift-adoption tests still living in `apps/guest`).

### Step 5: Point `apps/workflows` at `libs/profile`

In the six files listed in Current state: replace the import
`"github.com/ahmedhesham6/sshai/apps/guest"` with
`"github.com/ahmedhesham6/sshai/libs/profile"` and every `guest.` qualifier
with `profile.`. Watch for an existing `profile` import in any of the files —
if one already exists, just drop the guest import and rewrite qualifiers.

**Verify**:
- `grep -rn "apps/guest" apps/workflows/` → no output.
- `go build ./...` → exit 0.
- `go test ./apps/workflows/...` → pass, except the three known pre-existing
  failures listed above (confirm they fail identically on the base commit
  before ignoring).

### Step 6: Format, vet, commit

`gofmt -w` the touched files; run the Format and Vet commands from the table.

**Verify**: `gofmt -l apps libs` → no output; `go vet ./apps/guest/...
./apps/workflows/... ./libs/profile/...` → exit 0; `git status --short` shows
only in-scope files.

## Test plan

No new tests: this is a declaration move with zero behavior change, and the
moved code's tests (`apps/guest/drift_adoption_test.go`,
`profile_materialization_test.go`, and the six workflows test files) already
cover it — they now exercise the moved declarations through the aliases and
the new import respectively. The full existing suites for the three touched
packages are the regression net:
`umask 077 && go test ./apps/guest/... ./libs/profile/... && go test ./apps/workflows/...`.

## Done criteria

Machine-checkable. ALL must hold:

- [ ] `grep -rn "apps/guest" apps/workflows/` → no output
- [ ] `go build ./...` → exit 0
- [ ] `umask 077 && go test ./apps/guest/... ./libs/profile/...` → exit 0
- [ ] `go test ./apps/workflows/...` → no failures beyond the three
      pre-existing ones (verified against base commit)
- [ ] `gofmt -l apps libs` → no output
- [ ] `git status --short` shows no files outside the Scope list
- [ ] `plans/README.md` status row updated

## STOP conditions

Stop and report back (do not improvise) if:

- The drift check shows changes in `apps/workflows/environment_create*.go`,
  `libs/domain/environment.go`, or `libs/db/environment_creation*` — a
  parallel session (ReserveInitialRuntime work) is active in those files.
  Report the diff overlap; do not rebase over uncommitted parallel work.
- `git status` shows uncommitted changes in any in-scope directory before you
  start.
- Step 1 finds an exported name from the move set already declared in
  `libs/profile`.
- Any moved declaration references a guest identifier not listed in this plan
  (that means the surface grew since `c2c4e2d`).
- A verification fails twice after a reasonable fix attempt.

## Maintenance notes

- The alias shim `apps/guest/profile_compat.go` is transitional. plans/002
  (adapter extraction) shrinks guest-internal use of these names; remove the
  shim when `grep -rn "MaterializationMode\|InstalledMaterialization" apps/guest --include='*.go' | grep -v profile_compat` shows only `profile.`-qualified uses.
- Name stutter (`profile.ProfileMaterializationResult`) is accepted here to
  keep the move mechanical; renaming to `profile.MaterializationResult` is a
  deliberate follow-up, not part of this plan.
- Reviewer focus: the Step 2 unqualification edits (`profile.PlanOperation` →
  `PlanOperation`) and that no declaration was *copied* (old + new both live)
  instead of moved.
