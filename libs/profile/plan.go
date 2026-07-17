package profile

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/ahmedhesham6/sshai/libs/capsule"
	capsuleoci "github.com/ahmedhesham6/sshai/libs/capsule/oci"
	"github.com/ahmedhesham6/sshai/libs/domain"
)

// MaterializationFile is one regular file in a native directory plan. Path is
// relative to the plan's Target and is never an absolute filesystem path.
type MaterializationFile struct {
	Path    string
	Content []byte
	Mode    os.FileMode
}

// ProfileMaterialization is the canonical Component-to-native plan consumed
// by the generic safety engine. It contains no legacy profile artifact.
type ProfileMaterialization struct {
	ID                       string
	LockID                   string
	LockDigest               string
	CapsuleDigest            string
	ComponentID              string
	ComponentDigest          string
	AdapterID                string
	AdapterVersion           string
	TargetAgentVersion       string
	Scope                    domain.ComponentScope
	NonSecretOverridesDigest string
	SecretVersionIdentifiers []string
	EffectiveCacheKey        string
	EffectiveCacheKeyChanged bool

	Kind         domain.ComponentType
	TrustClass   domain.TrustClass
	Requirements domain.ComponentRequirements

	Mode   MaterializationMode
	Root   MaterializationRoot
	Target string
	// Selector is retained for JSON/TOML key ownership. "$" means the whole file.
	Selector string

	Content       []byte
	ContentSize   int64
	ContentDigest string
	FileMode      os.FileMode
	Files         []MaterializationFile
	Directory     bool
	FilePaths     []string

	LastAppliedDigest string
	ObservedDigest    string
	RequirementState  RequirementState

	CredentialRequirementDigest string
	ApprovalRequired            bool
	ApprovalReason              string
}

// ApprovalMarker is an explicit user decision for one exact Component
// digest. A marker never authorizes a different digest.
type ApprovalMarker struct {
	ComponentID     string
	ComponentDigest string
	LockID          string
	LockDigest      string
}

// CapsuleLockMaterializationBatch is the guest input for a complete
// lock-derived materialization. Grants are consulted only when the local OCI
// cache does not already contain a verified Capsule.
type CapsuleLockMaterializationBatch struct {
	Lock      domain.CapsuleLock
	OwnerID   string
	Grants    capsuleoci.GrantProvider
	CacheRoot string

	HomeRoot      string
	WorkspaceRoot string
	Intent        PlanIntent
	Installed     []InstalledMaterialization
	Approvals     map[string]ApprovalMarker

	AdapterID                string `json:"adapterId"`
	TargetAgentVersion       string
	NonSecretOverridesDigest string
	SecretVersionIdentifiers []string
	Metrics                  domain.Metrics
}

// EffectiveCacheKeyFields is the non-secret cache identity specified by
// spec/19. Resolved secret values are never accepted here.
type EffectiveCacheKeyFields struct {
	ComponentDigest          string                `json:"componentDigest"`
	AdapterID                string                `json:"adapterId"`
	AdapterVersion           string                `json:"adapterVersion"`
	TargetAgentVersion       string                `json:"targetAgentVersion"`
	Scope                    domain.ComponentScope `json:"scope"`
	NonSecretOverridesDigest string                `json:"nonSecretOverridesDigest"`
	SecretVersionIdentifiers []string              `json:"secretVersionIdentifiers"`
}

func (key EffectiveCacheKeyFields) Digest() string {
	key.SecretVersionIdentifiers = append([]string(nil), key.SecretVersionIdentifiers...)
	sort.Strings(key.SecretVersionIdentifiers)
	encoded, _ := json.Marshal(key)
	digest := sha256.Sum256(encoded)
	return "sha256:" + hex.EncodeToString(digest[:])
}

// CapsuleFile is one verified file resolved from a Capsule Component layer,
// with Path relative to the Component root.
type CapsuleFile struct {
	Path    string
	Content []byte
	Mode    os.FileMode
}

func MaterializationContentDigest(content []byte) string {
	digest := sha256.Sum256(content)
	return "sha256:" + hex.EncodeToString(digest[:])
}

func DirectoryMaterializationDigest(files []MaterializationFile) string {
	ordered := CloneMaterializationFiles(files)
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].Path < ordered[j].Path })
	hash := sha256.New()
	for _, file := range ordered {
		fmt.Fprintf(hash, "%s\x00%o\x00", file.Path, file.Mode.Perm())
		hash.Write(file.Content)
		hash.Write([]byte{0})
	}
	return "sha256:" + hex.EncodeToString(hash.Sum(nil))
}

func CloneMaterializationFiles(files []MaterializationFile) []MaterializationFile {
	cloned := make([]MaterializationFile, len(files))
	for index, file := range files {
		cloned[index] = MaterializationFile{Path: file.Path, Content: append([]byte(nil), file.Content...), Mode: file.Mode}
	}
	return cloned
}

func ToMaterializationFiles(files []CapsuleFile) []MaterializationFile {
	result := make([]MaterializationFile, len(files))
	for index, file := range files {
		result[index] = MaterializationFile{Path: file.Path, Content: append([]byte(nil), file.Content...), Mode: file.Mode}
	}
	return result
}

func ComponentRequirementDigest(component capsule.Component) string {
	secrets := append([]string(nil), component.Requirements.Secrets...)
	sort.Strings(secrets)
	encoded, _ := json.Marshal(secrets)
	return MaterializationContentDigest(encoded)
}

func MaterializationFilePaths(files []MaterializationFile) []string {
	paths := make([]string, len(files))
	for index, file := range files {
		paths[index] = file.Path
	}
	sort.Strings(paths)
	return paths
}

func FilepathExt(name string) string {
	index := strings.LastIndexByte(name, '.')
	if index < 0 || strings.Contains(name[index:], "/") {
		return ""
	}
	return name[index:]
}
