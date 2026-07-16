package domain_test

import (
	"testing"

	"github.com/ahmedhesham6/sshai/libs/domain"
)

func TestComponentValidation(t *testing.T) {
	base := domain.Component{
		ID: "config:editor", Type: domain.ComponentConfig, MediaType: "application/json",
		Digest: sha256Digest('a'), SizeBytes: 42, Scope: domain.ScopeUser, TrustClass: domain.TrustDeclarative,
		Requirements: domain.ComponentRequirements{Commands: []string{"git"}, Secrets: []string{"github-token"}},
	}
	for _, test := range []struct {
		name   string
		mutate func(*domain.Component)
	}{
		{name: "valid", mutate: func(*domain.Component) {}},
		{name: "missing ID", mutate: func(component *domain.Component) { component.ID = "" }},
		{name: "unqualified ID", mutate: func(component *domain.Component) { component.ID = "editor" }},
		{name: "ID type mismatch", mutate: func(component *domain.Component) { component.ID = "skill:editor" }},
		{name: "invalid type", mutate: func(component *domain.Component) { component.Type = domain.ComponentType("unknown") }},
		{name: "missing media type", mutate: func(component *domain.Component) { component.MediaType = " " }},
		{name: "invalid digest", mutate: func(component *domain.Component) { component.Digest = "invalid" }},
		{name: "negative size", mutate: func(component *domain.Component) { component.SizeBytes = -1 }},
		{name: "invalid scope", mutate: func(component *domain.Component) { component.Scope = domain.ComponentScope("machine") }},
		{name: "invalid trust", mutate: func(component *domain.Component) { component.TrustClass = domain.TrustClass("trusted") }},
		{name: "empty command requirement", mutate: func(component *domain.Component) { component.Requirements.Commands = []string{" "} }},
		{name: "empty secret requirement", mutate: func(component *domain.Component) { component.Requirements.Secrets = []string{""} }},
	} {
		t.Run(test.name, func(t *testing.T) {
			component := base
			test.mutate(&component)
			if err := component.Validate(); (test.name == "valid") != (err == nil) {
				t.Fatalf("Component.Validate() error = %v", err)
			}
		})
	}
}

func TestComponentEnumsMatchCapsulePackagingValues(t *testing.T) {
	// Keep these domain values aligned with libs/capsule/model.go; this package
	// intentionally does not import the packaging-side model.
	if got := []domain.ComponentType{
		domain.ComponentConfig, domain.ComponentSkill, domain.ComponentCommand, domain.ComponentSubagent,
		domain.ComponentHook, domain.ComponentIntegration, domain.ComponentPermissionPolicy,
		domain.ComponentTemplate, domain.ComponentExtension,
	}; len(got) != 9 || string(got[5]) != "integration" || string(got[6]) != "permission-policy" {
		t.Fatalf("Component type values = %#v", got)
	}
	if domain.ScopeUser != "user" || domain.ScopeProject != "project" ||
		domain.TrustDeclarative != "declarative" || domain.TrustExecutable != "executable" || domain.TrustPermission != "permission" {
		t.Fatal("Component scope or trust values do not match the packaging contract")
	}
}

func TestComponentValidationRejectsTypeTrustMismatches(t *testing.T) {
	for _, test := range []struct {
		name          string
		componentType domain.ComponentType
		trustClass    domain.TrustClass
	}{
		{name: "permission policy declared", componentType: domain.ComponentPermissionPolicy, trustClass: domain.TrustDeclarative},
		{name: "permission policy executable", componentType: domain.ComponentPermissionPolicy, trustClass: domain.TrustExecutable},
		{name: "hook declared", componentType: domain.ComponentHook, trustClass: domain.TrustDeclarative},
		{name: "hook permission", componentType: domain.ComponentHook, trustClass: domain.TrustPermission},
		{name: "extension declared", componentType: domain.ComponentExtension, trustClass: domain.TrustDeclarative},
		{name: "extension permission", componentType: domain.ComponentExtension, trustClass: domain.TrustPermission},
	} {
		t.Run(test.name, func(t *testing.T) {
			component := domain.Component{
				ID: "", Type: test.componentType, MediaType: "application/json", Digest: sha256Digest('a'),
				SizeBytes: 1, Scope: domain.ScopeUser, TrustClass: test.trustClass,
			}
			component.ID = string(component.Type) + ":item"
			if err := component.Validate(); err == nil {
				t.Fatalf("Component.Validate() accepted %s with trust %s", component.Type, component.TrustClass)
			}
		})
	}
}

func TestComponentValidationAcceptsRequiredTypeTrustClasses(t *testing.T) {
	for _, component := range []domain.Component{
		{ID: "permission-policy:workspace", Type: domain.ComponentPermissionPolicy, MediaType: "application/json", Digest: sha256Digest('a'), Scope: domain.ScopeUser, TrustClass: domain.TrustPermission},
		{ID: "hook:format", Type: domain.ComponentHook, MediaType: "application/json", Digest: sha256Digest('b'), Scope: domain.ScopeUser, TrustClass: domain.TrustExecutable},
		{ID: "extension:agent", Type: domain.ComponentExtension, MediaType: "application/json", Digest: sha256Digest('c'), Scope: domain.ScopeUser, TrustClass: domain.TrustExecutable},
	} {
		if err := component.Validate(); err != nil {
			t.Fatalf("Component.Validate(%s, %s) = %v", component.Type, component.TrustClass, err)
		}
	}
}
