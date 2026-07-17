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

func TestBuildServicesWithFullDependenciesBindsAllThreeServices(t *testing.T) {
	creationActions, err := workflows.NewEnvironmentCreationActions(&environmentCreationRepositoryFake{}, &pinnedProfileVersionResolverFake{})
	if err != nil {
		t.Fatalf("construct Environment creation actions: %v", err)
	}
	profileResolveActions := workflows.NewProfileResolveActions(&profileResolveRepositoryFake{})
	services := buildServices(serviceDependencies{
		polarDeliverer: polarEventDelivererFake{}, polarStore: polarDeliveryStoreFake{}, now: time.Now,
		environmentCreate: workflows.EnvironmentCreateDefinition(testfixtures.NewProvider(), creationActions, idGenerator{}, time.Now, "image-v1"),
		profileResolve:    workflows.ProfileResolveDefinition(profileResolveActions, capsuleResolverFake{}, idGenerator{}, time.Now),
	})
	names := make([]string, len(services))
	for index, service := range services {
		names[index] = service.Name()
	}
	want := []string{workflows.BillingDeliveryService, workflows.EnvironmentCreateService, workflows.ProfileResolveService}
	if len(names) != len(want) {
		t.Fatalf("bound services = %v, want %v", names, want)
	}
	for index, name := range want {
		if names[index] != name {
			t.Fatalf("bound services = %v, want %v", names, want)
		}
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
