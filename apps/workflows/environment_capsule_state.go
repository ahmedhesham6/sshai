package workflows

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/ahmedhesham6/sshai/apps/guest"
	dbstore "github.com/ahmedhesham6/sshai/libs/db"
	"github.com/ahmedhesham6/sshai/libs/domain"
)

var errInvalidEnvironmentCapsuleState = errors.New("invalid Environment Capsule state")

// PinnedProfileVersionResolver adapts the profile.resolve pathway to the
// Environment create workflow's durable activity boundary.
type PinnedProfileVersionResolver interface {
	ResolvePinnedProfileVersion(context.Context, string, time.Time) (EnvironmentCapsuleState, error)
}

type EnvironmentCapsuleStateRepository interface {
	PersistEnvironmentCapsuleState(context.Context, dbstore.EnvironmentCapsuleStateInput) error
}

type environmentCreationCapsuleActions struct {
	resolver   PinnedProfileVersionResolver
	repository EnvironmentCapsuleStateRepository
}

func NewEnvironmentCreationCapsuleActions(resolver PinnedProfileVersionResolver, repository EnvironmentCapsuleStateRepository) EnvironmentCreationCapsuleActions {
	return &environmentCreationCapsuleActions{resolver: resolver, repository: repository}
}

func (actions *environmentCreationCapsuleActions) ResolvePinnedProfileVersion(ctx context.Context, operationID string, at time.Time) (EnvironmentCapsuleState, error) {
	return actions.resolver.ResolvePinnedProfileVersion(ctx, operationID, at)
}

func (actions *environmentCreationCapsuleActions) PersistEnvironmentCapsuleState(ctx context.Context, operationID string, state EnvironmentCapsuleState) error {
	lock, err := domain.CreateCapsuleLock(state.CapsuleLock)
	if err != nil {
		return fmt.Errorf("%w: malformed Capsule Lock: %v", errInvalidEnvironmentCapsuleState, err)
	}
	records, err := EnvironmentMaterializationRecords(lock.Snapshot(), state.Materializations)
	if err != nil {
		return err
	}
	return actions.repository.PersistEnvironmentCapsuleState(ctx, dbstore.EnvironmentCapsuleStateInput{
		OperationID: operationID, EnvironmentID: lock.Snapshot().EnvironmentID, CapsuleLock: lock, UpgradePolicy: state.UpgradePolicy, Materializations: records,
	})
}

// EnvironmentMaterializationRecords enriches guest state with Component type
// and trust identity from the validated Capsule Lock before persistence.
func EnvironmentMaterializationRecords(lock domain.CapsuleLockSnapshot, installed []guest.InstalledMaterialization) ([]dbstore.EnvironmentMaterialization, error) {
	records := make([]dbstore.EnvironmentMaterialization, len(installed))
	for index, materialization := range installed {
		component, ok := lock.ResolvedComponents[materialization.ComponentID]
		if !ok {
			return nil, fmt.Errorf("%w: Component %q is not resolved by Capsule Lock %q", errInvalidEnvironmentCapsuleState, materialization.ComponentID, lock.ID)
		}
		if materialization.LockID != lock.ID || materialization.LockDigest != lock.Digest ||
			materialization.CapsuleDigest != component.CapsuleDigest || materialization.ComponentDigest != component.ComponentDigest ||
			materialization.Scope != component.Scope {
			return nil, fmt.Errorf("%w: Materialization %q does not match Capsule Lock %q", errInvalidEnvironmentCapsuleState, materialization.ComponentID, lock.ID)
		}
		records[index] = dbstore.EnvironmentMaterialization{
			EnvironmentID: lock.EnvironmentID, ID: materialization.ID, LockID: materialization.LockID,
			LockDigest: materialization.LockDigest, CapsuleDigest: materialization.CapsuleDigest,
			ComponentID: materialization.ComponentID, ComponentDigest: materialization.ComponentDigest,
			AdapterID: materialization.AdapterID, AdapterVersion: materialization.AdapterVersion,
			TargetAgentVersion: materialization.TargetAgentVersion, Scope: materialization.Scope,
			ComponentType: component.Type, TrustClass: component.TrustClass,
			NonSecretOverridesDigest: materialization.NonSecretOverridesDigest,
			SecretVersionIdentifiers: append([]string(nil), materialization.SecretVersionIdentifiers...),
			EffectiveCacheKey:        materialization.EffectiveCacheKey, Mode: string(materialization.Mode), Root: string(materialization.Root),
			Target: materialization.Target, Selector: materialization.Selector, Directory: materialization.Directory,
			FilePaths: append([]string(nil), materialization.FilePaths...), LastAppliedDigest: materialization.LastAppliedDigest,
			ObservedDigest: materialization.ObservedDigest, CredentialRequirementDigest: materialization.CredentialRequirementDigest,
			CreatedAt: lock.CreatedAt, UpdatedAt: lock.CreatedAt,
		}
	}
	return records, nil
}
