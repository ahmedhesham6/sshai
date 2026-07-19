package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"time"

	sshproxy "github.com/ahmedhesham6/sshai/apps/ssh-proxy"
	"github.com/ahmedhesham6/sshai/libs/auth"
	dbstore "github.com/ahmedhesham6/sshai/libs/db"
	"github.com/jackc/pgx/v5"
)

const resolveSSHQuery = `
SELECT r.id, r.status, r.private_address, r.boot_id, r.version
FROM users u
JOIN environments e ON e.owner_user_id = u.id
LEFT JOIN runtimes r
  ON r.environment_id = e.id AND r.id = e.current_runtime_id
WHERE u.workos_user_id = $1
  AND e.id = $2
  AND e.region = $3
  AND e.deleted_at IS NULL`

const currentBootAttemptQuery = `
SELECT r.id, r.version, r.status
FROM environments e
JOIN runtimes r
  ON r.environment_id = e.id AND r.id = e.current_runtime_id
WHERE e.id = $1
  AND e.region = $2
  AND e.deleted_at IS NULL`

type rowQuerier interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}

// postgresRouteStore is deliberately owner-scoped at the SQL boundary. A
// route is emitted only from the current Runtime row when its domain-enforced
// ready state still carries both the current boot ID and private address.
// CurrentBootAttempt omits the owner predicate because it is called only after
// ResolveSSH has authorized the same request and is used solely to derive an
// idempotency key, never to return route data.
type postgresRouteStore struct {
	queries rowQuerier
	region  string
}

type postgresIntentStore struct {
	store *dbstore.Store
	now   func() time.Time
}

func (store postgresIntentStore) Consume(ctx context.Context, subject auth.Subject, intentID, environmentID string) (sshproxy.ConnectionIntentAttempt, error) {
	if store.store == nil || store.now == nil {
		return sshproxy.ConnectionIntentAttempt{}, errors.New("consume Connection Intent: store is unavailable")
	}
	record, err := store.store.ConsumeConnectionIntent(ctx, subject.WorkOSUserID, intentID, environmentID, store.now().UTC())
	if err != nil {
		switch {
		case errors.Is(err, dbstore.ErrConnectionIntentNotFound):
			return sshproxy.ConnectionIntentAttempt{}, sshproxy.ErrConnectionIntentNotFound
		case errors.Is(err, dbstore.ErrConnectionIntentExpired):
			return sshproxy.ConnectionIntentAttempt{}, sshproxy.ErrConnectionIntentExpired
		case errors.Is(err, dbstore.ErrConnectionIntentUsed):
			return sshproxy.ConnectionIntentAttempt{}, sshproxy.ErrConnectionIntentUsed
		default:
			return sshproxy.ConnectionIntentAttempt{}, err
		}
	}
	return sshproxy.ConnectionIntentAttempt{OperationID: record.OperationID}, nil
}

func (store postgresRouteStore) ResolveSSH(ctx context.Context, subject auth.Subject, environmentID string) (sshproxy.EnvironmentSSHRoute, error) {
	var runtimeID, status, privateAddress, bootID *string
	var version *int64
	err := store.queries.QueryRow(ctx, resolveSSHQuery, subject.WorkOSUserID, environmentID, store.region).Scan(
		&runtimeID, &status, &privateAddress, &bootID, &version,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return sshproxy.EnvironmentSSHRoute{}, sshproxy.ErrEnvironmentNotFound
	}
	if err != nil {
		return sshproxy.EnvironmentSSHRoute{}, fmt.Errorf("resolve Environment SSH route: %w", err)
	}
	if status != nil && *status == "error" {
		return sshproxy.EnvironmentSSHRoute{}, sshproxy.ErrRuntimeStartFailed
	}
	if runtimeID == nil || status == nil || *status != "ready" || privateAddress == nil || bootID == nil || version == nil || *version < 1 || *bootID == "" {
		return sshproxy.EnvironmentSSHRoute{}, sshproxy.ErrRuntimeNotReady
	}
	return sshproxy.EnvironmentSSHRoute{
		RuntimeID: *runtimeID, BootID: *bootID,
		PrivateAddress: net.JoinHostPort(*privateAddress, "22"),
	}, nil
}

func (store postgresRouteStore) CurrentBootAttempt(ctx context.Context, environmentID string) (sshproxy.RuntimeBootAttempt, error) {
	var attempt sshproxy.RuntimeBootAttempt
	err := store.queries.QueryRow(ctx, currentBootAttemptQuery, environmentID, store.region).Scan(
		&attempt.RuntimeID, &attempt.RuntimeVersion, &attempt.RuntimeStatus,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return sshproxy.RuntimeBootAttempt{}, sshproxy.ErrEnvironmentNotFound
	}
	if err != nil {
		return sshproxy.RuntimeBootAttempt{}, fmt.Errorf("resolve Runtime boot attempt: %w", err)
	}
	return attempt, nil
}
