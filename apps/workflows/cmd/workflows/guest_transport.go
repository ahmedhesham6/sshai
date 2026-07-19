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
	workflows.RuntimeManagedConfigurationReconciler
	workflows.RuntimeGuestShutdownPreparer
}

type configuredGuestTransport struct {
	client *guestcontrol.Client
}

func newRuntimeGuestTransport(config guestControlConfig) (runtimeGuestTransport, error) {
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
	return &configuredGuestTransport{client: client}, nil
}

func (transport *configuredGuestTransport) WaitForRuntimeReady(ctx context.Context, request workflows.RuntimeGuestReadinessRequest) (workflows.RuntimeGuestReadiness, error) {
	status, err := transport.client.ReadReadiness(ctx, guestTarget(request))
	if err != nil {
		return workflows.RuntimeGuestReadiness{}, err
	}
	return workflows.RuntimeGuestReadiness{
		BootID: status.Snapshot.BootID, PrivateIPv4: status.PrivateIPv4,
		DataMounted: readinessAtLeast(status.Snapshot.Level, guest.ReadinessDataMounted),
	}, nil
}

func (transport *configuredGuestTransport) ReconcileRuntimeSSHKeys(ctx context.Context, request workflows.RuntimeGuestReadinessRequest) error {
	return transport.client.ReconcileSSHKeys(ctx, guestTarget(request))
}

func (transport *configuredGuestTransport) ReconcileRuntimeManagedConfiguration(ctx context.Context, request workflows.RuntimeGuestReadinessRequest) error {
	return transport.client.ReconcileManagedConfiguration(ctx, guestTarget(request))
}

func (transport *configuredGuestTransport) PrepareRuntimeShutdown(ctx context.Context, request workflows.RuntimeGuestReadinessRequest) error {
	return transport.client.PrepareShutdown(ctx, guestTarget(request))
}

// ApplyProjectSeed and ApplyMaterialization expose the create-workflow guest
// contracts without changing environment_create's current dependency shape.
func (transport *configuredGuestTransport) ApplyProjectSeed(ctx context.Context, request guestcontrol.ProjectSeedRequest) error {
	return transport.client.ApplyProjectSeed(ctx, request)
}

func (transport *configuredGuestTransport) ApplyMaterialization(ctx context.Context, request guestcontrol.MaterializationRequest) ([]profile.ProfileMaterializationResult, error) {
	return transport.client.ApplyMaterialization(ctx, request)
}

func guestTarget(request workflows.RuntimeGuestReadinessRequest) guestcontrol.Target {
	return guestcontrol.Target{
		OwnerUserID: request.OwnerUserID, EnvironmentID: request.EnvironmentID, RuntimeID: request.RuntimeID,
		ProviderID: request.ProviderID, PrivateIPv4: request.PrivateIPv4,
	}
}

func readinessAtLeast(actual, expected guest.ReadinessLevel) bool {
	order := map[guest.ReadinessLevel]int{
		guest.ReadinessAllocated: 1, guest.ReadinessDataMounted: 2, guest.ReadinessSSHReady: 3,
		guest.ReadinessProjectReady: 4, guest.ReadinessAgentsValidated: 5,
	}
	return order[actual] >= order[expected]
}

var (
	_ runtimeGuestTransport = unavailableGuestTransport{}
	_ runtimeGuestTransport = (*configuredGuestTransport)(nil)
)
