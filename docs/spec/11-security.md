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

## Profile artifacts

- Content-addressed immutable object storage.
- Digests verified before materialization.
- Executable content separately classified.
- Selecting content does not authorize execution.
- No automatic skill/hook/plugin execution.
- Managed updates never overwrite drift.
- Setup runner has no secrets and no network by default.

## Credentials

- Profiles store Credential Requirements and references only.
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
- profile artifact bodies unless explicitly redacted and access-controlled for support.

## Destructive operations

Environment deletion requires exact-name confirmation, recent authentication, unique-state inventory, provider ownership verification, and the private-alpha backup/grace procedure. Force-stop and force-detach are operator-only, audited, and never automatic.
