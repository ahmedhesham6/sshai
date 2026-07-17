package main

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/ahmedhesham6/sshai/apps/workflows"
	"github.com/ahmedhesham6/sshai/libs/capsule/oci"
	"github.com/ahmedhesham6/sshai/libs/db"
	"github.com/ahmedhesham6/sshai/libs/domain"
	"github.com/google/uuid"
)

// idGenerator is the production workflows.IDGenerator: random UUIDs, the same
// generation strategy apps/control-plane/cmd/control-plane/main.go uses for
// its own IDGenerator seam.
type idGenerator struct{}

func (idGenerator) NewID() string { return uuid.NewString() }

// capsuleResolverAdapter wraps a *oci.Resolver as a workflows.CapsuleResolver.
// The two packages define field-for-field identical CapsuleResolution types
// (see oci.Resolver's doc comment for why they cannot share one definition:
// libs packages must not import apps packages), so the adapter only needs to
// copy fields across the boundary.
type capsuleResolverAdapter struct {
	resolver *oci.Resolver
}

// newCapsuleResolverAdapter adapts resolver to workflows.CapsuleResolver.
func newCapsuleResolverAdapter(resolver *oci.Resolver) *capsuleResolverAdapter {
	return &capsuleResolverAdapter{resolver: resolver}
}

func (adapter *capsuleResolverAdapter) Resolve(ctx context.Context, ownerID string, ref domain.CapsuleRef) (workflows.CapsuleResolution, error) {
	if adapter == nil || adapter.resolver == nil {
		return workflows.CapsuleResolution{}, errors.New("resolve Capsule Ref: Capsule resolver adapter is not configured")
	}
	resolution, err := adapter.resolver.Resolve(ctx, ownerID, ref)
	if err != nil {
		return workflows.CapsuleResolution{}, err
	}
	return workflows.CapsuleResolution{
		OwnerID: resolution.OwnerID, Digest: resolution.Digest,
		Components: resolution.Components, DiffSinceLastApproval: resolution.DiffSinceLastApproval,
	}, nil
}

var _ workflows.CapsuleResolver = (*capsuleResolverAdapter)(nil)

// profileResolveStateStore bridges *db.Store's LoadProfileResolveState,
// which returns db.ProfileResolveState, to the workflows package's
// unexported profileResolveStateRepository seam, which requires
// workflows.ProfileResolveState. The two types mirror each other field by
// field (see libs/db/profile_resolve.go's ProfileResolveState doc comment)
// but are distinct named types, so db.Store does not itself satisfy that
// interface — only this small conversion wrapper does. db.Store already
// satisfies workflows.ProfileResolveRepository directly, so embedding it
// here carries every other method (RecordProfileResolveInvocation,
// LoadProfileVersion, PersistCapsuleLock, CompleteProfileResolve) unchanged.
type profileResolveStateStore struct {
	*db.Store
}

func (bridge *profileResolveStateStore) LoadProfileResolveState(ctx context.Context, environmentID string) (workflows.ProfileResolveState, error) {
	state, err := bridge.Store.LoadProfileResolveState(ctx, environmentID)
	if err != nil {
		return workflows.ProfileResolveState{}, err
	}
	return workflows.ProfileResolveState{
		ManagedTargets:             state.ManagedTargets,
		LastApprovedCapsuleDigests: state.LastApprovedCapsuleDigests,
		PersistedUpgradePolicy:     state.PersistedUpgradePolicy,
	}, nil
}

// emptyTreeCapsuleDigest is the canonical sha256 digest of an empty byte
// string. It is the same placeholder apps/workflows.profileResolveWorkflow's
// resolveCapsules uses for an Environment with no reviewed project Capsule
// yet — Environment creation never has one either, since a project Capsule
// only exists once a guest has produced project-scoped Components.
const emptyTreeCapsuleDigest = "sha256:e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"

// environmentCreatePinLoader is the read-only seam pinnedProfileVersionResolver
// uses to map an EnvironmentCreate Operation ID to the Environment and pinned
// Profile Version it targets. *db.Store satisfies it via
// LoadEnvironmentCreatePin.
type environmentCreatePinLoader interface {
	LoadEnvironmentCreatePin(context.Context, string) (db.EnvironmentCreatePin, error)
}

// profileVersionLoader is the read-only seam pinnedProfileVersionResolver
// uses to load a Profile Version's Capsule Refs. *db.Store satisfies it via
// LoadProfileVersion (shared with workflows.ProfileResolveRepository).
type profileVersionLoader interface {
	LoadProfileVersion(context.Context, string, string) (domain.ProfileVersionData, error)
}

// pinnedProfileVersionResolver is the production
// workflows.PinnedProfileVersionResolver: it maps an EnvironmentCreate
// Operation ID to its Environment and pinned Profile Version (the read-only
// pin query), loads that Profile Version's Capsule Refs, resolves each ref
// against the Capsule store, composes the resolved Components through
// domain.ResolveCapsuleComposition — the same ratified composition rules
// profileResolveWorkflow.resolveCapsules applies (per-ref Exclusions,
// identical-contribution dedupe, hard conflict errors, ordered config merge,
// permission Components never merged) — and locks the result with
// domain.CreateCapsuleLock. No Capsule Lock exists yet during Environment
// creation, so every locked Capsule and resolved Component here is newly
// minted from the pinned Profile Version — never fabricated: a Profile
// Version with no Capsule Refs, any Capsule Ref that fails to resolve
// (including the unsupported-tag case oci.Resolver reports), and any
// Component composition conflict are all propagated as errors rather than
// producing an empty, partial, or silently merged lock.
type pinnedProfileVersionResolver struct {
	pins     environmentCreatePinLoader
	versions profileVersionLoader
	resolver workflows.CapsuleResolver
	ids      workflows.IDGenerator
}

// newPinnedProfileVersionResolver creates the production pinned Profile
// Version resolver from its three seams.
func newPinnedProfileVersionResolver(pins environmentCreatePinLoader, versions profileVersionLoader, resolver workflows.CapsuleResolver, ids workflows.IDGenerator) *pinnedProfileVersionResolver {
	return &pinnedProfileVersionResolver{pins: pins, versions: versions, resolver: resolver, ids: ids}
}

func (resolver *pinnedProfileVersionResolver) ResolvePinnedProfileVersion(ctx context.Context, operationID string, at time.Time) (workflows.EnvironmentCapsuleState, error) {
	if resolver == nil || resolver.pins == nil || resolver.versions == nil || resolver.resolver == nil || resolver.ids == nil {
		return workflows.EnvironmentCapsuleState{}, errors.New("resolve pinned Profile Version: resolver is not configured")
	}
	pin, err := resolver.pins.LoadEnvironmentCreatePin(ctx, operationID)
	if err != nil {
		return workflows.EnvironmentCapsuleState{}, fmt.Errorf("resolve pinned Profile Version: load Environment create pin: %w", err)
	}
	version, err := resolver.versions.LoadProfileVersion(ctx, pin.EnvironmentID, pin.PinnedProfileVersionID)
	if err != nil {
		return workflows.EnvironmentCapsuleState{}, fmt.Errorf("resolve pinned Profile Version: load Profile Version: %w", err)
	}
	if len(version.CapsuleRefs) == 0 {
		return workflows.EnvironmentCapsuleState{}, fmt.Errorf("resolve pinned Profile Version: Profile Version %q has no Capsule Refs to lock", pin.PinnedProfileVersionID)
	}
	locked := make([]domain.LockedCapsule, 0, len(version.CapsuleRefs))
	sources := make([]domain.CapsuleComponentSet, 0, len(version.CapsuleRefs))
	for _, ref := range version.CapsuleRefs {
		// A pinned Profile Version locks exact content. Mirroring
		// profileResolveWorkflow.resolveCapsules' pin-freshness pre-check and
		// exactOrResolvedDigest post-check (unexported in package workflows,
		// replicated here): every ref must embed an exact digest, and the
		// resolver's answer must match it — so this path can never silently
		// start accepting moving tag refs if the store grows a tag index.
		exactDigest, err := embeddedExactDigest(ref.Ref)
		if err != nil {
			return workflows.EnvironmentCapsuleState{}, fmt.Errorf("resolve pinned Profile Version: %w", err)
		}
		resolution, err := resolver.resolver.Resolve(ctx, pin.OwnerUserID, ref)
		if err != nil {
			return workflows.EnvironmentCapsuleState{}, fmt.Errorf("resolve pinned Profile Version: resolve Capsule Ref %q: %w", ref.Ref, err)
		}
		if resolution.Digest != "" && resolution.Digest != exactDigest {
			return workflows.EnvironmentCapsuleState{}, fmt.Errorf("resolve pinned Profile Version: Capsule Ref %q resolved to %q, want pinned digest %q", ref.Ref, resolution.Digest, exactDigest)
		}
		locked = append(locked, domain.LockedCapsule{Ref: ref.Ref, Digest: exactDigest})
		sources = append(sources, domain.CapsuleComponentSet{
			Ref: ref.Ref, Digest: exactDigest, Exclusions: ref.Exclusions, Components: resolution.Components,
		})
	}
	// No project Capsule exists at Environment creation time, so composition
	// runs with a nil project source — matching resolveCapsules when
	// ProjectCapsuleDigest is absent.
	composition, err := domain.ResolveCapsuleComposition(sources, nil)
	if err != nil {
		return workflows.EnvironmentCapsuleState{}, fmt.Errorf("resolve pinned Profile Version: resolve Components: %w", err)
	}
	components := make(map[string]domain.ResolvedComponent, len(composition.Components))
	for id, component := range composition.Components {
		capsuleDigest := composition.ComponentCapsuleDigests[id]
		if capsuleDigest == "" {
			capsuleDigest = emptyTreeCapsuleDigest
		}
		components[id] = domain.ResolvedComponent{
			ID: id, Type: component.Type, CapsuleDigest: capsuleDigest, ComponentDigest: component.Digest,
			Scope: component.Scope, TrustClass: component.TrustClass, Requirements: component.Requirements,
			Provenance: component.Provenance, Sources: composition.ComponentSources[id],
		}
	}
	lock, err := domain.CreateCapsuleLock(domain.CapsuleLockSnapshot{
		ID: resolver.ids.NewID(), EnvironmentID: pin.EnvironmentID, ProfileVersionID: pin.PinnedProfileVersionID,
		ProjectCapsuleDigest: emptyTreeCapsuleDigest, Capsules: locked, ResolvedComponents: components, CreatedAt: at.UTC(),
	})
	if err != nil {
		return workflows.EnvironmentCapsuleState{}, fmt.Errorf("resolve pinned Profile Version: %w", err)
	}
	return workflows.EnvironmentCapsuleState{CapsuleLock: lock.Snapshot()}, nil
}

// embeddedExactDigest extracts the exact digest a pinned Capsule Ref embeds
// (owner/<owner>/capsule@sha256:<digest>). A ref without one — a moving tag
// form — cannot participate in a pinned Profile Version resolution.
func embeddedExactDigest(ref string) (string, error) {
	at := strings.LastIndex(ref, "@sha256:")
	if at < 0 {
		return "", fmt.Errorf("Capsule Ref %q is pinned but does not embed an exact digest", ref)
	}
	return ref[at+1:], nil
}

var _ workflows.PinnedProfileVersionResolver = (*pinnedProfileVersionResolver)(nil)
