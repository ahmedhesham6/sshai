package guest

import (
	"errors"
	"fmt"
	"path"
	"strings"

	"github.com/ahmedhesham6/sshai/libs/capsule"
	"github.com/ahmedhesham6/sshai/libs/domain"
)

const (
	codexAdapterID      = "codex"
	codexAdapterVersion = "v1"
)

type codexAdapter struct{}

func init() {
	registerCapsuleAdapter(codexAdapter{})
}

func (codexAdapter) ID() string {
	return codexAdapterID
}

func (codexAdapter) Version() string {
	return codexAdapterVersion
}

func (codexAdapter) Translate(snapshot domain.CapsuleLockSnapshot, capsuleDigest string, component capsule.Component, files []capsuleFile, installed InstalledMaterialization, hasInstalled bool, batch CapsuleLockMaterializationBatch) (ProfileMaterialization, error) {
	return translateCodexComponent(snapshot, capsuleDigest, component, files, installed, hasInstalled, batch)
}

func translateCodexComponent(snapshot domain.CapsuleLockSnapshot, capsuleDigest string, component capsule.Component, files []capsuleFile, installed InstalledMaterialization, hasInstalled bool, batch CapsuleLockMaterializationBatch) (ProfileMaterialization, error) {
	if len(files) == 0 {
		return ProfileMaterialization{}, errors.New("Codex Component has no files")
	}
	componentID := component.ID
	scope := domain.ComponentScope(component.Scope)
	root := MaterializationHome
	if scope == domain.ScopeProject {
		root = MaterializationWorkspace
	}
	selector := "$"
	target := ""
	content := files[0].Content
	mode := files[0].Mode
	switch component.Type {
	case capsule.ComponentTypeConfig:
		target = ".codex/config.toml"
		selector = claudeSelector(componentID)
	case capsule.ComponentTypeCommand:
		name := claudeComponentName(componentID, "command")
		target = path.Join(".codex", "prompts", name+".md")
	case capsule.ComponentTypeIntegration:
		target = ".codex/config.toml"
		selector = codexIntegrationSelector(componentID)
	case capsule.ComponentTypeSkill, capsule.ComponentTypeSubagent, capsule.ComponentTypeHook, capsule.ComponentTypeExtension, capsule.ComponentTypePermissionPolicy:
		return ProfileMaterialization{}, fmt.Errorf("Codex adapter does not support Component type %q", component.Type)
	default:
		return ProfileMaterialization{}, fmt.Errorf("Codex adapter does not support Component type %q", component.Type)
	}

	if target == "" {
		return ProfileMaterialization{}, errors.New("Codex adapter produced an empty target")
	}
	contentDigest := materializationContentDigest(content)
	requirementDigest := componentRequirementDigest(component)
	key := EffectiveCacheKeyFields{
		ComponentDigest: component.Digest, AdapterID: codexAdapterID, AdapterVersion: codexAdapterVersion,
		TargetAgentVersion: batch.TargetAgentVersion, Scope: scope, NonSecretOverridesDigest: batch.NonSecretOverridesDigest,
		SecretVersionIdentifiers: append([]string(nil), batch.SecretVersionIdentifiers...),
	}
	effectiveCacheKey := key.Digest()
	effectiveCacheKeyChanged := hasInstalled && installed.EffectiveCacheKey != effectiveCacheKey
	approvalRequired, approvalReason := false, ""
	if component.TrustClass == capsule.TrustPermission || component.Type == capsule.ComponentTypeHook || component.Type == capsule.ComponentTypeExtension || component.Type == capsule.ComponentTypePermissionPolicy {
		approvalRequired, approvalReason = true, "permission component requires explicit consent"
	}
	if component.Type == capsule.ComponentTypeIntegration {
		approvalRequired, approvalReason = true, "integration component is never auto-applied"
	}
	transition := !hasInstalled || installed.LastAppliedDigest != contentDigest
	if transition && component.TrustClass == capsule.TrustExecutable {
		approvalRequired, approvalReason = true, "executable Component transition requires renewed review"
	}
	if hasInstalled && installed.CredentialRequirementDigest != requirementDigest {
		approvalRequired, approvalReason = true, "Credential Requirement changed and requires explicit consent"
	}
	item := ProfileMaterialization{
		ID: componentID, LockID: snapshot.ID, LockDigest: snapshot.Digest, CapsuleDigest: capsuleDigest, ComponentID: componentID, ComponentDigest: component.Digest,
		AdapterID: codexAdapterID, AdapterVersion: codexAdapterVersion, TargetAgentVersion: batch.TargetAgentVersion,
		NonSecretOverridesDigest: batch.NonSecretOverridesDigest, SecretVersionIdentifiers: append([]string(nil), batch.SecretVersionIdentifiers...),
		Scope: scope, Kind: domain.ComponentType(component.Type), TrustClass: domain.TrustClass(component.TrustClass),
		Requirements: domain.ComponentRequirements{Commands: append([]string(nil), component.Requirements.Commands...), Secrets: append([]string(nil), component.Requirements.Secrets...)},
		Mode:         MaterializationManaged, Root: root, Target: target, Selector: selector, Content: append([]byte(nil), content...), ContentSize: int64(len(content)), ContentDigest: contentDigest,
		FileMode:          mode,
		LastAppliedDigest: installed.LastAppliedDigest, ObservedDigest: installed.ObservedDigest, CredentialRequirementDigest: requirementDigest,
		ApprovalRequired: approvalRequired, ApprovalReason: approvalReason,
		EffectiveCacheKeyChanged: effectiveCacheKeyChanged,
	}
	if scope == domain.ScopeProject {
		item.Mode = MaterializationSeeded
	}
	item.EffectiveCacheKey = effectiveCacheKey
	return item, nil
}

func codexIntegrationSelector(id string) string {
	declared := claudeSelector(id)
	if strings.HasPrefix(declared, "$.mcp_servers.") {
		name := strings.TrimPrefix(declared, "$.mcp_servers.")
		if name != "" && !strings.Contains(name, ".") {
			return declared
		}
	}
	name := claudeComponentName(id, "integration")
	return "$.mcp_servers." + name
}
