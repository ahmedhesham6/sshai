package testfixtures_test

import (
	"testing"

	"github.com/ahmedhesham6/sshai/libs/provider"
	"github.com/ahmedhesham6/sshai/libs/testfixtures"
)

func TestFakeProviderEnsuresOneDataVolumePerEnvironment(t *testing.T) {
	fake := testfixtures.NewProvider()
	request := provider.EnsureDataVolumeRequest{
		EnvironmentID: "environment-1", OperationID: "operation-1",
		Region: "us-east-1", AvailabilityZone: "us-east-1a",
	}
	first, err := fake.EnsureDataVolume(t.Context(), request)
	if err != nil {
		t.Fatalf("EnsureDataVolume(): %v", err)
	}
	second, err := fake.EnsureDataVolume(t.Context(), request)
	if err != nil {
		t.Fatalf("replay EnsureDataVolume(): %v", err)
	}
	if first != second || first.Provider != "fake" || first.EnvironmentID != request.EnvironmentID || first.AvailabilityZone != request.AvailabilityZone {
		t.Fatalf("replayed Data Volume differs: %#v != %#v", first, second)
	}
	if got := fake.DataVolumeCreateCount(); got != 1 {
		t.Fatalf("provider mutations = %d, want 1", got)
	}
}

func TestFakeProviderRejectsConflictingDataVolumePlacement(t *testing.T) {
	fake := testfixtures.NewProvider()
	request := provider.EnsureDataVolumeRequest{
		EnvironmentID: "environment-1", OperationID: "operation-1",
		Region: "us-east-1", AvailabilityZone: "us-east-1a",
	}
	if _, err := fake.EnsureDataVolume(t.Context(), request); err != nil {
		t.Fatalf("EnsureDataVolume(): %v", err)
	}
	request.AvailabilityZone = "us-east-1b"
	if _, err := fake.EnsureDataVolume(t.Context(), request); err == nil {
		t.Fatal("conflicting EnsureDataVolume() error = nil")
	}
	if got := fake.DataVolumeCreateCount(); got != 1 {
		t.Fatalf("provider mutations = %d, want 1", got)
	}
}

func TestFakeProviderResolvesRuntimeContractIdentityOnAttachment(t *testing.T) {
	fake := testfixtures.NewProvider()
	request := provider.EnsureRuntimeRequest{RuntimeSpec: provider.RuntimeSpec{
		RuntimeID: "runtime-1", EnvironmentID: "environment-1", Sequence: 1,
		Region: "us-east-1", AvailabilityZone: "us-east-1a", RuntimePreset: "standard",
		ImageVersion: "image-v1", DataVolumeProviderID: "volume-1",
	}, OperationID: "operation-1"}
	allocated, err := fake.EnsureRuntime(t.Context(), request)
	if err != nil {
		t.Fatal(err)
	}
	attached, err := fake.EnsureRuntimeDataVolumeAttachment(t.Context(), provider.RuntimeLifecycleRequest{RuntimeSpec: request.RuntimeSpec, ProviderID: allocated.ProviderID})
	if err != nil {
		t.Fatal(err)
	}
	if attached.Provider != "fixture" || attached.SystemVolumeProviderID == "" {
		t.Fatalf("attached fixture Runtime identity = %#v", attached)
	}
}
