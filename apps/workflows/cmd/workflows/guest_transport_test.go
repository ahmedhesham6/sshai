package main

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/ahmedhesham6/sshai/apps/guest"
	guestcontrol "github.com/ahmedhesham6/sshai/apps/guest/control"
	"github.com/ahmedhesham6/sshai/apps/workflows"
	"github.com/ahmedhesham6/sshai/libs/capsule/oci"
	"github.com/ahmedhesham6/sshai/libs/domain"
	"github.com/ahmedhesham6/sshai/libs/profile"
)

func TestCapsuleMaterializationGrantSourceMintsEveryExactPullCapability(t *testing.T) {
	provider := newMemoryGrantProvider()
	client, err := oci.NewClient("user-1", provider)
	if err != nil {
		t.Fatal(err)
	}
	capsuleValue := buildSingleComponentCapsule(t, "config:editor", "editor content\n")
	publication, err := client.Publish(t.Context(), capsuleValue)
	if err != nil {
		t.Fatal(err)
	}
	state := workflows.EnvironmentCapsuleState{CapsuleLock: domain.CapsuleLockSnapshot{ResolvedComponents: map[string]domain.ResolvedComponent{
		"config:editor": {ID: "config:editor", CapsuleDigest: capsuleValue.Digest},
	}}}
	grants, err := (capsuleMaterializationGrantSource{provider: provider}).MaterializationReadGrants(t.Context(), "user-1", state)
	if err != nil {
		t.Fatal(err)
	}
	wantKeys := append([]string{publication.IndexKey}, publication.BlobKeys...)
	if len(grants) != len(wantKeys) {
		t.Fatalf("materialization grants = %d, want %d (%v)", len(grants), len(wantKeys), wantKeys)
	}
	for _, key := range wantKeys {
		grant, present := grants[key]
		if !present || grant.URL == "" || grant.ExpiresAt.IsZero() {
			t.Fatalf("materialization grant %q = %#v, present=%v", key, grant, present)
		}
	}
}

func TestCapsuleMaterializationGrantSourceClassifiesImmutableMetadataPermanent(t *testing.T) {
	state := workflows.EnvironmentCapsuleState{CapsuleLock: domain.CapsuleLockSnapshot{ResolvedComponents: map[string]domain.ResolvedComponent{
		"config:editor": {ID: "config:editor", CapsuleDigest: "not-a-digest"},
	}}}
	_, err := (capsuleMaterializationGrantSource{provider: newMemoryGrantProvider()}).MaterializationReadGrants(t.Context(), "user-1", state)
	var classified interface{ Transient() bool }
	if !errors.As(err, &classified) || classified.Transient() {
		t.Fatalf("MaterializationReadGrants() error = %T %v, want permanent immutable-content classification", err, err)
	}
}

func TestConfiguredGuestTransportMapsWorkflowSeamsToControlRequests(t *testing.T) {
	targetRequest := workflows.RuntimeGuestReadinessRequest{
		OwnerUserID: "user-1", EnvironmentID: "environment-1", RuntimeID: "runtime-1",
		ProviderID: "instance-1", PrivateIPv4: "10.0.0.8",
	}
	client := &guestControlClientFake{materializationResults: []profile.ProfileMaterializationResult{{ComponentID: "config:editor"}}}
	seeds := &projectSeedSourceFake{input: guest.ProjectSeedApplicationInput{RepositoryURL: "https://example.invalid/repository.git", BaseRevision: "0123456789012345678901234567890123456789"}}
	grants := &capsuleGrantSourceFake{grants: map[string]guestcontrol.ReadGrant{
		"owner/user-1/index/manifest/sha256/capsule": {URL: "https://capsules.example/read", ExpiresAt: time.Now().Add(time.Minute)},
	}}
	transport := &configuredGuestTransport{client: client, projectSeeds: seeds, capsuleGrants: grants}

	if err := transport.RestoreRuntimeSSHHostIdentity(t.Context(), targetRequest); err != nil {
		t.Fatal(err)
	}
	createRequest := workflows.EnvironmentCreateGuestRequest{OperationID: "operation-1", RuntimeGuestReadinessRequest: targetRequest}
	if err := transport.RestoreEnvironmentSSHIdentity(t.Context(), createRequest); err != nil {
		t.Fatal(err)
	}
	if err := transport.EnsureEnvironmentProjectSeedApplied(t.Context(), workflows.EnvironmentProjectSeedRequest{Guest: createRequest, ProjectSeedID: "seed-1"}); err != nil {
		t.Fatal(err)
	}
	state := workflows.EnvironmentCapsuleState{
		CapsuleLock:      domain.CapsuleLockSnapshot{ID: "lock-1", EnvironmentID: "environment-1", Digest: "sha256:lock"},
		Materializations: []profile.InstalledMaterialization{{ComponentID: "config:editor"}},
	}
	results, err := transport.EnsureEnvironmentCapsuleMaterialized(t.Context(), workflows.EnvironmentCapsuleMaterializationRequest{Guest: createRequest, State: state})
	if err != nil {
		t.Fatal(err)
	}
	if err := transport.ValidateEnvironmentToolchain(t.Context(), createRequest); err != nil {
		t.Fatal(err)
	}

	wantTarget := guestTarget(targetRequest)
	if client.hostIdentityCalls != 2 || client.hostIdentityTarget != wantTarget || client.toolchainTarget != wantTarget {
		t.Fatalf("host identity/toolchain mapping = calls:%d host:%#v toolchain:%#v", client.hostIdentityCalls, client.hostIdentityTarget, client.toolchainTarget)
	}
	if seeds.ownerID != "user-1" || seeds.environmentID != "environment-1" || seeds.projectSeedID != "seed-1" || client.seed.Target != wantTarget || client.seed.Seed.RepositoryURL != seeds.input.RepositoryURL {
		t.Fatalf("Project Seed mapping = source:%s/%s/%s request:%#v", seeds.ownerID, seeds.environmentID, seeds.projectSeedID, client.seed)
	}
	if grants.ownerID != "user-1" || grants.state.CapsuleLock.ID != "lock-1" || client.materialization.Target != wantTarget ||
		client.materialization.OwnerID != "user-1" || client.materialization.Intent != profile.IntentReconcile ||
		len(client.materialization.Installed) != 1 || len(client.materialization.ReadGrants) != 1 || len(results) != 1 {
		t.Fatalf("Materialization mapping = grants:%#v request:%#v results:%#v", grants, client.materialization, results)
	}
}

func TestUnavailableGuestTransportClassifiesEveryWorkflowSeamPermanent(t *testing.T) {
	transport := unavailableGuestTransport{}
	createRequest := workflows.EnvironmentCreateGuestRequest{}
	tests := []struct {
		name string
		run  func() error
	}{
		{name: "Runtime SSH host identity", run: func() error {
			return transport.RestoreRuntimeSSHHostIdentity(t.Context(), workflows.RuntimeGuestReadinessRequest{})
		}},
		{name: "Environment SSH identity", run: func() error { return transport.RestoreEnvironmentSSHIdentity(t.Context(), createRequest) }},
		{name: "Project Seed", run: func() error {
			return transport.EnsureEnvironmentProjectSeedApplied(t.Context(), workflows.EnvironmentProjectSeedRequest{})
		}},
		{name: "Capsule Materialization", run: func() error {
			_, err := transport.EnsureEnvironmentCapsuleMaterialized(t.Context(), workflows.EnvironmentCapsuleMaterializationRequest{})
			return err
		}},
		{name: "toolchain", run: func() error { return transport.ValidateEnvironmentToolchain(t.Context(), createRequest) }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := test.run()
			var classified interface{ Transient() bool }
			if !errors.As(err, &classified) || classified.Transient() {
				t.Fatalf("error = %T %v, want permanent classified error", err, err)
			}
		})
	}
}

type guestControlClientFake struct {
	hostIdentityCalls      int
	hostIdentityTarget     guestcontrol.Target
	seed                   guestcontrol.ProjectSeedRequest
	materialization        guestcontrol.MaterializationRequest
	materializationResults []profile.ProfileMaterializationResult
	toolchainTarget        guestcontrol.Target
}

func (*guestControlClientFake) ReadReadiness(context.Context, guestcontrol.Target) (guestcontrol.ReadinessStatus, error) {
	return guestcontrol.ReadinessStatus{}, nil
}
func (fake *guestControlClientFake) ApplyProjectSeed(_ context.Context, request guestcontrol.ProjectSeedRequest) error {
	fake.seed = request
	return nil
}
func (fake *guestControlClientFake) RestoreSSHHostIdentity(_ context.Context, target guestcontrol.Target) (guest.SSHHostIdentityStatus, error) {
	fake.hostIdentityCalls++
	fake.hostIdentityTarget = target
	return guest.SSHHostIdentityStatus{}, nil
}
func (*guestControlClientFake) ReconcileSSHKeys(context.Context, guestcontrol.Target) error {
	return nil
}
func (*guestControlClientFake) ReconcileManagedConfiguration(context.Context, guestcontrol.Target) error {
	return nil
}
func (*guestControlClientFake) PrepareShutdown(context.Context, guestcontrol.Target) error {
	return nil
}
func (fake *guestControlClientFake) ApplyMaterialization(_ context.Context, request guestcontrol.MaterializationRequest) ([]profile.ProfileMaterializationResult, error) {
	fake.materialization = request
	return fake.materializationResults, nil
}
func (fake *guestControlClientFake) ValidateToolchain(_ context.Context, target guestcontrol.Target) error {
	fake.toolchainTarget = target
	return nil
}

type projectSeedSourceFake struct {
	input                                 guest.ProjectSeedApplicationInput
	ownerID, environmentID, projectSeedID string
}

func (fake *projectSeedSourceFake) LoadProjectSeedApplication(_ context.Context, ownerID, environmentID, projectSeedID string) (guest.ProjectSeedApplicationInput, error) {
	fake.ownerID, fake.environmentID, fake.projectSeedID = ownerID, environmentID, projectSeedID
	return fake.input, nil
}

type capsuleGrantSourceFake struct {
	grants  map[string]guestcontrol.ReadGrant
	ownerID string
	state   workflows.EnvironmentCapsuleState
}

func (fake *capsuleGrantSourceFake) MaterializationReadGrants(_ context.Context, ownerID string, state workflows.EnvironmentCapsuleState) (map[string]guestcontrol.ReadGrant, error) {
	fake.ownerID, fake.state = ownerID, state
	return fake.grants, nil
}
