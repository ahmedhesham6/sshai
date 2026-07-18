package db

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/ahmedhesham6/sshai/libs/db/internal/dbsql"
	"github.com/ahmedhesham6/sshai/libs/domain"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

// EnvironmentDetail is the read-only projection an owner-scoped Environment
// query resolves to: the Environment aggregate, its Auto-stop Policy, its
// current Runtime (if any Runtime has ever been provisioned), the in-flight
// Operation ID (if one is active), and its pinned Capsule Lock (if the
// Environment has resolved one yet).
type EnvironmentDetail struct {
	Environment        domain.Environment
	AutoStopMode       domain.AutoStopMode
	GracePeriodSeconds int
	Runtime            *domain.Runtime
	ActiveOperationID  *string
	CapsuleLock        *domain.CapsuleLock
}

// GetOwnedEnvironment loads a single Environment owned by ownerID. An absent
// or foreign Environment reports ErrReferenceNotOwned.
func (store *Store) GetOwnedEnvironment(ctx context.Context, ownerID, environmentID string) (EnvironmentDetail, error) {
	if strings.TrimSpace(ownerID) == "" || strings.TrimSpace(environmentID) == "" {
		return EnvironmentDetail{}, errors.New("get owned Environment: canonical owner and Environment IDs are required")
	}
	row, err := store.queries.GetOwnedEnvironmentDetail(ctx, dbsql.GetOwnedEnvironmentDetailParams{
		EnvironmentID: environmentID, OwnerUserID: ownerID,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return EnvironmentDetail{}, ErrReferenceNotOwned
	}
	if err != nil {
		return EnvironmentDetail{}, fmt.Errorf("get owned Environment: %w", err)
	}
	return store.environmentDetailFromRow(ctx, environmentDetailRow(row))
}

// ListOwnedEnvironments loads every Environment owned by ownerID, ordered by
// creation time then ID.
func (store *Store) ListOwnedEnvironments(ctx context.Context, ownerID string) ([]EnvironmentDetail, error) {
	if strings.TrimSpace(ownerID) == "" {
		return nil, errors.New("list owned Environments: canonical owner User ID is required")
	}
	rows, err := store.queries.ListOwnedEnvironmentDetails(ctx, ownerID)
	if err != nil {
		return nil, fmt.Errorf("list owned Environments: %w", err)
	}
	details := make([]EnvironmentDetail, len(rows))
	for index, row := range rows {
		detail, err := store.environmentDetailFromRow(ctx, environmentDetailRow(row))
		if err != nil {
			return nil, err
		}
		details[index] = detail
	}
	return details, nil
}

// environmentDetailRow is the common shape shared by GetOwnedEnvironmentDetailRow
// and ListOwnedEnvironmentDetailsRow, letting a single restore path serve
// both the Get and the List query.
type environmentDetailRow struct {
	ID                          string
	OwnerUserID                 string
	Name                        string
	Slug                        string
	Lifecycle                   string
	Health                      string
	EnvironmentRegion           string
	EnvironmentAvailabilityZone string
	EnvironmentRuntimePreset    string
	PinnedProfileVersionID      string
	CapsuleLockID               *string
	UpgradePolicy               string
	CurrentRuntimeID            *string
	EnvironmentCreatedAt        pgtype.Timestamptz
	EnvironmentUpdatedAt        pgtype.Timestamptz
	DeletedAt                   pgtype.Timestamptz
	EnvironmentVersion          int64
	AutoStopPolicyID            string
	AutoStopMode                string
	GracePeriodSeconds          int32
	ActiveOperationID           *string
}

func (store *Store) environmentDetailFromRow(ctx context.Context, row environmentDetailRow) (EnvironmentDetail, error) {
	if !row.EnvironmentCreatedAt.Valid || !row.EnvironmentUpdatedAt.Valid {
		return EnvironmentDetail{}, errors.New("restore Environment: database returned invalid timestamps")
	}
	environment, err := domain.RestoreEnvironment(domain.EnvironmentSnapshot{
		ID: row.ID, OwnerUserID: row.OwnerUserID, Name: row.Name, Slug: row.Slug,
		Lifecycle: domain.EnvironmentLifecycle(row.Lifecycle), Health: domain.EnvironmentHealth(row.Health),
		Region: row.EnvironmentRegion, AvailabilityZone: row.EnvironmentAvailabilityZone,
		RuntimePreset: row.EnvironmentRuntimePreset, PinnedProfileVersionID: row.PinnedProfileVersionID,
		CapsuleLockID: row.CapsuleLockID, UpgradePolicy: domain.UpgradePolicy(row.UpgradePolicy),
		CurrentRuntimeID: row.CurrentRuntimeID, AutoStopPolicyID: row.AutoStopPolicyID,
		CreatedAt: row.EnvironmentCreatedAt.Time, UpdatedAt: row.EnvironmentUpdatedAt.Time,
		DeletedAt: optionalTime(row.DeletedAt), Version: row.EnvironmentVersion,
	})
	if err != nil {
		return EnvironmentDetail{}, fmt.Errorf("restore Environment: %w", err)
	}
	detail := EnvironmentDetail{
		Environment: environment, AutoStopMode: domain.AutoStopMode(row.AutoStopMode),
		GracePeriodSeconds: int(row.GracePeriodSeconds), ActiveOperationID: row.ActiveOperationID,
	}
	if row.CurrentRuntimeID != nil {
		runtime, err := store.restoreRuntimeByID(ctx, *row.CurrentRuntimeID)
		if err != nil {
			return EnvironmentDetail{}, err
		}
		detail.Runtime = &runtime
	}
	if row.CapsuleLockID != nil {
		lockRow, err := store.queries.GetCapsuleLockForEnvironment(ctx, dbsql.GetCapsuleLockForEnvironmentParams{
			LockID: *row.CapsuleLockID, EnvironmentID: row.ID,
		})
		if err != nil {
			return EnvironmentDetail{}, fmt.Errorf("get owned Environment: load pinned Capsule Lock: %w", err)
		}
		lock, err := restoreCapsuleLock(lockRow)
		if err != nil {
			return EnvironmentDetail{}, err
		}
		detail.CapsuleLock = &lock
	}
	return detail, nil
}

func (store *Store) restoreRuntimeByID(ctx context.Context, runtimeID string) (domain.Runtime, error) {
	row, err := store.queries.GetRuntimeByID(ctx, runtimeID)
	if err != nil {
		return domain.Runtime{}, fmt.Errorf("get owned Environment: load Runtime: %w", err)
	}
	if !row.CreatedAt.Valid || !row.UpdatedAt.Valid {
		return domain.Runtime{}, errors.New("restore Runtime: database returned invalid timestamps")
	}
	runtime, err := domain.RestoreRuntime(domain.RuntimeSnapshot{
		ID: row.ID, EnvironmentID: row.EnvironmentID, Sequence: row.Sequence,
		Status: domain.RuntimeStatus(row.Status), RuntimePreset: row.RuntimePreset,
		Region: row.Region, AvailabilityZone: row.AvailabilityZone, ImageVersion: row.ImageVersion,
		ProviderInstanceRef: row.ProviderInstanceRef, PrivateAddress: row.PrivateAddress, BootID: row.BootID,
		StartedAt: optionalTime(row.StartedAt), StoppedAt: optionalTime(row.StoppedAt), RetiredAt: optionalTime(row.RetiredAt),
		CreatedAt: row.CreatedAt.Time, UpdatedAt: row.UpdatedAt.Time, Version: row.Version,
	})
	if err != nil {
		return domain.Runtime{}, fmt.Errorf("restore Runtime: %w", err)
	}
	return runtime, nil
}
