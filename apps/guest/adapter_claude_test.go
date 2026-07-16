package guest

import (
	"strings"
	"testing"

	"github.com/ahmedhesham6/sshai/libs/capsule"
	"github.com/ahmedhesham6/sshai/libs/domain"
)

func TestClaudeAdapterDeclarativeAliasesSensitiveSurfacesRequireApproval(t *testing.T) {
	tests := []struct {
		name       string
		component  capsule.Component
		wantTarget string
		wantSelect string
		wantReason string
	}{
		{
			name: "integration alias",
			component: capsule.Component{
				ID: "config:.mcp.json#$.mcpServers.github", Type: capsule.ComponentTypeConfig,
				Scope: capsule.ScopeUser, TrustClass: capsule.TrustDeclarative,
			},
			wantTarget: ".mcp.json", wantSelect: "$.mcpServers.github", wantReason: "integration",
		},
		{
			name: "permission alias",
			component: capsule.Component{
				ID: "config:.claude/settings.json#$.permissions.allow", Type: capsule.ComponentTypeConfig,
				Scope: capsule.ScopeUser, TrustClass: capsule.TrustDeclarative,
			},
			wantTarget: ".claude/settings.json", wantSelect: "$.permissions.allow", wantReason: "permission",
		},
		{
			name: "benign config",
			component: capsule.Component{
				ID: "config:CLAUDE.md", Type: capsule.ComponentTypeConfig,
				Scope: capsule.ScopeUser, TrustClass: capsule.TrustDeclarative,
			},
			wantTarget: "CLAUDE.md", wantSelect: "$",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			item, err := (claudeAdapter{}).Translate(domain.CapsuleLockSnapshot{}, "sha256:capsule", test.component, []capsuleFile{{Content: []byte("content"), Mode: 0o644}}, InstalledMaterialization{}, false, CapsuleLockMaterializationBatch{})
			if err != nil {
				t.Fatal(err)
			}
			if item.Target != test.wantTarget || item.Selector != test.wantSelect {
				t.Fatalf("resolved destination = %q/%q, want %q/%q", item.Target, item.Selector, test.wantTarget, test.wantSelect)
			}
			if test.wantReason == "" {
				if item.ApprovalRequired {
					t.Fatalf("benign destination unexpectedly requires approval: %q", item.ApprovalReason)
				}
				return
			}
			if !item.ApprovalRequired || !strings.Contains(item.ApprovalReason, test.wantReason) {
				t.Fatalf("approval = %t/%q, want reason containing %q", item.ApprovalRequired, item.ApprovalReason, test.wantReason)
			}
		})
	}
}

func TestClaudeAdapterExecutableComponentDigestChangeRequiresRenewedReview(t *testing.T) {
	component := capsule.Component{
		ID: "command:review", Type: capsule.ComponentTypeCommand, Scope: capsule.ScopeUser,
		TrustClass: capsule.TrustExecutable, Digest: "new-component-digest",
	}
	content := []byte("review prompt\n")
	item, err := (claudeAdapter{}).Translate(domain.CapsuleLockSnapshot{}, "sha256:capsule", component, []capsuleFile{{Content: content, Mode: 0o755}}, InstalledMaterialization{
		ComponentID: component.ID, ComponentDigest: "old-component-digest",
		LastAppliedDigest: materializationContentDigest(content), CredentialRequirementDigest: componentRequirementDigest(component),
	}, true, CapsuleLockMaterializationBatch{})
	if err != nil {
		t.Fatal(err)
	}
	if !item.ApprovalRequired || item.ApprovalReason != "executable Component transition requires renewed review" {
		t.Fatalf("approval = %t/%q, want renewed review for component-digest transition", item.ApprovalRequired, item.ApprovalReason)
	}
}

func TestClaudeAdapterFirstInstallCredentialRequirementRequiresConsent(t *testing.T) {
	component := capsule.Component{
		ID: "config:CLAUDE.md", Type: capsule.ComponentTypeConfig, Scope: capsule.ScopeUser,
		TrustClass: capsule.TrustDeclarative, Requirements: capsule.Requirements{Secrets: []string{"TOKEN"}},
	}
	item, err := (claudeAdapter{}).Translate(domain.CapsuleLockSnapshot{}, "sha256:capsule", component, []capsuleFile{{Content: []byte("instructions\n"), Mode: 0o644}}, InstalledMaterialization{}, false, CapsuleLockMaterializationBatch{})
	if err != nil {
		t.Fatal(err)
	}
	if !item.ApprovalRequired || !strings.Contains(item.ApprovalReason, "Credential Requirement") || !strings.Contains(item.ApprovalReason, "explicit consent") {
		t.Fatalf("approval = %t/%q, want first-install credential consent", item.ApprovalRequired, item.ApprovalReason)
	}
}
