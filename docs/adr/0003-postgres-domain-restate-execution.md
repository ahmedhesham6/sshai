# PostgreSQL owns product state and Restate owns durable execution

RDS PostgreSQL is authoritative for product aggregates, ledgers, projections, and audit records. Restate Cloud is authoritative for workflow journals, retries, timers, and keyed execution; operation rows in PostgreSQL are projections rather than a second workflow engine. This avoids both a hand-built queue and duplicated domain authority.
