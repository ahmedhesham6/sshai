# SSH access, proxy, and networking

## Client contract

OpenSSH behavior is the compatibility contract. The same stable alias must support:

- interactive PTY;
- noninteractive commands and exit codes;
- SCP, SFTP, and rsync;
- local, remote, and dynamic forwarding;
- keepalives;
- multiple concurrent connections;
- Codex desktop and mainstream Remote SSH IDEs.

## Generated client configuration

The CLI adds one include to the user's primary SSH configuration:

```sshconfig
Include ~/.config/devm/ssh/config
```

It owns only the included file. A generated Environment entry resembles:

```sshconfig
Host api-dev
    HostName env_01JEXAMPLE
    User dev
    IdentityFile ~/.ssh/id_ed25519
    UserKnownHostsFile ~/.config/devm/known_hosts
    ProxyCommand devm ssh-proxy --environment %h
    ServerAliveInterval 30
```

`HostName` is a stable logical identifier consumed by the proxy, not DNS for the Runtime.

## SSH-key onboarding

1. Discover existing `.pub` Ed25519 keys.
2. Default selection (2026-07-18): exactly one key found → use it silently; multiple → pick the most-recently-used, print which, and offer an override flag. No interactive stall in the common path.
3. Generate a dedicated `devm` Ed25519 key when no suitable key exists.
4. Upload only the selected public key and fingerprint.
5. Store the private-key path in local CLI configuration only.
6. Materialize active public keys into the Environment during create/start reconciliation.

Private-key contents are never uploaded or logged.

## Proxy flow

1. OpenSSH launches `devm ssh-proxy` through `ProxyCommand`.
2. The CLI refreshes its WorkOS access token when necessary.
3. The CLI requests the Environment route and opens an authenticated WSS connection to its regional proxy.
4. The proxy verifies JWT signature, issuer, audience, expiry, and Environment ownership.
5. If the Runtime is stopped, the proxy requests an idempotent start Operation and streams progress control messages while waiting.
6. The proxy resolves the Runtime's observed private address only after current-boot readiness.
7. The proxy opens a TCP connection to private port 22.
8. Binary WebSocket frames bridge bytes without terminating SSH encryption.

The proxy must apply backpressure and bounded buffers. It must not log SSH payload bytes.

## Regional routing

The global API returns the Environment's regional proxy URL. Each enabled region runs its own proxy service inside the regional VPC. Cross-region proxy-to-Runtime traffic is forbidden.

## Runtime networking

- Runtimes have private IPv4 addresses only.
- Runtime security groups accept port 22 only from the regional proxy security group.
- Outbound access routes through regional managed NAT egress.
- An S3 gateway endpoint should be present to reduce NAT usage for supported platform flows.
- There are no per-Environment public DNS records, Elastic IPs, public port 22 rules, or user-managed CIDR allowlists.

## Host identity

The Runtime SSH host key belongs to the Environment, not the system volume. Store it on the durable data volume and restore it during Runtime replacement. A future SSH host-certificate CA may replace persistent keys, but the MVP must never train users to ignore host-key changes.

## Failure behavior

- Auth failure closes before Runtime start.
- Insufficient credits returns a product-level refusal before compute allocation once zero-balance policy is defined.
- Start failure returns an SSH-proxy error naming the failed semantic step and confirming persistent state remains intact.
- WebSocket disconnect does not automatically stop the Runtime; the Auto-stop Policy evaluates observed activity.
- A stale private address is never reused without boot/readiness confirmation.
