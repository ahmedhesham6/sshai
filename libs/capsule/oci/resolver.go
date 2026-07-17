package oci

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/ahmedhesham6/sshai/libs/capsule"
	"github.com/ahmedhesham6/sshai/libs/contracts"
	"github.com/ahmedhesham6/sshai/libs/domain"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

// CapsuleResolution is the content-addressed result of resolving one owned
// Capsule Ref against the S3-backed OCI store.
//
// This type mirrors apps/workflows.CapsuleResolution's exported fields
// (OwnerID, Digest, Components, DiffSinceLastApproval) exactly. It is defined
// separately here, rather than imported, because libs packages must not
// import apps packages (that would be a layering violation and, since
// apps/workflows already imports libs/domain and would need to import
// libs/capsule/oci for wiring, an import cycle). Keep the two struct
// definitions in lockstep: if either changes, update the other and the small
// field-by-field adapter that converts between them at the wiring site (see
// Resolver's doc comment).
type CapsuleResolution struct {
	// OwnerID is the store namespace verified by Resolve. It is always the
	// caller-supplied ownerID: the Client backing Resolve is constructed with
	// that owner's prefix, so the resolution can never cross owners.
	OwnerID string
	// Digest is the resolved Capsule content digest.
	Digest string
	// Components lists the resolved Capsule's manifest Components.
	Components []domain.Component
	// DiffSinceLastApproval is left empty; Resolve only performs
	// content-addressed digest resolution and does not itself track
	// approval history. Callers that need a freshness diff compute it from
	// the digest change, as apps/workflows.resolveCapsules already does.
	DiffSinceLastApproval string
}

// Resolver resolves owned Capsule Refs against the S3-backed, content
// addressed Capsule OCI store described by ADR 0009 and spec 19. It matches
// the exact method signature of apps/workflows.CapsuleResolver:
//
//	Resolve(context.Context, string, domain.CapsuleRef) (CapsuleResolution, error)
//
// apps/workflows.CapsuleResolver requires apps/workflows.CapsuleResolution as
// its return type, and libs/capsule/oci cannot import apps/workflows (see the
// CapsuleResolution doc comment). Wiring a *Resolver into
// workflows.ProfileResolveDefinition therefore takes a few lines at the call
// site: wrap Resolver.Resolve in a workflows.CapsuleResolverFunc that converts
// this package's CapsuleResolution into workflows.CapsuleResolution
// field-by-field. That wiring, and the compile-time assertion that the
// adapter satisfies workflows.CapsuleResolver, lands with the future slice
// that wires a production CapsuleResolver into apps/workflows/cmd/workflows.
//
// Resolver only resolves exact-digest Capsule Refs
// (owner/<owner>/capsule@sha256:<digest>). The MVP capsule store is
// content-addressed only (ADR 0009, docs/spec/19-capsule-packaging.md): the
// OCI index is keyed by Capsule digest (see IndexKey), and neither the store
// nor the control plane persists a moving tag/name to digest mapping or a
// write path that would populate one. A Capsule Ref that uses the tag form
// (owner/<owner>/capsule:<tag>) — which apps/workflows.domain.FreshnessTrack
// and FreshnessReview both allow — therefore cannot be resolved yet. Resolve
// returns a clear error for that case rather than fabricating tag
// infrastructure; adding a real tag/name index is a product decision for a
// later slice.
type Resolver struct {
	grants  GrantProvider
	options []ClientOption
}

// NewResolver creates a Resolver backed by grants. options configure every
// per-owner Client the Resolver constructs (see WithParallelism).
func NewResolver(grants GrantProvider, options ...ClientOption) *Resolver {
	return &Resolver{grants: grants, options: append([]ClientOption(nil), options...)}
}

// Resolve resolves ref against the owner-scoped Capsule store. It never reads
// outside ownerID's prefix: the underlying Client is constructed fresh for
// ownerID on every call, and every object key that Client reads is prefixed
// by that owner ID (see BlobKey and IndexKey).
func (resolver *Resolver) Resolve(ctx context.Context, ownerID string, ref domain.CapsuleRef) (CapsuleResolution, error) {
	if resolver == nil || resolver.grants == nil {
		return CapsuleResolution{}, errors.New("resolve capsule ref: resolver is not configured")
	}
	parsed, err := contracts.ParseOwnedCapsuleRef(ref.Ref)
	if err != nil {
		return CapsuleResolution{}, fmt.Errorf("resolve capsule ref %q: %w", ref.Ref, err)
	}
	if parsed.OwnerID != ownerID {
		return CapsuleResolution{}, fmt.Errorf("resolve capsule ref %q: ref owner %q does not match requesting owner %q", ref.Ref, parsed.OwnerID, ownerID)
	}
	if parsed.Digest == "" {
		return CapsuleResolution{}, fmt.Errorf("resolve capsule ref %q: tag %q resolution is not supported: the MVP capsule store is content-addressed only and has no tag index (ADR 0009)", ref.Ref, parsed.Tag)
	}
	client, err := NewClient(ownerID, resolver.grants, resolver.options...)
	if err != nil {
		return CapsuleResolution{}, fmt.Errorf("resolve capsule ref %q: %w", ref.Ref, err)
	}
	manifest, err := client.ResolveManifest(ctx, parsed.Digest)
	if err != nil {
		return CapsuleResolution{}, fmt.Errorf("resolve capsule ref %q: %w", ref.Ref, err)
	}
	return CapsuleResolution{
		OwnerID:    ownerID,
		Digest:     parsed.Digest,
		Components: convertComponents(manifest.Components),
	}, nil
}

func convertComponents(components []capsule.Component) []domain.Component {
	if components == nil {
		return nil
	}
	converted := make([]domain.Component, len(components))
	for index, component := range components {
		converted[index] = domain.Component{
			ID:         component.ID,
			Type:       domain.ComponentType(component.Type),
			MediaType:  component.MediaType,
			Digest:     component.Digest,
			SizeBytes:  component.SizeBytes,
			Scope:      domain.ComponentScope(component.Scope),
			TrustClass: domain.TrustClass(component.TrustClass),
			Requirements: domain.ComponentRequirements{
				Commands: append([]string(nil), component.Requirements.Commands...),
				Secrets:  append([]string(nil), component.Requirements.Secrets...),
			},
		}
	}
	return converted
}

// ResolveManifest fetches and verifies the Capsule manifest addressed by
// targetDigest without pulling Component layers. It performs the same index
// lookup, image-manifest fetch, config fetch, and digest verification steps
// as Pull, but stops short of downloading layer blobs: resolution only needs
// the manifest's Component descriptors, not their content, so it stays cheap
// enough to run on every Profile resolve.
func (client *Client) ResolveManifest(ctx context.Context, targetDigest string) (capsule.Manifest, error) {
	if client == nil {
		return capsule.Manifest{}, errors.New("resolve capsule manifest: client is nil")
	}
	target, err := parseSHA256Digest(targetDigest)
	if err != nil {
		return capsule.Manifest{}, fmt.Errorf("resolve capsule manifest: target digest: %w", err)
	}
	indexBytes, err := client.readObject(ctx, IndexKey(client.ownerID, target.String()), maxIndexSize)
	if err != nil {
		return capsule.Manifest{}, fmt.Errorf("resolve capsule manifest: fetch OCI index: %w", err)
	}
	manifestDescriptor, err := findManifestDescriptor(indexBytes, target.String())
	if err != nil {
		return capsule.Manifest{}, fmt.Errorf("resolve capsule manifest: resolve target %s: %w", target, err)
	}
	manifestBytes, err := client.readObjectAsBlob(ctx, BlobKey(client.ownerID, manifestDescriptor.Digest.String()), manifestDescriptor)
	if err != nil {
		return capsule.Manifest{}, fmt.Errorf("resolve capsule manifest: fetch image manifest: %w", err)
	}
	var imageManifest ocispec.Manifest
	if err := json.Unmarshal(manifestBytes, &imageManifest); err != nil {
		return capsule.Manifest{}, fmt.Errorf("resolve capsule manifest: decode image manifest: %w", err)
	}
	if imageManifest.MediaType != ocispec.MediaTypeImageManifest || imageManifest.ArtifactType != capsule.ArtifactMediaType {
		return capsule.Manifest{}, errors.New("resolve capsule manifest: remote object is not a Capsule image manifest")
	}
	if imageManifest.Config.MediaType != ConfigMediaType {
		return capsule.Manifest{}, fmt.Errorf("resolve capsule manifest: config media type %q is invalid", imageManifest.Config.MediaType)
	}
	configBytes, err := client.readObjectAsBlob(ctx, BlobKey(client.ownerID, imageManifest.Config.Digest.String()), imageManifest.Config)
	if err != nil {
		return capsule.Manifest{}, fmt.Errorf("resolve capsule manifest: fetch config: %w", err)
	}
	var manifest capsule.Manifest
	if err := json.Unmarshal(configBytes, &manifest); err != nil {
		return capsule.Manifest{}, fmt.Errorf("resolve capsule manifest: decode config: %w", err)
	}
	computedCapsuleDigest, err := capsule.ComputeCapsuleDigest(manifest)
	if err != nil {
		return capsule.Manifest{}, fmt.Errorf("resolve capsule manifest: compute Capsule digest: %w", err)
	}
	if computedCapsuleDigest != target.String() {
		return capsule.Manifest{}, fmt.Errorf("resolve capsule manifest: manifest %s digest mismatch: resolves Capsule digest %s, want %s", manifestDescriptor.Digest, computedCapsuleDigest, target)
	}
	return manifest, nil
}
