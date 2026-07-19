package main

import (
	"context"
	"testing"
	"time"

	"github.com/ahmedhesham6/sshai/apps/workflows"
	"github.com/ahmedhesham6/sshai/libs/billing"
	"github.com/ahmedhesham6/sshai/libs/db"
	"github.com/ahmedhesham6/sshai/libs/domain"
	"github.com/ahmedhesham6/sshai/libs/testfixtures"
)

func TestBuildServicesWithBillingOnlyDependencies(t *testing.T) {
	services := buildServices(serviceDependencies{now: time.Now})
	names := make([]string, len(services))
	for index, service := range services {
		names[index] = service.Name()
	}
	if len(names) != 1 || names[0] != workflows.BillingDeliveryService {
		t.Fatalf("bound services = %v, want [%s]", names, workflows.BillingDeliveryService)
	}
}

func TestBuildServicesWithFullDependenciesBindsAllProductionServices(t *testing.T) {
	creationActions, err := workflows.NewEnvironmentCreationActions(&environmentCreationRepositoryFake{}, &pinnedProfileVersionResolverFake{})
	if err != nil {
		t.Fatalf("construct Environment creation actions: %v", err)
	}
	profileResolveActions := workflows.NewProfileResolveActions(&profileResolveRepositoryFake{})
	guest := unavailableGuestTransport{}
	services := buildServices(serviceDependencies{
		polarDeliverer: polarEventDelivererFake{}, polarStore: polarDeliveryStoreFake{}, now: time.Now,
		environmentCreate: workflows.EnvironmentCreateDefinitionWithDependencies(workflows.EnvironmentCreateDependencies{
			Provider: testfixtures.NewProvider(), Actions: creationActions, Capsules: creationActions,
			SSHIdentity: guest, GuestReadiness: guest, SSHKeys: guest, ProjectSeed: guest, Materializer: guest,
			Credentials: workflows.NoProjectCredentialBinder{}, Toolchain: guest,
			IDs: idGenerator{}, Now: time.Now, ImageVersion: "image-v1",
		}),
		profileResolve: workflows.ProfileResolveDefinition(profileResolveActions, capsuleResolverFake{}, idGenerator{}, time.Now),
		autoStop:       workflows.AutoStopDefinition(nil, nil),
		runtimeStart:   workflows.RuntimeStartDefinition(workflows.RuntimeStartDependencies{}),
		runtimeStop:    workflows.RuntimeStopDefinition(workflows.RuntimeStopDependencies{}),
		runtimeReplace: workflows.RuntimeReplaceDefinition(workflows.RuntimeReplaceDependencies{}),
	})
	names := make([]string, len(services))
	for index, service := range services {
		names[index] = service.Name()
	}
	want := []string{
		workflows.BillingDeliveryService, workflows.EnvironmentCreateService, workflows.ProfileResolveService,
		workflows.AutoStopService, workflows.RuntimeStartService, workflows.RuntimeStopService, workflows.RuntimeReplaceService,
	}
	if len(names) != len(want) {
		t.Fatalf("bound services = %v, want %v", names, want)
	}
	for index, name := range want {
		if names[index] != name {
			t.Fatalf("bound services = %v, want %v", names, want)
		}
	}
}

func TestLoadConfigUsesRatifiedInfrastructureDefaults(t *testing.T) {
	for name, value := range map[string]string{
		"DATABASE_URL": "postgres://example", "POLAR_EVENTS_ENDPOINT": "https://polar.example/events",
		"POLAR_ACCESS_TOKEN": "token", "CAPSULE_BUCKET": "capsules", "RUNTIME_ENVIRONMENT_NAME": "devm",
		"RUNTIME_AMI": "ami-1", "RUNTIME_SUBNET_ID": "subnet-1", "RUNTIME_SECURITY_GROUP_ID": "sg-1",
		"RUNTIME_PRESETS": "standard=m7i.large", "IMAGE_VERSION": "image-v1",
	} {
		t.Setenv(name, value)
	}
	for _, name := range []string{"DEFAULT_REGION", "DATA_VOLUME_SIZE_GIB", "RUNTIME_SYSTEM_VOLUME_GIB"} {
		t.Setenv(name, "")
	}

	config, err := loadConfig()
	if err != nil {
		t.Fatalf("loadConfig(): %v", err)
	}
	if config.defaultRegion != "eu-central-1" || config.dataVolumeSizeGiB != 100 || config.runtimeSystemVolumeGiB != 30 {
		t.Fatalf("infrastructure defaults = region:%q data:%d system:%d", config.defaultRegion, config.dataVolumeSizeGiB, config.runtimeSystemVolumeGiB)
	}
}

type polarEventDelivererFake struct{}

func (polarEventDelivererFake) Deliver(context.Context, billing.CreditsUsedEvent) error { return nil }

type polarDeliveryStoreFake struct{}

func (polarDeliveryStoreFake) PolarDeliveryEvent(context.Context, string) (billing.CreditsUsedEvent, bool, bool, error) {
	return billing.CreditsUsedEvent{}, false, false, nil
}

func (polarDeliveryStoreFake) RecordPolarDeliverySuccess(context.Context, string, time.Time) error {
	return nil
}

type environmentCreationRepositoryFake struct{}

func (environmentCreationRepositoryFake) RecordEnvironmentCreateInvocation(context.Context, string, string, time.Time) (domain.EnvironmentCreation, error) {
	return domain.EnvironmentCreation{}, nil
}

func (environmentCreationRepositoryFake) InventoryEnvironmentState(context.Context, string, domain.EnvironmentStateReservation) (domain.EnvironmentState, error) {
	return domain.EnvironmentState{}, nil
}

func (environmentCreationRepositoryFake) ReserveInitialRuntime(context.Context, string, domain.RuntimeReservation) (domain.Runtime, error) {
	return domain.Runtime{}, nil
}

func (environmentCreationRepositoryFake) PersistEnvironmentCreateRuntimeTransition(context.Context, string, int64, domain.RuntimeSnapshot) error {
	return nil
}

func (environmentCreationRepositoryFake) FinishEnvironmentCreateOperation(context.Context, string, domain.OperationStatus, string, string, time.Time) error {
	return nil
}

func (environmentCreationRepositoryFake) CompleteEnvironmentCreation(context.Context, string, time.Time) (domain.EnvironmentCreation, error) {
	return domain.EnvironmentCreation{}, nil
}

func (environmentCreationRepositoryFake) PersistEnvironmentCapsuleState(context.Context, db.EnvironmentCapsuleStateInput) error {
	return nil
}

type pinnedProfileVersionResolverFake struct{}

func (pinnedProfileVersionResolverFake) ResolvePinnedProfileVersion(context.Context, string, time.Time) (workflows.EnvironmentCapsuleState, error) {
	return workflows.EnvironmentCapsuleState{}, nil
}

type profileResolveRepositoryFake struct{}

func (profileResolveRepositoryFake) RecordProfileResolveInvocation(context.Context, string, string, string, string, *string, time.Time) error {
	return nil
}

func (profileResolveRepositoryFake) LoadProfileVersion(context.Context, string, string) (domain.ProfileVersionData, error) {
	return domain.ProfileVersionData{}, nil
}

func (profileResolveRepositoryFake) PersistCapsuleLock(context.Context, domain.CapsuleLock) (domain.CapsuleLock, error) {
	return domain.CapsuleLock{}, nil
}

func (profileResolveRepositoryFake) CompleteProfileResolve(context.Context, string, time.Time) error {
	return nil
}

func (profileResolveRepositoryFake) LoadProfileResolveState(context.Context, string) (workflows.ProfileResolveState, error) {
	return workflows.ProfileResolveState{}, nil
}

type capsuleResolverFake struct{}

func (capsuleResolverFake) Resolve(context.Context, string, domain.CapsuleRef) (workflows.CapsuleResolution, error) {
	return workflows.CapsuleResolution{}, nil
}
