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
	endpoint        string
	certificateFile string
	privateKeyFile  string
	caFile          string
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
	configured := 0
	for _, value := range []string{config.endpoint, config.certificateFile, config.privateKeyFile, config.caFile} {
		if value != "" {
			configured++
		}
	}
	if configured == 0 {
		return unavailableGuestTransport{}, nil
	}
	if configured != 4 {
		return nil, errors.New("GUEST_CONTROL_ENDPOINT, GUEST_CONTROL_TLS_CERT_FILE, GUEST_CONTROL_TLS_KEY_FILE, and GUEST_CONTROL_TLS_CA_FILE are all required when guest control is configured")
	}
	client, err := guestcontrol.NewClient(guestcontrol.ClientConfig{
		Endpoint: config.endpoint, CertificateFile: config.certificateFile,
		PrivateKeyFile: config.privateKeyFile, CAFile: config.caFile,
	})
	if err != nil {
		return nil, err
	}
	return &configuredGuestTransport{client: client}, nil
}

func (transport *configuredGuestTransport) WaitForRuntimeReady(ctx context.Context, request workflows.RuntimeGuestReadinessRequest) (workflows.RuntimeGuestReadiness, error) {
	status, err := transport.client.WaitForReadiness(ctx, guestTarget(request), guest.ReadinessDataMounted)
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
