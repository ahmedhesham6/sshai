package domain_test

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/ahmedhesham6/sshai/libs/domain"
)

func TestResolveCapsuleCompositionReportsThreeCapsuleConflictAndSources(t *testing.T) {
	capsules := []domain.CapsuleComponentSet{
		{Ref: "registry.example.com/team/base:stable", Digest: sha256Digest('a'), Components: []domain.Component{compositionComponent("skill:lint", domain.ComponentSkill, 'b')}},
		{Ref: "registry.example.com/team/personal:stable", Digest: sha256Digest('c'), Components: []domain.Component{compositionComponent("skill:lint", domain.ComponentSkill, 'd')}},
		{Ref: "registry.example.com/team/project:stable", Digest: sha256Digest('e'), Components: []domain.Component{compositionComponent("skill:lint", domain.ComponentSkill, 'f')}},
	}

	_, err := domain.ResolveCapsuleComposition(capsules, nil)
	if err == nil || !errors.Is(err, domain.ErrComponentConflict) {
		t.Fatalf("ResolveCapsuleComposition() error = %v, want component conflict", err)
	}
	message := err.Error()
	for _, want := range []string{"skill:lint", capsules[0].Ref, capsules[1].Ref, capsules[2].Ref} {
		if !strings.Contains(message, want) {
			t.Fatalf("conflict error %q does not mention %q", message, want)
		}
	}
	var conflict *domain.ComponentConflictError
	if !errors.As(err, &conflict) || len(conflict.Conflicts) != 1 || len(conflict.Conflicts[0].Capsules) != 3 {
		t.Fatalf("conflict details = %#v, want one conflict with three capsules", conflict)
	}
}

func TestResolveCapsuleCompositionMergesJSONWithPrecedenceAndProvenance(t *testing.T) {
	capsules := []domain.CapsuleComponentSet{
		{Ref: "registry.example.com/team/base:stable", Digest: sha256Digest('a'), Components: []domain.Component{
			mergeableConfig("config:settings", `{"editor":{"theme":"light"},"shared":"base","baseOnly":true}`, 'b'),
		}},
		{Ref: "registry.example.com/team/personal:stable", Digest: sha256Digest('c'), Components: []domain.Component{
			mergeableConfig("config:settings", `{"editor":{"font":"mono"},"shared":"personal","personalOnly":true}`, 'd'),
		}},
	}
	project := &domain.CapsuleComponentSet{Ref: "registry.example.com/team/project:stable", Digest: sha256Digest('e'), Components: []domain.Component{
		mergeableConfig("config:settings", `{"editor":{"theme":"dark"},"shared":"project","projectOnly":true}`, 'f'),
	}}

	result, err := domain.ResolveCapsuleComposition(capsules, project)
	if err != nil {
		t.Fatalf("ResolveCapsuleComposition(): %v", err)
	}
	resolved, ok := result.Components["config:settings"]
	if !ok {
		t.Fatal("merged config is absent")
	}
	var document map[string]any
	if err := json.Unmarshal(resolved.Content, &document); err != nil {
		t.Fatalf("merged config is not JSON: %v", err)
	}
	if document["shared"] != "project" || document["baseOnly"] != true || document["personalOnly"] != true || document["projectOnly"] != true {
		t.Fatalf("merged config = %#v, want project precedence and all keys", document)
	}
	editor := document["editor"].(map[string]any)
	if editor["theme"] != "dark" || editor["font"] != "mono" {
		t.Fatalf("merged nested config = %#v, want nested merge", editor)
	}
	wantProvenance := map[string]string{
		"editor.theme": project.Ref,
		"editor.font":  capsules[1].Ref,
		"shared":       project.Ref,
		"baseOnly":     capsules[0].Ref,
		"personalOnly": capsules[1].Ref,
		"projectOnly":  project.Ref,
	}
	for key, want := range wantProvenance {
		if got := resolved.Provenance[key]; got != want {
			t.Errorf("provenance[%q] = %q, want %q", key, got, want)
		}
	}
}

func TestResolveCapsuleCompositionMixedScopeMergeUsesProjectScope(t *testing.T) {
	user := mergeableConfig("config:settings", `{"user":true}`, 'b')
	project := mergeableConfig("config:settings", `{"project":true}`, 'd')
	project.Scope = domain.ScopeProject

	result, err := domain.ResolveCapsuleComposition([]domain.CapsuleComponentSet{
		{Ref: "registry.example.com/team/user:stable", Digest: sha256Digest('a'), Components: []domain.Component{user}},
	}, &domain.CapsuleComponentSet{
		Ref: "registry.example.com/team/project:stable", Digest: sha256Digest('c'), Components: []domain.Component{project},
	})
	if err != nil {
		t.Fatalf("ResolveCapsuleComposition(): %v", err)
	}
	resolved := result.Components["config:settings"]
	if resolved.Scope != domain.ScopeProject {
		t.Fatalf("mixed-scope merged component scope = %q, want project", resolved.Scope)
	}
}

func TestResolveCapsuleCompositionAppliesExclusionsAndRecomputesPermissionReview(t *testing.T) {
	capsules := []domain.CapsuleComponentSet{
		{Ref: "registry.example.com/team/base:stable", Digest: sha256Digest('a'), Exclusions: []string{"config:excluded"}, Components: []domain.Component{
			compositionComponent("config:excluded", domain.ComponentConfig, 'b'),
			compositionComponent("permission-policy:base", domain.ComponentPermissionPolicy, 'c'),
		}},
		{Ref: "registry.example.com/team/personal:stable", Digest: sha256Digest('d'), Components: []domain.Component{
			compositionComponent("permission-policy:personal", domain.ComponentPermissionPolicy, 'e'),
		}},
	}
	result, err := domain.ResolveCapsuleComposition(capsules, nil)
	if err != nil {
		t.Fatalf("ResolveCapsuleComposition(): %v", err)
	}
	if _, excluded := result.Components["config:excluded"]; excluded {
		t.Fatal("excluded component was resolved")
	}
	if result.Classification != domain.RequiresReview || result.PermissionPolicyDigest == "" {
		t.Fatalf("permission composition = %#v, want recomputed requires-review policy", result)
	}
}

func TestResolveCapsuleCompositionMergesTOMLConfigWithPerKeyProvenance(t *testing.T) {
	capsules := []domain.CapsuleComponentSet{
		{Ref: "registry.example.com/team/base:stable", Digest: sha256Digest('a'), Components: []domain.Component{
			mergeableTOMLConfig("config:tool", "name = \"base\"\n[editor]\nfont = \"serif\"\n", 'b'),
		}},
		{Ref: "registry.example.com/team/overlay:stable", Digest: sha256Digest('c'), Components: []domain.Component{
			mergeableTOMLConfig("config:tool", "name = \"overlay\"\n[editor]\ntheme = \"dark\"\n", 'd'),
		}},
	}
	result, err := domain.ResolveCapsuleComposition(capsules, nil)
	if err != nil {
		t.Fatalf("ResolveCapsuleComposition(): %v", err)
	}
	resolved := result.Components["config:tool"]
	if !strings.Contains(string(resolved.Content), "name = 'overlay'") || !strings.Contains(string(resolved.Content), "font = 'serif'") || !strings.Contains(string(resolved.Content), "theme = 'dark'") {
		t.Fatalf("merged TOML = %q, want overlay precedence and nested keys", resolved.Content)
	}
	if resolved.Provenance["name"] != capsules[1].Ref || resolved.Provenance["editor.font"] != capsules[0].Ref || resolved.Provenance["editor.theme"] != capsules[1].Ref {
		t.Fatalf("TOML provenance = %#v", resolved.Provenance)
	}
}

func TestResolveCapsuleCompositionTOMLHashInsideQuotedStringIsMergeable(t *testing.T) {
	left := mergeableTOMLConfig("config:tool", "url = \"https://x/a#b\"\n", 'b')
	right := mergeableTOMLConfig("config:tool", "name = \"overlay\"\n", 'd')
	result, err := domain.ResolveCapsuleComposition([]domain.CapsuleComponentSet{
		{Ref: "registry.example.com/team/base:stable", Digest: sha256Digest('a'), Components: []domain.Component{left}},
		{Ref: "registry.example.com/team/overlay:stable", Digest: sha256Digest('c'), Components: []domain.Component{right}},
	}, nil)
	if err != nil {
		t.Fatalf("TOML value containing hash was rejected: %v", err)
	}
	if !strings.Contains(string(result.Components["config:tool"].Content), "https://x/a#b") {
		t.Fatalf("merged TOML = %q, want quoted hash-preserving URL", result.Components["config:tool"].Content)
	}
}

func compositionComponent(id string, componentType domain.ComponentType, digestCharacter byte) domain.Component {
	trustClass := domain.TrustClass(domain.TrustDeclarative)
	if componentType == domain.ComponentPermissionPolicy {
		trustClass = domain.TrustPermission
	}
	return domain.Component{ID: id, Type: componentType, MediaType: "application/octet-stream", Digest: sha256Digest(digestCharacter), Scope: domain.ScopeUser, TrustClass: trustClass}
}

func mergeableConfig(id, content string, digestCharacter byte) domain.Component {
	component := compositionComponent(id, domain.ComponentConfig, digestCharacter)
	component.MediaType = "application/json"
	component.Content = []byte(content)
	return component
}

func mergeableTOMLConfig(id, content string, digestCharacter byte) domain.Component {
	component := compositionComponent(id, domain.ComponentConfig, digestCharacter)
	component.MediaType = "application/toml"
	component.Content = []byte(content)
	return component
}
