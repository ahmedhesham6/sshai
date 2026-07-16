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
	opencodeAdapterID      = "opencode"
	opencodeAdapterVersion = "v1"
)

type opencodeAdapter struct{}

func init() {
	registerCapsuleAdapter(opencodeAdapter{})
}

func (opencodeAdapter) ID() string {
	return opencodeAdapterID
}

func (opencodeAdapter) Version() string {
	return opencodeAdapterVersion
}

func (opencodeAdapter) Translate(snapshot domain.CapsuleLockSnapshot, capsuleDigest string, component capsule.Component, files []capsuleFile, installed InstalledMaterialization, hasInstalled bool, batch CapsuleLockMaterializationBatch) (ProfileMaterialization, error) {
	return translateOpenCodeComponent(snapshot, capsuleDigest, component, files, installed, hasInstalled, batch)
}

func translateOpenCodeComponent(snapshot domain.CapsuleLockSnapshot, capsuleDigest string, component capsule.Component, files []capsuleFile, installed InstalledMaterialization, hasInstalled bool, batch CapsuleLockMaterializationBatch) (ProfileMaterialization, error) {
	if len(files) == 0 {
		return ProfileMaterialization{}, errors.New("OpenCode Component has no files")
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
	if component.Type == capsule.ComponentTypeConfig {
		target = openCodeConfigPath(scope)
		selector = claudeSelector(componentID)
	} else if component.Type == capsule.ComponentTypeSubagent {
		name := claudeComponentName(componentID, "subagent")
		target = path.Join(openCodeAgentDirectory(scope), name+".md")
	} else if component.Type == capsule.ComponentTypeCommand {
		name := claudeComponentName(componentID, "command")
		target = path.Join(openCodeCommandDirectory(scope), name+".md")
	} else if component.Type == capsule.ComponentTypeIntegration {
		target = openCodeConfigPath(scope)
		selector = "$.mcp." + openCodeIntegrationName(componentID)
	} else if component.Type == capsule.ComponentTypePermissionPolicy {
		target = openCodeConfigPath(scope)
		selector = "$.permission"
	} else {
		return ProfileMaterialization{}, fmt.Errorf("OpenCode adapter does not support Component type %q", component.Type)
	}

	if target == "" {
		return ProfileMaterialization{}, errors.New("OpenCode adapter produced an empty target")
	}
	contentDigest := materializationContentDigest(content)
	requirementDigest := componentRequirementDigest(component)
	key := EffectiveCacheKeyFields{
		ComponentDigest: component.Digest, AdapterID: opencodeAdapterID, AdapterVersion: opencodeAdapterVersion,
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
		AdapterID: opencodeAdapterID, AdapterVersion: opencodeAdapterVersion, TargetAgentVersion: batch.TargetAgentVersion,
		NonSecretOverridesDigest: batch.NonSecretOverridesDigest, SecretVersionIdentifiers: append([]string(nil), batch.SecretVersionIdentifiers...),
		Scope: scope, Kind: domain.ComponentType(component.Type), TrustClass: domain.TrustClass(component.TrustClass),
		Requirements: domain.ComponentRequirements{Commands: append([]string(nil), component.Requirements.Commands...), Secrets: append([]string(nil), component.Requirements.Secrets...)},
		Mode:         MaterializationManaged, Root: root, Target: target, Selector: selector, Content: append([]byte(nil), content...), ContentSize: int64(len(content)), ContentDigest: contentDigest,
		FileMode: mode, FilePaths: materializationFilePaths(toMaterializationFiles(files)), LastAppliedDigest: installed.LastAppliedDigest, ObservedDigest: installed.ObservedDigest, CredentialRequirementDigest: requirementDigest,
		ApprovalRequired: approvalRequired, ApprovalReason: approvalReason, EffectiveCacheKeyChanged: effectiveCacheKeyChanged,
	}
	if scope == domain.ScopeProject {
		item.Mode = MaterializationSeeded
	}
	item.EffectiveCacheKey = effectiveCacheKey
	return item, nil
}

func openCodeConfigPath(scope domain.ComponentScope) string {
	if scope == domain.ScopeProject {
		return "opencode.json"
	}
	return path.Join(".config", "opencode", "opencode.json")
}

func openCodeAgentDirectory(scope domain.ComponentScope) string {
	if scope == domain.ScopeProject {
		return path.Join(".opencode", "agent")
	}
	return path.Join(".config", "opencode", "agent")
}

func openCodeCommandDirectory(scope domain.ComponentScope) string {
	if scope == domain.ScopeProject {
		return path.Join(".opencode", "command")
	}
	return path.Join(".config", "opencode", "command")
}

func openCodeIntegrationName(id string) string {
	_, suffix, _ := strings.Cut(id, ":")
	_, declaredSelector, found := strings.Cut(suffix, "#")
	if found {
		if name := strings.TrimPrefix(declaredSelector, "$.mcp."); name != declaredSelector && name != "" {
			return name
		}
	}
	return claudeComponentName(id, "integration")
}
