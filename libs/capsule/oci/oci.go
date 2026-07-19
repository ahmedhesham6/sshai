// Package oci assembles and distributes Capsule artifacts using OCI image
// layouts. It intentionally uses an image manifest and an owner-scoped index
// lookup.
package oci

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/ahmedhesham6/sshai/libs/capsule"
	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	oras "oras.land/oras-go/v2"
	"oras.land/oras-go/v2/content"
	orasoci "oras.land/oras-go/v2/content/oci"
	"oras.land/oras-go/v2/errdef"
)

const (
	// ConfigMediaType identifies the canonical Capsule manifest config blob.
	ConfigMediaType = "application/vnd.devm.capsule.manifest.v1+json"

	// AnnotationComponentType identifies the Component type on a layer.
	AnnotationComponentType = "devm.component.type"
	// AnnotationComponentID identifies the stable Component ID on a layer.
	AnnotationComponentID = "devm.component.id"
	// AnnotationComponentScope identifies the Component scope on a layer.
	AnnotationComponentScope = "devm.component.scope"
	// AnnotationComponentTrust identifies the digest-bound Component trust class.
	AnnotationComponentTrust = "devm.component.trust"
)

// Artifact describes the descriptors written by Assemble.
type Artifact struct {
	// ManifestDescriptor identifies the OCI image manifest blob.
	ManifestDescriptor ocispec.Descriptor
	// ConfigDescriptor identifies the canonical Capsule manifest blob.
	ConfigDescriptor ocispec.Descriptor
	// LayerDescriptors are ordered like the Capsule manifest Components.
	LayerDescriptors []ocispec.Descriptor
}

// Assemble writes capsuleValue as an immutable OCI artifact into store. The
// Capsule digest is also stored as an index reference so a caller can resolve
// an artifact by its Capsule digest without a registry tag lookup.
func Assemble(ctx context.Context, store oras.Target, capsuleValue capsule.Capsule) (Artifact, error) {
	if store == nil {
		return Artifact{}, errors.New("assemble capsule: OCI store is required")
	}
	if err := validateCapsuleForAssembly(capsuleValue); err != nil {
		return Artifact{}, err
	}

	// Capsules are immutable and this package never needs deletion, so keep the
	// store's optional garbage collection behavior disabled.
	if layoutStore, ok := store.(*orasoci.Store); ok {
		layoutStore.AutoGC = false
	}
	manifestJSON, err := capsuleValue.Manifest.CanonicalJSON()
	if err != nil {
		return Artifact{}, fmt.Errorf("assemble capsule: canonicalize manifest: %w", err)
	}
	configDescriptor := content.NewDescriptorFromBytes(ConfigMediaType, manifestJSON)
	if err := pushBlob(ctx, store, configDescriptor, manifestJSON); err != nil {
		return Artifact{}, fmt.Errorf("assemble capsule: push config: %w", err)
	}

	layerDescriptors := make([]ocispec.Descriptor, len(capsuleValue.Layers))
	for index, layer := range capsuleValue.Layers {
		component := capsuleValue.Manifest.Components[index]
		layerDigest, err := digest.Parse(layer.Digest)
		if err != nil || layerDigest.Algorithm() != digest.SHA256 {
			return Artifact{}, fmt.Errorf("assemble capsule: component %q has invalid layer digest %q", component.ID, layer.Digest)
		}
		descriptor := ocispec.Descriptor{
			MediaType: layer.MediaType,
			Digest:    layerDigest,
			Size:      int64(len(layer.Bytes)),
			Annotations: map[string]string{
				AnnotationComponentType:  string(component.Type),
				AnnotationComponentID:    component.ID,
				AnnotationComponentScope: string(component.Scope),
				AnnotationComponentTrust: string(component.TrustClass),
			},
		}
		if err := pushBlob(ctx, store, descriptor, layer.Bytes); err != nil {
			return Artifact{}, fmt.Errorf("assemble capsule: push component %q: %w", component.ID, err)
		}
		layerDescriptors[index] = descriptor
	}

	manifest := ocispec.Manifest{
		Versioned:    ocispec.Manifest{}.Versioned,
		MediaType:    ocispec.MediaTypeImageManifest,
		ArtifactType: capsule.ArtifactMediaType,
		Config:       configDescriptor,
		Layers:       layerDescriptors,
	}
	manifest.SchemaVersion = 2
	manifestJSONBytes, err := json.Marshal(manifest)
	if err != nil {
		return Artifact{}, fmt.Errorf("assemble capsule: encode OCI manifest: %w", err)
	}
	manifestDescriptor := content.NewDescriptorFromBytes(ocispec.MediaTypeImageManifest, manifestJSONBytes)
	manifestDescriptor.ArtifactType = capsule.ArtifactMediaType
	if err := pushBlob(ctx, store, manifestDescriptor, manifestJSONBytes); err != nil {
		return Artifact{}, fmt.Errorf("assemble capsule: push OCI manifest: %w", err)
	}
	if err := store.Tag(ctx, manifestDescriptor, capsuleValue.Digest); err != nil {
		return Artifact{}, fmt.Errorf("assemble capsule: index by capsule digest: %w", err)
	}

	return Artifact{
		ManifestDescriptor: manifestDescriptor,
		ConfigDescriptor:   configDescriptor,
		LayerDescriptors:   layerDescriptors,
	}, nil
}

// Parse resolves reference from store and reconstructs the Capsule manifest
// and every layer. The config, image manifest, and layers are all checked
// against their declared OCI digests before they are returned.
func Parse(ctx context.Context, store oras.ReadOnlyTarget, reference string) (capsule.Capsule, error) {
	if store == nil {
		return capsule.Capsule{}, errors.New("parse capsule: OCI store is required")
	}
	if reference == "" {
		return capsule.Capsule{}, errors.New("parse capsule: reference is required")
	}
	manifestDescriptor, err := store.Resolve(ctx, reference)
	if err != nil {
		return capsule.Capsule{}, fmt.Errorf("parse capsule: resolve %q: %w", reference, err)
	}
	manifestBytes, err := fetchVerified(ctx, store, manifestDescriptor)
	if err != nil {
		return capsule.Capsule{}, fmt.Errorf("parse capsule: fetch OCI manifest: %w", err)
	}
	var imageManifest ocispec.Manifest
	if err := json.Unmarshal(manifestBytes, &imageManifest); err != nil {
		return capsule.Capsule{}, invalidContent("parse capsule: decode OCI manifest: %v", err)
	}
	if imageManifest.MediaType != ocispec.MediaTypeImageManifest {
		return capsule.Capsule{}, invalidContent("parse capsule: OCI manifest media type %q is invalid", imageManifest.MediaType)
	}
	if imageManifest.ArtifactType != capsule.ArtifactMediaType {
		return capsule.Capsule{}, invalidContent("parse capsule: artifact type %q is invalid", imageManifest.ArtifactType)
	}
	if imageManifest.Config.MediaType != ConfigMediaType {
		return capsule.Capsule{}, invalidContent("parse capsule: config media type %q is invalid", imageManifest.Config.MediaType)
	}

	configBytes, err := fetchVerified(ctx, store, imageManifest.Config)
	if err != nil {
		return capsule.Capsule{}, fmt.Errorf("parse capsule: fetch config: %w", err)
	}
	var manifest capsule.Manifest
	if err := json.Unmarshal(configBytes, &manifest); err != nil {
		return capsule.Capsule{}, invalidContent("parse capsule: decode Capsule manifest: %v", err)
	}
	canonicalManifest, err := manifest.CanonicalJSON()
	if err != nil {
		return capsule.Capsule{}, invalidContent("parse capsule: validate Capsule manifest: %v", err)
	}
	if !bytes.Equal(configBytes, canonicalManifest) {
		return capsule.Capsule{}, invalidContent("parse capsule: config blob is not canonical Capsule manifest JSON")
	}
	computedCapsuleDigest, err := capsule.ComputeCapsuleDigest(manifest)
	if err != nil {
		return capsule.Capsule{}, invalidContent("parse capsule: compute Capsule digest: %v", err)
	}
	if reference != manifestDescriptor.Digest.String() && reference != computedCapsuleDigest {
		return capsule.Capsule{}, invalidContent("parse capsule: reference %q does not match Capsule digest %q", reference, computedCapsuleDigest)
	}
	if len(imageManifest.Layers) != len(manifest.Components) {
		return capsule.Capsule{}, invalidContent("parse capsule: layer count %d does not match component count %d", len(imageManifest.Layers), len(manifest.Components))
	}

	layers := make([]capsule.Layer, len(imageManifest.Layers))
	for index, descriptor := range imageManifest.Layers {
		component := manifest.Components[index]
		if err := validateLayerDescriptor(descriptor, component); err != nil {
			return capsule.Capsule{}, invalidContent("parse capsule: component %q: %v", component.ID, err)
		}
		layerBytes, err := fetchVerified(ctx, store, descriptor)
		if err != nil {
			return capsule.Capsule{}, fmt.Errorf("parse capsule: fetch component %q: %w", component.ID, err)
		}
		indexEntries, err := parseLayerIndex(layerBytes)
		if err != nil {
			return capsule.Capsule{}, invalidContent("parse capsule: component %q: %v", component.ID, err)
		}
		layers[index] = capsule.Layer{
			ComponentID: component.ID,
			MediaType:   descriptor.MediaType,
			Digest:      descriptor.Digest.String(),
			SizeBytes:   descriptor.Size,
			Bytes:       append([]byte(nil), layerBytes...),
			Index:       indexEntries,
		}
	}

	return capsule.Capsule{Manifest: manifest, Layers: layers, Digest: computedCapsuleDigest}, nil
}

func validateCapsuleForAssembly(capsuleValue capsule.Capsule) error {
	canonicalManifest, err := capsuleValue.Manifest.CanonicalJSON()
	if err != nil {
		return fmt.Errorf("assemble capsule: validate manifest: %w", err)
	}
	computedDigest := digest.FromBytes(canonicalManifest).String()
	if capsuleValue.Digest != computedDigest {
		return fmt.Errorf("assemble capsule: Capsule digest %q does not match manifest digest %q", capsuleValue.Digest, computedDigest)
	}
	if len(capsuleValue.Layers) != len(capsuleValue.Manifest.Components) {
		return fmt.Errorf("assemble capsule: layer count %d does not match component count %d", len(capsuleValue.Layers), len(capsuleValue.Manifest.Components))
	}
	for index, layer := range capsuleValue.Layers {
		component := capsuleValue.Manifest.Components[index]
		if layer.ComponentID != component.ID {
			return fmt.Errorf("assemble capsule: layer %d belongs to %q, want %q", index, layer.ComponentID, component.ID)
		}
		if layer.MediaType != component.MediaType || layer.MediaType != capsule.LayerMediaType(component.Type) {
			return fmt.Errorf("assemble capsule: component %q has invalid layer media type %q", component.ID, layer.MediaType)
		}
		if component.Digest != layer.Digest {
			return fmt.Errorf("assemble capsule: component %q digest %q does not match layer digest %q", component.ID, component.Digest, layer.Digest)
		}
		if component.SizeBytes != layer.SizeBytes {
			return fmt.Errorf("assemble capsule: component %q size %d does not match layer size %d", component.ID, component.SizeBytes, layer.SizeBytes)
		}
		if layer.SizeBytes != int64(len(layer.Bytes)) {
			return fmt.Errorf("assemble capsule: component %q size %d does not match bytes %d", component.ID, layer.SizeBytes, len(layer.Bytes))
		}
		if digest.FromBytes(layer.Bytes).String() != layer.Digest {
			return fmt.Errorf("assemble capsule: component %q layer digest does not match bytes", component.ID)
		}
		if _, err := layer.CanonicalIndexJSON(); err != nil {
			return fmt.Errorf("assemble capsule: component %q index: %w", component.ID, err)
		}
	}
	return nil
}

func validateLayerDescriptor(descriptor ocispec.Descriptor, component capsule.Component) error {
	if descriptor.MediaType != component.MediaType {
		return fmt.Errorf("layer media type %q does not match component media type %q", descriptor.MediaType, component.MediaType)
	}
	if descriptor.Digest.String() != component.Digest {
		return fmt.Errorf("layer digest %q does not match component digest %q", descriptor.Digest, component.Digest)
	}
	if descriptor.Size != component.SizeBytes {
		return fmt.Errorf("layer size %d does not match component size %d", descriptor.Size, component.SizeBytes)
	}
	wantAnnotations := map[string]string{
		AnnotationComponentType:  string(component.Type),
		AnnotationComponentID:    component.ID,
		AnnotationComponentScope: string(component.Scope),
		AnnotationComponentTrust: string(component.TrustClass),
	}
	for key, want := range wantAnnotations {
		if got := descriptor.Annotations[key]; got != want {
			return fmt.Errorf("layer annotation %q = %q, want %q", key, got, want)
		}
	}
	return nil
}

func pushBlob(ctx context.Context, store content.Storage, descriptor ocispec.Descriptor, data []byte) error {
	exists, err := store.Exists(ctx, descriptor)
	if err != nil {
		return err
	}
	if exists {
		return nil
	}
	if err := store.Push(ctx, descriptor, bytes.NewReader(data)); err != nil && !errors.Is(err, errdef.ErrAlreadyExists) {
		return err
	}
	return nil
}

func fetchVerified(ctx context.Context, store content.Fetcher, descriptor ocispec.Descriptor) ([]byte, error) {
	data, err := content.FetchAll(ctx, store, descriptor)
	if err != nil {
		return nil, err
	}
	if int64(len(data)) != descriptor.Size {
		return nil, invalidContent("blob %s size %d does not match expected %d", descriptor.Digest, len(data), descriptor.Size)
	}
	if digest.FromBytes(data) != descriptor.Digest {
		return nil, invalidContent("blob digest mismatch: got %s, want %s", digest.FromBytes(data), descriptor.Digest)
	}
	return data, nil
}
