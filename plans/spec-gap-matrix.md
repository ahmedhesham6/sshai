# Spec-gap matrix

Generated 2026-07-17 against `main` @ `248f362` by five parallel spec-vs-code
auditors, consolidated by the advisor. Statuses: IMPLEMENTED / PARTIAL /
MISSING. Spec-marked deferrals are listed but are NOT gaps. This file feeds
the slice graph in `plans/README.md`.

## Cluster: product surfaces & DX (specs 01, 12)

**Verdict: the user-facing layer is the largest gap in the product.**

### CLI (`apps/cli/cmd/devm`) — 5 of ~28 promised commands exist
- IMPLEMENTED: `login`, `inspect`, `plan`, `capsule capture`, `capsule build`, `ssh-proxy` (internal ProxyCommand), and the safe/review/requires_authorization/excluded/conflict plan grouping with evidence + source digests (capsule.go:92-124).
- MISSING: bare `devm` (the primary attach/start/connect journey), `status`, `stop`, `delete`, `doctor`, `logout`, `capsule publish|inspect|diff`, the **entire `profile` noun** (list/show/create/fork/add/remove/publish/apply) — despite `libs/profile` existing underneath.
- PARTIAL: local state layout `~/.config/devm/` (config/auth/ssh exercised; profiles/projects dirs unused pending commands).

### Web (`apps/web`) — login + placeholder only
- IMPLEMENTED: WorkOS AuthKit sign-in/callback (`routes/api/auth/*`, `server/auth.ts`); compliant "no editor/terminal UI" by omission.
- MISSING: all 10 promised surfaces — environment list/detail (9-section IA), runtime start/stop, activity + auto-stop config, profile views/diffs, operation progress, SSH setup/key management, credit balance/Polar portal, delete safety flow, operator diagnostics. Root cause consistent with `@sshai/contracts` TS client having zero consumers.

### Docs site
- MISSING: no `apps/docs` (framework choice explicitly open per spec 12:118 — absence is the gap, stack is not).

### DX findings
- No root `dev` script; only `apps/web` has one. No documented run/dev loop for the CLI (`go run ./cmd/devm` appears nowhere).
- CLI env vars `DEVM_WORKOS_CLIENT_ID`, `DEVM_CONTROL_PLANE_URL` (main.go:48-49) documented nowhere — `devm login` unrunnable without reading source.
- `apps/web/.env.example` exists (4 WorkOS vars, placeholders) but is unreferenced by README/AGENTS.
- README has zero install/build/run steps (honest, but empty); AGENTS.md is the only real dev guide and is accurate for build/test, not run.

### Deferred by spec (not gaps)
Teams/orgs/RBAC/sharing; background-agent queues/agent UI; bidirectional sync, auto git mutation, credential import, home archives, auto skill/hook execution; multi-runtime, cross-region, Windows, k8s, other providers, full devcontainer conformance, custom images/GPUs/public ports/browser IDEs, snapshot/fork UI; multiple Environments per repo in first CLI; docs framework choice.

## Cluster: runtime, storage, activity, SSH (specs 05, 06, 07)

**Verdict: domain + provider layers are spec-faithful; the gaps are at the seams — guest signal completeness, CLI wiring, and proxy start-on-connect.**

### Spec 05 — Environment/Runtime/storage: mostly IMPLEMENTED
Lifecycles, health, single-writable-runtime, replacement-retire ordering, encrypted gp3 volumes with correct DeleteOnTermination on both sides, AZ pinning with PlacementConflict, readiness levels 1–5, durability-class metadata — all verified with evidence (`libs/domain/{environment,runtime}.go`, `libs/provideraws/{provider,runtime}.go`, `apps/guest/{persistent_state,readiness}.go`).
- MISSING: the entire **deletion safety UX** — pre-delete disclosures (unpushed commits/untracked files/sizes/activity/credential bindings), exact-name confirmation, recent-auth check; `BeginDeletion` only flips lifecycle. Data-volume never deleted anywhere (safe by absence; explicit delete workflow absent).
- PARTIAL: logical-mount OS binding (spec-sanctioned subdir first impl); runtime start/stop → billing linkage untraced; CLI display of incomplete readiness steps.

### Spec 06 — Activity & auto-stop: evaluator solid, guest signal incomplete
Policy modes, grace timers, Restate virtual object keyed by env, timer cancel/expiry/refresh flow, stale-snapshot blocking, conflict suppression, manual-stop semantics — IMPLEMENTED (`libs/domain/autostop.go`, `apps/workflows/auto_stop*.go`, `apps/guest/activity.go`).
- MISSING (the big one): guest `ActivitySnapshot` omits 3 of 7 required sources — **IDE remote-server connections, systemd-scope/cgroup user processes, unknown user-owned processes** — so `when_fully_idle`'s "unknown processes block stop" safety rule can never fire end-to-end (domain field exists, always 0). Also missing: cgroup-escape/executable-identity-change detection, versioned detector metadata, plain-language predicate display in CLI/web.
- PARTIAL: stop Operation records only reason+idempotency key (spec wants policy, qualifying snapshots, grace interval); transition audit telemetry; policy-change audit.

### Spec 07 — SSH & networking: proxy datapath + infra excellent, client onboarding unreachable
Byte-bridge proxy with backpressure and no payload logging, JWT verify + ownership, private-IP-only runtimes, SG-referenced port 22, NAT + S3 gateway endpoint, durable host keys restored on replacement, stale-address protection via bootID — IMPLEMENTED (`apps/ssh-proxy/proxy.go`, `regional-cell/main.tf`, `apps/guest/ssh_identity.go`).
- PARTIAL (wiring): `renderSSHConfig` + `ensureSSHInclude` fully implemented with **no non-test caller** — no CLI subcommand exposes SSH config setup.
- MISSING: proxy flow step 5 — **start-on-connect** (proxy 409s on stopped Runtime instead of requesting an idempotent start Operation and streaming progress); interactive key selection; dedicated Ed25519 keygen recommendation; start-failure errors naming the failed semantic step.

### Deferred by spec (not gaps)
Auto-stop default grace/polling values; insufficient-credits refusal (awaits zero-balance policy); SSH host-certificate CA; dir-based logical mounts.

## Cluster: domain model, profiles/projects, capsule packaging (specs 02, 04, 19)

**Verdict: the capsule/profile core is the strongest area of the codebase; the project-side domain model is the hole.**

### Verified IMPLEMENTED (evidence on file in the audit)
Deterministic USTAR packaging (sorted walk, pinned mtime, uid/gid 0, cleared gzip header, xattr strip), digest-verified OCI assemble/parse/pull with owner-prefixed S3 grants and no Referrers dependency, four-level change detection, composition/merge/conflict with per-key provenance, permission components never merged/auto_safe, three-way managed rule with atomic writes + rollback, drift adoption with executable review, capture allowlist scanner (private keys never read, secrets → Credential Requirements), Project Seed bundle/patch/archive immutable + content-addressed, profile linear history with stale-head rejection, owner-only capsule refs enforced.

### Gaps
- MISSING — **`ProjectBinding` and `ProjectSpec` aggregates**: not modeled, no tables; only a `ProjectCapsuleDigest` string is plumbed. **Project discovery/compilation** (Dockerfile/compose/devcontainer/package-manager/port detection → reviewed project capsule) does not exist; `libs/projectseed` covers git state only.
- MISSING — **`CredentialRequirement`/`CredentialBinding` aggregates**: only `CredentialRequirementDigest` survives on materializations; environment-held bindings unmodeled.
- MISSING — **constrained setup runner** (setup commands with no secrets/no network).
- MISSING — `go.mod` has no `toolchain` directive (spec 19 determinism requirement); determinism CI gate PARTIAL (test exists under `pnpm check`, no dedicated cross-machine gate).
- PARTIAL — `referenced` materialization mode plumbed end-to-end but no adapter ever emits it.
- PARTIAL — `CapsuleLock.ProfileVersionID == Environment.PinnedProfileVersionID` invariant not enforced at the domain boundary (holds only by construction).
- PARTIAL — `ActivitySnapshot` exists as transient auto-stop data, not the spec'd persisted aggregate.

### Deferred by spec (not gaps)
Hosted Zot registry, external registries, cosign/Notation signing, ORAS fallback-tag resolution → sharing milestone; owner-only refs interim constraint (implemented); automatic profile merging; project-binding relink; export/import surface; devcontainer as intent-only; region immutability in MVP.

## Cluster: architecture, workflows, API, operations (specs 03, 08, 09, 14)

**Verdict: the product's engine room is the biggest gap — the API contract is complete but ~65% unserved, and the durable-workflow layer is a skeleton.**

### API (spec 09) — contract complete, 9 of 26 endpoints served
- IMPLEMENTED: full `/v1` OpenAPI contract; 202-for-commands; Idempotency-Key (+409 on conflict); X-Request-ID; capsule-access presigning; ssh-keys CRUD; profile publish; uploads; project-seeds; environment create dispatch.
- MISSING (all → `contracts.Unimplemented` 501): **every read model** (`/me`, `/billing`, `/billing/portal`, GET profiles ×3, GET environments ×2, GET operations, GET events), **every runtime lifecycle command** (start/stop/replace, apply-profile, auto-stop-policy PUT, DELETE environment), and **POST /connection-intents** (which the CLI ssh-proxy path calls — the proxy flow has no server side).
- PARTIAL: 422/429 error mappings unobserved; ownership/idempotency verified only on the 9 live endpoints.

### Workflows (spec 08) — 3 of 13 operation types exist
- `environment.create`: PARTIAL — reserves DB records, ensures data volume, resolves profile→lock; **steps 4–11 missing** (EC2 system volume/runtime, attach, network+SSH identity, guest boot, seed apply, credentials, agent validation).
- `profile.resolve`: PARTIAL — resolve→lock done; plan→apply/materialize never invoked from the workflow (library fully exists in libs/profile + libs/adapters).
- `billing.deliver`: IMPLEMENTED. `auto_stop`: coordinator exists as the only Restate virtual object.
- MISSING: `environment.delete`, `runtime.start/stop/replace` (application scaffolding in runtime_commands.go, but no workflow, no `SendRuntimeOperation` sender, no control-plane wiring), `capsule.publish`, `profile.prune`, `project.seed`, `credential.bind`, `environment.reconcile` as workflows; no general Environment virtual object; no periodic reconciliation drivers.
- **No binary serves the workflow services** — only control-plane and devm have `main()`; `restate.NewServer`/Bind appears only in testfixtures.
- Error taxonomy ~12/19 codes present.

### Architecture (spec 03) — baseline solid
Turborepo/Go module/chi/oapi-codegen/pgx+sqlc/goose/oras/WorkOS/Polar/S3-store all IMPLEMENTED. PARTIAL: ssh-proxy lacks start-through-control-plane; guest mTLS control channel + systemd packaging not evident.

### Operations (spec 14) — essentially absent
- MISSING: OpenTelemetry (indirect dep only), ~31 of ~35 metrics, reconciliation loop drivers, operator view, alerts, runbooks. IMPLEMENTED: profile/materialization reconciliation comparison logic (non-destructive drift reporting), request-id middleware.

### Deferred by spec (not gaps)
Docs-site tech choice; hosted registry + registry-token endpoint; external registries/signing; SSE streaming.

## Cluster: billing, security, infrastructure, testing (specs 10, 11, 13, 15)

**Verdict: ledger/provider core strong; money-metering half-built, destructive-op security boundary unimplemented, image/sshd hardening and budget controls absent.**

### Billing (spec 10)
- IMPLEMENTED: single credit pool, versioned rate table, compute usage intervals (open/close/orphan-reconcile, idempotent debits), atomic debit+balance+outbox tx, Restate delivery, webhook signature verify + receipt-first idempotency.
- MISSING: **storage metering entirely** (only the rate type exists); local↔Polar meter reconciliation; order/payment + external-meter webhook projections; checkout/portal creation; entire billing UX.

### Security (spec 11)
- IMPLEMENTED: owner-authorized queries, proxy bearer-before-bridge, host-key persistence, trust classes, provider tag verification before mutation, IMDSv2, no instance role, private-only runtimes, data-volume delete protection.
- MISSING: **environment deletion flow** (ConfirmName exists only in the generated client — no server handler, no recent-auth, no inventory/backup/grace); force-stop/force-detach operator path; sshd hardening directives (password/kbd-interactive/root login never disabled in any image config).
- PARTIAL: token audience enforced as `client_id` not `aud`; no central log-redaction layer; IAM role separation pending planned iam module.

### Infrastructure (spec 13)
- IMPLEMENTED: RDS (encrypted/private/backups), regional cell (VPC/subnets/NAT/S3 endpoint/SGs), capsule-store (owner prefixes, presign IAM, secure transport), runtime resource shape, Packer Ubuntu AMI + manifest.
- MISSING: **image build gates almost entirely** (no OpenSSH hardening test, Docker/volume/reboot smoke, vuln scan, SBOM, promotion); **budget controls entirely** (quota, kill switch, leak detector, alarms); RDS role separation; object-storage lifecycle policies.

### Testing (spec 15)
- IMPLEMENTED: domain suites (autostop thorough), provider conformance, WorkOS/Polar fixtures, workflow restart/replay tests, ssh-proxy binary-frame test.
- MISSING: guest protocol-version compatibility tests, real-AWS smoke, SSH compatibility matrix, storage-billing + meter-reconciliation tests, acceptance demo/launch-gate harness.

### Deferred by spec (not gaps)
Zero-credit policy (pre-paid-launch decision); signing → sharing milestone; scanner open-sourcing; Multi-AZ pre-paid-launch; Zot registry; planned terraform modules/environments.

---

# Consolidated ranking (all clusters)

1. **The end-to-end journey does not run**: 17 of 26 API endpoints 501; create workflow stops at DB reservation (steps 4–11 missing); no runtime start/stop/replace workflows; **no binary serves the workflow services**; connection-intents has no server side; CLI missing the entire lifecycle vocabulary.
   **Deepened by S1 execution (2026-07-17, verified)**: 3 of the 4 existing workflow services cannot run in production at all — their dependencies exist only as test fakes: `PinnedProfileVersionResolver` (blocks EnvironmentCreate), `CapsuleResolver` (blocks ProfileResolve; production S3-store tag/digest resolution deliberately unbuilt per its doc comment), `db.Store` missing `LoadProfileResolveState` (ProfileResolve never runnable against the real DB — latent libs/db gap), `AutoStopSnapshotSource` + `RuntimeStopDispatcher` (block AutoStop). Only BillingDelivery is production-wireable today; slice S1 ships it and leaves growth seams.
   **S1c blockers (verified 2026-07-17)**: (a) `oci.GrantProvider` has NO production implementation anywhere — only test doubles; control-plane builds an S3 presigner but only for the user-facing HTTP endpoint. This also means the GUEST's capsule pulls (`apps/guest/capsule_materialization.go` consumes GrantProvider) have no production grant source. Ruling: direct S3-backed GrantProvider in `libs/capsule/oci` (workflows service holds AWS creds by ratified architecture; the HTTP endpoint authenticates users, not services). (b) No side-effect-free db query maps an EnvironmentCreate operationID → {owner, environment, pinned Profile Version} — the only mapping method also records invocations. Both folded into S1c's re-scope.
   **S1b-2 shipped** (`b27e026`): production digest-based CapsuleResolver over the S3 store (`libs/capsule/oci/resolver.go`). **New named product decision (verified 2026-07-17)**: refs support a moving tag form and `track`/`review` freshness policies require resolving it, but neither the content-addressed store nor the DB has any tag→digest mapping — tag resolution needs either a Postgres capsule-publication tag index or the sharing-milestone registry. Until decided, tag refs fail with an explicit error; `pin`/digest refs work end-to-end.
2. **Read models** (GET environments/profiles/operations/events, /me, /billing) — blocks CLI status, web surfaces, and the contracts-client wiring.
3. **Deletion safety flow** (specs 05+11) — a security boundary, currently contract-only.
4. **Last-mile wiring**: SSH config onboarding coded but uncalled; proxy start-on-connect; guest activity 3 missing sources; plan→apply not invoked from workflow.
5. **Project-side domain**: ProjectBinding/ProjectSpec/CredentialRequirement/Binding aggregates + project discovery.
6. **Billing completion**: storage metering, meter reconciliation, then UI.
7. **Hardening**: sshd directives, image build gates, budget controls.
8. **Observability**: OTel, metrics, reconciliation drivers, operator view.
9. Quick wins: determinism CI gate (spec 19's "toolchain pin" resolves HERE, not in go.mod — a `toolchain` directive equal to the `go` line is stripped as redundant by Go tooling; the real pin is CI with GOTOOLCHAIN=local + exact toolchain install — verified 2026-07-17), lock↔pinned-version invariant, `referenced` mode emission, DX docs (CLI env vars, dev loop, README quickstart — slice S0).
