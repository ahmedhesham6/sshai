package capsule

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"
)

var sha256DigestPattern = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)

type canonicalManifest struct {
	SchemaVersion int                   `json:"schemaVersion"`
	Name          string                `json:"name"`
	Components    []canonicalComponent  `json:"components"`
	Requirements  canonicalRequirements `json:"requirements"`
}

type canonicalComponent struct {
	ID           string                `json:"id"`
	Type         ComponentType         `json:"type"`
	Scope        Scope                 `json:"scope"`
	TrustClass   TrustClass            `json:"trustClass"`
	MediaType    string                `json:"mediaType"`
	Digest       string                `json:"digest"`
	SizeBytes    int64                 `json:"sizeBytes"`
	Requirements canonicalRequirements `json:"requirements"`
}

type canonicalRequirements struct {
	Commands []string `json:"commands"`
	Secrets  []string `json:"secrets"`
}

// CanonicalJSON returns the stable manifest encoding used for Capsule
// digests. Components remain ordered; set-like requirements are sorted and
// deduplicated.
func (manifest Manifest) CanonicalJSON() ([]byte, error) {
	if err := manifest.validate(true); err != nil {
		return nil, fmt.Errorf("canonicalize capsule manifest: %w", err)
	}

	components := make([]canonicalComponent, len(manifest.Components))
	for index, component := range manifest.Components {
		components[index] = canonicalComponent{
			ID:           component.ID,
			Type:         component.Type,
			Scope:        component.Scope,
			TrustClass:   component.TrustClass,
			MediaType:    component.MediaType,
			Digest:       component.Digest,
			SizeBytes:    component.SizeBytes,
			Requirements: canonicalizeRequirements(component.Requirements),
		}
	}

	return json.Marshal(canonicalManifest{
		SchemaVersion: manifest.SchemaVersion,
		Name:          manifest.Name,
		Components:    components,
		Requirements:  canonicalizeRequirements(manifest.Requirements),
	})
}

// CanonicalIndexJSON returns the stable path-to-file metadata map embedded as
// the index.json entry in a layer.
func (layer Layer) CanonicalIndexJSON() ([]byte, error) {
	index := make(map[string]canonicalIndexEntry, len(layer.Index))
	for _, entry := range layer.Index {
		if strings.TrimSpace(entry.Path) == "" {
			return nil, fmt.Errorf("canonicalize layer index: file path is required")
		}
		if !sha256DigestPattern.MatchString(entry.Digest) {
			return nil, fmt.Errorf("canonicalize layer index: digest for %q is invalid", entry.Path)
		}
		if entry.Mode != 0o644 && entry.Mode != 0o755 {
			return nil, fmt.Errorf("canonicalize layer index: mode for %q is invalid", entry.Path)
		}
		if _, exists := index[entry.Path]; exists {
			return nil, fmt.Errorf("canonicalize layer index: duplicate path %q", entry.Path)
		}
		index[entry.Path] = canonicalIndexEntry{Digest: entry.Digest, Mode: entry.Mode}
	}
	return json.Marshal(index)
}

type canonicalIndexEntry struct {
	Digest string `json:"digest"`
	Mode   uint32 `json:"mode"`
}

// ComputeCapsuleDigest returns the sha256 digest of the canonical manifest.
func ComputeCapsuleDigest(manifest Manifest) (string, error) {
	canonical, err := manifest.CanonicalJSON()
	if err != nil {
		return "", err
	}
	return digestBytes(canonical), nil
}

func canonicalizeRequirements(requirements Requirements) canonicalRequirements {
	commands := append([]string{}, requirements.Commands...)
	secrets := append([]string{}, requirements.Secrets...)
	sort.Strings(commands)
	sort.Strings(secrets)
	return canonicalRequirements{Commands: deduplicateSorted(commands), Secrets: deduplicateSorted(secrets)}
}

func deduplicateSorted(values []string) []string {
	if len(values) < 2 {
		return values
	}
	unique := values[:1]
	for _, value := range values[1:] {
		if value != unique[len(unique)-1] {
			unique = append(unique, value)
		}
	}
	return unique
}

func (manifest Manifest) validate(requireDigests bool) error {
	if manifest.SchemaVersion != SchemaVersion {
		return fmt.Errorf("schema version must be %d", SchemaVersion)
	}
	if strings.TrimSpace(manifest.Name) == "" {
		return fmt.Errorf("capsule name is required")
	}
	if err := validateRequirements(manifest.Requirements); err != nil {
		return fmt.Errorf("capsule requirements: %w", err)
	}

	seenIDs := make(map[string]struct{}, len(manifest.Components))
	for index, component := range manifest.Components {
		if err := component.validate(requireDigests); err != nil {
			return fmt.Errorf("component %d: %w", index, err)
		}
		if _, exists := seenIDs[component.ID]; exists {
			return fmt.Errorf("duplicate component ID %q", component.ID)
		}
		seenIDs[component.ID] = struct{}{}
	}
	return nil
}

func (component Component) validate(requireDigest bool) error {
	if strings.TrimSpace(component.ID) == "" {
		return fmt.Errorf("ID is required")
	}
	prefix := string(component.Type) + ":"
	if !component.Type.valid() || !strings.HasPrefix(component.ID, prefix) || strings.TrimPrefix(component.ID, prefix) == "" {
		return fmt.Errorf("ID %q is not type-qualified for %q", component.ID, component.Type)
	}
	if !component.Scope.valid() {
		return fmt.Errorf("scope %q is invalid", component.Scope)
	}
	if !component.TrustClass.valid() {
		return fmt.Errorf("trust class %q is invalid", component.TrustClass)
	}
	if required, constrained := requiredTrustClass(component.Type); constrained && component.TrustClass != required {
		return fmt.Errorf("component type %q requires trust class %q, got %q", component.Type, required, component.TrustClass)
	}
	if component.MediaType != "" && component.MediaType != LayerMediaType(component.Type) {
		return fmt.Errorf("media type %q is invalid", component.MediaType)
	}
	if requireDigest && !sha256DigestPattern.MatchString(component.Digest) {
		return fmt.Errorf("digest %q is invalid", component.Digest)
	}
	if component.Digest != "" && !sha256DigestPattern.MatchString(component.Digest) {
		return fmt.Errorf("digest %q is invalid", component.Digest)
	}
	if component.SizeBytes < 0 {
		return fmt.Errorf("size bytes cannot be negative")
	}
	if err := validateRequirements(component.Requirements); err != nil {
		return fmt.Errorf("requirements: %w", err)
	}
	return nil
}

func requiredTrustClass(componentType ComponentType) (TrustClass, bool) {
	switch componentType {
	case ComponentTypeHook, ComponentTypeExtension:
		return TrustExecutable, true
	case ComponentTypePermissionPolicy:
		return TrustPermission, true
	default:
		return "", false
	}
}

func validateRequirements(requirements Requirements) error {
	for _, command := range requirements.Commands {
		if strings.TrimSpace(command) == "" {
			return fmt.Errorf("command names cannot be empty")
		}
	}
	for _, secret := range requirements.Secrets {
		if strings.TrimSpace(secret) == "" {
			return fmt.Errorf("secret names cannot be empty")
		}
	}
	return nil
}

func digestBytes(content []byte) string {
	digest := sha256.Sum256(content)
	return "sha256:" + hex.EncodeToString(digest[:])
}
