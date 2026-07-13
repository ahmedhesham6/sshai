# Use Go for the backend and TypeScript for product websites

The CLI, API, workflow service, SSH proxy, guest supervisor, provider adapters, and billing integration use Go in a Turborepo monorepo; TanStack Start and the documentation site use TypeScript. The product is dominated by process supervision, SSH streaming, concurrency, and infrastructure lifecycles, so a shared Go backend is more valuable than TypeScript end to end.
