package guest

import (
	"os"
	"strings"
	"testing"

	"github.com/ahmedhesham6/sshai/libs/capsule"
	"github.com/ahmedhesham6/sshai/libs/domain"
)

func TestOpenCodeAdapterMapsConfigSubagentCommandIntegrationAndPermission(t *testing.T) {
	adapter, err := capsuleAdapterFor("opencode")
	if err != nil {
		t.Fatal(err)
	}

	snapshot := domain.CapsuleLockSnapshot{ID: "lock-1", Digest: "lock-digest"}
	batch := CapsuleLockMaterializationBatch{TargetAgentVersion: "opencode-1"}
	tests := []struct {
		name      string
		component capsule.Component
		content   string
		want      ProfileMaterialization
	}{
		{
			name:      "config",
			component: capsule.Component{ID: "config:settings#$.theme", Type: capsule.ComponentTypeConfig, Scope: capsule.ScopeUser, TrustClass: capsule.TrustDeclarative, Digest: "config-digest"},
			content:   `"dark"`,
			want:      ProfileMaterialization{Root: MaterializationHome, Target: ".config/opencode/opencode.json", Selector: "$.theme", Mode: MaterializationManaged},
		},
		{
			name:      "subagent",
			component: capsule.Component{ID: "subagent:reviewer.md", Type: capsule.ComponentTypeSubagent, Scope: capsule.ScopeUser, TrustClass: capsule.TrustDeclarative, Digest: "subagent-digest"},
			content:   "Review changes\n",
			want:      ProfileMaterialization{Root: MaterializationHome, Target: ".config/opencode/agent/reviewer.md", Selector: "$", Mode: MaterializationManaged},
		},
		{
			name:      "command",
			component: capsule.Component{ID: "command:deploy.md", Type: capsule.ComponentTypeCommand, Scope: capsule.ScopeUser, TrustClass: capsule.TrustDeclarative, Digest: "command-digest"},
			content:   "Deploy safely\n",
			want:      ProfileMaterialization{Root: MaterializationHome, Target: ".config/opencode/command/deploy.md", Selector: "$", Mode: MaterializationManaged},
		},
		{
			name:      "integration",
			component: capsule.Component{ID: "integration:github", Type: capsule.ComponentTypeIntegration, Scope: capsule.ScopeUser, TrustClass: capsule.TrustDeclarative, Digest: "integration-digest"},
			content:   `{"type":"remote","url":"https://example.test/mcp"}`,
			want:      ProfileMaterialization{Root: MaterializationHome, Target: ".config/opencode/opencode.json", Selector: "$.mcp.github", Mode: MaterializationManaged},
		},
		{
			name:      "permission",
			component: capsule.Component{ID: "permission-policy:workspace", Type: capsule.ComponentTypePermissionPolicy, Scope: capsule.ScopeUser, TrustClass: capsule.TrustPermission, Digest: "permission-digest"},
			content:   `{"read":true}`,
			want:      ProfileMaterialization{Root: MaterializationHome, Target: ".config/opencode/opencode.json", Selector: "$.permission", Mode: MaterializationManaged},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := adapter.Translate(snapshot, "capsule-digest", test.component, []capsuleFile{{Path: "content", Content: []byte(test.content), Mode: 0o644}}, InstalledMaterialization{}, false, batch)
			if err != nil {
				t.Fatal(err)
			}
			if got.Root != test.want.Root || got.Target != test.want.Target || got.Selector != test.want.Selector || got.Mode != test.want.Mode {
				t.Fatalf("materialization = %#v, want root=%q target=%q selector=%q mode=%q", got, test.want.Root, test.want.Target, test.want.Selector, test.want.Mode)
			}
			if got.ContentSize != int64(len(test.content)) || string(got.Content) != test.content || got.FileMode != os.FileMode(0o644) {
				t.Fatalf("content metadata = size:%d content:%q mode:%o", got.ContentSize, got.Content, got.FileMode.Perm())
			}
			if len(got.FilePaths) != 1 || got.FilePaths[0] != "content" {
				t.Fatalf("file paths = %#v, want [content]", got.FilePaths)
			}
			if got.AdapterID != "opencode" || got.AdapterVersion != "v1" {
				t.Fatalf("adapter metadata = %q/%q, want opencode/v1", got.AdapterID, got.AdapterVersion)
			}
			wantCacheKey := (EffectiveCacheKeyFields{
				ComponentDigest:          test.component.Digest,
				AdapterID:                "opencode",
				AdapterVersion:           "v1",
				TargetAgentVersion:       batch.TargetAgentVersion,
				Scope:                    domain.ComponentScope(test.component.Scope),
				NonSecretOverridesDigest: batch.NonSecretOverridesDigest,
				SecretVersionIdentifiers: batch.SecretVersionIdentifiers,
			}).Digest()
			if got.EffectiveCacheKey != wantCacheKey || got.EffectiveCacheKeyChanged {
				t.Fatalf("effective cache key = %q changed=%t, want %q unchanged", got.EffectiveCacheKey, got.EffectiveCacheKeyChanged, wantCacheKey)
			}
		})
	}
}

func TestOpenCodeAdapterScopeSelectsProjectVsHomeTargets(t *testing.T) {
	adapter, err := capsuleAdapterFor("opencode")
	if err != nil {
		t.Fatal(err)
	}

	snapshot := domain.CapsuleLockSnapshot{ID: "lock-1", Digest: "lock-digest"}
	batch := CapsuleLockMaterializationBatch{TargetAgentVersion: "opencode-1"}
	tests := []struct {
		name      string
		component capsule.Component
		wantRoot  MaterializationRoot
		wantMode  MaterializationMode
		wantPath  string
	}{
		{
			name:      "project config",
			component: capsule.Component{ID: "config:settings", Type: capsule.ComponentTypeConfig, Scope: capsule.ScopeProject, TrustClass: capsule.TrustDeclarative, Digest: "config-project"},
			wantRoot:  MaterializationWorkspace,
			wantMode:  MaterializationSeeded,
			wantPath:  "opencode.json",
		},
		{
			name:      "project subagent",
			component: capsule.Component{ID: "subagent:reviewer.md", Type: capsule.ComponentTypeSubagent, Scope: capsule.ScopeProject, TrustClass: capsule.TrustDeclarative, Digest: "subagent-project"},
			wantRoot:  MaterializationWorkspace,
			wantMode:  MaterializationSeeded,
			wantPath:  ".opencode/agent/reviewer.md",
		},
		{
			name:      "project command",
			component: capsule.Component{ID: "command:deploy.md", Type: capsule.ComponentTypeCommand, Scope: capsule.ScopeProject, TrustClass: capsule.TrustDeclarative, Digest: "command-project"},
			wantRoot:  MaterializationWorkspace,
			wantMode:  MaterializationSeeded,
			wantPath:  ".opencode/command/deploy.md",
		},
		{
			name:      "home config",
			component: capsule.Component{ID: "config:settings", Type: capsule.ComponentTypeConfig, Scope: capsule.ScopeUser, TrustClass: capsule.TrustDeclarative, Digest: "config-home"},
			wantRoot:  MaterializationHome,
			wantMode:  MaterializationManaged,
			wantPath:  ".config/opencode/opencode.json",
		},
		{
			name:      "home subagent",
			component: capsule.Component{ID: "subagent:reviewer.md", Type: capsule.ComponentTypeSubagent, Scope: capsule.ScopeUser, TrustClass: capsule.TrustDeclarative, Digest: "subagent-home"},
			wantRoot:  MaterializationHome,
			wantMode:  MaterializationManaged,
			wantPath:  ".config/opencode/agent/reviewer.md",
		},
		{
			name:      "home command",
			component: capsule.Component{ID: "command:deploy.md", Type: capsule.ComponentTypeCommand, Scope: capsule.ScopeUser, TrustClass: capsule.TrustDeclarative, Digest: "command-home"},
			wantRoot:  MaterializationHome,
			wantMode:  MaterializationManaged,
			wantPath:  ".config/opencode/command/deploy.md",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := adapter.Translate(snapshot, "capsule-digest", test.component, []capsuleFile{{Path: "content", Content: []byte("content"), Mode: 0o644}}, InstalledMaterialization{}, false, batch)
			if err != nil {
				t.Fatal(err)
			}
			if got.Root != test.wantRoot || got.Mode != test.wantMode || got.Target != test.wantPath {
				t.Fatalf("materialization = root:%q mode:%q target:%q, want root:%q mode:%q target:%q", got.Root, got.Mode, got.Target, test.wantRoot, test.wantMode, test.wantPath)
			}
		})
	}
}

func TestOpenCodeAdapterRejectsUnsupportedComponentTypes(t *testing.T) {
	adapter, err := capsuleAdapterFor("opencode")
	if err != nil {
		t.Fatal(err)
	}

	for _, componentType := range []capsule.ComponentType{capsule.ComponentTypeSkill, capsule.ComponentTypeHook, capsule.ComponentTypeExtension} {
		component := capsule.Component{ID: string(componentType) + ":unsupported", Type: componentType, Scope: capsule.ScopeUser, TrustClass: capsule.TrustDeclarative, Digest: "component-digest"}
		_, err := adapter.Translate(domain.CapsuleLockSnapshot{}, "capsule-digest", component, []capsuleFile{{Path: "content", Content: []byte("content"), Mode: 0o644}}, InstalledMaterialization{}, false, CapsuleLockMaterializationBatch{})
		want := `OpenCode adapter does not support Component type "` + string(componentType) + `"`
		if err == nil || err.Error() != want {
			t.Fatalf("Translate(%q) error = %v, want %q", componentType, err, want)
		}
	}
}

func TestOpenCodeAdapterIntegrationAndPermissionAlwaysRequireApproval(t *testing.T) {
	adapter, err := capsuleAdapterFor("opencode")
	if err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name          string
		component     capsule.Component
		content       string
		approval      string
		installedMode bool
	}{
		{
			name:          "integration",
			component:     capsule.Component{ID: "integration:github", Type: capsule.ComponentTypeIntegration, Scope: capsule.ScopeUser, TrustClass: capsule.TrustDeclarative, Digest: "integration-digest"},
			content:       `{"type":"remote"}`,
			approval:      "integration component is never auto-applied",
			installedMode: true,
		},
		{
			name:          "permission",
			component:     capsule.Component{ID: "permission-policy:workspace", Type: capsule.ComponentTypePermissionPolicy, Scope: capsule.ScopeUser, TrustClass: capsule.TrustPermission, Digest: "permission-digest"},
			content:       `{"read":true}`,
			approval:      "permission component requires explicit consent",
			installedMode: true,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			files := []capsuleFile{{Path: "content", Content: []byte(test.content), Mode: 0o644}}
			installed := InstalledMaterialization{}
			if test.installedMode {
				installed.LastAppliedDigest = materializationContentDigest(files[0].Content)
				installed.CredentialRequirementDigest = componentRequirementDigest(test.component)
			}
			got, err := adapter.Translate(domain.CapsuleLockSnapshot{}, "capsule-digest", test.component, files, installed, test.installedMode, CapsuleLockMaterializationBatch{})
			if err != nil {
				t.Fatal(err)
			}
			if !got.ApprovalRequired || got.ApprovalReason != test.approval {
				t.Fatalf("approval = %t/%q, want true/%q", got.ApprovalRequired, got.ApprovalReason, test.approval)
			}
		})
	}
}

func TestOpenCodeAdapterDeclarativeAliasesSensitiveSurfacesRequireApproval(t *testing.T) {
	tests := []struct {
		name         string
		selector     string
		wantReason   string
		wantApproval bool
	}{
		{name: "integration alias", selector: "$.mcp.github", wantReason: "integration", wantApproval: true},
		{name: "permission alias", selector: "$.permission", wantReason: "permission", wantApproval: true},
		{name: "benign selector", selector: "$.theme", wantApproval: false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			component := capsule.Component{ID: "config:opencode.json#" + test.selector, Type: capsule.ComponentTypeConfig, Scope: capsule.ScopeUser, TrustClass: capsule.TrustDeclarative}
			item, err := (opencodeAdapter{}).Translate(domain.CapsuleLockSnapshot{}, "sha256:capsule", component, []capsuleFile{{Content: []byte(`{"value":true}`), Mode: 0o644}}, InstalledMaterialization{}, false, CapsuleLockMaterializationBatch{})
			if err != nil {
				t.Fatal(err)
			}
			if item.ApprovalRequired != test.wantApproval {
				t.Fatalf("ApprovalRequired = %t, want %t (reason %q)", item.ApprovalRequired, test.wantApproval, item.ApprovalReason)
			}
			if test.wantReason != "" && !strings.Contains(item.ApprovalReason, test.wantReason) {
				t.Fatalf("ApprovalReason = %q, want it to contain %q", item.ApprovalReason, test.wantReason)
			}
		})
	}
}

func TestOpenCodeAdapterExecutableTransitionRequiresRenewedReview(t *testing.T) {
	adapter, err := capsuleAdapterFor("opencode")
	if err != nil {
		t.Fatal(err)
	}

	component := capsule.Component{ID: "command:deploy.md", Type: capsule.ComponentTypeCommand, Scope: capsule.ScopeUser, TrustClass: capsule.TrustExecutable, Digest: "first-component-digest"}
	oldFiles := []capsuleFile{{Path: "deploy.md", Content: []byte("deploy first\n"), Mode: 0o644}}
	old, err := adapter.Translate(domain.CapsuleLockSnapshot{}, "first-capsule-digest", component, oldFiles, InstalledMaterialization{}, false, CapsuleLockMaterializationBatch{})
	if err != nil {
		t.Fatal(err)
	}

	component.Digest = "second-component-digest"
	newFiles := []capsuleFile{{Path: "deploy.md", Content: []byte("deploy second\n"), Mode: 0o644}}
	installed := InstalledMaterialization{
		ComponentID:                 component.ID,
		EffectiveCacheKey:           old.EffectiveCacheKey,
		LastAppliedDigest:           old.ContentDigest,
		CredentialRequirementDigest: old.CredentialRequirementDigest,
	}
	got, err := adapter.Translate(domain.CapsuleLockSnapshot{}, "second-capsule-digest", component, newFiles, installed, true, CapsuleLockMaterializationBatch{})
	if err != nil {
		t.Fatal(err)
	}
	if !got.ApprovalRequired || got.ApprovalReason != "executable Component transition requires renewed review" {
		t.Fatalf("approval = %t/%q, want executable transition review", got.ApprovalRequired, got.ApprovalReason)
	}
}

func TestOpenCodeAdapterExecutableComponentDigestChangeRequiresRenewedReview(t *testing.T) {
	adapter, err := capsuleAdapterFor("opencode")
	if err != nil {
		t.Fatal(err)
	}
	component := capsule.Component{
		ID: "command:deploy.md", Type: capsule.ComponentTypeCommand, Scope: capsule.ScopeUser,
		TrustClass: capsule.TrustExecutable, Digest: "new-component-digest",
	}
	content := []byte("deploy prompt\n")
	item, err := adapter.Translate(domain.CapsuleLockSnapshot{}, "sha256:capsule", component, []capsuleFile{{Content: content, Mode: 0o755}}, InstalledMaterialization{
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

func TestOpenCodeAdapterFirstInstallCredentialRequirementRequiresConsent(t *testing.T) {
	adapter, err := capsuleAdapterFor("opencode")
	if err != nil {
		t.Fatal(err)
	}
	component := capsule.Component{
		ID: "config:opencode.json#$.theme", Type: capsule.ComponentTypeConfig, Scope: capsule.ScopeUser,
		TrustClass: capsule.TrustDeclarative, Requirements: capsule.Requirements{Secrets: []string{"TOKEN"}},
	}
	item, err := adapter.Translate(domain.CapsuleLockSnapshot{}, "sha256:capsule", component, []capsuleFile{{Content: []byte(`{"value":"dark"}`), Mode: 0o644}}, InstalledMaterialization{}, false, CapsuleLockMaterializationBatch{})
	if err != nil {
		t.Fatal(err)
	}
	if !item.ApprovalRequired || !strings.Contains(item.ApprovalReason, "Credential Requirement") || !strings.Contains(item.ApprovalReason, "explicit consent") {
		t.Fatalf("approval = %t/%q, want first-install credential consent", item.ApprovalRequired, item.ApprovalReason)
	}
}

func TestOpenCodeAdapterCredentialRequirementChangeRequiresConsent(t *testing.T) {
	adapter, err := capsuleAdapterFor("opencode")
	if err != nil {
		t.Fatal(err)
	}

	component := capsule.Component{
		ID: "config:settings#$.auth", Type: capsule.ComponentTypeConfig, Scope: capsule.ScopeUser, TrustClass: capsule.TrustDeclarative,
		Digest: "first-component-digest", Requirements: capsule.Requirements{Secrets: []string{"TOKEN_ONE"}},
	}
	old, err := adapter.Translate(domain.CapsuleLockSnapshot{}, "first-capsule-digest", component, []capsuleFile{{Path: "settings.json", Content: []byte(`{"token":"one"}`), Mode: 0o644}}, InstalledMaterialization{}, false, CapsuleLockMaterializationBatch{})
	if err != nil {
		t.Fatal(err)
	}

	component.Digest = "second-component-digest"
	component.Requirements = capsule.Requirements{Secrets: []string{"TOKEN_TWO"}}
	installed := InstalledMaterialization{
		ComponentID:                 component.ID,
		EffectiveCacheKey:           old.EffectiveCacheKey,
		LastAppliedDigest:           old.ContentDigest,
		CredentialRequirementDigest: old.CredentialRequirementDigest,
	}
	got, err := adapter.Translate(domain.CapsuleLockSnapshot{}, "second-capsule-digest", component, []capsuleFile{{Path: "settings.json", Content: []byte(`{"token":"two"}`), Mode: 0o644}}, installed, true, CapsuleLockMaterializationBatch{})
	if err != nil {
		t.Fatal(err)
	}
	if !got.ApprovalRequired || got.ApprovalReason != "Credential Requirement changed and requires explicit consent" {
		t.Fatalf("approval = %t/%q, want credential requirement consent", got.ApprovalRequired, got.ApprovalReason)
	}
}

func TestOpenCodeAdapterIntegrationSelectorUsesDeclaredName(t *testing.T) {
	adapter, err := capsuleAdapterFor("opencode")
	if err != nil {
		t.Fatal(err)
	}

	component := capsule.Component{ID: "integration:opencode.json#$.mcp.github", Type: capsule.ComponentTypeIntegration, Scope: capsule.ScopeUser, TrustClass: capsule.TrustDeclarative, Digest: "integration-digest"}
	got, err := adapter.Translate(domain.CapsuleLockSnapshot{}, "capsule-digest", component, []capsuleFile{{Path: "opencode.json", Content: []byte(`{"type":"remote"}`), Mode: 0o644}}, InstalledMaterialization{}, false, CapsuleLockMaterializationBatch{})
	if err != nil {
		t.Fatal(err)
	}
	if got.Selector != "$.mcp.github" {
		t.Fatalf("integration selector = %q, want $.mcp.github", got.Selector)
	}
}
