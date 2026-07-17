# devm

`devm` creates agent-ready remote development Environments from a repository and selected personal Profile, connects through standard SSH, preserves durable work, and stops compute according to a user-selected policy.

The repository currently contains the implementation specification baseline. Start with [docs/spec/README.md](./docs/spec/README.md).

## Development

Prerequisites: Go 1.26, Node >=24.14, and pnpm 11 (enable with `corepack enable`; the repo pins `pnpm@11.10.0` in `package.json`).

```sh
pnpm install     # install workspace dependencies
pnpm check       # full gate: contract:lint + format:check + lint + test
pnpm generate    # codegen: go generate per package + tsr generate for web routes
```

Run the web surface (fill in the WorkOS values in `.env`):

```sh
cd apps/web && cp .env.example .env
pnpm dev
```

Run the CLI (requires `DEVM_WORKOS_CLIENT_ID` and `DEVM_CONTROL_PLANE_URL` in the environment):

```sh
go run ./apps/cli/cmd/devm <command>
```

See [AGENTS.md](./AGENTS.md) for the full build model and testing gotchas.
