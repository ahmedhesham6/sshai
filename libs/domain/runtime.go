package domain

import (
	"errors"
	"fmt"
	"net/netip"
	"strings"
	"time"
)

var ErrStaleRuntimeObservation = errors.New("stale Runtime observation")

type RuntimeStatus string

const (
	RuntimeAbsent       RuntimeStatus = "absent"
	RuntimeProvisioning RuntimeStatus = "provisioning"
	RuntimeStarting     RuntimeStatus = "starting"
	RuntimeReady        RuntimeStatus = "ready"
	RuntimeStopping     RuntimeStatus = "stopping"
	RuntimeStopped      RuntimeStatus = "stopped"
	RuntimeReplacing    RuntimeStatus = "replacing"
	RuntimeError        RuntimeStatus = "error"
)

type RuntimeReservation struct {
	ID               string
	EnvironmentID    string
	Sequence         int64
	RuntimePreset    string
	Region           string
	AvailabilityZone string
	ImageVersion     string
	CreatedAt        time.Time
}

type RuntimeSnapshot struct {
	ID                  string
	EnvironmentID       string
	Sequence            int64
	Status              RuntimeStatus
	RuntimePreset       string
	Region              string
	AvailabilityZone    string
	ImageVersion        string
	ProviderInstanceRef *string
	PrivateAddress      *string
	BootID              *string
	StartedAt           *time.Time
	StoppedAt           *time.Time
	RetiredAt           *time.Time
	CreatedAt           time.Time
	UpdatedAt           time.Time
	Version             int64
}

type Runtime struct{ snapshot RuntimeSnapshot }

type RuntimeReadinessObservation struct {
	ProviderInstanceRef string
	BootID              string
	PrivateAddress      string
	ExpectedVersion     int64
	ObservedAt          time.Time
}

type RuntimeStateObservation struct {
	ProviderInstanceRef string
	ExpectedVersion     int64
	ObservedAt          time.Time
}

func ReserveRuntime(reservation RuntimeReservation) (Runtime, error) {
	snapshot := RuntimeSnapshot{
		ID: reservation.ID, EnvironmentID: reservation.EnvironmentID, Sequence: reservation.Sequence,
		Status: RuntimeAbsent, RuntimePreset: reservation.RuntimePreset, Region: reservation.Region,
		AvailabilityZone: reservation.AvailabilityZone, ImageVersion: reservation.ImageVersion,
		CreatedAt: reservation.CreatedAt, UpdatedAt: reservation.CreatedAt, Version: 1,
	}
	if err := validateRuntimeSnapshot(snapshot); err != nil {
		return Runtime{}, fmt.Errorf("reserve Runtime: %w", err)
	}
	return Runtime{snapshot: snapshot}, nil
}

func RestoreRuntime(snapshot RuntimeSnapshot) (Runtime, error) {
	if err := validateRuntimeSnapshot(snapshot); err != nil {
		return Runtime{}, fmt.Errorf("restore Runtime: %w", err)
	}
	return Runtime{snapshot: cloneRuntimeSnapshot(snapshot)}, nil
}

func validateRuntimeSnapshot(snapshot RuntimeSnapshot) error {
	required := []struct {
		name  string
		value string
	}{
		{name: "ID", value: snapshot.ID},
		{name: "Environment ID", value: snapshot.EnvironmentID},
		{name: "Runtime Preset", value: snapshot.RuntimePreset},
		{name: "region", value: snapshot.Region},
		{name: "availability zone", value: snapshot.AvailabilityZone},
		{name: "image version", value: snapshot.ImageVersion},
	}
	for _, field := range required {
		if strings.TrimSpace(field.value) == "" || field.value != strings.TrimSpace(field.value) {
			return fmt.Errorf("invalid Runtime: %s is required", field.name)
		}
	}
	if snapshot.Sequence < 1 || snapshot.Version < 1 {
		return errors.New("invalid Runtime: sequence and version must be positive")
	}
	if !snapshot.Status.valid() {
		return fmt.Errorf("invalid Runtime: unknown status %q", snapshot.Status)
	}
	if snapshot.CreatedAt.IsZero() {
		return errors.New("invalid Runtime: creation time is required")
	}
	if snapshot.UpdatedAt.IsZero() || snapshot.UpdatedAt.Before(snapshot.CreatedAt) {
		return errors.New("invalid Runtime: update time is invalid")
	}
	for _, observed := range []struct {
		name  string
		value *time.Time
	}{
		{name: "started time", value: snapshot.StartedAt},
		{name: "stopped time", value: snapshot.StoppedAt},
		{name: "retired time", value: snapshot.RetiredAt},
	} {
		if observed.value != nil && (observed.value.Before(snapshot.CreatedAt) || observed.value.After(snapshot.UpdatedAt)) {
			return fmt.Errorf("invalid Runtime: %s is outside the Runtime lifetime", observed.name)
		}
	}
	if err := validateRuntimeState(snapshot); err != nil {
		return err
	}
	return nil
}

func validateRuntimeState(snapshot RuntimeSnapshot) error {
	provider := present(snapshot.ProviderInstanceRef)
	privateAddress := present(snapshot.PrivateAddress)
	boot := present(snapshot.BootID)
	if snapshot.ProviderInstanceRef != nil && (!provider || *snapshot.ProviderInstanceRef != strings.TrimSpace(*snapshot.ProviderInstanceRef)) {
		return errors.New("invalid Runtime: provider instance reference cannot be empty")
	}
	if snapshot.PrivateAddress != nil {
		address, err := netip.ParseAddr(*snapshot.PrivateAddress)
		if err != nil || !address.Is4() || !address.IsPrivate() {
			return errors.New("invalid Runtime: private IPv4 address is required")
		}
	}
	if snapshot.BootID != nil && (!boot || *snapshot.BootID != strings.TrimSpace(*snapshot.BootID)) {
		return errors.New("invalid Runtime: boot ID cannot be empty")
	}
	if snapshot.Status == RuntimeAbsent {
		if snapshot.RetiredAt == nil && (provider || privateAddress || boot || snapshot.StartedAt != nil || snapshot.StoppedAt != nil) {
			return errors.New("invalid Runtime: unretired absent Runtime has lifecycle observations")
		}
		if snapshot.RetiredAt != nil && (!provider || privateAddress || boot) {
			return errors.New("invalid Runtime: retired Runtime requires a provider instance and no writable route")
		}
		return nil
	}
	if snapshot.RetiredAt != nil {
		return errors.New("invalid Runtime: only an absent Runtime may be retired")
	}
	if !provider {
		return errors.New("invalid Runtime: provider instance reference is required")
	}
	switch snapshot.Status {
	case RuntimeProvisioning:
		if snapshot.StartedAt != nil || snapshot.StoppedAt != nil || privateAddress || boot {
			return errors.New("invalid Runtime: provisioning Runtime has boot observations")
		}
	case RuntimeStarting, RuntimeStopping:
		if snapshot.StartedAt == nil || snapshot.StoppedAt != nil || privateAddress || boot {
			return fmt.Errorf("invalid Runtime: %s Runtime has inconsistent boot observations", snapshot.Status)
		}
	case RuntimeReady:
		if snapshot.StartedAt == nil || snapshot.StoppedAt != nil || !privateAddress || !boot {
			return errors.New("invalid Runtime: ready Runtime requires started time, current boot ID, and private IPv4 address")
		}
	case RuntimeStopped:
		if snapshot.StartedAt == nil || snapshot.StoppedAt == nil || privateAddress || boot {
			return errors.New("invalid Runtime: stopped Runtime has inconsistent stop observations")
		}
	case RuntimeReplacing, RuntimeError:
		if privateAddress || boot {
			return fmt.Errorf("invalid Runtime: %s Runtime cannot expose a writable route", snapshot.Status)
		}
	}
	return nil
}

func present(value *string) bool { return value != nil && strings.TrimSpace(*value) != "" }

func (status RuntimeStatus) valid() bool {
	switch status {
	case RuntimeAbsent, RuntimeProvisioning, RuntimeStarting, RuntimeReady, RuntimeStopping, RuntimeStopped, RuntimeReplacing, RuntimeError:
		return true
	default:
		return false
	}
}

func (runtime Runtime) Snapshot() RuntimeSnapshot { return cloneRuntimeSnapshot(runtime.snapshot) }

func (runtime Runtime) Provision(providerInstanceRef string, at time.Time) (Runtime, error) {
	if providerInstanceRef == "" {
		return Runtime{}, errors.New("provision Runtime: provider instance reference is required")
	}
	next, err := runtime.transition(at)
	if err != nil {
		return Runtime{}, fmt.Errorf("provision Runtime: %w", err)
	}
	if next.Status == RuntimeProvisioning && next.ProviderInstanceRef != nil && *next.ProviderInstanceRef == providerInstanceRef {
		return runtime, nil
	}
	if next.Status != RuntimeAbsent || next.RetiredAt != nil {
		return Runtime{}, fmt.Errorf("provision Runtime: status is %q", next.Status)
	}
	next.Status = RuntimeProvisioning
	next.ProviderInstanceRef = &providerInstanceRef
	return finalizeRuntimeTransition(next, at)
}

func (runtime Runtime) BeginStart(at time.Time) (Runtime, error) {
	next, err := runtime.transition(at)
	if err != nil {
		return Runtime{}, fmt.Errorf("start Runtime: %w", err)
	}
	if next.Status == RuntimeStarting {
		return runtime, nil
	}
	if next.Status != RuntimeProvisioning && next.Status != RuntimeStopped {
		return Runtime{}, fmt.Errorf("start Runtime: status is %q", next.Status)
	}
	next.Status = RuntimeStarting
	next.StartedAt = &at
	next.StoppedAt = nil
	next.PrivateAddress = nil
	next.BootID = nil
	return finalizeRuntimeTransition(next, at)
}

func (runtime Runtime) MarkReady(observation RuntimeReadinessObservation) (Runtime, error) {
	current := runtime.Snapshot()
	if current.Status == RuntimeReady && current.ProviderInstanceRef != nil && current.BootID != nil && current.PrivateAddress != nil &&
		*current.ProviderInstanceRef == observation.ProviderInstanceRef && *current.BootID == observation.BootID && *current.PrivateAddress == observation.PrivateAddress {
		return runtime, nil
	}
	if current.Status != RuntimeStarting {
		return Runtime{}, fmt.Errorf("mark Runtime ready: status is %q", current.Status)
	}
	if err := validateCurrentRuntimeObservation(current, observation.ProviderInstanceRef, observation.ExpectedVersion); err != nil {
		return Runtime{}, err
	}
	next, err := runtime.transition(observation.ObservedAt)
	if err != nil {
		return Runtime{}, fmt.Errorf("mark Runtime ready: %w", err)
	}
	address, parseErr := netip.ParseAddr(observation.PrivateAddress)
	if strings.TrimSpace(observation.BootID) == "" || parseErr != nil || !address.Is4() || !address.IsPrivate() {
		return Runtime{}, errors.New("mark Runtime ready: current boot ID and private IPv4 address are required")
	}
	next.Status = RuntimeReady
	next.BootID = &observation.BootID
	next.PrivateAddress = &observation.PrivateAddress
	return finalizeRuntimeTransition(next, observation.ObservedAt)
}

func (runtime Runtime) BeginStop(at time.Time) (Runtime, error) {
	next, err := runtime.transition(at)
	if err != nil {
		return Runtime{}, fmt.Errorf("stop Runtime: %w", err)
	}
	if next.Status == RuntimeStopping || next.Status == RuntimeStopped {
		return runtime, nil
	}
	if next.Status != RuntimeReady {
		return Runtime{}, fmt.Errorf("stop Runtime: status is %q", next.Status)
	}
	next.Status = RuntimeStopping
	next.PrivateAddress = nil
	next.BootID = nil
	return finalizeRuntimeTransition(next, at)
}

func (runtime Runtime) MarkStopped(observation RuntimeStateObservation) (Runtime, error) {
	current := runtime.Snapshot()
	if current.Status == RuntimeStopped && current.ProviderInstanceRef != nil && *current.ProviderInstanceRef == observation.ProviderInstanceRef {
		return runtime, nil
	}
	if current.Status != RuntimeStopping {
		return Runtime{}, fmt.Errorf("mark Runtime stopped: status is %q", current.Status)
	}
	if err := validateCurrentRuntimeObservation(current, observation.ProviderInstanceRef, observation.ExpectedVersion); err != nil {
		return Runtime{}, err
	}
	next, err := runtime.transition(observation.ObservedAt)
	if err != nil {
		return Runtime{}, fmt.Errorf("mark Runtime stopped: %w", err)
	}
	next.Status = RuntimeStopped
	next.StoppedAt = &observation.ObservedAt
	return finalizeRuntimeTransition(next, observation.ObservedAt)
}

func (runtime Runtime) MarkError(observation RuntimeStateObservation) (Runtime, error) {
	current := runtime.Snapshot()
	if current.Status == RuntimeError && current.ProviderInstanceRef != nil && *current.ProviderInstanceRef == observation.ProviderInstanceRef {
		return runtime, nil
	}
	if current.Status == RuntimeAbsent || current.Status == RuntimeReplacing {
		return Runtime{}, fmt.Errorf("mark Runtime error: status is %q", current.Status)
	}
	if err := validateCurrentRuntimeObservation(current, observation.ProviderInstanceRef, observation.ExpectedVersion); err != nil {
		return Runtime{}, err
	}
	next, err := runtime.transition(observation.ObservedAt)
	if err != nil {
		return Runtime{}, fmt.Errorf("mark Runtime error: %w", err)
	}
	next.Status = RuntimeError
	next.PrivateAddress = nil
	next.BootID = nil
	return finalizeRuntimeTransition(next, observation.ObservedAt)
}

func (runtime Runtime) BeginReplacement(at time.Time) (Runtime, error) {
	next, err := runtime.transition(at)
	if err != nil {
		return Runtime{}, fmt.Errorf("replace Runtime: %w", err)
	}
	if next.Status == RuntimeReplacing {
		return runtime, nil
	}
	if next.Status != RuntimeReady && next.Status != RuntimeStopped && next.Status != RuntimeError {
		return Runtime{}, fmt.Errorf("replace Runtime: status is %q", next.Status)
	}
	next.Status = RuntimeReplacing
	next.PrivateAddress = nil
	next.BootID = nil
	return finalizeRuntimeTransition(next, at)
}

func (runtime Runtime) Retire(observation RuntimeStateObservation) (Runtime, error) {
	current := runtime.Snapshot()
	if current.Status == RuntimeAbsent && current.RetiredAt != nil && current.ProviderInstanceRef != nil && *current.ProviderInstanceRef == observation.ProviderInstanceRef {
		return runtime, nil
	}
	if current.Status != RuntimeReplacing {
		return Runtime{}, fmt.Errorf("retire Runtime: status is %q", current.Status)
	}
	if err := validateCurrentRuntimeObservation(current, observation.ProviderInstanceRef, observation.ExpectedVersion); err != nil {
		return Runtime{}, err
	}
	next, err := runtime.transition(observation.ObservedAt)
	if err != nil {
		return Runtime{}, fmt.Errorf("retire Runtime: %w", err)
	}
	next.Status = RuntimeAbsent
	next.RetiredAt = &observation.ObservedAt
	return finalizeRuntimeTransition(next, observation.ObservedAt)
}

func validateCurrentRuntimeObservation(current RuntimeSnapshot, providerInstanceRef string, expectedVersion int64) error {
	if expectedVersion != current.Version || current.ProviderInstanceRef == nil || *current.ProviderInstanceRef != providerInstanceRef {
		return ErrStaleRuntimeObservation
	}
	return nil
}

func (runtime Runtime) transition(at time.Time) (RuntimeSnapshot, error) {
	next := runtime.Snapshot()
	if err := validateRuntimeSnapshot(next); err != nil {
		return RuntimeSnapshot{}, err
	}
	if at.IsZero() || at.Before(next.UpdatedAt) {
		return RuntimeSnapshot{}, errors.New("transition time precedes current state")
	}
	return next, nil
}

func finalizeRuntimeTransition(next RuntimeSnapshot, at time.Time) (Runtime, error) {
	const maximumVersion = int64(9223372036854775807)
	if next.Version == maximumVersion {
		return Runtime{}, errors.New("Runtime version exhausted")
	}
	next.UpdatedAt = at.Round(0).UTC()
	next.Version++
	if err := validateRuntimeSnapshot(next); err != nil {
		return Runtime{}, fmt.Errorf("finalize Runtime transition: %w", err)
	}
	return Runtime{snapshot: cloneRuntimeSnapshot(next)}, nil
}

func cloneRuntimeSnapshot(snapshot RuntimeSnapshot) RuntimeSnapshot {
	clone := snapshot
	clone.ProviderInstanceRef = cloneString(snapshot.ProviderInstanceRef)
	clone.PrivateAddress = cloneString(snapshot.PrivateAddress)
	clone.BootID = cloneString(snapshot.BootID)
	clone.StartedAt = cloneCanonicalTime(snapshot.StartedAt)
	clone.StoppedAt = cloneCanonicalTime(snapshot.StoppedAt)
	clone.RetiredAt = cloneCanonicalTime(snapshot.RetiredAt)
	return clone
}
