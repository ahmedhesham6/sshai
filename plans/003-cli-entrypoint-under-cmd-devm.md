# Plan 003: Move the CLI's `package main` under `apps/cli/cmd/devm/`, pinning one entrypoint convention

> **Executor instructions**: Follow this plan step by step. Run every
> verification command and confirm the expected result before moving to the
> next step. If anything in the "STOP conditions" section occurs, stop and
> report — do not improvise. When done, update the status row for this plan
> in `plans/README.md` — unless a reviewer dispatched you and told you they
> maintain the index.
>
> **Drift check (run first)**: `git diff --stat c2c4e2d..HEAD -- apps/cli .gitignore`
> If any in-scope file changed since this plan was written, compare the
> "Current state" facts against the live tree before proceeding; on a
> mismatch, treat it as a STOP condition.

## Status

- **Priority**: P2
- **Effort**: S
- **Risk**: LOW (moving `package main` files nothing can import)
- **Depends on**: none
- **Category**: tech-debt
- **Planned at**: commit `c2c4e2d`, 2026-07-17

## Why this matters

The repo has three entrypoint styles across five services: `apps/control-plane`
uses the Go-conventional `cmd/control-plane/main.go`; `apps/cli` puts ~20
`package main` files directly at the app root (so `go build .` there drops a
binary named `cli`, even though the product CLI is named `devm` — see
`README.md`: "`devm` creates agent-ready remote development Environments…");
and `apps/guest` / `apps/workflows` / `apps/ssh-proxy` have no `main()` at all
(phase-pending services). This plan makes `cmd/<binary-name>/` the single
convention by moving the CLI under `apps/cli/cmd/devm/`, which also fixes the
binary name. The phase-pending apps get no fabricated entrypoints — recording
their status is plans/004's job.

## Current state

- `apps/cli/` contains (verified at `c2c4e2d`): `capsule.go`, `capsule_test.go`,
  `inspect.go`, `inspect_test.go`, `local_state.go`, `login.go`,
  `login_test.go`, `main.go`, `plan.go`, `plan_test.go`, `ssh.go`,
  `ssh_include.go`, `ssh_proxy.go`, `ssh_proxy_test.go`, `ssh_test.go`,
  `token_lock_unix.go`, `token_lock_unsupported.go`, `token_session.go`,
  `token_session_test.go`, `workos_login.go`, plus `package.json` and a stray
  built binary `cli` (git-ignored). `main.go` starts:

  ```go
  package main

  import (
      ...
      "github.com/ahmedhesham6/sshai/libs/auth"
      "github.com/ahmedhesham6/sshai/libs/profile"
  )

  func main() {
      if err := run(context.Background(), os.Args[1:]); err != nil {
  ```

- The exemplar convention: `apps/control-plane/cmd/control-plane/main.go`
  (binary named after its dir).

- `.gitignore` (repo root) currently contains the line `apps/cli/cli` — the
  ignore for the old binary drop location.

- `apps/cli/package.json` scripts run from the package dir with `./...`
  (`"build": "go build ./..."`, `"test": "go test ./..."`), so they keep
  working unchanged after the move — `./...` recurses into `cmd/devm/`.

- Because everything in `apps/cli` is `package main`, no other package can
  import it; the move cannot break any importer. Confirm anyway in Step 1.

## Commands you will need

| Purpose | Command | Expected on success |
|---|---|---|
| Locate Go | `which go \|\| export PATH=/tmp/sshai-tools/go/bin:$PATH; go version` | prints go1.26.x |
| Build all | `go build ./...` (repo root) | exit 0 |
| CLI tests | `go test ./apps/cli/...` | all pass |
| Binary build | `cd apps/cli/cmd/devm && go build .` | produces `./devm`, exit 0 |
| Format | `gofmt -l apps/cli` | no output |

## Scope

**In scope**:
- `apps/cli/*.go` → `apps/cli/cmd/devm/*.go` (git mv, all Go files including tests)
- `.gitignore` (one-line swap)

**Out of scope** (do NOT touch):
- `apps/cli/package.json` — stays at `apps/cli/` (Turborepo discovers packages
  by the `apps/*` glob; nesting it deeper would drop the package from the
  workspace).
- `apps/guest`, `apps/workflows`, `apps/ssh-proxy` — do NOT add `main.go`
  stubs; their runtime wiring is not designed yet.
- `apps/control-plane` — already conventional.
- Any Go source edits beyond the move: no renames, no refactors, package
  clause stays `package main`.

## Git workflow

- Branch: `advisor/003-cli-cmd-devm`.
- Commit style: `refactor(cli): move devm entrypoint under cmd/devm`.
- Do NOT push or open a PR unless the operator instructed it.

## Steps

### Step 1: Confirm the move is closed

**Verify** (both):
- `grep -rn "sshai/apps/cli" --include='*.go' . ` → no output (nothing imports the CLI).
- `grep -L "^package main" apps/cli/*.go` → no output (every Go file there is
  `package main`; a non-main file → STOP condition).

### Step 2: Move the files

`mkdir -p apps/cli/cmd/devm && git mv apps/cli/*.go apps/cli/cmd/devm/`
(leave `package.json` in place; the untracked `cli` binary can be deleted:
`rm -f apps/cli/cli`).

**Verify**: `ls apps/cli` → exactly `cmd` and `package.json`;
`go build ./...` → exit 0.

### Step 3: Update `.gitignore`

Replace the line `apps/cli/cli` with `apps/cli/cmd/devm/devm`.

**Verify**: `cd apps/cli/cmd/devm && go build . && git check-ignore devm` →
prints `devm` (ignored); `git status --short` shows no untracked binary.

### Step 4: Full check and commit

**Verify**: `go test ./apps/cli/...` → all pass; `gofmt -l apps/cli` → no
output; `git status --short` shows only renames + `.gitignore`.

## Test plan

No new tests — the CLI's existing `_test.go` files move with it and must pass
unchanged (`go test ./apps/cli/...`). The `go build .` producing a binary
named `devm` in Step 3 is the behavioral acceptance check.

## Done criteria

- [ ] `apps/cli/` contains only `cmd/` and `package.json`
- [ ] `cd apps/cli/cmd/devm && go build .` → produces `devm`, exit 0
- [ ] `go test ./apps/cli/...` → exit 0
- [ ] `git check-ignore apps/cli/cmd/devm/devm` → exit 0
- [ ] `git log --stat -1` shows renames (R) not delete+add pairs
- [ ] `plans/README.md` status row updated

## STOP conditions

Stop and report back (do not improvise) if:

- Step 1 finds any importer of `apps/cli` or any non-`package main` file.
- `git status` shows uncommitted changes under `apps/cli` before starting
  (possible parallel session).
- Build or tests fail after the move in a way a missing-file check
  (`git status` for files left behind) doesn't explain.

## Maintenance notes

- The convention this plan pins: **an app ships its binary from
  `apps/<svc>/cmd/<binary-name>/main.go`**. `apps/guest`, `apps/workflows`,
  and `apps/ssh-proxy` are library-only until their runtime wiring lands and
  should gain `cmd/` dirs at that point — plans/004 records this status in
  spec 16, and reviewers should hold new binaries to it.
- If a release pipeline is added later, point it at `./apps/*/cmd/*` as the
  buildable-binary glob.
