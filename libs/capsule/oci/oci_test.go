package oci_test

import (
	"bytes"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/ahmedhesham6/sshai/libs/capsule"
	oci "github.com/ahmedhesham6/sshai/libs/capsule/oci"
	"oras.land/oras-go/v2/content"
	orasoci "oras.land/oras-go/v2/content/oci"
)

func TestAssembleAndParseRoundTripsCapsuleThroughOCIStore(t *testing.T) {
	t.Parallel()
	capsuleValue := buildTestCapsule(t, map[string]string{
		"config:editor": "editor = vim\n",
		"skill:review":  "review skill\n",
	})
	storeRoot := t.TempDir()
	store, err := orasoci.New(storeRoot)
	if err != nil {
		t.Fatalf("create OCI store: %v", err)
	}

	artifact, err := oci.Assemble(t.Context(), store, capsuleValue)
	if err != nil {
		t.Fatalf("Assemble() error = %v", err)
	}
	if got, want := artifact.ManifestDescriptor.ArtifactType, capsule.ArtifactMediaType; got != want {
		t.Fatalf("artifact type = %q, want %q", got, want)
	}
	if got, want := len(artifact.LayerDescriptors), len(capsuleValue.Manifest.Components); got != want {
		t.Fatalf("layer descriptor count = %d, want %d", got, want)
	}
	for index, descriptor := range artifact.LayerDescriptors {
		component := capsuleValue.Manifest.Components[index]
		if got, want := descriptor.MediaType, component.MediaType; got != want {
			t.Errorf("layer %d media type = %q, want %q", index, got, want)
		}
		annotations := descriptor.Annotations
		for key, want := range map[string]string{
			oci.AnnotationComponentType:  string(component.Type),
			oci.AnnotationComponentID:    component.ID,
			oci.AnnotationComponentScope: string(component.Scope),
			oci.AnnotationComponentTrust: string(component.TrustClass),
		} {
			if got := annotations[key]; got != want {
				t.Errorf("layer %d annotation %q = %q, want %q", index, key, got, want)
			}
		}
	}

	configBytes, err := content.FetchAll(t.Context(), store, artifact.ConfigDescriptor)
	if err != nil {
		t.Fatalf("fetch config: %v", err)
	}
	wantConfig, err := capsuleValue.Manifest.CanonicalJSON()
	if err != nil {
		t.Fatalf("canonical manifest: %v", err)
	}
	if !bytes.Equal(configBytes, wantConfig) {
		t.Fatalf("config blob = %s, want canonical manifest %s", configBytes, wantConfig)
	}

	got, err := oci.Parse(t.Context(), store, capsuleValue.Digest)
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}
	if got.Digest != capsuleValue.Digest {
		t.Fatalf("parsed capsule digest = %q, want %q", got.Digest, capsuleValue.Digest)
	}
	if !reflect.DeepEqual(got.Manifest, capsuleValue.Manifest) {
		t.Fatalf("parsed manifest = %#v, want %#v", got.Manifest, capsuleValue.Manifest)
	}
	if len(got.Layers) != len(capsuleValue.Layers) {
		t.Fatalf("parsed layer count = %d, want %d", len(got.Layers), len(capsuleValue.Layers))
	}
	for index := range got.Layers {
		if !bytes.Equal(got.Layers[index].Bytes, capsuleValue.Layers[index].Bytes) {
			t.Errorf("parsed layer %d bytes differ", index)
		}
		if !reflect.DeepEqual(got.Layers[index].Index, capsuleValue.Layers[index].Index) {
			t.Errorf("parsed layer %d index = %#v, want %#v", index, got.Layers[index].Index, capsuleValue.Layers[index].Index)
		}
	}
}

func TestParseRejectsDigestMismatchForEveryBlob(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name string
		blob func(oci.Artifact) string
	}{
		{name: "config", blob: func(artifact oci.Artifact) string { return artifact.ConfigDescriptor.Digest.String() }},
		{name: "layer", blob: func(artifact oci.Artifact) string { return artifact.LayerDescriptors[0].Digest.String() }},
		{name: "manifest", blob: func(artifact oci.Artifact) string { return artifact.ManifestDescriptor.Digest.String() }},
	} {
		t.Run(test.name, func(t *testing.T) {
			capsuleValue := buildTestCapsule(t, map[string]string{"config:editor": "editor = vim\n"})
			storeRoot := t.TempDir()
			store, err := orasoci.New(storeRoot)
			if err != nil {
				t.Fatalf("create OCI store: %v", err)
			}
			artifact, err := oci.Assemble(t.Context(), store, capsuleValue)
			if err != nil {
				t.Fatalf("Assemble() error = %v", err)
			}
			blobDigest := test.blob(artifact)
			blobPath := filepath.Join(storeRoot, "blobs", "sha256", strings.TrimPrefix(blobDigest, "sha256:"))
			if err := os.Chmod(blobPath, 0o644); err != nil {
				t.Fatalf("make %s blob writable: %v", test.name, err)
			}
			if err := os.WriteFile(blobPath, []byte("tampered"), 0o644); err != nil {
				t.Fatalf("tamper %s blob: %v", test.name, err)
			}

			_, err = oci.Parse(t.Context(), store, capsuleValue.Digest)
			if err == nil {
				t.Fatal("Parse() error = nil, want digest mismatch")
			}
			if !strings.Contains(strings.ToLower(err.Error()), "digest") {
				t.Fatalf("Parse() error = %v, want digest mismatch", err)
			}
		})
	}
}

func TestAssembleRejectsManifestLayerDescriptorMismatch(t *testing.T) {
	t.Parallel()
	capsuleValue := buildTestCapsule(t, map[string]string{"config:editor": "editor = vim\n"})
	capsuleValue.Manifest.Components[0].Digest = "sha256:" + strings.Repeat("0", 64)
	computedDigest, err := capsule.ComputeCapsuleDigest(capsuleValue.Manifest)
	if err != nil {
		t.Fatalf("recompute Capsule digest: %v", err)
	}
	capsuleValue.Digest = computedDigest
	store, err := orasoci.New(t.TempDir())
	if err != nil {
		t.Fatalf("create OCI store: %v", err)
	}

	_, err = oci.Assemble(t.Context(), store, capsuleValue)
	if err == nil {
		t.Fatal("Assemble() error = nil, want manifest/layer mismatch rejection")
	}
	if !strings.Contains(strings.ToLower(err.Error()), "digest") {
		t.Fatalf("Assemble() error = %v, want digest mismatch context", err)
	}
}

func buildTestCapsule(t *testing.T, contents map[string]string) capsule.Capsule {
	t.Helper()
	root := t.TempDir()
	ids := make([]string, 0, len(contents))
	for id := range contents {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	manifest := capsule.Manifest{
		SchemaVersion: capsule.SchemaVersion,
		Name:          "test-capsule",
		Requirements:  capsule.Requirements{Commands: []string{}, Secrets: []string{}},
	}
	directories := make(map[string]string, len(ids))
	for index, id := range ids {
		componentType := capsule.ComponentType(strings.SplitN(id, ":", 2)[0])
		manifest.Components = append(manifest.Components, capsule.Component{
			ID:         id,
			Type:       componentType,
			Scope:      capsule.ScopeProject,
			TrustClass: trustClassForType(componentType),
			Requirements: capsule.Requirements{
				Commands: []string{},
				Secrets:  []string{},
			},
		})
		componentRoot := filepath.Join(root, "component-"+string(rune('a'+index)))
		if err := os.MkdirAll(componentRoot, 0o755); err != nil {
			t.Fatalf("create component root: %v", err)
		}
		if err := os.WriteFile(filepath.Join(componentRoot, "content.txt"), []byte(contents[id]), 0o644); err != nil {
			t.Fatalf("write component content: %v", err)
		}
		directories[id] = componentRoot
	}
	built, err := capsule.NewBuilder(1700000000).Build(manifest, directories)
	if err != nil {
		t.Fatalf("build test capsule: %v", err)
	}
	return built
}

// trustClassForType mirrors the type-to-trust mapping enforced in
// libs/capsule (requiredTrustClass) and libs/domain, so fixtures built by
// component id stay valid as the mapping evolves.
func trustClassForType(componentType capsule.ComponentType) capsule.TrustClass {
	switch componentType {
	case capsule.ComponentTypeHook, capsule.ComponentTypeExtension:
		return capsule.TrustExecutable
	case capsule.ComponentTypePermissionPolicy:
		return capsule.TrustPermission
	default:
		return capsule.TrustDeclarative
	}
}
