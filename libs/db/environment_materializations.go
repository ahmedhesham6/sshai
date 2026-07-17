package db

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/ahmedhesham6/sshai/libs/db/internal/dbsql"
	"github.com/ahmedhesham6/sshai/libs/domain"
	"github.com/jackc/pgx/v5"
)

var ErrEnvironmentMaterializationLockMismatch = errors.New("Environment Materialization does not match persisted Capsule Lock")

type EnvironmentPin struct {
	EnvironmentID string
	CapsuleLockID *string
	UpgradePolicy domain.UpgradePolicy
}

type EnvironmentPinInput struct {
	EnvironmentID string
	CapsuleLockID *string
	UpgradePolicy domain.UpgradePolicy
}

// EnvironmentMaterialization is the durable projection of one guest
// InstalledMaterialization plus the resolved Component identity needed to
// validate its Capsule Lock.
type EnvironmentMaterialization struct {
	EnvironmentID               string
	ID                          string
	LockID                      string
	LockDigest                  string
	CapsuleDigest               string
	ComponentID                 string
	ComponentDigest             string
	AdapterID                   string
	AdapterVersion              string
	TargetAgentVersion          string
	Scope                       domain.ComponentScope
	ComponentType               domain.ComponentType
	TrustClass                  domain.TrustClass
	NonSecretOverridesDigest    string
	SecretVersionIdentifiers    []string
	EffectiveCacheKey           string
	Mode                        string
	Root                        string
	Target                      string
	Selector                    string
	Directory                   bool
	FilePaths                   []string
	LastAppliedDigest           string
	ObservedDigest              string
	CredentialRequirementDigest string
	CreatedAt                   time.Time
	UpdatedAt                   time.Time
}

type EnvironmentCapsuleStateInput struct {
	OperationID      string
	EnvironmentID    string
	CapsuleLock      domain.CapsuleLock
	UpgradePolicy    domain.UpgradePolicy
	Materializations []EnvironmentMaterialization
}

func (store *Store) PersistEnvironmentCapsuleState(ctx context.Context, input EnvironmentCapsuleStateInput) error {
	lockSnapshot := input.CapsuleLock.Snapshot()
	if strings.TrimSpace(input.OperationID) == "" || strings.TrimSpace(lockSnapshot.EnvironmentID) == "" {
		return errors.New("persist Environment Capsule state: Operation and Capsule Lock ownership are required")
	}
	policy := input.UpgradePolicy
	if policy == "" {
		policy = domain.UpgradeManual
	}
	if !policy.Valid() {
		return fmt.Errorf("persist Environment Capsule state: upgrade policy %q is invalid", policy)
	}
	tx, err := store.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("persist Environment Capsule state: begin transaction: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	queries := store.queries.WithTx(tx)
	creation, err := lockEnvironmentCreation(ctx, queries, input.OperationID)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrReferenceNotOwned
	}
	if err != nil {
		return fmt.Errorf("persist Environment Capsule state: load persisted Operation: %w", err)
	}
	environmentID := creation.Environment().Snapshot().ID
	if lockSnapshot.EnvironmentID != environmentID {
		return errors.New("persist Environment Capsule state: persisted Operation and Capsule Lock ownership differs")
	}
	persistedLock, err := persistCapsuleLockForEnvironmentState(ctx, queries, input.CapsuleLock)
	if err != nil {
		return err
	}
	persistedLockSnapshot := persistedLock.Snapshot()
	if err := validateEnvironmentMaterializationsAgainstLock(input.Materializations, environmentID, persistedLockSnapshot); err != nil {
		return err
	}
	persistedLockID := persistedLockSnapshot.ID
	if _, err := queries.UpsertEnvironmentPin(ctx, dbsql.UpsertEnvironmentPinParams{
		CapsuleLockID: &persistedLockID, UpgradePolicy: string(policy), EnvironmentID: environmentID,
	}); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrReferenceNotOwned
		}
		return fmt.Errorf("persist Environment Capsule state: persist Environment pin: %w", err)
	}
	if _, err := queries.DeleteEnvironmentMaterializations(ctx, environmentID); err != nil {
		return fmt.Errorf("persist Environment Capsule state: replace Materializations: %w", err)
	}
	for _, record := range input.Materializations {
		record.EnvironmentID = environmentID
		record.LockID = persistedLockID
		if err := upsertEnvironmentMaterialization(ctx, queries, record); err != nil {
			return err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("persist Environment Capsule state: commit: %w", err)
	}
	return nil
}

func persistCapsuleLockForEnvironmentState(ctx context.Context, queries *dbsql.Queries, candidate domain.CapsuleLock) (domain.CapsuleLock, error) {
	snapshot := candidate.Snapshot()
	existing, err := queries.GetCapsuleLockByTarget(ctx, dbsql.GetCapsuleLockByTargetParams{
		EnvironmentID: snapshot.EnvironmentID, ProfileVersionID: snapshot.ProfileVersionID, ProjectCapsuleDigest: snapshot.ProjectCapsuleDigest,
	})
	if err == nil {
		lock, restoreErr := restoreCapsuleLock(existing)
		if restoreErr != nil {
			return domain.CapsuleLock{}, restoreErr
		}
		if lock.Snapshot().Digest != snapshot.Digest {
			return domain.CapsuleLock{}, ErrCapsuleLockConflict
		}
		return lock, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return domain.CapsuleLock{}, fmt.Errorf("persist Environment Capsule state: load Capsule Lock: %w", err)
	}
	capsules, err := json.Marshal(snapshot.Capsules)
	if err != nil {
		return domain.CapsuleLock{}, fmt.Errorf("persist Environment Capsule state: encode Capsules: %w", err)
	}
	components, err := json.Marshal(snapshot.ResolvedComponents)
	if err != nil {
		return domain.CapsuleLock{}, fmt.Errorf("persist Environment Capsule state: encode Components: %w", err)
	}
	if err := queries.InsertCapsuleLock(ctx, dbsql.InsertCapsuleLockParams{
		ID: snapshot.ID, EnvironmentID: snapshot.EnvironmentID, ProfileVersionID: snapshot.ProfileVersionID,
		ProjectCapsuleDigest: snapshot.ProjectCapsuleDigest, Digest: snapshot.Digest, Capsules: capsules,
		ResolvedComponents: components, CreatedAt: timestamp(snapshot.CreatedAt),
	}); err != nil {
		return domain.CapsuleLock{}, fmt.Errorf("persist Environment Capsule state: insert Capsule Lock: %w", err)
	}
	return candidate, nil
}

func (store *Store) GetEnvironmentPin(ctx context.Context, environmentID string) (EnvironmentPin, error) {
	if strings.TrimSpace(environmentID) == "" {
		return EnvironmentPin{}, errors.New("get Environment pin: Environment ID is required")
	}
	row, err := store.queries.GetEnvironmentPin(ctx, environmentID)
	if errors.Is(err, pgx.ErrNoRows) {
		return EnvironmentPin{}, ErrReferenceNotOwned
	}
	if err != nil {
		return EnvironmentPin{}, fmt.Errorf("get Environment pin: %w", err)
	}
	return environmentPinFromRow(row.EnvironmentID, row.CapsuleLockID, row.UpgradePolicy)
}

func (store *Store) UpsertEnvironmentPin(ctx context.Context, input EnvironmentPinInput) (EnvironmentPin, error) {
	if strings.TrimSpace(input.EnvironmentID) == "" {
		return EnvironmentPin{}, errors.New("upsert Environment pin: Environment ID is required")
	}
	if input.CapsuleLockID != nil && (strings.TrimSpace(*input.CapsuleLockID) == "" || *input.CapsuleLockID != strings.TrimSpace(*input.CapsuleLockID)) {
		return EnvironmentPin{}, errors.New("upsert Environment pin: Capsule Lock ID is invalid")
	}
	policy := input.UpgradePolicy
	if policy == "" {
		policy = domain.UpgradeManual
	}
	if !policy.Valid() {
		return EnvironmentPin{}, fmt.Errorf("upsert Environment pin: upgrade policy %q is invalid", policy)
	}
	row, err := store.queries.UpsertEnvironmentPin(ctx, dbsql.UpsertEnvironmentPinParams{
		CapsuleLockID: input.CapsuleLockID, UpgradePolicy: string(policy), EnvironmentID: input.EnvironmentID,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return EnvironmentPin{}, ErrReferenceNotOwned
	}
	if err != nil {
		return EnvironmentPin{}, fmt.Errorf("upsert Environment pin: %w", err)
	}
	return environmentPinFromRow(row.EnvironmentID, row.CapsuleLockID, row.UpgradePolicy)
}

func (store *Store) ListEnvironmentMaterializations(ctx context.Context, environmentID string) ([]EnvironmentMaterialization, error) {
	if strings.TrimSpace(environmentID) == "" {
		return nil, errors.New("list Environment Materializations: Environment ID is required")
	}
	rows, err := store.queries.ListEnvironmentMaterializations(ctx, environmentID)
	if err != nil {
		return nil, fmt.Errorf("list Environment Materializations: %w", err)
	}
	result := make([]EnvironmentMaterialization, len(rows))
	for index, row := range rows {
		result[index], err = environmentMaterializationFromRow(row)
		if err != nil {
			return nil, err
		}
	}
	return result, nil
}

func (store *Store) UpsertEnvironmentMaterializations(ctx context.Context, records []EnvironmentMaterialization) error {
	if len(records) == 0 {
		return nil
	}
	tx, err := store.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("upsert Environment Materializations: begin transaction: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	queries := store.queries.WithTx(tx)
	for _, record := range records {
		if err := upsertEnvironmentMaterialization(ctx, queries, record); err != nil {
			return err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("upsert Environment Materializations: commit: %w", err)
	}
	return nil
}

func (store *Store) ReplaceEnvironmentMaterializationsForLock(ctx context.Context, environmentID, lockID string, records []EnvironmentMaterialization) error {
	if strings.TrimSpace(environmentID) == "" || strings.TrimSpace(lockID) == "" {
		return errors.New("replace Environment Materializations: Environment and Capsule Lock IDs are required")
	}
	tx, err := store.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("replace Environment Materializations: begin transaction: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	queries := store.queries.WithTx(tx)
	lockRow, err := queries.GetCapsuleLockForEnvironment(ctx, dbsql.GetCapsuleLockForEnvironmentParams{
		LockID: lockID, EnvironmentID: environmentID,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrReferenceNotOwned
	}
	if err != nil {
		return fmt.Errorf("replace Environment Materializations: load Capsule Lock: %w", err)
	}
	lock, err := restoreCapsuleLock(lockRow)
	if err != nil {
		return err
	}
	if err := validateEnvironmentMaterializationsAgainstLock(records, environmentID, lock.Snapshot()); err != nil {
		return err
	}
	if _, err := queries.DeleteEnvironmentMaterializations(ctx, environmentID); err != nil {
		return fmt.Errorf("replace Environment Materializations: delete existing rows: %w", err)
	}
	for _, record := range records {
		if record.EnvironmentID == "" {
			record.EnvironmentID = environmentID
		}
		if err := upsertEnvironmentMaterialization(ctx, queries, record); err != nil {
			return err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("replace Environment Materializations: commit: %w", err)
	}
	return nil
}

func upsertEnvironmentMaterialization(ctx context.Context, queries *dbsql.Queries, record EnvironmentMaterialization) error {
	if err := validateEnvironmentMaterialization(record); err != nil {
		return err
	}
	secrets, err := json.Marshal(nonNilStrings(record.SecretVersionIdentifiers))
	if err != nil {
		return fmt.Errorf("upsert Environment Materialization %q: encode secret version identifiers: %w", record.ComponentID, err)
	}
	filePaths, err := json.Marshal(nonNilStrings(record.FilePaths))
	if err != nil {
		return fmt.Errorf("upsert Environment Materialization %q: encode file paths: %w", record.ComponentID, err)
	}
	updated, err := queries.UpsertEnvironmentMaterialization(ctx, dbsql.UpsertEnvironmentMaterializationParams{
		EnvironmentID: record.EnvironmentID, LockID: record.LockID, ID: environmentMaterializationOptionalString(record.ID),
		LockDigest: record.LockDigest, CapsuleDigest: record.CapsuleDigest, ComponentID: record.ComponentID,
		ComponentDigest: record.ComponentDigest, AdapterID: record.AdapterID, AdapterVersion: record.AdapterVersion,
		TargetAgentVersion: record.TargetAgentVersion, Scope: string(record.Scope), ComponentType: string(record.ComponentType),
		TrustClass: string(record.TrustClass), NonSecretOverridesDigest: environmentMaterializationOptionalString(record.NonSecretOverridesDigest),
		SecretVersionIdentifiers: secrets, EffectiveCacheKey: record.EffectiveCacheKey, Mode: environmentMaterializationOptionalString(record.Mode),
		Root: environmentMaterializationOptionalString(record.Root), Target: environmentMaterializationOptionalString(record.Target), Selector: environmentMaterializationOptionalString(record.Selector),
		Directory: record.Directory, FilePaths: filePaths, LastAppliedDigest: environmentMaterializationOptionalString(record.LastAppliedDigest),
		ObservedDigest: environmentMaterializationOptionalString(record.ObservedDigest), CredentialRequirementDigest: environmentMaterializationOptionalString(record.CredentialRequirementDigest),
		CreatedAt: timestamp(record.CreatedAt), UpdatedAt: timestamp(record.UpdatedAt),
	})
	if err != nil {
		return fmt.Errorf("upsert Environment Materialization %q: %w", record.ComponentID, err)
	}
	if updated != 1 {
		return ErrReferenceNotOwned
	}
	return nil
}

func validateEnvironmentMaterialization(record EnvironmentMaterialization) error {
	for name, value := range map[string]string{
		"Environment ID": record.EnvironmentID, "Capsule Lock ID": record.LockID, "Lock digest": record.LockDigest,
		"Capsule digest": record.CapsuleDigest, "Component ID": record.ComponentID, "Component digest": record.ComponentDigest,
		"Adapter ID": record.AdapterID, "Adapter version": record.AdapterVersion, "target agent version": record.TargetAgentVersion,
		"scope": string(record.Scope), "Component type": string(record.ComponentType), "trust class": string(record.TrustClass),
		"effective cache key": record.EffectiveCacheKey,
	} {
		if strings.TrimSpace(value) == "" {
			return fmt.Errorf("upsert Environment Materialization %q: %s is required", record.ComponentID, name)
		}
	}
	if record.CreatedAt.IsZero() || record.UpdatedAt.IsZero() {
		return fmt.Errorf("upsert Environment Materialization %q: timestamps are required", record.ComponentID)
	}
	if record.UpdatedAt.Before(record.CreatedAt) {
		return fmt.Errorf("upsert Environment Materialization %q: updated time is before created time", record.ComponentID)
	}
	return nil
}

func validateEnvironmentMaterializationsAgainstLock(records []EnvironmentMaterialization, environmentID string, lock domain.CapsuleLockSnapshot) error {
	if lock.EnvironmentID != environmentID {
		return fmt.Errorf("%w: Capsule Lock %q belongs to Environment %q, want %q", ErrEnvironmentMaterializationLockMismatch, lock.ID, lock.EnvironmentID, environmentID)
	}
	for _, record := range records {
		component, ok := lock.ResolvedComponents[record.ComponentID]
		if !ok {
			return fmt.Errorf("%w: Component %q is absent from Capsule Lock %q", ErrEnvironmentMaterializationLockMismatch, record.ComponentID, lock.ID)
		}
		if record.EnvironmentID != "" && record.EnvironmentID != environmentID {
			return fmt.Errorf("%w: Component %q belongs to Environment %q, want %q", ErrEnvironmentMaterializationLockMismatch, record.ComponentID, record.EnvironmentID, environmentID)
		}
		if record.LockID != lock.ID || record.LockDigest != lock.Digest ||
			record.CapsuleDigest != component.CapsuleDigest || record.ComponentDigest != component.ComponentDigest ||
			record.ComponentType != component.Type || record.TrustClass != component.TrustClass || record.Scope != component.Scope {
			return fmt.Errorf("%w: Component %q identity differs from Capsule Lock %q", ErrEnvironmentMaterializationLockMismatch, record.ComponentID, lock.ID)
		}
	}
	return nil
}

func environmentPinFromRow(environmentID string, capsuleLockID *string, policy string) (EnvironmentPin, error) {
	upgradePolicy := domain.UpgradePolicy(policy)
	if !upgradePolicy.Valid() {
		return EnvironmentPin{}, fmt.Errorf("restore Environment pin: invalid upgrade policy %q", policy)
	}
	return EnvironmentPin{EnvironmentID: environmentID, CapsuleLockID: capsuleLockID, UpgradePolicy: upgradePolicy}, nil
}

func environmentMaterializationFromRow(row dbsql.EnvironmentMaterialization) (EnvironmentMaterialization, error) {
	var secretVersionIdentifiers, filePaths []string
	if err := json.Unmarshal(row.SecretVersionIdentifiers, &secretVersionIdentifiers); err != nil {
		return EnvironmentMaterialization{}, fmt.Errorf("restore Environment Materialization %q: decode secret version identifiers: %w", row.ComponentID, err)
	}
	if err := json.Unmarshal(row.FilePaths, &filePaths); err != nil {
		return EnvironmentMaterialization{}, fmt.Errorf("restore Environment Materialization %q: decode file paths: %w", row.ComponentID, err)
	}
	if !row.CreatedAt.Valid || !row.UpdatedAt.Valid {
		return EnvironmentMaterialization{}, fmt.Errorf("restore Environment Materialization %q: database returned invalid timestamps", row.ComponentID)
	}
	return EnvironmentMaterialization{
		EnvironmentID: row.EnvironmentID, ID: environmentMaterializationStringValue(row.ID), LockID: row.LockID, LockDigest: row.LockDigest,
		CapsuleDigest: row.CapsuleDigest, ComponentID: row.ComponentID, ComponentDigest: row.ComponentDigest,
		AdapterID: row.AdapterID, AdapterVersion: row.AdapterVersion, TargetAgentVersion: row.TargetAgentVersion,
		Scope: domain.ComponentScope(row.Scope), ComponentType: domain.ComponentType(row.ComponentType), TrustClass: domain.TrustClass(row.TrustClass),
		NonSecretOverridesDigest: environmentMaterializationStringValue(row.NonSecretOverridesDigest), SecretVersionIdentifiers: secretVersionIdentifiers,
		EffectiveCacheKey: row.EffectiveCacheKey, Mode: environmentMaterializationStringValue(row.Mode), Root: environmentMaterializationStringValue(row.Root), Target: environmentMaterializationStringValue(row.Target),
		Selector: environmentMaterializationStringValue(row.Selector), Directory: row.Directory, FilePaths: filePaths,
		LastAppliedDigest: environmentMaterializationStringValue(row.LastAppliedDigest), ObservedDigest: environmentMaterializationStringValue(row.ObservedDigest),
		CredentialRequirementDigest: environmentMaterializationStringValue(row.CredentialRequirementDigest), CreatedAt: row.CreatedAt.Time, UpdatedAt: row.UpdatedAt.Time,
	}, nil
}

func environmentMaterializationOptionalString(value string) *string {
	if value == "" {
		return nil
	}
	return &value
}

func environmentMaterializationStringValue(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}

func nonNilStrings(value []string) []string {
	if value == nil {
		return []string{}
	}
	return append([]string(nil), value...)
}
