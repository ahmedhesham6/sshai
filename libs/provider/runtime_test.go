package provider_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/ahmedhesham6/sshai/libs/provider"
	"github.com/ahmedhesham6/sshai/libs/provider/providertest"
)

type memoryRuntimeAdapter struct {
	runtime          provider.Runtime
	dataVolumeExists bool
	dataAttached     bool
}

func TestRuntimeProviderConformance(t *testing.T) {
	providertest.RunRuntimeLifecycle(t, func(t *testing.T) providertest.RuntimeHarness {
		adapter := &memoryRuntimeAdapter{dataVolumeExists: true}
		return providertest.RuntimeHarness{
			Adapter: adapter,
			Request: provider.EnsureRuntimeRequest{
				RuntimeSpec: provider.RuntimeSpec{
					RuntimeID: "runtime-1", EnvironmentID: "environment-1", Sequence: 1,
					Region: "us-east-1", AvailabilityZone: "us-east-1a", RuntimePreset: "standard",
					ImageVersion: "ubuntu-2026-07-13", DataVolumeProviderID: "volume-1",
				},
				OperationID: "operation-1",
			},
			AssertDataVolumePreserved: func(t *testing.T) {
				t.Helper()
				if !adapter.dataVolumeExists || adapter.dataAttached {
					t.Fatalf("Data Volume existence/attachment = %t/%t", adapter.dataVolumeExists, adapter.dataAttached)
				}
			},
		}
	})
}

func TestProviderErrorClassifiesTransientFailuresAndPreservesCause(t *testing.T) {
	cause := errors.New("transport")
	for _, code := range []provider.ErrorCode{
		provider.ErrorCodeCapacityUnavailable, provider.ErrorCodeRateLimited, provider.ErrorCodeUnavailable,
	} {
		providerError := provider.NewError(code, "failed", cause)
		if !providerError.Transient() || !errors.Is(providerError, cause) {
			t.Fatalf("provider error %s transient/cause = %t/%t", code, providerError.Transient(), errors.Is(providerError, cause))
		}
	}
	if provider.NewError(provider.ErrorCodeResourceDiverged, "diverged", nil).Transient() {
		t.Fatal("divergence classified as transient")
	}
}

func (adapter *memoryRuntimeAdapter) EnsureRuntime(_ context.Context, request provider.EnsureRuntimeRequest) (provider.Runtime, error) {
	if !validRuntimeSpec(request.RuntimeSpec) || strings.TrimSpace(request.OperationID) == "" {
		return provider.Runtime{}, provider.NewError(provider.ErrorCodeInvalidRequest, "Runtime spec is required", nil)
	}
	if adapter.runtime.ProviderID == "" {
		adapter.runtime = observedRuntime(request.RuntimeSpec, "memory-runtime-1", provider.RuntimeStatePending)
		adapter.dataAttached = true
	} else if adapter.runtime.RuntimeSpec != request.RuntimeSpec {
		return provider.Runtime{}, provider.NewError(provider.ErrorCodeResourceDiverged, "Runtime spec diverged", nil)
	}
	return adapter.runtime, nil
}

func (adapter *memoryRuntimeAdapter) StartRuntime(_ context.Context, request provider.RuntimeLifecycleRequest) (provider.Runtime, error) {
	if err := adapter.validate(request); err != nil {
		return provider.Runtime{}, err
	}
	adapter.runtime.State = provider.RuntimeStateRunning
	adapter.runtime.PrivateIPv4 = "10.0.0.4"
	return adapter.runtime, nil
}

func (adapter *memoryRuntimeAdapter) StopRuntime(_ context.Context, request provider.RuntimeLifecycleRequest) (provider.Runtime, error) {
	if err := adapter.validate(request); err != nil {
		return provider.Runtime{}, err
	}
	adapter.runtime.State = provider.RuntimeStateStopped
	adapter.runtime.PrivateIPv4 = ""
	return adapter.runtime, nil
}

func (adapter *memoryRuntimeAdapter) RetireRuntime(_ context.Context, request provider.RuntimeLifecycleRequest) (provider.Runtime, error) {
	if err := adapter.validate(request); err != nil {
		return provider.Runtime{}, err
	}
	adapter.runtime.State = provider.RuntimeStateTerminated
	adapter.runtime.PrivateIPv4 = ""
	adapter.dataAttached = false
	return adapter.runtime, nil
}

func (adapter *memoryRuntimeAdapter) ObserveRuntime(_ context.Context, request provider.RuntimeLifecycleRequest) (provider.Runtime, error) {
	if err := adapter.validate(request); err != nil {
		return provider.Runtime{}, err
	}
	return adapter.runtime, nil
}

func (adapter *memoryRuntimeAdapter) validate(request provider.RuntimeLifecycleRequest) error {
	if !validRuntimeSpec(request.RuntimeSpec) || strings.TrimSpace(request.ProviderID) == "" {
		return provider.NewError(provider.ErrorCodeInvalidRequest, "Runtime identity is required", nil)
	}
	if adapter.runtime.ProviderID != request.ProviderID || adapter.runtime.RuntimeSpec != request.RuntimeSpec {
		return provider.NewError(provider.ErrorCodeResourceDiverged, "Runtime identity diverged", nil)
	}
	return nil
}

func validRuntimeSpec(spec provider.RuntimeSpec) bool {
	return spec.Sequence > 0 && strings.TrimSpace(spec.RuntimeID) != "" && strings.TrimSpace(spec.EnvironmentID) != "" &&
		strings.TrimSpace(spec.Region) != "" && strings.TrimSpace(spec.AvailabilityZone) != "" &&
		strings.TrimSpace(spec.RuntimePreset) != "" && strings.TrimSpace(spec.ImageVersion) != "" &&
		strings.TrimSpace(spec.DataVolumeProviderID) != ""
}

func observedRuntime(spec provider.RuntimeSpec, providerID string, state provider.RuntimeState) provider.Runtime {
	return provider.Runtime{
		RuntimeSpec: spec, ProviderID: providerID, State: state,
	}
}
