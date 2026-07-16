// Package capsule models and deterministically packages Capsule Components.
//
// The package deliberately owns packaging-layer types. It does not import the
// domain package; domain integration is responsible for a later boundary.
// Layers deliberately use the USTAR tar format: names must be ASCII and fit
// USTAR's 100-byte name field or 155-byte prefix plus name split. Symlinks,
// hardlinks, xattrs, and ACLs are not represented. These constraints are
// deliberate because spec 19 requires portable, deterministic layers whose
// metadata cannot introduce machine-specific state or PAX extensions.
package capsule

import "fmt"

const (
	// SchemaVersion is the version of the Capsule manifest schema.
	SchemaVersion = 1
	// ArtifactMediaType identifies a Capsule OCI artifact.
	ArtifactMediaType = "application/vnd.devm.capsule.v1"
	// IndexPath is the reserved layer path containing the canonical file index.
	IndexPath = "index.json"
)

// ComponentType is the type-qualified namespace of a Component.
type ComponentType string

const (
	// ComponentTypeConfig identifies a configuration Component.
	ComponentTypeConfig ComponentType = "config"
	// ComponentTypeSkill identifies a skill Component.
	ComponentTypeSkill ComponentType = "skill"
	// ComponentTypeCommand identifies a command Component.
	ComponentTypeCommand ComponentType = "command"
	// ComponentTypeSubagent identifies a subagent Component.
	ComponentTypeSubagent ComponentType = "subagent"
	// ComponentTypeHook identifies a hook Component.
	ComponentTypeHook ComponentType = "hook"
	// ComponentTypeIntegration identifies an integration Component.
	ComponentTypeIntegration ComponentType = "integration"
	// ComponentTypePermissionPolicy identifies a permission-policy Component.
	ComponentTypePermissionPolicy ComponentType = "permission-policy"
	// ComponentTypeTemplate identifies a template Component.
	ComponentTypeTemplate ComponentType = "template"
	// ComponentTypeExtension identifies an extension Component.
	ComponentTypeExtension ComponentType = "extension"
)

// Scope identifies whether a Component is user- or project-scoped.
type Scope string

const (
	// ScopeUser identifies a user-scoped Component.
	ScopeUser Scope = "user"
	// ScopeProject identifies a project-scoped Component.
	ScopeProject Scope = "project"
)

// TrustClass describes the review boundary for a Component.
type TrustClass string

const (
	// TrustDeclarative identifies a declarative Component.
	TrustDeclarative TrustClass = "declarative"
	// TrustExecutable identifies an executable Component.
	TrustExecutable TrustClass = "executable"
	// TrustPermission identifies a permission Component.
	TrustPermission TrustClass = "permission"
)

// Requirements contains names and references only. It never carries resolved
// command values or secret values.
type Requirements struct {
	// Commands lists required command names.
	Commands []string `json:"commands"`
	// Secrets lists required secret names or references.
	Secrets []string `json:"secrets"`
}

// Component is a Capsule manifest descriptor. Components retain their order
// in Manifest.Components.
type Component struct {
	// ID is the stable type-qualified Component identifier.
	ID string `json:"id"`
	// Type identifies the Component namespace and layer media type.
	Type ComponentType `json:"type"`
	// Scope identifies whether the Component applies to a user or project.
	Scope Scope `json:"scope"`
	// TrustClass identifies the Component's review boundary.
	TrustClass TrustClass `json:"trustClass"`
	// MediaType is the type-specific OCI layer media type.
	MediaType string `json:"mediaType"`
	// Digest is the sha256 digest of the Component layer.
	Digest string `json:"digest"`
	// SizeBytes is the compressed Component layer size.
	SizeBytes int64 `json:"sizeBytes"`
	// Requirements contains command and secret requirements for the Component.
	Requirements Requirements `json:"requirements"`
}

// Manifest is the Capsule config blob. Its canonical JSON is the input to the
// Capsule digest.
type Manifest struct {
	// SchemaVersion identifies the manifest schema version.
	SchemaVersion int `json:"schemaVersion"`
	// Name is the human-readable Capsule name.
	Name string `json:"name"`
	// Components contains the ordered Component descriptors.
	Components []Component `json:"components"`
	// Requirements contains command and secret requirements for the Capsule.
	Requirements Requirements `json:"requirements"`
}

// FileIndexEntry describes one regular file in a layer.
type FileIndexEntry struct {
	// Path is the slash-separated path of a regular file in the layer.
	Path string `json:"path"`
	// Digest is the sha256 digest of the file content.
	Digest string `json:"digest"`
	// Mode is the normalized file mode, either 0644 or 0755.
	Mode uint32 `json:"mode"`
}

// Layer is a deterministic compressed Component layer and its canonical file
// index. Bytes contains the complete gzip-compressed tar stream.
type Layer struct {
	// ComponentID is the manifest Component ID represented by this layer.
	ComponentID string
	// MediaType is the type-specific OCI layer media type.
	MediaType string
	// Digest is the sha256 digest of the compressed tar stream.
	Digest string
	// SizeBytes is the compressed tar stream size.
	SizeBytes int64
	// Bytes contains the complete deterministic gzip-compressed tar stream.
	Bytes []byte
	// Index contains canonical file metadata for regular files in the layer.
	Index []FileIndexEntry
}

// Capsule is the result of building a manifest and one layer per Component.
type Capsule struct {
	// Manifest is the built manifest with layer descriptors populated.
	Manifest Manifest
	// Layers contains one deterministic layer per manifest Component.
	Layers []Layer
	// Digest is the sha256 digest of the canonical manifest.
	Digest string
}

func (componentType ComponentType) valid() bool {
	switch componentType {
	case ComponentTypeConfig, ComponentTypeSkill, ComponentTypeCommand,
		ComponentTypeSubagent, ComponentTypeHook, ComponentTypeIntegration,
		ComponentTypePermissionPolicy, ComponentTypeTemplate, ComponentTypeExtension:
		return true
	default:
		return false
	}
}

func (scope Scope) valid() bool {
	return scope == ScopeUser || scope == ScopeProject
}

func (trust TrustClass) valid() bool {
	return trust == TrustDeclarative || trust == TrustExecutable || trust == TrustPermission
}

// LayerMediaType returns the exact OCI media type for a Component layer.
func LayerMediaType(componentType ComponentType) string {
	return fmt.Sprintf("application/vnd.devm.capsule.%s.v1.tar+gzip", componentType)
}
