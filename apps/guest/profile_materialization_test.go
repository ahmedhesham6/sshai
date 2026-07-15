package guest_test

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ahmedhesham6/sshai/apps/guest"
	"github.com/ahmedhesham6/sshai/libs/profile"
)

func TestManagedProfileMaterializationCreatesVerifiedContent(t *testing.T) {
	home := t.TempDir()
	content := []byte("Use Go.\n")
	digest := materializationDigest(content)
	results, err := guest.ApplyProfileMaterializations(guest.ProfileMaterializationBatch{
		HomeRoot: home,
		Intent:   profile.IntentReconcile,
		Items: []guest.ProfileMaterialization{{
			ID: "agents", ArtifactID: "artifact-1", Mode: guest.MaterializationManaged,
			Root: guest.MaterializationHome, Target: "AGENTS.md", ContentSize: int64(len(content)),
			Artifact: &profile.Artifact{Kind: "agent_instruction", Path: "AGENTS.md", Selector: "$", SourceLocator: "AGENTS.md#$", ContentDigest: digest, Mode: 0o640, Content: content},
		}},
	})
	if err != nil {
		t.Fatalf("apply Profile Materializations: %v", err)
	}
	materialized, err := os.ReadFile(filepath.Join(home, "AGENTS.md"))
	if err != nil {
		t.Fatal(err)
	}
	if string(materialized) != "Use Go.\n" {
		t.Fatalf("materialized content = %q", materialized)
	}
	info, err := os.Stat(filepath.Join(home, "AGENTS.md"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o640 {
		t.Fatalf("materialized mode = %o", info.Mode().Perm())
	}
	if len(results) != 1 || results[0].Operation != profile.OperationCreate || results[0].LastAppliedDigest != digest || results[0].ObservedDigest != digest {
		t.Fatalf("results = %#v", results)
	}
	result := results[0]
	if result.ID != "agents" || result.ArtifactID != "artifact-1" || result.Mode != guest.MaterializationManaged || result.Adapter != "file" || result.AdapterVersion != "v1" || result.Root != guest.MaterializationHome || result.Target != "AGENTS.md" || result.Selector != "$" || result.DesiredDigest != digest {
		t.Fatalf("persistence result = %#v", result)
	}
}

func TestManagedProfileMaterializationUpdatesOnlyAnUnchangedTarget(t *testing.T) {
	home := t.TempDir()
	target := filepath.Join(home, "AGENTS.md")
	old := []byte("Use Go 1.26.\n")
	updated := []byte("Use Go 1.27.\n")
	if err := os.WriteFile(target, old, 0o600); err != nil {
		t.Fatal(err)
	}

	results, err := applyManagedArtifact(t, home, updated, materializationDigest(old), materializationDigest(old))
	if err != nil {
		t.Fatalf("update unchanged target: %v", err)
	}
	if results[0].Operation != profile.OperationUpdate {
		t.Fatalf("operation = %q", results[0].Operation)
	}
	assertMaterializedContent(t, target, updated)

	remote := []byte("Environment-owned edit.\n")
	if err := os.WriteFile(target, remote, 0o600); err != nil {
		t.Fatal(err)
	}
	latest := []byte("Use Go 1.28.\n")
	results, err = applyManagedArtifact(t, home, latest, materializationDigest(updated), materializationDigest(remote))
	if !errors.Is(err, guest.ErrProfileMaterializationBlocked) {
		t.Fatalf("conflicting update error = %v", err)
	}
	if len(results) != 1 || results[0].Operation != profile.OperationConflict {
		t.Fatalf("conflict results = %#v", results)
	}
	assertMaterializedContent(t, target, remote)
}

func TestProfileMaterializationBatchFailsClosedOnDrift(t *testing.T) {
	home := t.TempDir()
	desired := []byte("desired\n")
	remote := []byte("remote edit\n")
	if err := os.WriteFile(filepath.Join(home, "AGENTS.md"), remote, 0o600); err != nil {
		t.Fatal(err)
	}
	desiredDigest := materializationDigest(desired)
	second := []byte("second\n")
	secondDigest := materializationDigest(second)
	results, err := guest.ApplyProfileMaterializations(guest.ProfileMaterializationBatch{
		HomeRoot: home,
		Intent:   profile.IntentReconcile,
		Items: []guest.ProfileMaterialization{
			{
				ID: "drifted", ArtifactID: "artifact-1", Mode: guest.MaterializationManaged,
				Root: guest.MaterializationHome, Target: "AGENTS.md", ContentSize: int64(len(desired)),
				LastAppliedDigest: desiredDigest, ObservedDigest: materializationDigest(remote),
				Artifact: &profile.Artifact{Kind: "agent_instruction", Path: "AGENTS.md", Selector: "$", SourceLocator: "AGENTS.md#$", ContentDigest: desiredDigest, Mode: 0o600, Content: desired},
			},
			{
				ID: "safe-create", ArtifactID: "artifact-2", Mode: guest.MaterializationManaged,
				Root: guest.MaterializationHome, Target: "CLAUDE.md", ContentSize: int64(len(second)),
				Artifact: &profile.Artifact{Kind: "agent_instruction", Path: "CLAUDE.md", Selector: "$", SourceLocator: "CLAUDE.md#$", ContentDigest: secondDigest, Mode: 0o600, Content: second},
			},
		},
	})
	if !errors.Is(err, guest.ErrProfileMaterializationBlocked) {
		t.Fatalf("drift error = %v", err)
	}
	if len(results) != 2 || results[0].Operation != profile.OperationDrift || results[1].Operation != profile.OperationCreate {
		t.Fatalf("results = %#v", results)
	}
	assertMaterializedContent(t, filepath.Join(home, "AGENTS.md"), remote)
	if _, err := os.Stat(filepath.Join(home, "CLAUDE.md")); !os.IsNotExist(err) {
		t.Fatalf("safe item was applied despite batch drift: %v", err)
	}
}

func TestSeededProfileMaterializationCreatesOnceThenTransfersOwnership(t *testing.T) {
	home := t.TempDir()
	seed := []byte("initial preference\n")
	seedDigest := materializationDigest(seed)
	item := guest.ProfileMaterialization{
		ID: "shell", ArtifactID: "artifact-1", Mode: guest.MaterializationSeeded,
		Root: guest.MaterializationHome, Target: ".bashrc", ContentSize: int64(len(seed)),
		Artifact: &profile.Artifact{Kind: "shell_preferences", Path: ".bashrc", Selector: "$", SourceLocator: ".bashrc#$", ContentDigest: seedDigest, ContainsExecutable: true, Mode: 0o600, Content: seed},
	}
	results, err := guest.ApplyProfileMaterializations(guest.ProfileMaterializationBatch{HomeRoot: home, Intent: profile.IntentReconcile, Items: []guest.ProfileMaterialization{item}})
	if err != nil || results[0].Operation != profile.OperationCreate {
		t.Fatalf("seed create: results=%#v err=%v", results, err)
	}

	environmentOwned := []byte("environment-owned preference\n")
	if err := os.WriteFile(filepath.Join(home, ".bashrc"), environmentOwned, 0o600); err != nil {
		t.Fatal(err)
	}
	changedSource := []byte("new Profile preference\n")
	item.Artifact.Content = changedSource
	item.Artifact.ContentDigest = materializationDigest(changedSource)
	item.ContentSize = int64(len(changedSource))
	item.LastAppliedDigest = seedDigest
	item.ObservedDigest = materializationDigest(environmentOwned)
	results, err = guest.ApplyProfileMaterializations(guest.ProfileMaterializationBatch{HomeRoot: home, Intent: profile.IntentReconcile, Items: []guest.ProfileMaterialization{item}})
	if err != nil || results[0].Operation != profile.OperationSkip {
		t.Fatalf("seed replay: results=%#v err=%v", results, err)
	}
	assertMaterializedContent(t, filepath.Join(home, ".bashrc"), environmentOwned)

	otherHome := t.TempDir()
	preexisting := []byte("preexisting\n")
	if err := os.WriteFile(filepath.Join(otherHome, ".bashrc"), preexisting, 0o600); err != nil {
		t.Fatal(err)
	}
	item.LastAppliedDigest = ""
	item.ObservedDigest = materializationDigest(preexisting)
	results, err = guest.ApplyProfileMaterializations(guest.ProfileMaterializationBatch{HomeRoot: otherHome, Intent: profile.IntentReconcile, Items: []guest.ProfileMaterialization{item}})
	if !errors.Is(err, guest.ErrProfileMaterializationBlocked) || results[0].Operation != profile.OperationConflict {
		t.Fatalf("preexisting seed target: results=%#v err=%v", results, err)
	}
	assertMaterializedContent(t, filepath.Join(otherHome, ".bashrc"), preexisting)
}

func TestReferencedProfileMaterializationRecordsRequirementWithoutCopyingContent(t *testing.T) {
	requirementDigest := materializationDigest([]byte("github:repo"))
	item := guest.ProfileMaterialization{
		ID: "github", ArtifactID: "artifact-1", Mode: guest.MaterializationReferenced,
		Target: "credential/github/repo", RequirementState: profile.RequirementNeedsInput,
		Artifact: &profile.Artifact{Path: "credential/github/repo", Selector: "$", SourceLocator: "credential/github/repo#$", ContentDigest: requirementDigest},
	}
	results, err := guest.ApplyProfileMaterializations(guest.ProfileMaterializationBatch{Intent: profile.IntentReconcile, Items: []guest.ProfileMaterialization{item}})
	if !errors.Is(err, guest.ErrProfileMaterializationBlocked) {
		t.Fatalf("unbound requirement error = %v", err)
	}
	if len(results) != 1 || results[0].Operation != profile.OperationRequiresInput || results[0].Adapter != "reference" || results[0].DesiredDigest != requirementDigest {
		t.Fatalf("unbound requirement results = %#v", results)
	}

	item.RequirementState = profile.RequirementBound
	results, err = guest.ApplyProfileMaterializations(guest.ProfileMaterializationBatch{Intent: profile.IntentReconcile, Items: []guest.ProfileMaterialization{item}})
	if err != nil || len(results) != 1 || results[0].Operation != profile.OperationSkip || results[0].Adapter != "reference" {
		t.Fatalf("bound requirement: results=%#v err=%v", results, err)
	}
	if _, err := os.Stat("credential/github/repo"); !os.IsNotExist(err) {
		t.Fatalf("referenced content was copied: %v", err)
	}
}

func TestRemovedProfileArtifactIsOrphanedUntilExplicitPrune(t *testing.T) {
	home := t.TempDir()
	content := []byte("keep until prune\n")
	target := filepath.Join(home, "AGENTS.md")
	if err := os.WriteFile(target, content, 0o600); err != nil {
		t.Fatal(err)
	}
	digest := materializationDigest(content)
	item := guest.ProfileMaterialization{
		ID: "agents", ArtifactID: "artifact-old", Mode: guest.MaterializationManaged,
		Root: guest.MaterializationHome, Target: "AGENTS.md", Selector: "$",
		LastAppliedDigest: digest, ObservedDigest: digest,
	}
	results, err := guest.ApplyProfileMaterializations(guest.ProfileMaterializationBatch{HomeRoot: home, Intent: profile.IntentReconcile, Items: []guest.ProfileMaterialization{item}})
	if err != nil || len(results) != 1 || results[0].Operation != profile.OperationOrphan {
		t.Fatalf("orphan reconcile: results=%#v err=%v", results, err)
	}
	assertMaterializedContent(t, target, content)

	results, err = guest.ApplyProfileMaterializations(guest.ProfileMaterializationBatch{HomeRoot: home, Intent: profile.IntentPrune, Items: []guest.ProfileMaterialization{item}})
	if err != nil || len(results) != 1 || results[0].Operation != profile.OperationRemove || results[0].LastAppliedDigest != "" || results[0].ObservedDigest != "" {
		t.Fatalf("explicit prune: results=%#v err=%v", results, err)
	}
	if _, err := os.Stat(target); !os.IsNotExist(err) {
		t.Fatalf("pruned target still exists: %v", err)
	}

	reference := guest.ProfileMaterialization{
		ID: "github", ArtifactID: "artifact-reference", Mode: guest.MaterializationReferenced,
		Target: "credential/github/repo", Selector: "$", ObservedDigest: digest,
		RequirementState: profile.RequirementBound,
	}
	results, err = guest.ApplyProfileMaterializations(guest.ProfileMaterializationBatch{Intent: profile.IntentReconcile, Items: []guest.ProfileMaterialization{reference}})
	if err != nil || results[0].Operation != profile.OperationOrphan || results[0].Adapter != "reference" {
		t.Fatalf("referenced orphan: results=%#v err=%v", results, err)
	}
	results, err = guest.ApplyProfileMaterializations(guest.ProfileMaterializationBatch{Intent: profile.IntentPrune, Items: []guest.ProfileMaterialization{reference}})
	if err != nil || results[0].Operation != profile.OperationRemove || results[0].ObservedDigest != "" {
		t.Fatalf("referenced prune: results=%#v err=%v", results, err)
	}
}

func TestJSONSelectorMaterializationPreservesUnknownFields(t *testing.T) {
	home := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, ".claude"), 0o700); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(home, ".claude", "settings.json")
	original := []byte(`{"theme":"light","permissions":{"allow":["Read"]},"unknown":{"keep":true,"precise":9007199254740993}}`)
	if err := os.WriteFile(target, original, 0o600); err != nil {
		t.Fatal(err)
	}
	desired := []byte(`"dark"`)
	results, err := guest.ApplyProfileMaterializations(guest.ProfileMaterializationBatch{
		HomeRoot: home,
		Intent:   profile.IntentReconcile,
		Items: []guest.ProfileMaterialization{{
			ID: "claude-theme", ArtifactID: "artifact-1", Mode: guest.MaterializationManaged,
			Root: guest.MaterializationHome, Target: ".claude/settings.json", ContentSize: int64(len(desired)),
			LastAppliedDigest: materializationDigest([]byte(`"light"`)), ObservedDigest: materializationDigest([]byte(`"light"`)),
			Artifact: &profile.Artifact{Kind: "claude_settings", Path: ".claude/settings.json", Selector: "$.theme", SourceLocator: ".claude/settings.json#$.theme", ContentDigest: materializationDigest(desired), Mode: 0o600, Content: desired},
		}},
	})
	if err != nil || len(results) != 1 || results[0].Operation != profile.OperationUpdate {
		t.Fatalf("JSON selector update: results=%#v err=%v", results, err)
	}
	var settings map[string]any
	content, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(content, &settings); err != nil {
		t.Fatalf("decode materialized settings: %v", err)
	}
	if settings["theme"] != "dark" {
		t.Fatalf("theme = %#v", settings["theme"])
	}
	unknown := settings["unknown"].(map[string]any)
	permissions := settings["permissions"].(map[string]any)
	if unknown["keep"] != true || len(permissions["allow"].([]any)) != 1 {
		t.Fatalf("unknown fields were not preserved: %#v", settings)
	}
	if !strings.Contains(string(content), "9007199254740993") {
		t.Fatalf("unknown numeric precision was not preserved: %s", content)
	}
}

func TestProfileMaterializationRejectsTraversalAndSymlinkEscapes(t *testing.T) {
	home := t.TempDir()
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(home, "escape")); err != nil {
		t.Fatal(err)
	}
	content := []byte("must stay inside\n")
	digest := materializationDigest(content)
	for _, target := range []string{"../outside", "escape/config"} {
		t.Run(target, func(t *testing.T) {
			_, err := guest.ApplyProfileMaterializations(guest.ProfileMaterializationBatch{
				HomeRoot: home,
				Intent:   profile.IntentReconcile,
				Items: []guest.ProfileMaterialization{{
					ID: "unsafe", ArtifactID: "artifact-1", Mode: guest.MaterializationManaged,
					Root: guest.MaterializationHome, Target: target, ContentSize: int64(len(content)),
					Artifact: &profile.Artifact{Kind: "agent_instruction", Path: target, Selector: "$", SourceLocator: target + "#$", ContentDigest: digest, Mode: 0o600, Content: content},
				}},
			})
			if err == nil {
				t.Fatal("unsafe target was accepted")
			}
		})
	}
	if _, err := os.Stat(filepath.Join(outside, "config")); !os.IsNotExist(err) {
		t.Fatalf("symlink escape wrote outside the home root: %v", err)
	}

	symlinkedRoot := filepath.Join(t.TempDir(), "home")
	if err := os.Symlink(home, symlinkedRoot); err != nil {
		t.Fatal(err)
	}
	_, err := applyManagedArtifact(t, symlinkedRoot, content, "", "")
	if err == nil {
		t.Fatal("symlinked State Component root was accepted")
	}
}

func TestPruningJSONSelectorPreservesUnmanagedFields(t *testing.T) {
	home := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, ".claude"), 0o700); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(home, ".claude", "settings.json")
	if err := os.WriteFile(target, []byte(`{"theme":"dark","unknown":{"keep":true}}`), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(target, 0o640); err != nil {
		t.Fatal(err)
	}
	themeDigest := materializationDigest([]byte(`"dark"`))
	results, err := guest.ApplyProfileMaterializations(guest.ProfileMaterializationBatch{
		HomeRoot: home,
		Intent:   profile.IntentPrune,
		Items: []guest.ProfileMaterialization{{
			ID: "claude-theme", ArtifactID: "artifact-old", Mode: guest.MaterializationManaged,
			Root: guest.MaterializationHome, Target: ".claude/settings.json", Selector: "$.theme",
			LastAppliedDigest: themeDigest, ObservedDigest: themeDigest,
		}},
	})
	if err != nil || len(results) != 1 || results[0].Operation != profile.OperationRemove {
		t.Fatalf("selector prune: results=%#v err=%v", results, err)
	}
	var settings map[string]any
	content, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(content, &settings); err != nil {
		t.Fatal(err)
	}
	if _, exists := settings["theme"]; exists || settings["unknown"].(map[string]any)["keep"] != true {
		t.Fatalf("selector prune changed unmanaged fields: %#v", settings)
	}
	info, err := os.Stat(target)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o640 {
		t.Fatalf("selector prune changed mode to %o", info.Mode().Perm())
	}
}

func TestProfileMaterializationVerifiesDeclaredContentBeforeMutation(t *testing.T) {
	tests := []struct {
		name    string
		content []byte
		size    int64
		digest  string
		target  string
	}{
		{name: "size mismatch", content: []byte("content"), size: 6, digest: materializationDigest([]byte("content")), target: "AGENTS.md"},
		{name: "digest mismatch", content: []byte("content"), size: 7, digest: materializationDigest([]byte("different")), target: "AGENTS.md"},
		{name: "invalid JSON", content: []byte(`{"invalid"`), size: 10, digest: materializationDigest([]byte(`{"invalid"`)), target: ".claude/settings.json"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			home := t.TempDir()
			_, err := guest.ApplyProfileMaterializations(guest.ProfileMaterializationBatch{
				HomeRoot: home,
				Intent:   profile.IntentReconcile,
				Items: []guest.ProfileMaterialization{{
					ID: "invalid", ArtifactID: "artifact-1", Mode: guest.MaterializationManaged,
					Root: guest.MaterializationHome, Target: test.target, ContentSize: test.size,
					Artifact: &profile.Artifact{Kind: "agent_instruction", Path: test.target, Selector: "$", SourceLocator: test.target + "#$", ContentDigest: test.digest, Mode: 0o600, Content: test.content},
				}},
			})
			if err == nil {
				t.Fatal("invalid artifact was accepted")
			}
			if _, err := os.Stat(filepath.Join(home, filepath.FromSlash(test.target))); !os.IsNotExist(err) {
				t.Fatalf("invalid artifact mutated its target: %v", err)
			}
		})
	}
}

func TestProfileMaterializationNeverExecutesSelectedExecutableContent(t *testing.T) {
	home := t.TempDir()
	marker := filepath.Join(t.TempDir(), "executed")
	script := []byte("#!/bin/sh\ntouch " + marker + "\n")
	target := ".codex/skills/danger/scripts/run.sh"
	digest := materializationDigest(script)
	_, err := guest.ApplyProfileMaterializations(guest.ProfileMaterializationBatch{
		HomeRoot: home,
		Intent:   profile.IntentReconcile,
		Items: []guest.ProfileMaterialization{{
			ID: "skill-script", ArtifactID: "artifact-1", Mode: guest.MaterializationManaged,
			Root: guest.MaterializationHome, Target: target, ContentSize: int64(len(script)),
			Artifact: &profile.Artifact{Kind: "agent_skill_executable", Path: target, Selector: "$", SourceLocator: target + "#$", ContentDigest: digest, ContainsExecutable: true, Mode: 0o755, Content: script},
		}},
	})
	if err != nil {
		t.Fatalf("materialize reviewed executable content: %v", err)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("executable content ran during synchronization: %v", err)
	}
	info, err := os.Stat(filepath.Join(home, filepath.FromSlash(target)))
	if err != nil || info.Mode().Perm() != 0o755 {
		t.Fatalf("materialized executable metadata: info=%v err=%v", info, err)
	}
}

func TestProfileMaterializationRejectsUnknownPluginArtifacts(t *testing.T) {
	home := t.TempDir()
	content := []byte("plugin payload")
	target := ".codex/plugins/unknown/plugin.sh"
	_, err := guest.ApplyProfileMaterializations(guest.ProfileMaterializationBatch{
		HomeRoot: home,
		Intent:   profile.IntentReconcile,
		Items: []guest.ProfileMaterialization{{
			ID: "plugin", ArtifactID: "artifact-1", Mode: guest.MaterializationManaged,
			Root: guest.MaterializationHome, Target: target, ContentSize: int64(len(content)),
			Artifact: &profile.Artifact{Path: target, Selector: "$", SourceLocator: target + "#$", ContentDigest: materializationDigest(content), ContainsExecutable: true, Mode: 0o755, Content: content},
		}},
	})
	if err == nil {
		t.Fatal("unknown plugin artifact was accepted")
	}
	if _, err := os.Stat(filepath.Join(home, filepath.FromSlash(target))); !os.IsNotExist(err) {
		t.Fatalf("unknown plugin artifact was materialized: %v", err)
	}
}

func TestProfileMaterializationRejectsDuplicateTargetOwnershipBeforeMutation(t *testing.T) {
	home := t.TempDir()
	content := []byte("same target\n")
	digest := materializationDigest(content)
	items := make([]guest.ProfileMaterialization, 2)
	for index := range items {
		items[index] = guest.ProfileMaterialization{
			ID: "owner-" + string(rune('a'+index)), ArtifactID: "artifact-" + string(rune('a'+index)),
			Mode: guest.MaterializationManaged, Root: guest.MaterializationHome, Target: "AGENTS.md", ContentSize: int64(len(content)),
			Artifact: &profile.Artifact{Kind: "agent_instruction", Path: "AGENTS.md", Selector: "$", SourceLocator: "AGENTS.md#$", ContentDigest: digest, Mode: 0o600, Content: content},
		}
	}
	_, err := guest.ApplyProfileMaterializations(guest.ProfileMaterializationBatch{HomeRoot: home, Intent: profile.IntentReconcile, Items: items})
	if err == nil {
		t.Fatal("duplicate target ownership was accepted")
	}
	if _, err := os.Stat(filepath.Join(home, "AGENTS.md")); !os.IsNotExist(err) {
		t.Fatalf("duplicate ownership partially mutated the target: %v", err)
	}
}

func applyManagedArtifact(t *testing.T, home string, content []byte, lastApplied, observed string) ([]guest.ProfileMaterializationResult, error) {
	t.Helper()
	digest := materializationDigest(content)
	return guest.ApplyProfileMaterializations(guest.ProfileMaterializationBatch{
		HomeRoot: home,
		Intent:   profile.IntentReconcile,
		Items: []guest.ProfileMaterialization{{
			ID: "agents", ArtifactID: "artifact-1", Mode: guest.MaterializationManaged,
			Root: guest.MaterializationHome, Target: "AGENTS.md", ContentSize: int64(len(content)),
			LastAppliedDigest: lastApplied, ObservedDigest: observed,
			Artifact: &profile.Artifact{Kind: "agent_instruction", Path: "AGENTS.md", Selector: "$", SourceLocator: "AGENTS.md#$", ContentDigest: digest, Mode: 0o600, Content: content},
		}},
	})
}

func assertMaterializedContent(t *testing.T, target string, want []byte) {
	t.Helper()
	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(want) {
		t.Fatalf("materialized content = %q, want %q", got, want)
	}
}

func materializationDigest(content []byte) string {
	digest := sha256.Sum256(content)
	return "sha256:" + hex.EncodeToString(digest[:])
}
