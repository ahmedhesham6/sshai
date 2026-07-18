# Resolve Capsule tags through a Postgres tag index

Capsule tags are mutable pointers recorded in PostgreSQL at `capsule publish` time as owner-scoped `{owner, name, tag} → digest` rows; resolvers query this index to make the `track` and `review` freshness policies answerable without running a registry service. Capsule content remains immutable and digest-addressed in S3 (ADR 0009); only the tag pointer lives in the database. A hosted OCI registry with real tag lists remains deferred to the sharing milestone (2026-07-18).
