package main

import (
	"context"
	"errors"

	"github.com/ahmedhesham6/sshai/apps/guest"
	guestcontrol "github.com/ahmedhesham6/sshai/apps/guest/control"
	"github.com/ahmedhesham6/sshai/apps/workflows"
	"github.com/ahmedhesham6/sshai/libs/profile"
)

type guestControlConfig struct {
	port                 int
	serverName           string
	certificateDirectory string
	privateKeyDirectory  string
	caFile               string
}

type runtimeGuestTransport interface {
	workflows.RuntimeGuestReadinessSource
	workflows.RuntimeSSHKeyReconciler
	workflows.RuntimeSSHHostIdentityReconciler
	workflows.RuntimeManagedConfigurationReconciler
	workflows.RuntimeGuestShutdownPreparer
	workflows.EnvironmentSSHIdentityRestorer
	workflows.EnvironmentProjectSeedApplicator
	workflows.EnvironmentCapsuleApplier
	workflows.EnvironmentToolchainValidator
}

type guestControlClient interface {
	ReadReadiness(context.Context, guestcontrol.Target) (guestcontrol.ReadinessStatus, error)
	ApplyProjectSeed(context.Context, guestcontrol.ProjectSeedRequest) error
	RestoreSSHHostIdentity(context.Context, guestcontrol.Target) (guest.SSHHostIdentityStatus, error)
	ReconcileSSHKeys(context.Context, guestcontrol.Target) error
	ReconcileManagedConfiguration(context.Context, guestcontrol.Target) error
	PrepareShutdown(context.Context, guestcontrol.Target) error
	ApplyMaterialization(context.Context, guestcontrol.MaterializationRequest) ([]profile.ProfileMaterializationResult, error)
	ValidateToolchain(context.Context, guestcontrol.Target) error
}

type environmentProjectSeedSource interface {
	LoadProjectSeedApplication(context.Context, string, string, string) (guest.ProjectSeedApplicationInput, error)
}

type environmentCapsuleGrantSource interface {
	MaterializationReadGrants(context.Context, string, workflows.EnvironmentCapsuleState) (map[string]guestcontrol.ReadGrant, error)
}

type guestTransportDependencies struct {
	projectSeeds  environmentProjectSeedSource
	capsuleGrants environmentCapsuleGrantSource
}

type configuredGuestTransport struct {
	client        guestControlClient
	projectSeeds  environmentProjectSeedSource
	capsuleGrants environmentCapsuleGrantSource
}

func newRuntimeGuestTransport(config guestControlConfig, provided ...guestTransportDependencies) (runtimeGuestTransport, error) {
	if config.serverName != "" && config.certificateDirectory == "" && config.privateKeyDirectory == "" && config.caFile == "" {
		return nil, errors.New("GUEST_CONTROL_TLS_SERVER_NAME requires guest control TLS configuration")
	}
	configured := 0
	for _, value := range []string{config.certificateDirectory, config.privateKeyDirectory, config.caFile} {
		if value != "" {
			configured++
		}
	}
	if configured == 0 {
		return unavailableGuestTransport{}, nil
	}
	if configured != 3 {
		return nil, errors.New("GUEST_CONTROL_TLS_CERT_DIR, GUEST_CONTROL_TLS_KEY_DIR, and GUEST_CONTROL_TLS_CA_FILE are all required when guest control is configured")
	}
	certificates, err := guestcontrol.NewDirectoryClientCertificateSource(config.certificateDirectory, config.privateKeyDirectory)
	if err != nil {
		return nil, err
	}
	client, err := guestcontrol.NewClient(guestcontrol.ClientConfig{
		Port: config.port, ServerName: config.serverName, CertificateSource: certificates, CAFile: config.caFile,
	})
	if err != nil {
		return nil, err
	}
	if len(provided) != 1 || provided[0].projectSeeds == nil || provided[0].capsuleGrants == nil {
		return nil, errors.New("configured guest control requires Project Seed and Capsule grant sources")
	}
	return &configuredGuestTransport{client: client, projectSeeds: provided[0].projectSeeds, capsuleGrants: provided[0].capsuleGrants}, nil
}

func (transport *configuredGuestTransport) WaitForRuntimeReady(ctx context.Context, request workflows.RuntimeGuestReadinessRequest) (workflows.RuntimeGuestReadiness, error) {
	status, err := transport.client.ReadReadiness(ctx, guestTarget(request))
	if err != nil {
		return workflows.RuntimeGuestReadiness{}, err
	}
	return workflows.RuntimeGuestReadiness{
		BootID: status.Snapshot.BootID, PrivateIPv4: status.PrivateIPv4,
		DataMounted: guest.CompareReadiness(status.Snapshot.Level, guest.ReadinessDataMounted) >= 0,
	}, nil
}

func (transport *configuredGuestTransport) ReconcileRuntimeSSHKeys(ctx context.Context, request workflows.RuntimeGuestReadinessRequest) error {
	return transport.client.ReconcileSSHKeys(ctx, guestTarget(request))
}

func (transport *configuredGuestTransport) RestoreRuntimeSSHHostIdentity(ctx context.Context, request workflows.RuntimeGuestReadinessRequest) error {
	_, err := transport.client.RestoreSSHHostIdentity(ctx, guestTarget(request))
	return err
}

func (transport *configuredGuestTransport) ReconcileRuntimeManagedConfiguration(ctx context.Context, request workflows.RuntimeGuestReadinessRequest) error {
	return transport.client.ReconcileManagedConfiguration(ctx, guestTarget(request))
}

func (transport *configuredGuestTransport) PrepareRuntimeShutdown(ctx context.Context, request workflows.RuntimeGuestReadinessRequest) error {
	return transport.client.PrepareShutdown(ctx, guestTarget(request))
}

func (transport *configuredGuestTransport) RestoreEnvironmentSSHIdentity(ctx context.Context, request workflows.EnvironmentCreateGuestRequest) error {
	_, err := transport.client.RestoreSSHHostIdentity(ctx, guestTarget(request.RuntimeGuestReadinessRequest))
	return err
}

func (transport *configuredGuestTransport) EnsureEnvironmentProjectSeedApplied(ctx context.Context, request workflows.EnvironmentProjectSeedRequest) error {
	seed, err := transport.projectSeeds.LoadProjectSeedApplication(ctx, request.Guest.OwnerUserID, request.Guest.EnvironmentID, request.ProjectSeedID)
	if err != nil {
		return err
	}
	return transport.client.ApplyProjectSeed(ctx, guestcontrol.ProjectSeedRequest{
		Target: guestTarget(request.Guest.RuntimeGuestReadinessRequest), Seed: seed,
	})
}

func (transport *configuredGuestTransport) EnsureEnvironmentCapsuleMaterialized(ctx context.Context, request workflows.EnvironmentCapsuleMaterializationRequest) ([]profile.ProfileMaterializationResult, error) {
	grants, err := transport.capsuleGrants.MaterializationReadGrants(ctx, request.Guest.OwnerUserID, request.State)
	if err != nil {
		return nil, err
	}
	return transport.client.ApplyMaterialization(ctx, guestcontrol.MaterializationRequest{
		Target: guestTarget(request.Guest.RuntimeGuestReadinessRequest), Lock: request.State.CapsuleLock,
		OwnerID: request.Guest.OwnerUserID, Intent: profile.IntentReconcile,
		Installed: request.State.Materializations, ReadGrants: grants,
	})
}

func (transport *configuredGuestTransport) ValidateEnvironmentToolchain(ctx context.Context, request workflows.EnvironmentCreateGuestRequest) error {
	return transport.client.ValidateToolchain(ctx, guestTarget(request.RuntimeGuestReadinessRequest))
}

func guestTarget(request workflows.RuntimeGuestReadinessRequest) guestcontrol.Target {
	return guestcontrol.Target{
		OwnerUserID: request.OwnerUserID, EnvironmentID: request.EnvironmentID, RuntimeID: request.RuntimeID,
		ProviderID: request.ProviderID, PrivateIPv4: request.PrivateIPv4,
	}
}

var (
	_ runtimeGuestTransport = unavailableGuestTransport{}
	_ runtimeGuestTransport = (*configuredGuestTransport)(nil)
)
