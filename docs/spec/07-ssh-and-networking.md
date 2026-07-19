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
3. The CLI creates a Connection Intent, validates the returned regional `proxyUrl`, and opens that WSS endpoint with its bearer and the Intent ID in `X-Connection-Intent-ID`.
4. Before upgrading the socket, the proxy verifies JWT signature, issuer, audience, and expiry and validates the unexpired, unused Connection Intent for that subject and path Environment without consuming it. Immediately after a successful upgrade it atomically consumes the Intent; a raced reuse receives a terminal `intent-invalid` control failure and the socket closes.
5. The consumed Intent's nullable start Operation is authoritative. When present, the proxy waits for that Operation's Runtime readiness without issuing another start. Only an Intent without an Operation may use the proxy's idempotent start fallback.
6. The proxy resolves the Runtime's observed private address only after current-boot readiness and rechecks Environment ownership through the route lookup.
7. The proxy opens a TCP connection to private port 22.
8. Binary WebSocket frames bridge bytes without terminating SSH encryption.

Connection Intent expiry is checked only when the WSS attempt is admitted. Consuming the Intent authorizes that one attempt; expiry during the bounded readiness wait does not cancel or re-authorize the in-flight connection.

The proxy must apply backpressure and bounded buffers. Readiness admission defaults to 64 waiting connections globally and 4 per Environment, configurable with `MAX_WAITING_CONNECTIONS` and `MAX_WAITING_CONNECTIONS_PER_ENVIRONMENT`; excess admission receives HTTP 503 before upgrade. It must not log SSH payload bytes.

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
- Insufficient credits returns the client-owned, actionable `credits-blocked` refusal before compute allocation, whether the block is returned while creating the Connection Intent or by the proxy's fallback start.
- Start failure returns an SSH-proxy error naming the failed semantic step and confirming persistent state remains intact.
- WebSocket disconnect does not automatically stop the Runtime; the Auto-stop Policy evaluates observed activity.
- A stale private address is never reused without boot/readiness confirmation.
