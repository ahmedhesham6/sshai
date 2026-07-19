package provider

import (
	"context"
	"fmt"
)

type ErrorCode string

const (
	ErrorCodeInvalidRequest      ErrorCode = "INVALID_REQUEST"
	ErrorCodePlacementConflict   ErrorCode = "STATE_ATTACHMENT_CONFLICT"
	ErrorCodeResourceDiverged    ErrorCode = "RESOURCE_DIVERGED"
	ErrorCodeAuthorizationFailed ErrorCode = "PROVIDER_AUTH_FAILED"
	ErrorCodeCapacityUnavailable ErrorCode = "CAPACITY_UNAVAILABLE"
	ErrorCodeRateLimited         ErrorCode = "PROVIDER_RATE_LIMITED"
	ErrorCodeUnavailable         ErrorCode = "PROVIDER_UNAVAILABLE"
)

type Error struct {
	Code    ErrorCode
	message string
	cause   error
}

func NewError(code ErrorCode, message string, cause error) *Error {
	return &Error{Code: code, message: message, cause: cause}
}

func (providerError *Error) Error() string {
	return fmt.Sprintf("provider %s: %s", providerError.Code, providerError.message)
}

func (providerError *Error) Unwrap() error { return providerError.cause }

func (providerError *Error) Transient() bool {
	switch providerError.Code {
	case ErrorCodeCapacityUnavailable, ErrorCodeRateLimited, ErrorCodeUnavailable:
		return true
	default:
		return false
	}
}

type EnsureDataVolumeRequest struct {
	EnvironmentID    string
	OperationID      string
	Region           string
	AvailabilityZone string
}

type DataVolume struct {
	Provider         string
	ProviderID       string
	EnvironmentID    string
	Region           string
	AvailabilityZone string
}

type DataVolumeProvider interface {
	EnsureDataVolume(context.Context, EnsureDataVolumeRequest) (DataVolume, error)
}

type RuntimeState string

const (
	RuntimeStatePending    RuntimeState = "pending"
	RuntimeStateRunning    RuntimeState = "running"
	RuntimeStateStopping   RuntimeState = "stopping"
	RuntimeStateStopped    RuntimeState = "stopped"
	RuntimeStateTerminated RuntimeState = "terminated"
)

type RuntimeSpec struct {
	RuntimeID            string
	EnvironmentID        string
	Sequence             int64
	Region               string
	AvailabilityZone     string
	RuntimePreset        string
	ImageVersion         string
	DataVolumeProviderID string
}

type EnsureRuntimeRequest struct {
	RuntimeSpec
	OperationID string
}

type RuntimeLifecycleRequest struct {
	RuntimeSpec
	ProviderID string
}

type Runtime struct {
	RuntimeSpec
	ProviderID  string
	PrivateIPv4 string
	State       RuntimeState
}

type RuntimeProvider interface {
	EnsureRuntime(context.Context, EnsureRuntimeRequest) (Runtime, error)
	EnsureRuntimeDataVolumeAttachment(context.Context, RuntimeLifecycleRequest) (Runtime, error)
	StartRuntime(context.Context, RuntimeLifecycleRequest) (Runtime, error)
	StopRuntime(context.Context, RuntimeLifecycleRequest) (Runtime, error)
	RetireRuntime(context.Context, RuntimeLifecycleRequest) (Runtime, error)
	ObserveRuntime(context.Context, RuntimeLifecycleRequest) (Runtime, error)
}
