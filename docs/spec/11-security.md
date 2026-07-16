# Security and trust model

## Trust boundaries

| Boundary | Rule |
|---|---|
| User ↔ WorkOS | WorkOS authenticates; application authorizes ownership |
| CLI ↔ API/proxy | Short-lived WorkOS token over TLS |
| Proxy ↔ Runtime | Regional private network; SSH remains end-to-end encrypted |
| Workflow service ↔ AWS | Region-scoped least-privilege IAM roles |
| Control plane ↔ Polar | Server-side secret; immutable idempotent events |
| Guest ↔ control plane | Environment-scoped mTLS or signed credential |
| Profile compiler ↔ local files | Local-only discovery before selection |
| Runtime user ↔ guest | User has development privileges; guest telemetry is evidence, not billing authority |

## Identity and authorization

- Hosted WorkOS AuthKit handles web sign-in and CLI device authorization.
- API and proxy validate issuer, audience, signature, expiry, and subject.
- Every resource query includes owner authorization.
- No organization or shared-resource authorization path exists in the MVP.
- Sensitive operations require recent authentication when WorkOS supports the needed signal.

## SSH

- Ed25519 public-key authentication only.
- Password, keyboard-interactive, and root SSH login disabled.
- Runtime port 22 accepts only the regional proxy security group.
- Private keys never leave the client.
- Proxy authentication is required in addition to SSH-key authentication.
- Host keys persist with the Environment and are not regenerated during Runtime replacement.

## Local scanner

- Open source the scanner and bundle format before public launch.
- Scan known roots; do not archive home directories.
- Do not read private-key contents.
- Reject symlink escapes.
- Exclude unknown files.
- Run secret detection before packaging.
- Display every selected path and selector before upload.

## Capsule supply chain

Components carry one of three trust classes:

| Trust class | Rule |
|---|---|
| `declarative` | Selecting or packaging the Component never authorizes execution. |
| `executable` | A changed executable digest requires renewed review, with the diff since last approval shown. |
| `permission` | `permission-policy` Components are always re-consented with explicit itemized consent at apply time; they are never `auto_safe` and never trusted transitively by a signature or `track` policy. |

- Component trust class is digest-bound: it appears in the Capsule manifest and as the `devm.component.trust` layer annotation, so it is covered by the Capsule digest.
- Integration (MCP) Component changes and any Credential Requirement changes are never `auto_safe` and always require review.
- Capture uses an explicit allowlist with per-item consent; unknown content is excluded, private keys are never read, and secret scanning runs before packaging.
- Capture-time secret extraction includes MCP `env` blocks. Names and references become Credential Requirements; values are never copied into Capsule layers, registry metadata, plans, logs, or diffs.
- Capsule access uses short-lived presigned grants minted by the control plane and scoped per owner prefix. Guest pulls use the Environment's mTLS channel.
- Until signing ships, Environments may reference only Capsules owned by the authenticated user; the control plane enforces this constraint.
- Signing is deferred. The sharing milestone may use cosign keyless signing; Notation is the bring-your-own-PKI alternative. Provenance does not equal review, and signing never replaces the consent boundary.

## Capsules and Components

- Capsules are immutable, digest-addressed OCI artifacts with one layer per Component.
- Components are independently content-addressed and their digests are verified before materialization.
- No automatic skill, hook, or plugin execution.
- Environments materialize only from Capsule Locks.
- Managed updates never overwrite drift.
- Setup runner has no secrets and no network by default.

## Credentials

- Capsules declare Credential Requirements and references only; Environments hold Credential Bindings.
- Codex/Claude login caches are environment-specific and never exported.
- Git provider access uses scoped, preferably short-lived credentials.
- Static secrets are resolved just in time and never written to operation logs.
- Setup occurs before sensitive runtime credentials are exposed.
- Do not globally export every secret into interactive shells.

## Provider safety

- Tag every resource with environment ID, operation ID, region, environment, and managed-by.
- Verify ownership tags before mutation or deletion.
- Data volumes have deletion-on-termination disabled.
- Never compensate a later failure by deleting an existing persistent data volume.
- Runtime and service IAM roles are separate.
- Runtimes receive no control-plane AWS permissions.
- Require IMDSv2 and restrict metadata access.

## Network

- Private-only Runtimes.
- No public inbound ports.
- Regional proxy is the only SSH ingress.
- Managed NAT provides outbound internet.
- Security groups reference other managed security groups where possible.
- Cross-region proxy-to-Runtime access is denied.

## Logging and privacy

Never log:

- WorkOS access or refresh tokens;
- Polar secrets or full billing payloads containing personal data;
- SSH private keys or payload bytes;
- secret values or agent auth caches;
- environment variables and arbitrary command lines from Activity Snapshots;
- Capsule layer contents unless explicitly redacted and access-controlled for support.

## Destructive operations

Environment deletion requires exact-name confirmation, recent authentication, unique-state inventory, provider ownership verification, and the private-alpha backup/grace procedure. Force-stop and force-detach are operator-only, audited, and never automatic.
