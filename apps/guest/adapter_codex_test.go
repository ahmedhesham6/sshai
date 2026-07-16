package guest

import (
	"fmt"
	"strings"
	"testing"

	"github.com/ahmedhesham6/sshai/libs/capsule"
	"github.com/ahmedhesham6/sshai/libs/domain"
)

func TestCodexAdapterMapsConfigCommandAndIntegrationComponents(t *testing.T) {
	adapter := codexAdapter{}
	snapshot := domain.CapsuleLockSnapshot{ID: "lock-1", Digest: "sha256:lock"}
	batch := CapsuleLockMaterializationBatch{TargetAgentVersion: "codex-1", NonSecretOverridesDigest: "sha256:overrides", SecretVersionIdentifiers: []string{"secret-1"}}
	tests := []struct {
		name         string
		component    capsule.Component
		content      string
		wantRoot     MaterializationRoot
		wantMode     MaterializationMode
		wantTarget   string
		wantSelector string
		wantApproval bool
	}{
		{
			name: "config", component: capsule.Component{ID: "config:.codex/config.toml#$.model", Type: capsule.ComponentTypeConfig, Scope: capsule.ScopeUser, TrustClass: capsule.TrustDeclarative},
			content: "model = \"gpt-5\"\n", wantRoot: MaterializationHome, wantMode: MaterializationManaged,
			wantTarget: ".codex/config.toml", wantSelector: "$.model",
		},
		{
			name: "project config", component: capsule.Component{ID: "config:.codex/config.toml", Type: capsule.ComponentTypeConfig, Scope: capsule.ScopeProject, TrustClass: capsule.TrustDeclarative},
			content: "model = \"gpt-5\"\n", wantRoot: MaterializationWorkspace, wantMode: MaterializationSeeded,
			wantTarget: ".codex/config.toml", wantSelector: "$",
		},
		{
			name: "command", component: capsule.Component{ID: "command:review", Type: capsule.ComponentTypeCommand, Scope: capsule.ScopeUser, TrustClass: capsule.TrustDeclarative},
			content: "Review the change.\n", wantRoot: MaterializationHome, wantMode: MaterializationManaged,
			wantTarget: ".codex/prompts/review.md", wantSelector: "$",
		},
		{
			name: "integration", component: capsule.Component{ID: "integration:github", Type: capsule.ComponentTypeIntegration, Scope: capsule.ScopeUser, TrustClass: capsule.TrustDeclarative},
			content: "[mcp_servers.github]\ncommand = \"github-mcp\"\n", wantRoot: MaterializationHome, wantMode: MaterializationManaged,
			wantTarget: ".codex/config.toml", wantSelector: "$.mcp_servers.github", wantApproval: true,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			item, err := adapter.Translate(snapshot, "sha256:capsule", test.component, []capsuleFile{{Path: "content", Content: []byte(test.content), Mode: 0o644}}, InstalledMaterialization{}, false, batch)
			if err != nil {
				t.Fatalf("Translate() error = %v", err)
			}
			if item.AdapterID != "codex" || item.AdapterVersion != "v1" {
				t.Fatalf("adapter identity = %q/%q", item.AdapterID, item.AdapterVersion)
			}
			if item.Root != test.wantRoot || item.Mode != test.wantMode || item.Target != test.wantTarget || item.Selector != test.wantSelector {
				t.Fatalf("materialization = %#v", item)
			}
			if item.ApprovalRequired != test.wantApproval {
				t.Fatalf("ApprovalRequired = %t, want %t", item.ApprovalRequired, test.wantApproval)
			}
			wantKey := (EffectiveCacheKeyFields{
				ComponentDigest: test.component.Digest, AdapterID: "codex", AdapterVersion: "v1", TargetAgentVersion: batch.TargetAgentVersion,
				Scope: domain.ComponentScope(test.component.Scope), NonSecretOverridesDigest: batch.NonSecretOverridesDigest,
				SecretVersionIdentifiers: batch.SecretVersionIdentifiers,
			}).Digest()
			if item.EffectiveCacheKey != wantKey {
				t.Fatalf("EffectiveCacheKey = %q, want %q", item.EffectiveCacheKey, wantKey)
			}
		})
	}
}

func TestCodexAdapterRejectsUnsupportedComponentTypes(t *testing.T) {
	for _, componentType := range []capsule.ComponentType{
		capsule.ComponentTypeSkill, capsule.ComponentTypeSubagent, capsule.ComponentTypeHook,
		capsule.ComponentTypeExtension, capsule.ComponentTypePermissionPolicy,
	} {
		t.Run(string(componentType), func(t *testing.T) {
			_, err := (codexAdapter{}).Translate(domain.CapsuleLockSnapshot{}, "sha256:capsule", capsule.Component{
				ID: "unsupported:item", Type: componentType, Scope: capsule.ScopeUser, TrustClass: capsule.TrustDeclarative,
			}, []capsuleFile{{Path: "content", Content: []byte("content"), Mode: 0o644}}, InstalledMaterialization{}, false, CapsuleLockMaterializationBatch{})
			want := fmt.Sprintf("Codex adapter does not support Component type %q", componentType)
			if err == nil || err.Error() != want {
				t.Fatalf("error = %v, want %q", err, want)
			}
		})
	}
}

func TestCodexAdapterIntegrationAlwaysRequiresApproval(t *testing.T) {
	component := capsule.Component{ID: "integration:github", Type: capsule.ComponentTypeIntegration, Scope: capsule.ScopeUser, TrustClass: capsule.TrustDeclarative}
	content := []byte("[mcp_servers.github]\ncommand = \"github-mcp\"\n")
	installed := InstalledMaterialization{LastAppliedDigest: materializationContentDigest(content), CredentialRequirementDigest: componentRequirementDigest(component)}
	item, err := (codexAdapter{}).Translate(domain.CapsuleLockSnapshot{}, "sha256:capsule", component, []capsuleFile{{Path: "content", Content: content, Mode: 0o644}}, installed, true, CapsuleLockMaterializationBatch{})
	if err != nil {
		t.Fatal(err)
	}
	if !item.ApprovalRequired || item.ApprovalReason != "integration component is never auto-applied" {
		t.Fatalf("approval = %t/%q", item.ApprovalRequired, item.ApprovalReason)
	}
}

func TestCodexAdapterExecutableTransitionRequiresRenewedReview(t *testing.T) {
	component := capsule.Component{ID: "command:review", Type: capsule.ComponentTypeCommand, Scope: capsule.ScopeUser, TrustClass: capsule.TrustExecutable}
	item, err := (codexAdapter{}).Translate(domain.CapsuleLockSnapshot{}, "sha256:capsule", component, []capsuleFile{{Path: "content", Content: []byte("new prompt\n"), Mode: 0o755}}, InstalledMaterialization{
		LastAppliedDigest: materializationContentDigest([]byte("old prompt\n")), CredentialRequirementDigest: componentRequirementDigest(component),
	}, true, CapsuleLockMaterializationBatch{})
	if err != nil {
		t.Fatal(err)
	}
	if !item.ApprovalRequired || item.ApprovalReason != "executable Component transition requires renewed review" {
		t.Fatalf("approval = %t/%q", item.ApprovalRequired, item.ApprovalReason)
	}
	if strings.Contains(item.ApprovalReason, "Codex") {
		t.Fatal("approval reason unexpectedly changed adapter-independent policy text")
	}
}

func TestCodexAdapterRejectsEmptyFiles(t *testing.T) {
	_, err := (codexAdapter{}).Translate(domain.CapsuleLockSnapshot{}, "sha256:capsule", capsule.Component{ID: "config:settings", Type: capsule.ComponentTypeConfig}, nil, InstalledMaterialization{}, false, CapsuleLockMaterializationBatch{})
	if err == nil || err.Error() != "Codex Component has no files" {
		t.Fatalf("empty files error = %v", err)
	}
}
