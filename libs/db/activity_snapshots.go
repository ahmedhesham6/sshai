package db

import (
	"context"
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/ahmedhesham6/sshai/libs/db/internal/dbsql"
	"github.com/ahmedhesham6/sshai/libs/domain"
	"github.com/jackc/pgx/v5"
)

type AutoStopSnapshotState struct {
	RuntimeID        string
	Policy           domain.AutoStopPolicySnapshot
	PolicyGeneration uint64
	Snapshot         *domain.AutoStopActivitySnapshot
	Conflicts        []domain.AutoStopConflict
}

func (store *Store) StoreActivitySnapshot(ctx context.Context, environmentID string, snapshot domain.AutoStopActivitySnapshot) error {
	if environmentID == "" || snapshot.RuntimeID == "" || snapshot.Sequence == 0 || snapshot.Sequence > math.MaxInt64 || snapshot.ObservedAt.IsZero() {
		return errors.New("store Activity Snapshot: Environment, Runtime, sequence, and observation time are required")
	}
	counts := [...]int{snapshot.SSHConnections, snapshot.IDEConnections, snapshot.CodexProcesses, snapshot.ClaudeProcesses,
		snapshot.ProtectedProcesses, snapshot.SelectedContainers, snapshot.UnknownUserProcesses}
	for _, count := range counts {
		if count < 0 || count > math.MaxInt32 {
			return errors.New("store Activity Snapshot: counts must be non-negative 32-bit integers")
		}
	}
	inserted, err := store.queries.InsertActivitySnapshot(ctx, dbsql.InsertActivitySnapshotParams{
		RuntimeID: snapshot.RuntimeID, Sequence: int64(snapshot.Sequence), EnvironmentID: environmentID,
		ObservedAt: timestamp(snapshot.ObservedAt), SshConnections: int32(snapshot.SSHConnections),
		IdeConnections: int32(snapshot.IDEConnections), CodexProcesses: int32(snapshot.CodexProcesses),
		ClaudeProcesses: int32(snapshot.ClaudeProcesses), ProtectedProcesses: int32(snapshot.ProtectedProcesses),
		SelectedContainers: int32(snapshot.SelectedContainers), UnknownUserProcesses: int32(snapshot.UnknownUserProcesses),
	})
	if err != nil {
		return fmt.Errorf("store Activity Snapshot: insert: %w", err)
	}
	if inserted == 0 {
		row, err := store.queries.GetActivitySnapshot(ctx, dbsql.GetActivitySnapshotParams{
			RuntimeID: snapshot.RuntimeID, Sequence: int64(snapshot.Sequence), EnvironmentID: environmentID,
		})
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrReferenceNotOwned
		}
		if err != nil {
			return fmt.Errorf("store Activity Snapshot: load replay: %w", err)
		}
		persisted, err := activitySnapshotFromRow(row)
		if err != nil {
			return err
		}
		if !sameActivitySnapshot(persisted, snapshot) {
			return ErrIdempotencyConflict
		}
	}
	return nil
}

func sameActivitySnapshot(first, second domain.AutoStopActivitySnapshot) bool {
	firstObservedAt, secondObservedAt := first.ObservedAt, second.ObservedAt
	first.ObservedAt, second.ObservedAt = time.Time{}, time.Time{}
	return first == second && firstObservedAt.Equal(secondObservedAt)
}

func (store *Store) LatestAutoStopSnapshot(ctx context.Context, environmentID, runtimeID string) (AutoStopSnapshotState, error) {
	state, err := store.LoadAutoStopSnapshotState(ctx, environmentID, runtimeID)
	if err != nil {
		return AutoStopSnapshotState{}, err
	}
	state.Snapshot, err = store.LatestActivitySnapshot(ctx, environmentID, runtimeID)
	return state, err
}

func (store *Store) LoadAutoStopSnapshotState(ctx context.Context, environmentID, runtimeID string) (AutoStopSnapshotState, error) {
	if environmentID == "" || runtimeID == "" {
		return AutoStopSnapshotState{}, errors.New("load Auto-stop Snapshot state: Environment and Runtime are required")
	}
	policy, err := store.queries.GetAutoStopPolicyState(ctx, environmentID)
	if errors.Is(err, pgx.ErrNoRows) || err == nil && (policy.CurrentRuntimeID == nil || *policy.CurrentRuntimeID != runtimeID) {
		return AutoStopSnapshotState{}, ErrReferenceNotOwned
	}
	if err != nil {
		return AutoStopSnapshotState{}, fmt.Errorf("load Auto-stop Snapshot state: load Policy: %w", err)
	}
	state := AutoStopSnapshotState{
		RuntimeID: runtimeID,
		Policy: domain.AutoStopPolicySnapshot{
			ID: policy.ID, EnvironmentID: policy.EnvironmentID, Mode: domain.AutoStopMode(policy.Mode),
			GracePeriodSeconds: int(policy.GracePeriodSeconds),
		},
		PolicyGeneration: uint64(policy.Generation),
	}
	operationTypes, err := store.queries.ListActiveAutoStopOperationTypes(ctx, environmentID)
	if err != nil {
		return AutoStopSnapshotState{}, fmt.Errorf("load Auto-stop Snapshot state: load conflicts: %w", err)
	}
	for _, operationType := range operationTypes {
		switch domain.OperationType(operationType) {
		case domain.OperationEnvironmentCreate:
			state.Conflicts = append(state.Conflicts, domain.AutoStopConflictSetup)
		case domain.OperationProfileResolve:
			state.Conflicts = append(state.Conflicts, domain.AutoStopConflictMaterialization)
		case domain.OperationRuntimeStart:
			state.Conflicts = append(state.Conflicts, domain.AutoStopConflictStart)
		case domain.OperationRuntimeReplace:
			state.Conflicts = append(state.Conflicts, domain.AutoStopConflictReplace)
		}
	}
	return state, nil
}

func (store *Store) LatestActivitySnapshot(ctx context.Context, environmentID, runtimeID string) (*domain.AutoStopActivitySnapshot, error) {
	if environmentID == "" || runtimeID == "" {
		return nil, errors.New("read latest Activity Snapshot: Environment and Runtime are required")
	}
	row, err := store.queries.GetLatestActivitySnapshot(ctx, dbsql.GetLatestActivitySnapshotParams{
		RuntimeID: runtimeID, EnvironmentID: environmentID,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read latest Activity Snapshot: %w", err)
	}
	snapshot, err := activitySnapshotFromRow(row)
	if err != nil {
		return nil, err
	}
	return &snapshot, nil
}

func (store *Store) PruneActivitySnapshots(ctx context.Context, retainAfter time.Time) (int64, error) {
	if retainAfter.IsZero() {
		return 0, errors.New("prune Activity Snapshots: retention boundary is required")
	}
	pruned, err := store.queries.PruneActivitySnapshots(ctx, timestamp(retainAfter))
	if err != nil {
		return 0, fmt.Errorf("prune Activity Snapshots: %w", err)
	}
	return pruned, nil
}

func (store *Store) RuntimeStopDispatchOwner(ctx context.Context, environmentID, runtimeID string) (string, error) {
	row, err := store.queries.GetRuntimeStopDispatchOwner(ctx, environmentID)
	if errors.Is(err, pgx.ErrNoRows) || err == nil && (row.CurrentRuntimeID == nil || *row.CurrentRuntimeID != runtimeID) {
		return "", ErrReferenceNotOwned
	}
	if err != nil {
		return "", fmt.Errorf("load automatic Runtime stop owner: %w", err)
	}
	return row.OwnerUserID, nil
}

func activitySnapshotFromRow(row dbsql.ActivitySnapshot) (domain.AutoStopActivitySnapshot, error) {
	if !row.ObservedAt.Valid || row.Sequence < 1 {
		return domain.AutoStopActivitySnapshot{}, errors.New("restore Activity Snapshot: database returned invalid identity or observation time")
	}
	return domain.AutoStopActivitySnapshot{
		RuntimeID: row.RuntimeID, Sequence: uint64(row.Sequence), ObservedAt: row.ObservedAt.Time,
		SSHConnections: int(row.SshConnections), IDEConnections: int(row.IdeConnections),
		CodexProcesses: int(row.CodexProcesses), ClaudeProcesses: int(row.ClaudeProcesses),
		ProtectedProcesses: int(row.ProtectedProcesses), SelectedContainers: int(row.SelectedContainers),
		UnknownUserProcesses: int(row.UnknownUserProcesses),
	}, nil
}
