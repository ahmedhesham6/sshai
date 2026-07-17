package adapters

import (
	"errors"
	"fmt"
	"path"
	"strings"

	"github.com/ahmedhesham6/sshai/libs/capsule"
	"github.com/ahmedhesham6/sshai/libs/domain"
	"github.com/ahmedhesham6/sshai/libs/profile"
)

const (
	claudeAdapterID      = "claude"
	claudeAdapterVersion = "v1"
)

var claudeSensitiveSurfaces = []sensitiveMaterializationSurface{
	{Target: ".mcp.json", Selector: "$", Reason: "resolved destination overlaps Claude integration surface and requires explicit consent"},
	{Target: ".claude/settings.json", Selector: "$", Reason: "resolved destination overlaps Claude permission surface and requires explicit consent"},
}

type claudeAdapter struct{}

func init() {
	Register(claudeAdapter{})
}

func (claudeAdapter) ID() string {
	return claudeAdapterID
}

func (claudeAdapter) Version() string {
	return claudeAdapterVersion
}

func (claudeAdapter) Translate(snapshot domain.CapsuleLockSnapshot, capsuleDigest string, component capsule.Component, files []profile.CapsuleFile, installed profile.InstalledMaterialization, hasInstalled bool, batch profile.CapsuleLockMaterializationBatch) (profile.ProfileMaterialization, error) {
	return translateClaudeComponent(snapshot, capsuleDigest, component, files, installed, hasInstalled, batch)
}

func translateClaudeComponent(snapshot domain.CapsuleLockSnapshot, capsuleDigest string, component capsule.Component, files []profile.CapsuleFile, installed profile.InstalledMaterialization, hasInstalled bool, batch profile.CapsuleLockMaterializationBatch) (profile.ProfileMaterialization, error) {
	if len(files) == 0 {
		return profile.ProfileMaterialization{}, errors.New("Claude Component has no files")
	}
	componentID := component.ID
	scope := domain.ComponentScope(component.Scope)
	root := profile.MaterializationHome
	if scope == domain.ScopeProject {
		root = profile.MaterializationWorkspace
	}
	selector := "$"
	target := ""
	directory := false
	content := files[0].Content
	mode := files[0].Mode
	if component.Type == capsule.ComponentTypeSkill {
		directory = true
		name := claudeComponentName(componentID, "skill")
		target = path.Join(".claude", "skills", name)
		files = claudeSkillFiles(name, files)
	} else if component.Type == capsule.ComponentTypeSubagent {
		name := claudeComponentName(componentID, "subagent")
		target = path.Join(".claude", "agents", name+".md")
		content = files[0].Content
		mode = files[0].Mode
	} else if component.Type == capsule.ComponentTypeIntegration {
		target = ".mcp.json"
		content = files[0].Content
		mode = files[0].Mode
	} else if component.Type == capsule.ComponentTypePermissionPolicy || component.Type == capsule.ComponentTypeHook {
		target = ".claude/settings.json"
		selector = claudeSelector(componentID)
		if selector == "$" {
			if component.Type == capsule.ComponentTypePermissionPolicy {
				selector = "$.permissions"
			} else {
				selector = "$.hooks"
			}
		}
	} else if component.Type == capsule.ComponentTypeConfig || component.Type == capsule.ComponentTypeCommand {
		pathName := claudeComponentPath(componentID, files[0].Path)
		target = pathName
		selector = claudeSelector(componentID)
	} else {
		return profile.ProfileMaterialization{}, fmt.Errorf("Claude adapter does not support Component type %q", component.Type)
	}

	if target == "" {
		return profile.ProfileMaterialization{}, errors.New("Claude adapter produced an empty target")
	}
	contentDigest := profile.MaterializationContentDigest(content)
	if directory {
		contentDigest = profile.DirectoryMaterializationDigest(profile.ToMaterializationFiles(files))
	}
	requirementDigest := profile.ComponentRequirementDigest(component)
	key := profile.EffectiveCacheKeyFields{
		ComponentDigest: component.Digest, AdapterID: claudeAdapterID, AdapterVersion: claudeAdapterVersion,
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
	transition := !hasInstalled || installed.LastAppliedDigest != contentDigest || installed.ComponentDigest != component.Digest
	if transition && component.TrustClass == capsule.TrustExecutable {
		approvalRequired, approvalReason = true, "executable Component transition requires renewed review"
	}
	if !hasInstalled && len(component.Requirements.Secrets) > 0 {
		approvalRequired, approvalReason = true, "Credential Requirement requires explicit consent"
	}
	if hasInstalled && installed.CredentialRequirementDigest != requirementDigest {
		approvalRequired, approvalReason = true, "Credential Requirement changed and requires explicit consent"
	}
	if !approvalRequired {
		if required, reason := sensitiveMaterializationApproval(target, selector, claudeSensitiveSurfaces); required {
			approvalRequired, approvalReason = true, reason
		}
	}
	item := profile.ProfileMaterialization{
		ID: componentID, LockID: snapshot.ID, LockDigest: snapshot.Digest, CapsuleDigest: capsuleDigest, ComponentID: componentID, ComponentDigest: component.Digest,
		AdapterID: claudeAdapterID, AdapterVersion: claudeAdapterVersion, TargetAgentVersion: batch.TargetAgentVersion,
		NonSecretOverridesDigest: batch.NonSecretOverridesDigest, SecretVersionIdentifiers: append([]string(nil), batch.SecretVersionIdentifiers...),
		Scope: scope, Kind: domain.ComponentType(component.Type), TrustClass: domain.TrustClass(component.TrustClass),
		Requirements: domain.ComponentRequirements{Commands: append([]string(nil), component.Requirements.Commands...), Secrets: append([]string(nil), component.Requirements.Secrets...)},
		Mode:         profile.MaterializationManaged, Root: root, Target: target, Selector: selector, Content: append([]byte(nil), content...), ContentSize: int64(len(content)), ContentDigest: contentDigest,
		FileMode:  mode,
		Directory: directory, FilePaths: profile.MaterializationFilePaths(profile.ToMaterializationFiles(files)),
		LastAppliedDigest: installed.LastAppliedDigest, ObservedDigest: installed.ObservedDigest, CredentialRequirementDigest: requirementDigest,
		ApprovalRequired: approvalRequired, ApprovalReason: approvalReason,
		EffectiveCacheKeyChanged: effectiveCacheKeyChanged,
	}
	if directory {
		item.Content = nil
		item.ContentSize = 0
		item.Files = profile.ToMaterializationFiles(files)
	}
	if scope == domain.ScopeProject {
		item.Mode = profile.MaterializationSeeded
	}
	item.EffectiveCacheKey = effectiveCacheKey
	return item, nil
}

func claudeSkillFiles(name string, files []profile.CapsuleFile) []profile.CapsuleFile {
	prefixes := []string{path.Join(".claude", "skills", name) + "/", path.Join("skills", name) + "/", name + "/"}
	result := make([]profile.CapsuleFile, len(files))
	for index, file := range files {
		result[index] = file
		for _, prefix := range prefixes {
			if strings.HasPrefix(file.Path, prefix) {
				result[index].Path = strings.TrimPrefix(file.Path, prefix)
				break
			}
		}
	}
	return result
}

func claudeComponentName(id, kind string) string {
	_, suffix, _ := strings.Cut(id, ":")
	suffix, _, _ = strings.Cut(suffix, "#")
	if marker := strings.Index(suffix, "/skills/"); marker >= 0 {
		suffix = suffix[marker+len("/skills/"):]
	}
	suffix = strings.TrimPrefix(suffix, ".claude/skills/")
	suffix = strings.TrimPrefix(suffix, "skills/")
	suffix = strings.TrimSuffix(suffix, "/SKILL.md")
	suffix = strings.TrimSuffix(suffix, ".md")
	if slash := strings.LastIndexByte(suffix, '/'); slash >= 0 {
		suffix = suffix[slash+1:]
	}
	if suffix == "" {
		return kind
	}
	return suffix
}

func claudeComponentPath(id, fallback string) string {
	_, suffix, _ := strings.Cut(id, ":")
	suffix, _, _ = strings.Cut(suffix, "#")
	if suffix == "" || !strings.Contains(suffix, "/") && !strings.HasSuffix(suffix, ".md") {
		if profile.FilepathExt(fallback) != "" {
			return fallback
		}
		return suffix
	}
	return suffix
}

func claudeSelector(id string) string {
	_, suffix, _ := strings.Cut(id, "#")
	if suffix == "" {
		return "$"
	}
	return suffix
}
