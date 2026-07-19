package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ahmedhesham6/sshai/apps/workflows"
	"github.com/ahmedhesham6/sshai/libs/capsule"
	"github.com/ahmedhesham6/sshai/libs/capsule/oci"
	"github.com/ahmedhesham6/sshai/libs/contracts"
	dbstore "github.com/ahmedhesham6/sshai/libs/db"
	"github.com/ahmedhesham6/sshai/libs/domain"
)

// TestCapsuleResolverAdapterRoundTripsFields publishes a real Capsule through
// an in-memory GrantProvider and confirms capsuleResolverAdapter copies every
// oci.CapsuleResolution field into workflows.CapsuleResolution unchanged.
func TestCapsuleResolverAdapterRoundTripsFields(t *testing.T) {
	ctx := context.Background()
	grants := newMemoryGrantProvider()
	client, err := oci.NewClient("owner-1", grants)
	if err != nil {
		t.Fatalf("NewClient() error = %v", err)
	}
	value := buildSingleComponentCapsule(t, "config:editor", "editor content\n")
	if _, err := client.Publish(ctx, value); err != nil {
		t.Fatalf("Publish() error = %v", err)
	}

	adapter := newCapsuleResolverAdapter(oci.NewResolver(grants))
	ref := domain.CapsuleRef{Ref: contracts.FormatOwnedCapsuleRef("owner-1", value.Digest), FreshnessPolicy: domain.FreshnessPin}
	resolution, err := adapter.Resolve(ctx, "owner-1", ref)
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}
	if resolution.OwnerID != "owner-1" {
		t.Fatalf("resolution.OwnerID = %q, want owner-1", resolution.OwnerID)
	}
	if resolution.Digest != value.Digest {
		t.Fatalf("resolution.Digest = %q, want %q", resolution.Digest, value.Digest)
	}
	if len(resolution.Components) != 1 || resolution.Components[0].ID != "config:editor" {
		t.Fatalf("resolution.Components = %#v, want one config:editor Component", resolution.Components)
	}
	if resolution.DiffSinceLastApproval != "" {
		t.Fatalf("resolution.DiffSinceLastApproval = %q, want empty", resolution.DiffSinceLastApproval)
	}
}

func TestCapsuleResolverAdapterRejectsUnconfiguredAdapter(t *testing.T) {
	adapter := &capsuleResolverAdapter{}
	if _, err := adapter.Resolve(context.Background(), "owner-1", domain.CapsuleRef{Ref: "owner/owner-1/capsule@sha256:" + repeatHex('a')}); err == nil {
		t.Fatal("Resolve() error = nil, want configuration error for a nil resolver")
	}
}

func TestAutoStopSnapshotSourceExposesSingleNonBlockingReads(t *testing.T) {
	now := time.Date(2026, time.July, 19, 15, 0, 0, 0, time.UTC)
	policy := domain.AutoStopPolicySnapshot{
		ID: "policy-1", EnvironmentID: "environment-1", Mode: domain.AutoStopWhenFullyIdle, GracePeriodSeconds: 60,
	}
	state := dbstore.AutoStopSnapshotState{
		RuntimeID: "runtime-1", Policy: policy, PolicyGeneration: 1,
	}
	snapshot := &domain.AutoStopActivitySnapshot{RuntimeID: "runtime-1", Sequence: 7, ObservedAt: now.Add(-time.Minute)}
	store := &autoStopSnapshotStoreFake{state: state, snapshot: snapshot}
	source := newAutoStopSnapshotSource(store)
	request := workflows.AutoStopRefreshRequest{
		EnvironmentID: "environment-1", RuntimeID: "runtime-1", AfterSnapshotSequence: 7, FreshAfter: now,
	}
	observation, err := source.ReadAutoStopState(t.Context(), request)
	if err != nil {
		t.Fatal(err)
	}
	latest, err := source.ReadLatestSnapshot(t.Context(), request)
	if err != nil {
		t.Fatal(err)
	}
	if observation.Policy != policy || observation.Snapshot != nil || latest != snapshot {
		t.Fatalf("state/Snapshot = %#v/%#v", observation, latest)
	}
	if store.stateCalls != 1 || store.snapshotCalls != 1 {
		t.Fatalf("store calls = state:%d Snapshot:%d, want one each", store.stateCalls, store.snapshotCalls)
	}
}

func TestGuestControlTransportKeepsPermanentFailureWhenUnconfigured(t *testing.T) {
	transport, err := newRuntimeGuestTransport(guestControlConfig{})
	if err != nil {
		t.Fatalf("construct unconfigured guest transport: %v", err)
	}
	_, err = transport.WaitForRuntimeReady(t.Context(), workflows.RuntimeGuestReadinessRequest{})
	if err == nil {
		t.Fatal("unconfigured guest transport unexpectedly succeeded")
	}
	var classified interface{ Transient() bool }
	if !errors.As(err, &classified) || classified.Transient() {
		t.Fatalf("unconfigured error = %T %v, want permanent classified error", err, err)
	}
}

func TestGuestControlTransportRejectsPartialConfiguration(t *testing.T) {
	_, err := newRuntimeGuestTransport(guestControlConfig{endpoint: "https://10.0.0.8:9443"})
	if err == nil || !strings.Contains(err.Error(), "all required") {
		t.Fatalf("partial guest transport error = %v", err)
	}
}

type autoStopSnapshotStoreFake struct {
	state         dbstore.AutoStopSnapshotState
	snapshot      *domain.AutoStopActivitySnapshot
	stateCalls    int
	snapshotCalls int
}

func (fake *autoStopSnapshotStoreFake) LoadAutoStopSnapshotState(context.Context, string, string) (dbstore.AutoStopSnapshotState, error) {
	fake.stateCalls++
	return fake.state, nil
}

func (fake *autoStopSnapshotStoreFake) LatestActivitySnapshot(context.Context, string, string) (*domain.AutoStopActivitySnapshot, error) {
	fake.snapshotCalls++
	return fake.snapshot, nil
}

// TestPinnedProfileVersionResolverResolvesPinnedVersionIntoALock exercises the
// happy path of pinnedProfileVersionResolver: the pin query locates the
// Environment and pinned Profile Version, LoadProfileVersion supplies Capsule
// Refs, the Capsule resolver resolves each one, and the result becomes a
// valid Capsule Lock.
func TestPinnedProfileVersionResolverResolvesPinnedVersionIntoALock(t *testing.T) {
	at := time.Date(2026, time.July, 17, 12, 0, 0, 0, time.UTC)
	digest := "sha256:" + repeatHex('a')
	ref := domain.CapsuleRef{Ref: "owner/user-1/capsule@" + digest, FreshnessPolicy: domain.FreshnessPin}
	resolver := newPinnedProfileVersionResolver(
		pinLoaderFake{pin: dbstore.EnvironmentCreatePin{OwnerUserID: "user-1", EnvironmentID: "environment-1", PinnedProfileVersionID: "version-1"}},
		versionLoaderFake{version: domain.ProfileVersionData{ID: "version-1", CapsuleRefs: []domain.CapsuleRef{ref}}},
		capsuleResolverStub{expectedOwnerID: "user-1", resolutions: map[string]workflows.CapsuleResolution{
			ref.Ref: {OwnerID: "user-1", Digest: digest, Components: []domain.Component{
				lockableComponent("config:editor", "sha256:"+repeatHex('c')),
			}},
		}},
		fixedIDGenerator{id: "lock-1"},
	)

	state, err := resolver.ResolvePinnedProfileVersion(context.Background(), "operation-1", at)
	if err != nil {
		t.Fatalf("ResolvePinnedProfileVersion() error = %v", err)
	}
	if state.CapsuleLock.ID != "lock-1" || state.CapsuleLock.EnvironmentID != "environment-1" || state.CapsuleLock.ProfileVersionID != "version-1" {
		t.Fatalf("resolved Capsule Lock = %#v", state.CapsuleLock)
	}
	if len(state.CapsuleLock.Capsules) != 1 || state.CapsuleLock.Capsules[0].Digest != digest {
		t.Fatalf("resolved locked Capsules = %#v, want one entry with digest %q", state.CapsuleLock.Capsules, digest)
	}
	component, ok := state.CapsuleLock.ResolvedComponents["config:editor"]
	if !ok || component.CapsuleDigest != digest {
		t.Fatalf("resolved Components = %#v", state.CapsuleLock.ResolvedComponents)
	}
}

// TestPinnedProfileVersionResolverRejectsComponentConflicts pins two Capsule
// Refs whose resolutions both contribute the same Component ID with different
// digests. The ratified composition rules classify that as a hard conflict:
// environment creation must fail with domain.ErrComponentConflict, never
// produce a silently merged (last-write-wins) lock.
func TestPinnedProfileVersionResolverRejectsComponentConflicts(t *testing.T) {
	firstRef := domain.CapsuleRef{Ref: "owner/user-1/capsule@sha256:" + repeatHex('a'), FreshnessPolicy: domain.FreshnessPin}
	secondRef := domain.CapsuleRef{Ref: "owner/user-1/capsule@sha256:" + repeatHex('b'), FreshnessPolicy: domain.FreshnessPin}
	resolver := newPinnedProfileVersionResolver(
		pinLoaderFake{pin: dbstore.EnvironmentCreatePin{OwnerUserID: "user-1", EnvironmentID: "environment-1", PinnedProfileVersionID: "version-1"}},
		versionLoaderFake{version: domain.ProfileVersionData{ID: "version-1", CapsuleRefs: []domain.CapsuleRef{firstRef, secondRef}}},
		capsuleResolverStub{expectedOwnerID: "user-1", resolutions: map[string]workflows.CapsuleResolution{
			firstRef.Ref: {OwnerID: "user-1", Digest: "sha256:" + repeatHex('a'), Components: []domain.Component{
				lockableComponent("skill:review", "sha256:"+repeatHex('c')),
			}},
			secondRef.Ref: {OwnerID: "user-1", Digest: "sha256:" + repeatHex('b'), Components: []domain.Component{
				lockableComponent("skill:review", "sha256:"+repeatHex('d')),
			}},
		}},
		fixedIDGenerator{id: "lock-1"},
	)

	_, err := resolver.ResolvePinnedProfileVersion(context.Background(), "operation-1", time.Now())
	if err == nil {
		t.Fatal("ResolvePinnedProfileVersion() error = nil, want a Component conflict error")
	}
	if !errors.Is(err, domain.ErrComponentConflict) {
		t.Fatalf("ResolvePinnedProfileVersion() error = %v, want domain.ErrComponentConflict", err)
	}
}

// TestPinnedProfileVersionResolverHonorsCapsuleRefExclusions pins a Capsule
// Ref that excludes one of its two Components and confirms the excluded
// Component never reaches the persisted lock.
func TestPinnedProfileVersionResolverHonorsCapsuleRefExclusions(t *testing.T) {
	at := time.Date(2026, time.July, 17, 12, 0, 0, 0, time.UTC)
	digest := "sha256:" + repeatHex('a')
	ref := domain.CapsuleRef{
		Ref: "owner/user-1/capsule@" + digest, FreshnessPolicy: domain.FreshnessPin,
		Exclusions: []string{"config:editor"},
	}
	resolver := newPinnedProfileVersionResolver(
		pinLoaderFake{pin: dbstore.EnvironmentCreatePin{OwnerUserID: "user-1", EnvironmentID: "environment-1", PinnedProfileVersionID: "version-1"}},
		versionLoaderFake{version: domain.ProfileVersionData{ID: "version-1", CapsuleRefs: []domain.CapsuleRef{ref}}},
		capsuleResolverStub{expectedOwnerID: "user-1", resolutions: map[string]workflows.CapsuleResolution{
			ref.Ref: {OwnerID: "user-1", Digest: digest, Components: []domain.Component{
				lockableComponent("config:editor", "sha256:"+repeatHex('c')),
				lockableComponent("skill:review", "sha256:"+repeatHex('d')),
			}},
		}},
		fixedIDGenerator{id: "lock-1"},
	)

	state, err := resolver.ResolvePinnedProfileVersion(context.Background(), "operation-1", at)
	if err != nil {
		t.Fatalf("ResolvePinnedProfileVersion() error = %v", err)
	}
	if _, present := state.CapsuleLock.ResolvedComponents["config:editor"]; present {
		t.Fatalf("resolved Components = %#v, want excluded config:editor to be absent", state.CapsuleLock.ResolvedComponents)
	}
	if _, present := state.CapsuleLock.ResolvedComponents["skill:review"]; !present {
		t.Fatalf("resolved Components = %#v, want skill:review to be locked", state.CapsuleLock.ResolvedComponents)
	}
}

// TestPinnedProfileVersionResolverRejectsRefWithoutEmbeddedDigest pins a
// Capsule Ref in the moving tag form. A pinned Profile Version locks exact
// content, so the ref must be rejected by name before any store lookup —
// even if the Capsule store later grows a tag index.
func TestPinnedProfileVersionResolverRejectsRefWithoutEmbeddedDigest(t *testing.T) {
	ref := domain.CapsuleRef{Ref: "owner/user-1/capsule:stable", FreshnessPolicy: domain.FreshnessPin}
	resolver := newPinnedProfileVersionResolver(
		pinLoaderFake{pin: dbstore.EnvironmentCreatePin{OwnerUserID: "user-1", EnvironmentID: "environment-1", PinnedProfileVersionID: "version-1"}},
		versionLoaderFake{version: domain.ProfileVersionData{ID: "version-1", CapsuleRefs: []domain.CapsuleRef{ref}}},
		capsuleResolverStub{expectedOwnerID: "user-1"},
		fixedIDGenerator{id: "lock-1"},
	)

	_, err := resolver.ResolvePinnedProfileVersion(context.Background(), "operation-1", time.Now())
	if err == nil {
		t.Fatal("ResolvePinnedProfileVersion() error = nil, want a tag-form ref to be rejected")
	}
	if !strings.Contains(err.Error(), ref.Ref) {
		t.Fatalf("ResolvePinnedProfileVersion() error = %v, want the offending ref %q to be named", err, ref.Ref)
	}
}

// TestPinnedProfileVersionResolverRejectsMismatchedResolvedDigest confirms
// the post-resolution check: if the resolver answers with a digest other
// than the one the pinned ref embeds, the resolution fails rather than
// locking content the Profile Version never pinned.
func TestPinnedProfileVersionResolverRejectsMismatchedResolvedDigest(t *testing.T) {
	pinnedDigest := "sha256:" + repeatHex('a')
	ref := domain.CapsuleRef{Ref: "owner/user-1/capsule@" + pinnedDigest, FreshnessPolicy: domain.FreshnessPin}
	resolver := newPinnedProfileVersionResolver(
		pinLoaderFake{pin: dbstore.EnvironmentCreatePin{OwnerUserID: "user-1", EnvironmentID: "environment-1", PinnedProfileVersionID: "version-1"}},
		versionLoaderFake{version: domain.ProfileVersionData{ID: "version-1", CapsuleRefs: []domain.CapsuleRef{ref}}},
		capsuleResolverStub{expectedOwnerID: "user-1", resolutions: map[string]workflows.CapsuleResolution{
			ref.Ref: {OwnerID: "user-1", Digest: "sha256:" + repeatHex('b'), Components: []domain.Component{
				lockableComponent("config:editor", "sha256:"+repeatHex('c')),
			}},
		}},
		fixedIDGenerator{id: "lock-1"},
	)

	_, err := resolver.ResolvePinnedProfileVersion(context.Background(), "operation-1", time.Now())
	if err == nil {
		t.Fatal("ResolvePinnedProfileVersion() error = nil, want a mismatched resolved digest to be rejected")
	}
	if !strings.Contains(err.Error(), pinnedDigest) {
		t.Fatalf("ResolvePinnedProfileVersion() error = %v, want the pinned digest %q to be named", err, pinnedDigest)
	}
}

func TestPinnedProfileVersionResolverPropagatesRefResolutionFailure(t *testing.T) {
	ref := domain.CapsuleRef{Ref: "owner/user-1/capsule@sha256:" + repeatHex('b'), FreshnessPolicy: domain.FreshnessPin}
	resolver := newPinnedProfileVersionResolver(
		pinLoaderFake{pin: dbstore.EnvironmentCreatePin{OwnerUserID: "user-1", EnvironmentID: "environment-1", PinnedProfileVersionID: "version-1"}},
		versionLoaderFake{version: domain.ProfileVersionData{ID: "version-1", CapsuleRefs: []domain.CapsuleRef{ref}}},
		capsuleResolverStub{expectedOwnerID: "user-1", err: errors.New("tag resolution is not supported")},
		fixedIDGenerator{id: "lock-1"},
	)

	if _, err := resolver.ResolvePinnedProfileVersion(context.Background(), "operation-1", time.Now()); err == nil {
		t.Fatal("ResolvePinnedProfileVersion() error = nil, want the ref resolution failure to propagate")
	}
}

func TestPinnedProfileVersionResolverRejectsZeroRefPinnedVersion(t *testing.T) {
	resolver := newPinnedProfileVersionResolver(
		pinLoaderFake{pin: dbstore.EnvironmentCreatePin{OwnerUserID: "user-1", EnvironmentID: "environment-1", PinnedProfileVersionID: "version-1"}},
		versionLoaderFake{version: domain.ProfileVersionData{ID: "version-1"}},
		capsuleResolverStub{expectedOwnerID: "user-1"},
		fixedIDGenerator{id: "lock-1"},
	)

	if _, err := resolver.ResolvePinnedProfileVersion(context.Background(), "operation-1", time.Now()); err == nil {
		t.Fatal("ResolvePinnedProfileVersion() error = nil, want a zero-ref Profile Version to be rejected")
	}
}

func TestPinnedProfileVersionResolverPropagatesPinLookupFailure(t *testing.T) {
	resolver := newPinnedProfileVersionResolver(
		pinLoaderFake{err: dbstore.ErrReferenceNotOwned},
		versionLoaderFake{},
		capsuleResolverStub{expectedOwnerID: "user-1"},
		fixedIDGenerator{id: "lock-1"},
	)

	if _, err := resolver.ResolvePinnedProfileVersion(context.Background(), "operation-missing", time.Now()); !errors.Is(err, dbstore.ErrReferenceNotOwned) {
		t.Fatalf("ResolvePinnedProfileVersion() error = %v, want ErrReferenceNotOwned", err)
	}
}

type pinLoaderFake struct {
	pin dbstore.EnvironmentCreatePin
	err error
}

func (fake pinLoaderFake) LoadEnvironmentCreatePin(context.Context, string) (dbstore.EnvironmentCreatePin, error) {
	return fake.pin, fake.err
}

type versionLoaderFake struct {
	version domain.ProfileVersionData
	err     error
}

func (fake versionLoaderFake) LoadProfileVersion(context.Context, string, string) (domain.ProfileVersionData, error) {
	return fake.version, fake.err
}

type capsuleResolverStub struct {
	expectedOwnerID string
	resolutions     map[string]workflows.CapsuleResolution
	err             error
}

func (fake capsuleResolverStub) Resolve(_ context.Context, ownerID string, ref domain.CapsuleRef) (workflows.CapsuleResolution, error) {
	if ownerID != fake.expectedOwnerID {
		return workflows.CapsuleResolution{}, fmt.Errorf("Resolve() ownerID = %q, want %q", ownerID, fake.expectedOwnerID)
	}
	if fake.err != nil {
		return workflows.CapsuleResolution{}, fake.err
	}
	return fake.resolutions[ref.Ref], nil
}

type fixedIDGenerator struct{ id string }

func (fake fixedIDGenerator) NewID() string { return fake.id }

// lockableComponent builds a Component that passes
// domain.ResolveCapsuleComposition's per-Component validation (type-qualified
// ID, media type, SHA-256 digest, valid scope and trust class).
func lockableComponent(id, digest string) domain.Component {
	componentType := domain.ComponentType(strings.SplitN(id, ":", 2)[0])
	return domain.Component{
		ID: id, Type: componentType, MediaType: "application/vnd.sshai.component.content.v1.tar",
		Digest: digest, Scope: domain.ScopeUser, TrustClass: domain.TrustDeclarative,
	}
}

func repeatHex(character byte) string {
	value := make([]byte, 64)
	for index := range value {
		value[index] = character
	}
	return string(value)
}

// memoryGrantProvider is an in-memory oci.GrantProvider, letting adapter
// tests exercise a real *oci.Client/*oci.Resolver publish-then-resolve round
// trip without Docker or MinIO.
type memoryGrantProvider struct {
	mu      sync.Mutex
	objects map[string][]byte
}

func newMemoryGrantProvider() *memoryGrantProvider {
	return &memoryGrantProvider{objects: make(map[string][]byte)}
}

func (provider *memoryGrantProvider) Grant(_ context.Context, request oci.GrantRequest) (oci.Grant, error) {
	return oci.Grant{
		Read: func(context.Context) (io.ReadCloser, error) {
			provider.mu.Lock()
			data, ok := provider.objects[request.Key]
			provider.mu.Unlock()
			if !ok {
				return nil, errors.New("object not found: " + request.Key)
			}
			return io.NopCloser(bytes.NewReader(data)), nil
		},
		Write: func(_ context.Context, reader io.Reader, _ int64) error {
			data, err := io.ReadAll(reader)
			if err != nil {
				return err
			}
			provider.mu.Lock()
			provider.objects[request.Key] = data
			provider.mu.Unlock()
			return nil
		},
	}, nil
}

func buildSingleComponentCapsule(t *testing.T, componentID, content string) capsule.Capsule {
	t.Helper()
	root := t.TempDir()
	manifest := capsule.Manifest{
		SchemaVersion: capsule.SchemaVersion,
		Name:          "test-capsule",
		Requirements:  capsule.Requirements{Commands: []string{}, Secrets: []string{}},
		Components: []capsule.Component{{
			ID: componentID, Type: capsule.ComponentTypeConfig, Scope: capsule.ScopeProject, TrustClass: capsule.TrustDeclarative,
			Requirements: capsule.Requirements{Commands: []string{}, Secrets: []string{}},
		}},
	}
	componentRoot := filepath.Join(root, "component")
	if err := os.MkdirAll(componentRoot, 0o755); err != nil {
		t.Fatalf("create component root: %v", err)
	}
	if err := os.WriteFile(filepath.Join(componentRoot, "content.txt"), []byte(content), 0o644); err != nil {
		t.Fatalf("write component content: %v", err)
	}
	built, err := capsule.NewBuilder(1700000000).Build(manifest, map[string]string{componentID: componentRoot})
	if err != nil {
		t.Fatalf("build test Capsule: %v", err)
	}
	return built
}
