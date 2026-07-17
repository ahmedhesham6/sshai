package domain

import (
	"errors"
	"fmt"
	"strings"
	"time"
)

type EnvironmentLifecycle string

const (
	EnvironmentCreating EnvironmentLifecycle = "creating"
	EnvironmentActive   EnvironmentLifecycle = "active"
	EnvironmentDeleting EnvironmentLifecycle = "deleting"
	EnvironmentDeleted  EnvironmentLifecycle = "deleted"
)

type EnvironmentHealth string

const (
	EnvironmentHealthHealthy  EnvironmentHealth = "healthy"
	EnvironmentHealthDegraded EnvironmentHealth = "degraded"
	EnvironmentHealthBlocked  EnvironmentHealth = "blocked"
	EnvironmentHealthUnknown  EnvironmentHealth = "unknown"
)

type EnvironmentReservation struct {
	ID                     string
	OwnerUserID            string
	Name                   string
	Slug                   string
	Region                 string
	AvailabilityZone       string
	RuntimePreset          string
	PinnedProfileVersionID string
	AutoStopPolicyID       string
	CreatedAt              time.Time
}

type EnvironmentSnapshot struct {
	ID                     string
	OwnerUserID            string
	Name                   string
	Slug                   string
	Lifecycle              EnvironmentLifecycle
	Health                 EnvironmentHealth
	Region                 string
	AvailabilityZone       string
	RuntimePreset          string
	PinnedProfileVersionID string
	CapsuleLockID          *string
	UpgradePolicy          UpgradePolicy
	CurrentRuntimeID       *string
	AutoStopPolicyID       string
	CreatedAt              time.Time
	UpdatedAt              time.Time
	DeletedAt              *time.Time
	Version                int64
}

type Environment struct {
	snapshot EnvironmentSnapshot
}

type EnvironmentCreation struct {
	environment   Environment
	policy        AutoStopPolicy
	operation     Operation
	projectSeedID string
	sshKeyIDs     []string
}

var (
	ErrInitialRuntimeAlreadyReserved = errors.New("initial Runtime is already reserved")
	ErrInitialRuntimeRequired        = errors.New("initial Runtime is required before activation")
	ErrCapsuleLockAlreadyPinned      = errors.New("Environment already has a pinned Capsule Lock")
)

type EnvironmentCreateDispatch struct {
	OperationID      string
	EnvironmentID    string
	Region           string
	AvailabilityZone string
	RuntimePreset    string
}

func NewEnvironmentCreation(environment Environment, policy AutoStopPolicy, operation Operation, projectSeedID string, sshKeyIDs []string) (EnvironmentCreation, error) {
	environmentSnapshot, policySnapshot, operationSnapshot := environment.Snapshot(), policy.Snapshot(), operation.Snapshot()
	if environmentSnapshot.ID == "" || policySnapshot.ID == "" || operationSnapshot.ID == "" {
		return EnvironmentCreation{}, errors.New("create Environment reservation: Environment, Policy, and Operation are required")
	}
	if environmentSnapshot.AutoStopPolicyID != policySnapshot.ID || policySnapshot.EnvironmentID != environmentSnapshot.ID {
		return EnvironmentCreation{}, errors.New("create Environment reservation: Auto-stop Policy does not belong to Environment")
	}
	if operationSnapshot.EnvironmentID != environmentSnapshot.ID || operationSnapshot.RequestedByUserID != environmentSnapshot.OwnerUserID {
		return EnvironmentCreation{}, errors.New("create Environment reservation: Operation does not belong to Environment owner")
	}
	if projectSeedID == "" {
		return EnvironmentCreation{}, errors.New("create Environment reservation: Project Seed is required")
	}
	if len(sshKeyIDs) == 0 {
		return EnvironmentCreation{}, errors.New("create Environment reservation: at least one SSH Key is required")
	}
	seen := make(map[string]struct{}, len(sshKeyIDs))
	for _, keyID := range sshKeyIDs {
		if keyID == "" {
			return EnvironmentCreation{}, errors.New("create Environment reservation: SSH Key ID is required")
		}
		if _, duplicate := seen[keyID]; duplicate {
			return EnvironmentCreation{}, errors.New("create Environment reservation: SSH Key IDs must be unique")
		}
		seen[keyID] = struct{}{}
	}
	return EnvironmentCreation{
		environment: environment, policy: policy, operation: operation,
		projectSeedID: projectSeedID, sshKeyIDs: append([]string(nil), sshKeyIDs...),
	}, nil
}

func (creation EnvironmentCreation) Environment() Environment { return creation.environment }
func (creation EnvironmentCreation) Policy() AutoStopPolicy   { return creation.policy }
func (creation EnvironmentCreation) Operation() Operation     { return creation.operation }
func (creation EnvironmentCreation) ProjectSeedID() string    { return creation.projectSeedID }
func (creation EnvironmentCreation) SSHKeyIDs() []string {
	return append([]string(nil), creation.sshKeyIDs...)
}

func (creation EnvironmentCreation) RecordRestateInvocation(invocationID string) (EnvironmentCreation, error) {
	operation, err := creation.operation.RecordRestateInvocation(invocationID)
	if err != nil {
		return EnvironmentCreation{}, err
	}
	creation.operation = operation
	return creation, nil
}

func (creation EnvironmentCreation) ReserveInitialRuntime(reservation RuntimeReservation) (EnvironmentCreation, Runtime, error) {
	if creation.environment.Snapshot().CurrentRuntimeID != nil {
		return EnvironmentCreation{}, Runtime{}, ErrInitialRuntimeAlreadyReserved
	}
	runtime, err := ReserveRuntime(reservation)
	if err != nil {
		return EnvironmentCreation{}, Runtime{}, err
	}
	environment, err := creation.environment.AttachRuntime(runtime, reservation.CreatedAt)
	if err != nil {
		return EnvironmentCreation{}, Runtime{}, err
	}
	creation.environment = environment
	return creation, runtime, nil
}

func (creation EnvironmentCreation) Complete(at time.Time) (EnvironmentCreation, error) {
	if creation.environment.Snapshot().CurrentRuntimeID == nil {
		return EnvironmentCreation{}, ErrInitialRuntimeRequired
	}
	operation, err := creation.operation.Start(at)
	if err != nil {
		return EnvironmentCreation{}, err
	}
	environment, err := creation.environment.Activate(at)
	if err != nil {
		return EnvironmentCreation{}, err
	}
	environment, err = environment.UpdateHealth(EnvironmentHealthHealthy, at)
	if err != nil {
		return EnvironmentCreation{}, err
	}
	operation, err = operation.Succeed(at)
	if err != nil {
		return EnvironmentCreation{}, err
	}
	creation.environment, creation.operation = environment, operation
	return creation, nil
}

func ReserveEnvironment(reservation EnvironmentReservation) (Environment, error) {
	snapshot := EnvironmentSnapshot{
		ID:                     reservation.ID,
		OwnerUserID:            reservation.OwnerUserID,
		Name:                   reservation.Name,
		Slug:                   reservation.Slug,
		Lifecycle:              EnvironmentCreating,
		Health:                 EnvironmentHealthUnknown,
		Region:                 reservation.Region,
		AvailabilityZone:       reservation.AvailabilityZone,
		RuntimePreset:          reservation.RuntimePreset,
		PinnedProfileVersionID: reservation.PinnedProfileVersionID,
		UpgradePolicy:          UpgradeManual,
		AutoStopPolicyID:       reservation.AutoStopPolicyID,
		CreatedAt:              reservation.CreatedAt,
		UpdatedAt:              reservation.CreatedAt,
		Version:                1,
	}
	if err := validateEnvironmentSnapshot(snapshot); err != nil {
		return Environment{}, fmt.Errorf("reserve Environment: %w", err)
	}
	return Environment{snapshot: snapshot}, nil
}

func RestoreEnvironment(snapshot EnvironmentSnapshot) (Environment, error) {
	if snapshot.UpgradePolicy == "" {
		snapshot.UpgradePolicy = UpgradeManual
	}
	if err := validateEnvironmentSnapshot(snapshot); err != nil {
		return Environment{}, fmt.Errorf("restore Environment: %w", err)
	}

	return Environment{snapshot: cloneEnvironmentSnapshot(snapshot)}, nil
}

func validateEnvironmentSnapshot(snapshot EnvironmentSnapshot) error {
	required := []struct {
		name  string
		value string
	}{
		{name: "ID", value: snapshot.ID},
		{name: "owner User ID", value: snapshot.OwnerUserID},
		{name: "name", value: snapshot.Name},
		{name: "slug", value: snapshot.Slug},
		{name: "region", value: snapshot.Region},
		{name: "availability zone", value: snapshot.AvailabilityZone},
		{name: "Runtime Preset", value: snapshot.RuntimePreset},
		{name: "Profile Version ID", value: snapshot.PinnedProfileVersionID},
		{name: "Auto-stop Policy ID", value: snapshot.AutoStopPolicyID},
	}
	for _, field := range required {
		if strings.TrimSpace(field.value) == "" || field.value != strings.TrimSpace(field.value) {
			return fmt.Errorf("invalid Environment: %s is required", field.name)
		}
	}

	if !snapshot.Lifecycle.valid() {
		return fmt.Errorf("invalid Environment: unknown lifecycle %q", snapshot.Lifecycle)
	}
	if !snapshot.Health.valid() {
		return fmt.Errorf("invalid Environment: unknown health %q", snapshot.Health)
	}
	if !snapshot.UpgradePolicy.Valid() {
		return fmt.Errorf("invalid Environment: unknown upgrade policy %q", snapshot.UpgradePolicy)
	}
	if snapshot.CapsuleLockID != nil && (strings.TrimSpace(*snapshot.CapsuleLockID) == "" || *snapshot.CapsuleLockID != strings.TrimSpace(*snapshot.CapsuleLockID)) {
		return errors.New("invalid Environment: Capsule Lock ID cannot be empty")
	}
	if snapshot.CurrentRuntimeID != nil && (*snapshot.CurrentRuntimeID == "" || *snapshot.CurrentRuntimeID != strings.TrimSpace(*snapshot.CurrentRuntimeID)) {
		return errors.New("invalid Environment: current Runtime ID cannot be empty")
	}
	if snapshot.CreatedAt.IsZero() {
		return errors.New("invalid Environment: creation time is required")
	}
	if snapshot.UpdatedAt.IsZero() {
		return errors.New("invalid Environment: update time is required")
	}
	if snapshot.UpdatedAt.Before(snapshot.CreatedAt) {
		return errors.New("invalid Environment: update time is before creation time")
	}
	if snapshot.Lifecycle == EnvironmentDeleted {
		if snapshot.DeletedAt == nil {
			return errors.New("invalid Environment: deletion time is required for deleted lifecycle")
		}
		if snapshot.CurrentRuntimeID != nil {
			return errors.New("invalid Environment: deleted Environment cannot have a current Runtime")
		}
		if snapshot.DeletedAt.Before(snapshot.CreatedAt) {
			return errors.New("invalid Environment: deletion time is before creation time")
		}
		if !snapshot.DeletedAt.Equal(snapshot.UpdatedAt) {
			return errors.New("invalid Environment: deletion time must equal update time")
		}
	} else if snapshot.DeletedAt != nil {
		return errors.New("invalid Environment: deletion time requires deleted lifecycle")
	}
	if snapshot.Version < 1 {
		return errors.New("invalid Environment: version must be positive")
	}
	return nil
}

func (lifecycle EnvironmentLifecycle) valid() bool {
	switch lifecycle {
	case EnvironmentCreating, EnvironmentActive, EnvironmentDeleting, EnvironmentDeleted:
		return true
	default:
		return false
	}
}

func (health EnvironmentHealth) valid() bool {
	switch health {
	case EnvironmentHealthHealthy, EnvironmentHealthDegraded, EnvironmentHealthBlocked, EnvironmentHealthUnknown:
		return true
	default:
		return false
	}
}

func (environment Environment) Snapshot() EnvironmentSnapshot {
	return cloneEnvironmentSnapshot(environment.snapshot)
}

func (environment Environment) Activate(at time.Time) (Environment, error) {
	next, err := environment.transitionSnapshot(at)
	if err != nil {
		return Environment{}, fmt.Errorf("activate Environment: %w", err)
	}
	if next.Lifecycle != EnvironmentCreating {
		return Environment{}, fmt.Errorf("activate Environment: lifecycle is %q, want %q", next.Lifecycle, EnvironmentCreating)
	}

	next.Lifecycle = EnvironmentActive
	return finalizeEnvironmentTransition(next, at)
}

func (environment Environment) BeginDeletion(at time.Time) (Environment, error) {
	next, err := environment.transitionSnapshot(at)
	if err != nil {
		return Environment{}, fmt.Errorf("begin Environment deletion: %w", err)
	}
	if next.Lifecycle != EnvironmentActive {
		return Environment{}, fmt.Errorf("begin Environment deletion: lifecycle is %q, want %q", next.Lifecycle, EnvironmentActive)
	}

	next.Lifecycle = EnvironmentDeleting
	return finalizeEnvironmentTransition(next, at)
}

func (environment Environment) CompleteDeletion(at time.Time) (Environment, error) {
	next, err := environment.transitionSnapshot(at)
	if err != nil {
		return Environment{}, fmt.Errorf("complete Environment deletion: %w", err)
	}
	if next.Lifecycle != EnvironmentDeleting {
		return Environment{}, fmt.Errorf("complete Environment deletion: lifecycle is %q, want %q", next.Lifecycle, EnvironmentDeleting)
	}
	if next.CurrentRuntimeID != nil {
		return Environment{}, errors.New("complete Environment deletion: current Runtime must be detached")
	}

	next.Lifecycle = EnvironmentDeleted
	next.DeletedAt = &at
	return finalizeEnvironmentTransition(next, at)
}

func (environment Environment) UpdateHealth(health EnvironmentHealth, at time.Time) (Environment, error) {
	if !health.valid() {
		return Environment{}, fmt.Errorf("update Environment health: unknown health %q", health)
	}
	next, err := environment.transitionSnapshot(at)
	if err != nil {
		return Environment{}, fmt.Errorf("update Environment health: %w", err)
	}
	if next.Lifecycle == EnvironmentDeleted {
		return Environment{}, errors.New("update Environment health: deleted Environment is immutable")
	}
	if health == next.Health {
		return environment, nil
	}

	next.Health = health
	return finalizeEnvironmentTransition(next, at)
}

func (environment Environment) PinCapsuleLock(lock CapsuleLock, policy UpgradePolicy, at time.Time) (Environment, error) {
	if environment.snapshot.CapsuleLockID != nil {
		return Environment{}, ErrCapsuleLockAlreadyPinned
	}
	return environment.setCapsuleLock(lock, policy, at, false)
}

func (environment Environment) RepinCapsuleLock(lock CapsuleLock, policy UpgradePolicy, at time.Time) (Environment, error) {
	return environment.setCapsuleLock(lock, policy, at, true)
}

func (environment Environment) setCapsuleLock(lock CapsuleLock, policy UpgradePolicy, at time.Time, repin bool) (Environment, error) {
	lockSnapshot := lock.Snapshot()
	if strings.TrimSpace(lockSnapshot.ID) == "" {
		return Environment{}, errors.New("set Capsule Lock: Capsule Lock ID is required")
	}
	if lockSnapshot.EnvironmentID != environment.snapshot.ID {
		return Environment{}, errors.New("set Capsule Lock: Capsule Lock does not belong to Environment")
	}
	if !repin && environment.snapshot.CapsuleLockID != nil {
		return Environment{}, ErrCapsuleLockAlreadyPinned
	}
	if policy == "" {
		policy = UpgradeManual
	}
	if !policy.Valid() {
		return Environment{}, fmt.Errorf("set Capsule Lock: unknown upgrade policy %q", policy)
	}
	next, err := environment.transitionSnapshot(at)
	if err != nil {
		return Environment{}, fmt.Errorf("set Capsule Lock: %w", err)
	}
	if next.Lifecycle == EnvironmentDeleted {
		return Environment{}, errors.New("set Capsule Lock: deleted Environment is immutable")
	}
	next.CapsuleLockID = &lockSnapshot.ID
	next.UpgradePolicy = policy
	return finalizeEnvironmentTransition(next, at)
}

func (environment Environment) AttachRuntime(runtime Runtime, at time.Time) (Environment, error) {
	runtimeSnapshot := runtime.Snapshot()
	if err := validateInitialRuntime(environment.snapshot, runtimeSnapshot); err != nil {
		return Environment{}, fmt.Errorf("attach Runtime: %w", err)
	}
	next, err := environment.transitionSnapshot(at)
	if err != nil {
		return Environment{}, fmt.Errorf("attach Runtime: %w", err)
	}
	if next.Lifecycle != EnvironmentCreating && next.Lifecycle != EnvironmentActive {
		return Environment{}, fmt.Errorf("attach Runtime: Environment lifecycle %q does not accept a Runtime", next.Lifecycle)
	}
	if next.CurrentRuntimeID != nil {
		if *next.CurrentRuntimeID == runtimeSnapshot.ID {
			return environment, nil
		}
		return Environment{}, fmt.Errorf("attach Runtime: Environment already has current Runtime %q", *next.CurrentRuntimeID)
	}

	next.CurrentRuntimeID = &runtimeSnapshot.ID
	return finalizeEnvironmentTransition(next, at)
}

func validateInitialRuntime(environment EnvironmentSnapshot, runtime RuntimeSnapshot) error {
	if err := validateRuntimeSnapshot(runtime); err != nil {
		return fmt.Errorf("invalid Runtime: %w", err)
	}
	if err := validateRuntimeOwnership(environment, runtime); err != nil {
		return err
	}
	if runtime.Sequence != 1 || runtime.Status != RuntimeAbsent || runtime.RetiredAt != nil || runtime.ProviderInstanceRef != nil {
		return errors.New("initial Runtime must be a fresh sequence-1 reservation")
	}
	return nil
}

func (environment Environment) DetachRuntime(runtime Runtime, at time.Time) (Environment, error) {
	runtimeSnapshot := runtime.Snapshot()
	if err := validateRetiredRuntime(environment.snapshot, runtimeSnapshot); err != nil {
		return Environment{}, fmt.Errorf("detach Runtime: %w", err)
	}
	next, err := environment.transitionSnapshot(at)
	if err != nil {
		return Environment{}, fmt.Errorf("detach Runtime: %w", err)
	}
	if next.Lifecycle == EnvironmentDeleted {
		return Environment{}, errors.New("detach Runtime: deleted Environment is immutable")
	}
	if next.CurrentRuntimeID == nil {
		return environment, nil
	}
	if *next.CurrentRuntimeID != runtimeSnapshot.ID {
		return Environment{}, fmt.Errorf("detach Runtime: current Runtime is %q, not %q", *next.CurrentRuntimeID, runtimeSnapshot.ID)
	}

	next.CurrentRuntimeID = nil
	return finalizeEnvironmentTransition(next, at)
}

func validateRetiredRuntime(environment EnvironmentSnapshot, runtime RuntimeSnapshot) error {
	if err := validateRuntimeSnapshot(runtime); err != nil {
		return fmt.Errorf("invalid Runtime: %w", err)
	}
	if err := validateRuntimeOwnership(environment, runtime); err != nil {
		return err
	}
	if runtime.Status != RuntimeAbsent || runtime.RetiredAt == nil {
		return errors.New("Runtime must be retired before detachment")
	}
	return nil
}

func (environment Environment) ReplaceRuntime(retired Runtime, replacement Runtime, at time.Time) (Environment, error) {
	current, nextRuntime := retired.Snapshot(), replacement.Snapshot()
	if err := validateRuntimeReplacement(environment.snapshot, current, nextRuntime); err != nil {
		return Environment{}, fmt.Errorf("replace current Runtime: %w", err)
	}
	next, err := environment.transitionSnapshot(at)
	if err != nil {
		return Environment{}, fmt.Errorf("replace current Runtime: %w", err)
	}
	if next.Lifecycle != EnvironmentCreating && next.Lifecycle != EnvironmentActive {
		return Environment{}, fmt.Errorf("replace current Runtime: Environment lifecycle %q does not accept a Runtime", next.Lifecycle)
	}
	if next.CurrentRuntimeID != nil && *next.CurrentRuntimeID == nextRuntime.ID {
		return environment, nil
	}
	if next.CurrentRuntimeID == nil || *next.CurrentRuntimeID != current.ID {
		return Environment{}, errors.New("replace current Runtime: retired Runtime is not current")
	}
	next.CurrentRuntimeID = &nextRuntime.ID
	return finalizeEnvironmentTransition(next, at)
}

func validateRuntimeReplacement(environment EnvironmentSnapshot, current, replacement RuntimeSnapshot) error {
	if err := validateRuntimeSnapshot(current); err != nil {
		return fmt.Errorf("invalid retired Runtime: %w", err)
	}
	if err := validateRuntimeSnapshot(replacement); err != nil {
		return fmt.Errorf("invalid replacement Runtime: %w", err)
	}
	if err := validateRuntimeOwnership(environment, current); err != nil {
		return fmt.Errorf("retired Runtime ownership: %w", err)
	}
	if err := validateRuntimeOwnership(environment, replacement); err != nil {
		return fmt.Errorf("replacement Runtime ownership: %w", err)
	}
	if current.Status != RuntimeAbsent || current.RetiredAt == nil {
		return errors.New("current Runtime must be retired before replacement")
	}
	if replacement.Status != RuntimeAbsent || replacement.RetiredAt != nil || replacement.ProviderInstanceRef != nil {
		return errors.New("replacement Runtime must be a fresh reservation")
	}
	if replacement.Sequence != current.Sequence+1 {
		return errors.New("replacement Runtime sequence must immediately follow current Runtime")
	}
	return nil
}

func validateRuntimeOwnership(environment EnvironmentSnapshot, runtime RuntimeSnapshot) error {
	if runtime.EnvironmentID != environment.ID {
		return errors.New("Runtime does not belong to Environment")
	}
	if runtime.Region != environment.Region || runtime.AvailabilityZone != environment.AvailabilityZone || runtime.RuntimePreset != environment.RuntimePreset {
		return errors.New("Runtime placement or preset differs from Environment ownership")
	}
	return nil
}

func (environment Environment) transitionSnapshot(at time.Time) (EnvironmentSnapshot, error) {
	next := environment.Snapshot()
	if err := validateEnvironmentSnapshot(next); err != nil {
		return EnvironmentSnapshot{}, err
	}
	if at.IsZero() || at.Before(next.UpdatedAt) {
		return EnvironmentSnapshot{}, errors.New("transition time precedes current state")
	}
	return next, nil
}

func finalizeEnvironmentTransition(next EnvironmentSnapshot, at time.Time) (Environment, error) {
	const maximumVersion = int64(9223372036854775807)
	if next.Version == maximumVersion {
		return Environment{}, errors.New("Environment version exhausted")
	}
	next.UpdatedAt = at
	next.Version++
	if err := validateEnvironmentSnapshot(next); err != nil {
		return Environment{}, fmt.Errorf("finalize Environment transition: %w", err)
	}
	return Environment{snapshot: cloneEnvironmentSnapshot(next)}, nil
}

func cloneEnvironmentSnapshot(snapshot EnvironmentSnapshot) EnvironmentSnapshot {
	clone := snapshot
	if snapshot.CapsuleLockID != nil {
		value := *snapshot.CapsuleLockID
		clone.CapsuleLockID = &value
	}
	if snapshot.CurrentRuntimeID != nil {
		value := *snapshot.CurrentRuntimeID
		clone.CurrentRuntimeID = &value
	}
	if snapshot.DeletedAt != nil {
		value := *snapshot.DeletedAt
		clone.DeletedAt = &value
	}
	return clone
}
