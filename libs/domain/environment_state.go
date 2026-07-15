package domain

import (
	"encoding/json"
	"errors"
	"strings"
	"time"
)

type StateComponentKind string

const (
	StateWorkspace StateComponentKind = "workspace"
	StateHome      StateComponentKind = "home"
	StateServices  StateComponentKind = "services"
	StateCache     StateComponentKind = "cache"
)

type DurabilityClass string

const (
	DurabilityDurable    DurabilityClass = "durable"
	DurabilityDisposable DurabilityClass = "disposable"
)

type StateComponentSnapshot struct {
	ID                string
	EnvironmentID     string
	Kind              StateComponentKind
	Durability        DurabilityClass
	MountPath         string
	BackendResourceID string
	Health            EnvironmentHealth
	ObservedDigest    *string
	CreatedAt         time.Time
	UpdatedAt         time.Time
}

type DataVolumeResourceSnapshot struct {
	ID            string
	EnvironmentID string
	OperationID   string
	Provider      string
	Region        string
	ProviderID    string
	Metadata      json.RawMessage
	CreatedAt     time.Time
	DeletedAt     *time.Time
}

type EnvironmentStateReservation struct {
	WorkspaceID       string
	HomeID            string
	ServicesID        string
	CacheID           string
	BackendResourceID string
	Provider          string
	ProviderID        string
	Metadata          json.RawMessage
	CreatedAt         time.Time
}

type EnvironmentState struct {
	environment Environment
	components  []StateComponentSnapshot
	backend     DataVolumeResourceSnapshot
}

func ReserveEnvironmentState(environment Environment, operation Operation, reservation EnvironmentStateReservation) (EnvironmentState, error) {
	environmentSnapshot := environment.Snapshot()
	operationSnapshot := operation.Snapshot()
	if environmentSnapshot.Lifecycle != EnvironmentCreating {
		return EnvironmentState{}, errors.New("reserve Environment State: Environment is not creating")
	}
	if operationSnapshot.EnvironmentID != environmentSnapshot.ID || operationSnapshot.RequestedByUserID != environmentSnapshot.OwnerUserID || operationSnapshot.Type != OperationEnvironmentCreate {
		return EnvironmentState{}, errors.New("reserve Environment State: creation Operation does not belong to Environment owner")
	}
	if operationSnapshot.Status != OperationQueued && operationSnapshot.Status != OperationRunning {
		return EnvironmentState{}, errors.New("reserve Environment State: creation Operation is terminal")
	}
	seen := map[string]struct{}{reservation.BackendResourceID: {}}
	for _, policy := range stateComponentPolicies {
		value := policy.reservationID(reservation)
		if value == "" || value != strings.TrimSpace(value) {
			return EnvironmentState{}, errors.New("reserve Environment State: canonical identities are required")
		}
		if _, duplicate := seen[value]; duplicate {
			return EnvironmentState{}, errors.New("reserve Environment State: identities must be unique")
		}
		seen[value] = struct{}{}
	}
	if reservation.BackendResourceID == "" || reservation.BackendResourceID != strings.TrimSpace(reservation.BackendResourceID) {
		return EnvironmentState{}, errors.New("reserve Environment State: canonical backend identity is required")
	}
	for _, value := range []string{reservation.Provider, reservation.ProviderID} {
		if value == "" || value != strings.TrimSpace(value) {
			return EnvironmentState{}, errors.New("reserve Environment State: canonical provider ownership is required")
		}
	}
	if reservation.CreatedAt.IsZero() || reservation.CreatedAt.Before(environmentSnapshot.UpdatedAt) || !json.Valid(reservation.Metadata) {
		return EnvironmentState{}, errors.New("reserve Environment State: creation time and metadata are invalid")
	}
	createdAt := reservation.CreatedAt.Round(0).UTC()
	backend := DataVolumeResourceSnapshot{
		ID: reservation.BackendResourceID, EnvironmentID: environmentSnapshot.ID,
		OperationID: operationSnapshot.ID, Provider: reservation.Provider,
		Region:     environmentSnapshot.Region,
		ProviderID: reservation.ProviderID, Metadata: append(json.RawMessage(nil), reservation.Metadata...),
		CreatedAt: createdAt,
	}
	components := make([]StateComponentSnapshot, 0, len(stateComponentPolicies))
	for _, policy := range stateComponentPolicies {
		components = append(components, newStateComponent(policy.reservationID(reservation), environmentSnapshot.ID, policy, backend.ID, createdAt))
	}
	return restoreEnvironmentState(environment, operation, components, backend)
}

func RestoreEnvironmentState(environment Environment, operation Operation, components []StateComponentSnapshot, backend DataVolumeResourceSnapshot) (EnvironmentState, error) {
	return restoreEnvironmentState(environment, operation, components, backend)
}

func restoreEnvironmentState(environment Environment, operation Operation, components []StateComponentSnapshot, backend DataVolumeResourceSnapshot) (EnvironmentState, error) {
	environmentSnapshot := environment.Snapshot()
	operationSnapshot := operation.Snapshot()
	if environmentSnapshot.Lifecycle == EnvironmentDeleted {
		return EnvironmentState{}, errors.New("restore Environment State: deleted Environment has no current State")
	}
	if operationSnapshot.EnvironmentID != environmentSnapshot.ID || operationSnapshot.RequestedByUserID != environmentSnapshot.OwnerUserID || operationSnapshot.Type != OperationEnvironmentCreate {
		return EnvironmentState{}, errors.New("restore Environment State: creation Operation does not belong to Environment owner")
	}
	var metadata map[string]json.RawMessage
	if backend.ID == "" || backend.ID != strings.TrimSpace(backend.ID) ||
		backend.EnvironmentID != environmentSnapshot.ID || backend.Region != environmentSnapshot.Region ||
		backend.Provider == "" || backend.Provider != strings.TrimSpace(backend.Provider) ||
		backend.ProviderID == "" || backend.ProviderID != strings.TrimSpace(backend.ProviderID) ||
		backend.OperationID != operationSnapshot.ID ||
		backend.CreatedAt.IsZero() || backend.CreatedAt.Before(environmentSnapshot.CreatedAt) || backend.DeletedAt != nil ||
		json.Unmarshal(backend.Metadata, &metadata) != nil || metadata == nil {
		return EnvironmentState{}, errors.New("restore Environment State: data-volume Provider Resource is invalid")
	}
	byKind := make(map[StateComponentKind]StateComponentSnapshot, len(components))
	identities := map[string]struct{}{backend.ID: {}}
	for _, component := range components {
		policy, exists := stateComponentPolicyFor(component.Kind)
		if !exists || component.ID == "" || component.ID != strings.TrimSpace(component.ID) ||
			component.EnvironmentID != environmentSnapshot.ID || component.BackendResourceID != backend.ID ||
			component.Durability != policy.durability || component.MountPath != policy.mountPath ||
			!component.Health.valid() || component.CreatedAt.IsZero() || component.UpdatedAt.Before(component.CreatedAt) ||
			component.CreatedAt.Before(environmentSnapshot.CreatedAt) ||
			(component.ObservedDigest != nil && !contentDigestPattern.MatchString(*component.ObservedDigest)) {
			return EnvironmentState{}, errors.New("restore Environment State: State Component is invalid")
		}
		if _, duplicate := identities[component.ID]; duplicate {
			return EnvironmentState{}, errors.New("restore Environment State: State Component identity is duplicated")
		}
		identities[component.ID] = struct{}{}
		if _, duplicate := byKind[component.Kind]; duplicate {
			return EnvironmentState{}, errors.New("restore Environment State: State Component kind is duplicated")
		}
		component.ObservedDigest = cloneString(component.ObservedDigest)
		byKind[component.Kind] = component
	}
	ordered := make([]StateComponentSnapshot, 0, len(stateComponentPolicies))
	for _, policy := range stateComponentPolicies {
		component, exists := byKind[policy.kind]
		if !exists {
			return EnvironmentState{}, errors.New("restore Environment State: exact State Component set is required")
		}
		ordered = append(ordered, component)
	}
	backend.Metadata = append(json.RawMessage(nil), backend.Metadata...)
	return EnvironmentState{environment: environment, components: ordered, backend: backend}, nil
}

type stateComponentPolicy struct {
	kind          StateComponentKind
	durability    DurabilityClass
	mountPath     string
	reservationID func(EnvironmentStateReservation) string
}

var stateComponentPolicies = []stateComponentPolicy{
	{kind: StateWorkspace, durability: DurabilityDurable, mountPath: "/workspace", reservationID: func(input EnvironmentStateReservation) string { return input.WorkspaceID }},
	{kind: StateHome, durability: DurabilityDurable, mountPath: "/home/dev", reservationID: func(input EnvironmentStateReservation) string { return input.HomeID }},
	{kind: StateServices, durability: DurabilityDurable, mountPath: "/var/lib/docker", reservationID: func(input EnvironmentStateReservation) string { return input.ServicesID }},
	{kind: StateCache, durability: DurabilityDisposable, mountPath: "/var/cache/devm", reservationID: func(input EnvironmentStateReservation) string { return input.CacheID }},
}

func stateComponentPolicyFor(kind StateComponentKind) (stateComponentPolicy, bool) {
	for _, policy := range stateComponentPolicies {
		if policy.kind == kind {
			return policy, true
		}
	}
	return stateComponentPolicy{}, false
}

func newStateComponent(id, environmentID string, policy stateComponentPolicy, backendID string, createdAt time.Time) StateComponentSnapshot {
	return StateComponentSnapshot{
		ID: id, EnvironmentID: environmentID, Kind: policy.kind, Durability: policy.durability,
		MountPath: policy.mountPath, BackendResourceID: backendID, Health: EnvironmentHealthUnknown,
		CreatedAt: createdAt, UpdatedAt: createdAt,
	}
}

func (state EnvironmentState) Environment() Environment { return state.environment }

func (state EnvironmentState) Components() []StateComponentSnapshot {
	components := make([]StateComponentSnapshot, len(state.components))
	copy(components, state.components)
	for index := range components {
		components[index].ObservedDigest = cloneString(components[index].ObservedDigest)
	}
	return components
}

func (state EnvironmentState) Backend() DataVolumeResourceSnapshot {
	backend := state.backend
	backend.Metadata = append(json.RawMessage(nil), backend.Metadata...)
	backend.DeletedAt = cloneCanonicalTime(backend.DeletedAt)
	return backend
}

func (state EnvironmentState) DataVolumeProviderID() string { return state.backend.ProviderID }
