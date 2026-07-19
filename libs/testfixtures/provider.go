package testfixtures

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/ahmedhesham6/sshai/libs/provider"
)

type Provider struct {
	mu          sync.Mutex
	dataVolumes map[string]provider.DataVolume
	runtimes    map[string]provider.Runtime
	creates     int
}

func NewProvider() *Provider {
	return &Provider{dataVolumes: make(map[string]provider.DataVolume), runtimes: make(map[string]provider.Runtime)}
}

func (fake *Provider) EnsureDataVolume(_ context.Context, request provider.EnsureDataVolumeRequest) (provider.DataVolume, error) {
	if request.EnvironmentID == "" || request.OperationID == "" || request.Region == "" || request.AvailabilityZone == "" {
		return provider.DataVolume{}, errors.New("ensure Data Volume: Environment, Operation, region, and availability zone are required")
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if existing, present := fake.dataVolumes[request.EnvironmentID]; present {
		if existing.Region != request.Region || existing.AvailabilityZone != request.AvailabilityZone {
			return provider.DataVolume{}, fmt.Errorf("ensure Data Volume: Environment %q already has a volume in %s", request.EnvironmentID, existing.AvailabilityZone)
		}
		return existing, nil
	}
	volume := provider.DataVolume{
		Provider: "fake", ProviderID: "fake-volume-" + request.EnvironmentID, EnvironmentID: request.EnvironmentID,
		Region: request.Region, AvailabilityZone: request.AvailabilityZone,
	}
	fake.dataVolumes[request.EnvironmentID] = volume
	fake.creates++
	return volume, nil
}

func (fake *Provider) DataVolumeCreateCount() int {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	return fake.creates
}

func (fake *Provider) EnsureRuntime(_ context.Context, request provider.EnsureRuntimeRequest) (provider.Runtime, error) {
	if request.RuntimeID == "" || request.EnvironmentID == "" || request.OperationID == "" || request.DataVolumeProviderID == "" {
		return provider.Runtime{}, errors.New("ensure Runtime: Runtime, Environment, Operation, and Data Volume are required")
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if existing, ok := fake.runtimes[request.RuntimeID]; ok {
		if existing.RuntimeSpec != request.RuntimeSpec {
			return provider.Runtime{}, provider.NewError(provider.ErrorCodeResourceDiverged, "Runtime ownership diverged", nil)
		}
		return existing, nil
	}
	runtime := provider.Runtime{
		RuntimeSpec: request.RuntimeSpec, ProviderID: "fake-runtime-" + request.RuntimeID,
		PrivateIPv4: "10.0.0.8", State: provider.RuntimeStateRunning,
	}
	fake.runtimes[request.RuntimeID] = runtime
	return runtime, nil
}

func (fake *Provider) EnsureRuntimeDataVolumeAttachment(_ context.Context, request provider.RuntimeLifecycleRequest) (provider.Runtime, error) {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	runtime, ok := fake.runtimes[request.RuntimeID]
	if !ok || runtime.ProviderID != request.ProviderID || runtime.RuntimeSpec != request.RuntimeSpec {
		return provider.Runtime{}, provider.NewError(provider.ErrorCodeResourceDiverged, "Runtime attachment identity diverged", nil)
	}
	return runtime, nil
}

func (fake *Provider) ObserveRuntime(_ context.Context, request provider.RuntimeLifecycleRequest) (provider.Runtime, error) {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	runtime, ok := fake.runtimes[request.RuntimeID]
	if !ok || runtime.ProviderID != request.ProviderID || runtime.RuntimeSpec != request.RuntimeSpec {
		return provider.Runtime{}, provider.NewError(provider.ErrorCodeResourceDiverged, "Runtime observation diverged", nil)
	}
	return runtime, nil
}

func (fake *Provider) StartRuntime(ctx context.Context, request provider.RuntimeLifecycleRequest) (provider.Runtime, error) {
	return fake.ObserveRuntime(ctx, request)
}

func (fake *Provider) StopRuntime(_ context.Context, request provider.RuntimeLifecycleRequest) (provider.Runtime, error) {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	runtime, ok := fake.runtimes[request.RuntimeID]
	if !ok || runtime.ProviderID != request.ProviderID || runtime.RuntimeSpec != request.RuntimeSpec {
		return provider.Runtime{}, provider.NewError(provider.ErrorCodeResourceDiverged, "Runtime observation diverged", nil)
	}
	runtime.State = provider.RuntimeStateStopped
	runtime.PrivateIPv4 = ""
	fake.runtimes[request.RuntimeID] = runtime
	return runtime, nil
}

func (fake *Provider) RetireRuntime(_ context.Context, request provider.RuntimeLifecycleRequest) (provider.Runtime, error) {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	runtime, ok := fake.runtimes[request.RuntimeID]
	if !ok || runtime.ProviderID != request.ProviderID || runtime.RuntimeSpec != request.RuntimeSpec {
		return provider.Runtime{}, provider.NewError(provider.ErrorCodeResourceDiverged, "Runtime observation diverged", nil)
	}
	runtime.State = provider.RuntimeStateTerminated
	runtime.PrivateIPv4 = ""
	fake.runtimes[request.RuntimeID] = runtime
	return runtime, nil
}
