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
	creates     int
}

func NewProvider() *Provider {
	return &Provider{dataVolumes: make(map[string]provider.DataVolume)}
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
