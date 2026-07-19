package guest_test

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ahmedhesham6/sshai/apps/guest"
	"github.com/ahmedhesham6/sshai/libs/capsule"
	"github.com/ahmedhesham6/sshai/libs/capsule/oci"
	"github.com/ahmedhesham6/sshai/libs/domain"
	"github.com/ahmedhesham6/sshai/libs/profile"
)

func TestMaterializeCapsuleLockClaudeEndToEndUsesVerifiedCacheAndNoOp(t *testing.T) {
	claudeCapsule := buildClaudeCapsule(t, []claudeComponentFixture{
		{component: capsule.Component{ID: "skill:review", Type: capsule.ComponentTypeSkill, Scope: capsule.ScopeUser, TrustClass: capsule.TrustDeclarative}, files: map[string]string{"SKILL.md": "# Review\n"}},
		{component: capsule.Component{ID: "subagent:review", Type: capsule.ComponentTypeSubagent, Scope: capsule.ScopeUser, TrustClass: capsule.TrustDeclarative}, files: map[string]string{"review.md": "Review subagent\n"}},
		{component: capsule.Component{ID: "config:.claude/settings.json#$.permissions", Type: capsule.ComponentTypeConfig, Scope: capsule.ScopeUser, TrustClass: capsule.TrustDeclarative}, files: map[string]string{".claude/settings.json": `["Read"]`}},
		{component: capsule.Component{ID: "integration:.mcp.json", Type: capsule.ComponentTypeIntegration, Scope: capsule.ScopeProject, TrustClass: capsule.TrustDeclarative}, files: map[string]string{".mcp.json": `{"mcpServers":{"local":{"command":"echo"}}}`}},
		{component: capsule.Component{ID: "config:CLAUDE.md", Type: capsule.ComponentTypeConfig, Scope: capsule.ScopeUser, TrustClass: capsule.TrustDeclarative}, files: map[string]string{"CLAUDE.md": "Project instructions\n"}},
	})
	provider := newCapsuleObjectProvider(t)
	publisher, err := oci.NewClient("owner-1", provider)
	if err != nil {
		t.Fatalf("create publisher: %v", err)
	}
	if _, err := publisher.Publish(t.Context(), claudeCapsule); err != nil {
		t.Fatalf("publish Capsule: %v", err)
	}
	lock := capsuleLockFor(t, claudeCapsule)
	home, workspace, cache := t.TempDir(), t.TempDir(), t.TempDir()
	request := guest.CapsuleLockMaterializationBatch{
		Lock: lock, OwnerID: "owner-1", Grants: provider, CacheRoot: cache,
		HomeRoot: home, WorkspaceRoot: workspace, Intent: profile.IntentReconcile,
		Approvals: map[string]guest.ApprovalMarker{
			"integration:.mcp.json": {
				ComponentID: "integration:.mcp.json", ComponentDigest: lock.Snapshot().ResolvedComponents["integration:.mcp.json"].ComponentDigest,
				LockID: lock.Snapshot().ID, LockDigest: lock.Snapshot().Digest,
			},
			"config:.claude/settings.json#$.permissions": {
				ComponentID: "config:.claude/settings.json#$.permissions", ComponentDigest: lock.Snapshot().ResolvedComponents["config:.claude/settings.json#$.permissions"].ComponentDigest,
				LockID: lock.Snapshot().ID, LockDigest: lock.Snapshot().Digest,
			},
		},
		TargetAgentVersion: "claude-1",
	}
	first, err := guest.MaterializeCapsuleLock(t.Context(), request)
	if err != nil {
		t.Fatalf("first lock materialization: %v", err)
	}
	assertClaudeFile(t, filepath.Join(home, ".claude", "skills", "review", "SKILL.md"), "# Review\n")
	assertClaudeFile(t, filepath.Join(home, ".claude", "agents", "review.md"), "Review subagent\n")
	assertClaudeFile(t, filepath.Join(home, ".claude", "settings.json"), `{"permissions":["Read"]}`)
	assertClaudeFile(t, filepath.Join(home, "CLAUDE.md"), "Project instructions\n")
	assertClaudeFile(t, filepath.Join(workspace, ".mcp.json"), `{"mcpServers":{"local":{"command":"echo"}}}`)
	if got := countGrantReads(provider); got == 0 {
		t.Fatal("cold materialization did not read the local Capsule object store")
	}
	for _, result := range first {
		if result.ComponentID == "" || result.ComponentDigest == "" || result.Adapter != "claude" || result.AdapterVersion != "v1" || result.TargetAgentVersion != "claude-1" || result.EffectiveCacheKey == "" {
			t.Fatalf("installed result did not record the effective cache key fields: %#v", result)
		}
	}

	installed := guest.InstalledMaterializationsFromResults(first)
	provider.resetReads()
	secondRequest := request
	secondRequest.Installed = installed
	second, err := guest.MaterializeCapsuleLock(t.Context(), secondRequest)
	if err != nil {
		t.Fatalf("warm lock materialization: %v", err)
	}
	if got := countGrantReads(provider); got != 0 {
		t.Fatalf("warm materialization read the object store %d times", got)
	}
	for _, result := range second {
		if result.Operation != profile.OperationSkip {
			t.Fatalf("warm result for %q = %s, want skip", result.ComponentID, result.Operation)
		}
	}

	layerDigest := lock.Snapshot().ResolvedComponents["skill:review"].ComponentDigest
	cacheBlob := filepath.Join(cache, "capsule-oci", "blobs", "sha256", strings.TrimPrefix(layerDigest, "sha256:"))
	if err := os.Chmod(cacheBlob, 0o600); err != nil {
		t.Fatalf("make cached component writable: %v", err)
	}
	if err := os.WriteFile(cacheBlob, []byte("tampered"), 0o600); err != nil {
		t.Fatalf("tamper cached component: %v", err)
	}
	provider.resetReads()
	if _, err := guest.MaterializeCapsuleLock(t.Context(), secondRequest); err == nil {
		t.Fatal("tampered cached component was accepted")
	} else if !errors.Is(err, guest.ErrCapsuleContentInvalid) {
		t.Fatalf("tampered cached component error = %v, want immutable content classification", err)
	}
	if got := countGrantReads(provider); got != 0 {
		t.Fatalf("tampered cache fell back to the object store with %d reads", got)
	}
}

func TestMergedConfigLockMaterializesFromSourceLayersAndRerunIsNoOp(t *testing.T) {
	baseContent := `{"editor":{"theme":"light"},"base":true}`
	overlayContent := `{"editor":{"font":"mono"},"overlay":true}`
	baseCapsule := buildClaudeCapsule(t, []claudeComponentFixture{{
		component: capsule.Component{ID: "config:settings", Type: capsule.ComponentTypeConfig, Scope: capsule.ScopeUser, TrustClass: capsule.TrustDeclarative},
		files:     map[string]string{"settings.json": baseContent},
	}})
	overlayCapsule := buildClaudeCapsule(t, []claudeComponentFixture{{
		component: capsule.Component{ID: "config:settings", Type: capsule.ComponentTypeConfig, Scope: capsule.ScopeUser, TrustClass: capsule.TrustDeclarative},
		files:     map[string]string{"settings.json": overlayContent},
	}})
	baseLayer := baseCapsule.Manifest.Components[0]
	overlayLayer := overlayCapsule.Manifest.Components[0]
	composition, err := domain.ResolveCapsuleComposition([]domain.CapsuleComponentSet{
		{Ref: "registry.example.com/team/base:stable", Digest: baseCapsule.Digest, Components: []domain.Component{{
			ID: baseLayer.ID, Type: domain.ComponentConfig, MediaType: "application/json", Digest: baseLayer.Digest,
			SizeBytes: baseLayer.SizeBytes, Scope: domain.ScopeUser, TrustClass: domain.TrustDeclarative, Content: []byte(baseContent),
		}}},
		{Ref: "registry.example.com/team/overlay:stable", Digest: overlayCapsule.Digest, Components: []domain.Component{{
			ID: overlayLayer.ID, Type: domain.ComponentConfig, MediaType: "application/json", Digest: overlayLayer.Digest,
			SizeBytes: overlayLayer.SizeBytes, Scope: domain.ScopeUser, TrustClass: domain.TrustDeclarative, Content: []byte(overlayContent),
		}}},
	}, nil)
	if err != nil {
		t.Fatalf("compose merged config: %v", err)
	}
	resolved := composition.Components["config:settings"]
	lock, err := domain.CreateCapsuleLock(domain.CapsuleLockSnapshot{
		ID: "lock-merged", EnvironmentID: "environment-1", ProfileVersionID: "profile-1", ProjectCapsuleDigest: baseCapsule.Digest,
		Capsules: []domain.LockedCapsule{
			{Ref: "registry.example.com/team/base:stable", Digest: baseCapsule.Digest},
			{Ref: "registry.example.com/team/overlay:stable", Digest: overlayCapsule.Digest},
		},
		ResolvedComponents: map[string]domain.ResolvedComponent{"config:settings": {
			ID: "config:settings", Type: resolved.Type, CapsuleDigest: composition.ComponentCapsuleDigests["config:settings"], ComponentDigest: resolved.Digest,
			Scope: resolved.Scope, TrustClass: resolved.TrustClass, Requirements: resolved.Requirements, Sources: composition.ComponentSources["config:settings"],
		}},
		CreatedAt: time.Date(2026, time.July, 16, 12, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatalf("create merged Capsule Lock: %v", err)
	}
	if len(lock.Snapshot().ResolvedComponents["config:settings"].Sources) != 2 {
		t.Fatalf("merged lock sources = %#v, want both source layers", lock.Snapshot().ResolvedComponents["config:settings"].Sources)
	}

	provider := newCapsuleObjectProvider(t)
	publishCapsule(t, provider, baseCapsule)
	publishCapsule(t, provider, overlayCapsule)
	request := guest.CapsuleLockMaterializationBatch{
		Lock: lock, OwnerID: "owner-1", Grants: provider, CacheRoot: t.TempDir(), HomeRoot: t.TempDir(), WorkspaceRoot: t.TempDir(),
		Intent: profile.IntentReconcile, TargetAgentVersion: "claude-1",
	}
	first, err := guest.MaterializeCapsuleLock(t.Context(), request)
	if err != nil {
		t.Fatalf("materialize merged Capsule Lock: %v", err)
	}
	var document map[string]any
	content, err := os.ReadFile(filepath.Join(request.HomeRoot, "settings.json"))
	if err != nil {
		t.Fatalf("read merged settings: %v", err)
	}
	if err := json.Unmarshal(content, &document); err != nil {
		t.Fatalf("parse materialized merged settings: %v", err)
	}
	if document["base"] != true || document["overlay"] != true || document["editor"].(map[string]any)["theme"] != "light" || document["editor"].(map[string]any)["font"] != "mono" {
		t.Fatalf("materialized merged settings = %#v, want both source values", document)
	}

	request.Installed = guest.InstalledMaterializationsFromResults(first)
	second, err := guest.MaterializeCapsuleLock(t.Context(), request)
	if err != nil {
		t.Fatalf("rerun merged Capsule Lock: %v", err)
	}
	if len(second) != 1 || second[0].Operation != profile.OperationSkip {
		t.Fatalf("rerun merged materialization = %#v, want one no-op", second)
	}
}

func TestAgentVersionChangeInvalidatesMaterializationCacheKey(t *testing.T) {
	value := buildClaudeCapsule(t, []claudeComponentFixture{{
		component: capsule.Component{ID: "config:CLAUDE.md", Type: capsule.ComponentTypeConfig, Scope: capsule.ScopeUser, TrustClass: capsule.TrustDeclarative},
		files:     map[string]string{"CLAUDE.md": "instructions\n"},
	}})
	provider := newCapsuleObjectProvider(t)
	publishCapsule(t, provider, value)
	request := lockMaterializationRequest(t, provider, value)
	first, err := guest.MaterializeCapsuleLock(t.Context(), request)
	if err != nil {
		t.Fatalf("initial materialization: %v", err)
	}
	secondRequest := request
	secondRequest.TargetAgentVersion = "claude-2"
	secondRequest.Installed = guest.InstalledMaterializationsFromResults(first)
	second, err := guest.MaterializeCapsuleLock(t.Context(), secondRequest)
	if err != nil || len(second) != 1 || second[0].Operation != profile.OperationUpdate {
		t.Fatalf("agent version cache invalidation = %#v, err=%v; want update", second, err)
	}
}

func TestPruneRemovedSkillUsesPersistedDirectoryShape(t *testing.T) {
	value := buildClaudeCapsule(t, []claudeComponentFixture{{
		component: capsule.Component{ID: "skill:review", Type: capsule.ComponentTypeSkill, Scope: capsule.ScopeUser, TrustClass: capsule.TrustDeclarative},
		files:     map[string]string{"SKILL.md": "review\n", "scripts/run.sh": "#!/bin/sh\n"},
	}})
	empty := buildClaudeCapsule(t, nil)
	provider := newCapsuleObjectProvider(t)
	publishCapsule(t, provider, value)
	publishCapsule(t, provider, empty)
	request := lockMaterializationRequest(t, provider, value)
	first, err := guest.MaterializeCapsuleLock(t.Context(), request)
	if err != nil {
		t.Fatalf("seed skill: %v", err)
	}
	skillDir := filepath.Join(request.HomeRoot, ".claude", "skills", "review")
	if info, statErr := os.Stat(skillDir); statErr != nil || !info.IsDir() {
		t.Fatalf("seeded skill shape = %v/%v; want directory", info, statErr)
	}

	pruneRequest := request
	pruneRequest.Lock = capsuleLockFor(t, empty)
	pruneRequest.Installed = guest.InstalledMaterializationsFromResults(first)
	pruneRequest.Intent = profile.IntentPrune
	results, err := guest.MaterializeCapsuleLock(t.Context(), pruneRequest)
	if err != nil || len(results) != 1 || results[0].Operation != profile.OperationRemove {
		t.Fatalf("removed skill prune = %#v, err=%v; want directory removal", results, err)
	}
	if _, statErr := os.Stat(skillDir); !os.IsNotExist(statErr) {
		t.Fatalf("removed skill directory remains or was misread as a file: %v", statErr)
	}
}

func TestDuplicateLayerEntryIsRejectedBeforeAnyMaterialization(t *testing.T) {
	value := buildClaudeCapsule(t, []claudeComponentFixture{
		{component: capsule.Component{ID: "config:first", Type: capsule.ComponentTypeConfig, Scope: capsule.ScopeUser, TrustClass: capsule.TrustDeclarative}, files: map[string]string{"first.md": "first\n"}},
		{component: capsule.Component{ID: "config:second", Type: capsule.ComponentTypeConfig, Scope: capsule.ScopeUser, TrustClass: capsule.TrustDeclarative}, files: map[string]string{"second.md": "second\n"}},
	})
	duplicate := duplicateTarEntry(t, value.Layers[1].Bytes, "second.md")
	value.Layers[1].Bytes = duplicate
	value.Layers[1].Digest = materializationDigest(duplicate)
	value.Layers[1].SizeBytes = int64(len(duplicate))
	value.Manifest.Components[1].Digest = value.Layers[1].Digest
	value.Manifest.Components[1].SizeBytes = value.Layers[1].SizeBytes
	value.Digest, _ = capsule.ComputeCapsuleDigest(value.Manifest)

	provider := newCapsuleObjectProvider(t)
	publishCapsule(t, provider, value)
	request := lockMaterializationRequest(t, provider, value)
	results, err := guest.MaterializeCapsuleLock(t.Context(), request)
	if err == nil {
		t.Fatalf("duplicate layer entry was accepted: %#v", results)
	}
	for _, target := range []string{"first.md", "second.md"} {
		if _, statErr := os.Stat(filepath.Join(request.HomeRoot, target)); !os.IsNotExist(statErr) {
			t.Fatalf("duplicate layer failure partially materialized %s: %v", target, statErr)
		}
	}
}

func TestClaudePermissionComponentRequiresExplicitConsent(t *testing.T) {
	value := buildClaudeCapsule(t, []claudeComponentFixture{{
		component: capsule.Component{ID: "permission-policy:workspace", Type: capsule.ComponentTypePermissionPolicy, Scope: capsule.ScopeUser, TrustClass: capsule.TrustPermission},
		files:     map[string]string{"settings.json": `{"allow":["Bash"]}`},
	}})
	provider := newCapsuleObjectProvider(t)
	publishCapsule(t, provider, value)
	request := lockMaterializationRequest(t, provider, value)
	results, err := guest.MaterializeCapsuleLock(t.Context(), request)
	if !errors.Is(err, guest.ErrProfileMaterializationBlocked) || results[0].Operation != profile.OperationRequiresInput {
		t.Fatalf("permission materialization = %#v, err=%v; want explicit consent", results, err)
	}
	if _, statErr := os.Stat(filepath.Join(request.HomeRoot, ".claude", "settings.json")); !os.IsNotExist(statErr) {
		t.Fatalf("permission component was auto-applied: %v", statErr)
	}
}

func TestClaudeIntegrationComponentNeverAutoApplies(t *testing.T) {
	value := buildClaudeCapsule(t, []claudeComponentFixture{{
		component: capsule.Component{ID: "integration:.mcp.json", Type: capsule.ComponentTypeIntegration, Scope: capsule.ScopeUser, TrustClass: capsule.TrustDeclarative},
		files:     map[string]string{".mcp.json": `{"mcpServers":{"local":{"command":"echo"}}}`},
	}})
	provider := newCapsuleObjectProvider(t)
	publishCapsule(t, provider, value)
	request := lockMaterializationRequest(t, provider, value)
	results, err := guest.MaterializeCapsuleLock(t.Context(), request)
	if !errors.Is(err, guest.ErrProfileMaterializationBlocked) || results[0].Operation != profile.OperationRequiresInput {
		t.Fatalf("integration materialization = %#v, err=%v; want no auto-apply", results, err)
	}
	if _, statErr := os.Stat(filepath.Join(request.HomeRoot, ".mcp.json")); !os.IsNotExist(statErr) {
		t.Fatalf("integration component was auto-applied: %v", statErr)
	}
}

func TestClaudeCredentialRequirementChangeRequiresExplicitConsent(t *testing.T) {
	firstValue := buildClaudeCapsule(t, []claudeComponentFixture{{
		component: capsule.Component{ID: "config:CLAUDE.md", Type: capsule.ComponentTypeConfig, Scope: capsule.ScopeUser, TrustClass: capsule.TrustDeclarative, Requirements: capsule.Requirements{Secrets: []string{"TOKEN_ONE"}}},
		files:     map[string]string{"CLAUDE.md": "instructions\n"},
	}})
	secondValue := buildClaudeCapsule(t, []claudeComponentFixture{{
		component: capsule.Component{ID: "config:CLAUDE.md", Type: capsule.ComponentTypeConfig, Scope: capsule.ScopeUser, TrustClass: capsule.TrustDeclarative, Requirements: capsule.Requirements{Secrets: []string{"TOKEN_TWO"}}},
		files:     map[string]string{"CLAUDE.md": "instructions changed\n"},
	}})
	provider := newCapsuleObjectProvider(t)
	publishCapsule(t, provider, firstValue)
	firstRequest := lockMaterializationRequest(t, provider, firstValue)
	firstRequest.Approvals = approvalForLock(t, firstRequest.Lock, "config:CLAUDE.md")
	first, err := guest.MaterializeCapsuleLock(t.Context(), firstRequest)
	if err != nil {
		t.Fatalf("initial requirement materialization: %v", err)
	}
	publishCapsule(t, provider, secondValue)
	secondRequest := firstRequest
	secondRequest.Lock = capsuleLockFor(t, secondValue)
	secondRequest.Installed = guest.InstalledMaterializationsFromResults(first)
	results, err := guest.MaterializeCapsuleLock(t.Context(), secondRequest)
	if !errors.Is(err, guest.ErrProfileMaterializationBlocked) || results[0].Operation != profile.OperationRequiresInput {
		t.Fatalf("Credential Requirement change = %#v, err=%v; want explicit consent", results, err)
	}
}

func TestClaudeExecutableDigestChangeRequiresRenewedReview(t *testing.T) {
	firstCapsule := buildClaudeCapsule(t, []claudeComponentFixture{{
		component: capsule.Component{ID: "skill:review", Type: capsule.ComponentTypeSkill, Scope: capsule.ScopeUser, TrustClass: capsule.TrustExecutable},
		files:     map[string]string{"SKILL.md": "first\n"},
	}})
	secondCapsule := buildClaudeCapsule(t, []claudeComponentFixture{{
		component: capsule.Component{ID: "skill:review", Type: capsule.ComponentTypeSkill, Scope: capsule.ScopeUser, TrustClass: capsule.TrustExecutable},
		files:     map[string]string{"SKILL.md": "second\n"},
	}})
	provider := newCapsuleObjectProvider(t)
	publishCapsule(t, provider, firstCapsule)
	firstRequest := lockMaterializationRequest(t, provider, firstCapsule)
	firstRequest.Approvals = approvalForLock(t, firstRequest.Lock, "skill:review")
	first, err := guest.MaterializeCapsuleLock(t.Context(), firstRequest)
	if err != nil {
		t.Fatalf("initial executable materialization: %v", err)
	}
	publishCapsule(t, provider, secondCapsule)
	secondRequest := firstRequest
	secondRequest.Lock = capsuleLockFor(t, secondCapsule)
	secondRequest.Installed = guest.InstalledMaterializationsFromResults(first)
	secondRequest.Approvals = nil
	results, err := guest.MaterializeCapsuleLock(t.Context(), secondRequest)
	if !errors.Is(err, guest.ErrProfileMaterializationBlocked) || results[0].Operation != profile.OperationRequiresInput {
		t.Fatalf("executable digest change = %#v, err=%v; want renewed review", results, err)
	}
	assertClaudeFile(t, filepath.Join(firstRequest.HomeRoot, ".claude", "skills", "review", "SKILL.md"), "first\n")
}

func TestForgedInstalledComponentDigestCannotSuppressExecutableReview(t *testing.T) {
	firstCapsule := buildClaudeCapsule(t, []claudeComponentFixture{{
		component: capsule.Component{ID: "skill:review", Type: capsule.ComponentTypeSkill, Scope: capsule.ScopeUser, TrustClass: capsule.TrustExecutable},
		files:     map[string]string{"SKILL.md": "first\n"},
	}})
	secondCapsule := buildClaudeCapsule(t, []claudeComponentFixture{{
		component: capsule.Component{ID: "skill:review", Type: capsule.ComponentTypeSkill, Scope: capsule.ScopeUser, TrustClass: capsule.TrustExecutable},
		files:     map[string]string{"SKILL.md": "second\n"},
	}})
	provider := newCapsuleObjectProvider(t)
	publishCapsule(t, provider, firstCapsule)
	firstRequest := lockMaterializationRequest(t, provider, firstCapsule)
	firstRequest.Approvals = approvalForLock(t, firstRequest.Lock, "skill:review")
	first, err := guest.MaterializeCapsuleLock(t.Context(), firstRequest)
	if err != nil {
		t.Fatalf("initial executable materialization: %v", err)
	}

	publishCapsule(t, provider, secondCapsule)
	secondRequest := firstRequest
	secondRequest.Lock = capsuleLockWithID(t, secondCapsule, "lock-2")
	secondRequest.Installed = guest.InstalledMaterializationsFromResults(first)
	secondRequest.Installed[0].ComponentDigest = secondRequest.Lock.Snapshot().ResolvedComponents["skill:review"].ComponentDigest
	secondRequest.Approvals = nil
	results, err := guest.MaterializeCapsuleLock(t.Context(), secondRequest)
	if !errors.Is(err, guest.ErrProfileMaterializationBlocked) || len(results) != 1 || results[0].Operation != profile.OperationRequiresInput {
		t.Fatalf("forged installed digest = %#v, err=%v; want renewed review", results, err)
	}
	assertClaudeFile(t, filepath.Join(firstRequest.HomeRoot, ".claude", "skills", "review", "SKILL.md"), "first\n")
}

func TestCapsuleMaterializationRejectsDuplicateInstalledComponentIDs(t *testing.T) {
	value := buildClaudeCapsule(t, []claudeComponentFixture{{
		component: capsule.Component{ID: "config:CLAUDE.md", Type: capsule.ComponentTypeConfig, Scope: capsule.ScopeUser, TrustClass: capsule.TrustDeclarative},
		files:     map[string]string{"CLAUDE.md": "instructions\n"},
	}})
	provider := newCapsuleObjectProvider(t)
	publishCapsule(t, provider, value)
	request := lockMaterializationRequest(t, provider, value)
	request.Installed = []guest.InstalledMaterialization{
		{ComponentID: "config:CLAUDE.md"},
		{ComponentID: "config:CLAUDE.md"},
	}
	if _, err := guest.MaterializeCapsuleLock(t.Context(), request); err == nil || !strings.Contains(err.Error(), "duplicate installed Component ID") {
		t.Fatalf("duplicate installed Component IDs error = %v; want integrity rejection", err)
	}
}

func TestCapsuleMaterializationClassifiesLockMetadataMismatchAsImmutableInput(t *testing.T) {
	value := buildClaudeCapsule(t, []claudeComponentFixture{{
		component: capsule.Component{ID: "config:CLAUDE.md", Type: capsule.ComponentTypeConfig, Scope: capsule.ScopeUser, TrustClass: capsule.TrustDeclarative},
		files:     map[string]string{"CLAUDE.md": "instructions\n"},
	}})
	provider := newCapsuleObjectProvider(t)
	publishCapsule(t, provider, value)
	request := lockMaterializationRequest(t, provider, value)
	snapshot := request.Lock.Snapshot()
	component := snapshot.ResolvedComponents["config:CLAUDE.md"]
	component.ComponentDigest = "sha256:" + strings.Repeat("0", 64)
	snapshot.ResolvedComponents[component.ID] = component
	snapshot.Digest = ""
	lock, err := domain.CreateCapsuleLock(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	request.Lock = lock
	_, err = guest.MaterializeCapsuleLock(t.Context(), request)
	if !errors.Is(err, guest.ErrCapsuleContentInvalid) {
		t.Fatalf("metadata mismatch error = %T %v, want ErrCapsuleContentInvalid", err, err)
	}
}

func TestApprovalFromOneCapsuleLockCannotAuthorizeAnotherLock(t *testing.T) {
	value := buildClaudeCapsule(t, []claudeComponentFixture{{
		component: capsule.Component{ID: "integration:.mcp.json", Type: capsule.ComponentTypeIntegration, Scope: capsule.ScopeUser, TrustClass: capsule.TrustDeclarative},
		files:     map[string]string{".mcp.json": `{"mcpServers":{"local":{"command":"echo"}}}`},
	}})
	provider := newCapsuleObjectProvider(t)
	publishCapsule(t, provider, value)
	lockA := capsuleLockFor(t, value)
	lockB := capsuleLockWithID(t, value, "lock-b")
	component := lockA.Snapshot().ResolvedComponents["integration:.mcp.json"]
	request := lockMaterializationRequest(t, provider, value)
	request.Lock = lockB
	request.Approvals = map[string]guest.ApprovalMarker{
		component.ID: {
			ComponentID: component.ID, ComponentDigest: component.ComponentDigest,
			LockID: lockA.Snapshot().ID, LockDigest: lockA.Snapshot().Digest,
		},
	}
	results, err := guest.MaterializeCapsuleLock(t.Context(), request)
	if !errors.Is(err, guest.ErrProfileMaterializationBlocked) || len(results) != 1 || results[0].Operation != profile.OperationRequiresInput {
		t.Fatalf("cross-lock approval = %#v, err=%v; want closed rejection", results, err)
	}
	if _, statErr := os.Stat(filepath.Join(request.HomeRoot, ".mcp.json")); !os.IsNotExist(statErr) {
		t.Fatalf("cross-lock approval materialized content: %v", statErr)
	}
}

func TestProjectScopeClaudeComponentIsSeededAndNeverManaged(t *testing.T) {
	value := buildClaudeCapsule(t, []claudeComponentFixture{{
		component: capsule.Component{ID: "config:CLAUDE.md", Type: capsule.ComponentTypeConfig, Scope: capsule.ScopeProject, TrustClass: capsule.TrustDeclarative},
		files:     map[string]string{"CLAUDE.md": "project\n"},
	}})
	provider := newCapsuleObjectProvider(t)
	publishCapsule(t, provider, value)
	request := lockMaterializationRequest(t, provider, value)
	results, err := guest.MaterializeCapsuleLock(t.Context(), request)
	if err != nil || results[0].Mode != guest.MaterializationSeeded || results[0].Root != guest.MaterializationWorkspace {
		t.Fatalf("project materialization = %#v, err=%v; want seeded workspace", results, err)
	}
	assertClaudeFile(t, filepath.Join(request.WorkspaceRoot, "CLAUDE.md"), "project\n")
	managed := guest.ProfileMaterialization{ID: "project-managed", ComponentID: "project-managed", Kind: domain.ComponentConfig, Scope: domain.ScopeProject, TrustClass: domain.TrustDeclarative, Mode: guest.MaterializationManaged, Root: guest.MaterializationWorkspace, Target: "CLAUDE.md", Selector: "$", Content: []byte("must reject\n"), ContentSize: int64(len("must reject\n")), ContentDigest: materializationDigest([]byte("must reject\n")), FileMode: 0o644}
	if _, err := guest.ApplyProfileMaterializations(guest.ProfileMaterializationBatch{HomeRoot: request.HomeRoot, WorkspaceRoot: request.WorkspaceRoot, Intent: profile.IntentReconcile, Items: []guest.ProfileMaterialization{managed}}); err == nil {
		t.Fatal("project-scope component was accepted in managed mode")
	}
}

type claudeComponentFixture struct {
	component capsule.Component
	files     map[string]string
}

func buildClaudeCapsule(t *testing.T, fixtures []claudeComponentFixture) capsule.Capsule {
	t.Helper()
	root := t.TempDir()
	dirs := make(map[string]string, len(fixtures))
	manifestComponents := make([]capsule.Component, len(fixtures))
	for index, fixture := range fixtures {
		dir := filepath.Join(root, string(rune('a'+index)))
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		for name, content := range fixture.files {
			target := filepath.Join(dir, filepath.FromSlash(name))
			if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(target, []byte(content), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		manifestComponents[index] = fixture.component
		dirs[fixture.component.ID] = dir
	}
	built, err := (capsule.Builder{}).Build(capsule.Manifest{SchemaVersion: capsule.SchemaVersion, Name: "claude-test", Components: manifestComponents}, dirs)
	if err != nil {
		t.Fatalf("build test Capsule: %v", err)
	}
	return built
}

func capsuleLockFor(t *testing.T, value capsule.Capsule) domain.CapsuleLock {
	t.Helper()
	return capsuleLockWithID(t, value, "lock-1")
}

func capsuleLockWithID(t *testing.T, value capsule.Capsule, id string) domain.CapsuleLock {
	t.Helper()
	resolved := make(map[string]domain.ResolvedComponent, len(value.Manifest.Components))
	for _, component := range value.Manifest.Components {
		resolved[component.ID] = domain.ResolvedComponent{ID: component.ID, CapsuleDigest: value.Digest, ComponentDigest: component.Digest, Scope: domain.ComponentScope(component.Scope), TrustClass: domain.TrustClass(component.TrustClass)}
	}
	lock, err := domain.CreateCapsuleLock(domain.CapsuleLockSnapshot{
		ID: id, EnvironmentID: "environment-1", ProfileVersionID: "profile-1", ProjectCapsuleDigest: value.Digest,
		Capsules: []domain.LockedCapsule{{Ref: "registry.example.com/claude:test", Digest: value.Digest}}, ResolvedComponents: resolved,
		CreatedAt: time.Now().UTC(),
	})
	if err != nil {
		t.Fatalf("create Capsule Lock: %v", err)
	}
	return lock
}

func lockMaterializationRequest(t *testing.T, provider *capsuleObjectProvider, value capsule.Capsule) guest.CapsuleLockMaterializationBatch {
	t.Helper()
	return guest.CapsuleLockMaterializationBatch{
		Lock: capsuleLockFor(t, value), OwnerID: "owner-1", Grants: provider, CacheRoot: t.TempDir(), HomeRoot: t.TempDir(), WorkspaceRoot: t.TempDir(), Intent: profile.IntentReconcile, TargetAgentVersion: "claude-1",
	}
}

func approvalForLock(t *testing.T, lock domain.CapsuleLock, componentID string) map[string]guest.ApprovalMarker {
	t.Helper()
	component := lock.Snapshot().ResolvedComponents[componentID]
	snapshot := lock.Snapshot()
	return map[string]guest.ApprovalMarker{componentID: {ComponentID: componentID, ComponentDigest: component.ComponentDigest, LockID: snapshot.ID, LockDigest: snapshot.Digest}}
}

func publishCapsule(t *testing.T, provider *capsuleObjectProvider, value capsule.Capsule) {
	t.Helper()
	client, err := oci.NewClient("owner-1", provider)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.Publish(t.Context(), value); err != nil {
		t.Fatalf("publish Capsule: %v", err)
	}
}

type capsuleObjectProvider struct {
	objects map[string][]byte
	reads   int
}

func newCapsuleObjectProvider(t *testing.T) *capsuleObjectProvider {
	t.Helper()
	return &capsuleObjectProvider{objects: make(map[string][]byte)}
}

func (provider *capsuleObjectProvider) Grant(_ context.Context, request oci.GrantRequest) (oci.Grant, error) {
	if request.Operation == oci.GrantWrite {
		return oci.Grant{Write: func(_ context.Context, reader io.Reader, _ int64) error {
			content, err := io.ReadAll(reader)
			if err == nil {
				provider.objects[request.Key] = append([]byte(nil), content...)
			}
			return err
		}}, nil
	}
	content, ok := provider.objects[request.Key]
	if !ok {
		return oci.Grant{}, os.ErrNotExist
	}
	provider.reads++
	return oci.Grant{Read: func(context.Context) (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(content)), nil }}, nil
}

func (provider *capsuleObjectProvider) resetReads() { provider.reads = 0 }

func countGrantReads(provider *capsuleObjectProvider) int { return provider.reads }

func assertClaudeFile(t *testing.T, path, want string) {
	t.Helper()
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	if string(got) != want {
		t.Fatalf("%s = %q, want %q", path, got, want)
	}
}

func duplicateTarEntry(t *testing.T, compressed []byte, duplicateName string) []byte {
	t.Helper()
	reader, err := gzip.NewReader(bytes.NewReader(compressed))
	if err != nil {
		t.Fatalf("open layer gzip: %v", err)
	}
	defer reader.Close()
	var output bytes.Buffer
	writer := gzip.NewWriter(&output)
	tarWriter := tar.NewWriter(writer)
	tarReader := tar.NewReader(reader)
	for {
		header, readErr := tarReader.Next()
		if errors.Is(readErr, io.EOF) {
			break
		}
		if readErr != nil {
			t.Fatalf("read layer tar: %v", readErr)
		}
		content, readErr := io.ReadAll(tarReader)
		if readErr != nil {
			t.Fatalf("read layer entry %q: %v", header.Name, readErr)
		}
		if readErr = tarWriter.WriteHeader(header); readErr != nil {
			t.Fatalf("write layer entry %q: %v", header.Name, readErr)
		}
		if _, readErr = tarWriter.Write(content); readErr != nil {
			t.Fatalf("write layer content %q: %v", header.Name, readErr)
		}
		if header.Name == duplicateName {
			if readErr = tarWriter.WriteHeader(header); readErr != nil {
				t.Fatalf("write duplicate layer entry %q: %v", header.Name, readErr)
			}
			if _, readErr = tarWriter.Write(content); readErr != nil {
				t.Fatalf("write duplicate layer content %q: %v", header.Name, readErr)
			}
		}
	}
	if err := tarWriter.Close(); err != nil {
		t.Fatalf("close duplicate layer tar: %v", err)
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("close duplicate layer gzip: %v", err)
	}
	return output.Bytes()
}
