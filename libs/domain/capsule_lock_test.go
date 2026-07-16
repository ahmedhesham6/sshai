package domain_test

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/ahmedhesham6/sshai/libs/domain"
)

func TestCreateCapsuleLockValidatesAndCopiesState(t *testing.T) {
	createdAt := time.Date(2026, time.July, 13, 12, 0, 0, 0, time.UTC)
	capsuleDigest := sha256Digest('b')
	snapshot := domain.CapsuleLockSnapshot{
		ID: "lock-1", EnvironmentID: "environment-1", ProfileVersionID: "version-1",
		ProjectCapsuleDigest: sha256Digest('a'),
		Capsules:             []domain.LockedCapsule{{Ref: "registry.example.com/team/base:stable", Digest: capsuleDigest}},
		ResolvedComponents: map[string]domain.ResolvedComponent{
			"config:editor": {
				ID: "config:editor", CapsuleDigest: capsuleDigest, ComponentDigest: sha256Digest('c'),
				Scope: domain.ScopeUser, TrustClass: domain.TrustDeclarative,
			},
		},
		CreatedAt: createdAt,
	}
	snapshot.Digest = domain.ComputeCapsuleLockDigest(snapshot)
	lock, err := domain.CreateCapsuleLock(snapshot)
	if err != nil {
		t.Fatalf("CreateCapsuleLock(): %v", err)
	}
	snapshot.Capsules[0].Ref = "changed"
	snapshot.ResolvedComponents["config:editor"] = domain.ResolvedComponent{}
	got := lock.Snapshot()
	if got.ID != "lock-1" || got.Capsules[0].Ref != "registry.example.com/team/base:stable" || got.ResolvedComponents["config:editor"].ID != "config:editor" {
		t.Fatalf("Capsule Lock state = %#v", got)
	}
	got.Capsules[0].Ref = "changed"
	got.ResolvedComponents["config:editor"] = domain.ResolvedComponent{}
	if lock.Snapshot().Capsules[0].Ref != "registry.example.com/team/base:stable" || lock.Snapshot().ResolvedComponents["config:editor"].ID != "config:editor" {
		t.Fatal("Capsule Lock snapshot was mutable")
	}
}

func TestCreateCapsuleLockAcceptsProjectComponentFromProjectCapsule(t *testing.T) {
	snapshot := validCapsuleLockSnapshot()
	snapshot.ResolvedComponents["config:project"] = domain.ResolvedComponent{
		ID: "config:project", CapsuleDigest: snapshot.ProjectCapsuleDigest, ComponentDigest: sha256Digest('e'),
		Scope: domain.ScopeProject, TrustClass: domain.TrustDeclarative,
	}
	snapshot.Digest = ""

	if _, err := domain.CreateCapsuleLock(snapshot); err != nil {
		t.Fatalf("CreateCapsuleLock() rejected a project Capsule component: %v", err)
	}
}

func TestCapsuleLockRejectsInvalidState(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(*domain.CapsuleLockSnapshot)
	}{
		{name: "missing ID", mutate: func(snapshot *domain.CapsuleLockSnapshot) { snapshot.ID = "" }},
		{name: "missing capsules", mutate: func(snapshot *domain.CapsuleLockSnapshot) { snapshot.Capsules = nil }},
		{name: "invalid project capsule digest", mutate: func(snapshot *domain.CapsuleLockSnapshot) { snapshot.ProjectCapsuleDigest = "invalid" }},
		{name: "invalid locked capsule digest", mutate: func(snapshot *domain.CapsuleLockSnapshot) { snapshot.Capsules[0].Digest = "invalid" }},
		{name: "unlisted resolved capsule", mutate: func(snapshot *domain.CapsuleLockSnapshot) {
			component := snapshot.ResolvedComponents["config:editor"]
			component.CapsuleDigest = sha256Digest('z')
			snapshot.ResolvedComponents["config:editor"] = component
		}},
		{name: "invalid component digest", mutate: func(snapshot *domain.CapsuleLockSnapshot) {
			component := snapshot.ResolvedComponents["config:editor"]
			component.ComponentDigest = "invalid"
			snapshot.ResolvedComponents["config:editor"] = component
		}},
		{name: "resolved key mismatch", mutate: func(snapshot *domain.CapsuleLockSnapshot) {
			component := snapshot.ResolvedComponents["config:editor"]
			component.ID = "config:other"
			snapshot.ResolvedComponents["config:editor"] = component
		}},
		{name: "invalid scope", mutate: func(snapshot *domain.CapsuleLockSnapshot) {
			component := snapshot.ResolvedComponents["config:editor"]
			component.Scope = domain.ComponentScope("machine")
			snapshot.ResolvedComponents["config:editor"] = component
		}},
		{name: "invalid trust", mutate: func(snapshot *domain.CapsuleLockSnapshot) {
			component := snapshot.ResolvedComponents["config:editor"]
			component.TrustClass = domain.TrustClass("trusted")
			snapshot.ResolvedComponents["config:editor"] = component
		}},
		{name: "invalid lock digest", mutate: func(snapshot *domain.CapsuleLockSnapshot) { snapshot.Digest = "invalid" }},
		{name: "missing creation time", mutate: func(snapshot *domain.CapsuleLockSnapshot) { snapshot.CreatedAt = time.Time{} }},
		{name: "non-UTC creation time", mutate: func(snapshot *domain.CapsuleLockSnapshot) {
			snapshot.CreatedAt = time.Date(2026, time.July, 13, 12, 0, 0, 0, time.FixedZone("UTC+0", 0))
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			snapshot := validCapsuleLockSnapshot()
			test.mutate(&snapshot)
			if _, err := domain.CreateCapsuleLock(snapshot); err == nil {
				t.Fatal("invalid Capsule Lock was accepted")
			}
		})
	}
}

func TestCreateCapsuleLockComputesAndVerifiesContentDigest(t *testing.T) {
	snapshot := validCapsuleLockSnapshot()
	provided := snapshot.Digest
	snapshot.Digest = ""
	lock, err := domain.CreateCapsuleLock(snapshot)
	if err != nil {
		t.Fatalf("CreateCapsuleLock() with empty digest: %v", err)
	}
	if got := lock.Snapshot().Digest; got != provided || got != domain.ComputeCapsuleLockDigest(snapshot) {
		t.Fatalf("computed Capsule Lock digest = %q, want %q", got, provided)
	}

	tampered := validCapsuleLockSnapshot()
	tampered.ProjectCapsuleDigest = sha256Digest('z')
	if domain.ComputeCapsuleLockDigest(tampered) == provided {
		t.Fatal("tampering with a lock field did not change the computed digest")
	}
	if _, err := domain.CreateCapsuleLock(tampered); err == nil {
		t.Fatal("CreateCapsuleLock() accepted a digest that did not cover a tampered field")
	}
}

func TestResolveComponentsDeduplicatesAndClassifiesReview(t *testing.T) {
	base := component("config:editor", domain.ComponentConfig, 'a')
	resolved, classification, err := domain.ResolveComponents([][]domain.Component{{base}, {base}})
	if err != nil {
		t.Fatalf("ResolveComponents(): %v", err)
	}
	if len(resolved) != 1 || resolved["config:editor"].Digest != sha256Digest('a') || classification != domain.AutoSafe {
		t.Fatalf("resolved=%#v classification=%q", resolved, classification)
	}

	permission := component("permission-policy:workspace", domain.ComponentPermissionPolicy, 'b')
	permission.TrustClass = domain.TrustPermission
	_, classification, err = domain.ResolveComponents([][]domain.Component{{permission}})
	if err != nil || classification != domain.RequiresReview || !classification.NeverAutoSafe() {
		t.Fatalf("permission classification=%q error=%v", classification, err)
	}
	integration := component("integration:github", domain.ComponentIntegration, 'c')
	_, classification, err = domain.ResolveComponents([][]domain.Component{{integration}})
	if err != nil || classification != domain.RequiresReview || !domain.IsNeverAutoSafe(classification) {
		t.Fatalf("integration classification=%q error=%v", classification, err)
	}
	withSecret := component("config:credentialed", domain.ComponentConfig, 'd')
	withSecret.Requirements.Secrets = []string{"github-token"}
	_, classification, err = domain.ResolveComponents([][]domain.Component{{withSecret}})
	if err != nil || classification != domain.RequiresReview {
		t.Fatalf("credential classification=%q error=%v", classification, err)
	}
}

func TestResolveComponentsReportsConflictingIDs(t *testing.T) {
	first := component("config:editor", domain.ComponentConfig, 'a')
	second := component("config:editor", domain.ComponentConfig, 'b')
	otherFirst := component("skill:lint", domain.ComponentSkill, 'c')
	otherSecond := component("skill:lint", domain.ComponentSkill, 'd')
	_, _, err := domain.ResolveComponents([][]domain.Component{{first, otherFirst}, {second, otherSecond}})
	if err == nil || !errors.Is(err, domain.ErrComponentConflict) || !strings.Contains(err.Error(), "config:editor") || !strings.Contains(err.Error(), "skill:lint") {
		t.Fatalf("conflict error = %v", err)
	}
}

func TestResolveComponentsFlagsCredentialRequirementChanges(t *testing.T) {
	first := component("config:editor", domain.ComponentConfig, 'a')
	second := first
	second.Requirements.Secrets = []string{"new-token"}
	_, classification, err := domain.ResolveComponents([][]domain.Component{{first}, {second}})
	if err != nil || classification != domain.RequiresReview {
		t.Fatalf("credential requirement classification=%q error=%v", classification, err)
	}
}

func TestResolveComponentsRequiresReviewForExecutableTypes(t *testing.T) {
	hook := component("hook:format", domain.ComponentHook, 'e')
	hook.TrustClass = domain.TrustExecutable
	extension := component("extension:agent", domain.ComponentExtension, 'f')
	extension.TrustClass = domain.TrustExecutable
	for _, component := range []domain.Component{
		hook,
		extension,
	} {
		_, classification, err := domain.ResolveComponents([][]domain.Component{{component}})
		if err != nil || classification != domain.RequiresReview {
			t.Fatalf("%s classification=%q error=%v, want requires_review", component.Type, classification, err)
		}
	}
}

func validCapsuleLockSnapshot() domain.CapsuleLockSnapshot {
	capsuleDigest := sha256Digest('b')
	snapshot := domain.CapsuleLockSnapshot{
		ID: "lock-1", EnvironmentID: "environment-1", ProfileVersionID: "version-1",
		ProjectCapsuleDigest: sha256Digest('a'),
		Capsules:             []domain.LockedCapsule{{Ref: "registry.example.com/team/base:stable", Digest: capsuleDigest}},
		ResolvedComponents: map[string]domain.ResolvedComponent{
			"config:editor": {ID: "config:editor", CapsuleDigest: capsuleDigest, ComponentDigest: sha256Digest('c'), Scope: domain.ScopeUser, TrustClass: domain.TrustDeclarative},
		},
		CreatedAt: time.Date(2026, time.July, 13, 12, 0, 0, 0, time.UTC),
	}
	snapshot.Digest = domain.ComputeCapsuleLockDigest(snapshot)
	return snapshot
}

func component(id string, componentType domain.ComponentType, digestCharacter byte) domain.Component {
	return domain.Component{ID: id, Type: componentType, MediaType: "application/json", Digest: sha256Digest(digestCharacter), Scope: domain.ScopeUser, TrustClass: domain.TrustDeclarative}
}
