package profile_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/ahmedhesham6/sshai/libs/profile"
)

func TestCompilerProducesDeterministicImmutableVersionFromExplicitSelectors(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "AGENTS.md"), []byte("Use Go.\n"), 0o644)
	if err := os.MkdirAll(filepath.Join(root, ".claude"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(root, ".claude", "settings.json"), []byte("{\"theme\":\"dark\"}\n"), 0o600)

	first, err := profile.Compile(root, []profile.Selector{
		{Path: ".claude/settings.json", Selector: "$.theme"},
		{Path: "AGENTS.md", Selector: "$"},
	})
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	second, err := profile.Compile(root, []profile.Selector{
		{Path: "AGENTS.md", Selector: "$"},
		{Path: ".claude/settings.json", Selector: "$.theme"},
	})
	if err != nil {
		t.Fatalf("compile reordered selection: %v", err)
	}

	if first.Digest() != second.Digest() {
		t.Fatalf("digest changed with selector order: %q != %q", first.Digest(), second.Digest())
	}
	if got := first.Digest(); got != "sha256:844cd6993159b46b3c4bd822ff1255af9ddb4bcdc4671b6485a5cb65d56890d9" {
		t.Fatalf("canonical v2 digest = %q", got)
	}
	artifacts := first.Artifacts()
	if got := artifacts[0].Path; got != ".claude/settings.json" {
		t.Fatalf("first artifact path = %q", got)
	}
	if got := artifacts[0].ContentDigest; got != "sha256:7b9365c5e1321c0b8b2e1c980c2859b4515849c848e059f6789ff9ab5c89b2d3" {
		t.Fatalf("artifact content digest = %q", got)
	}
	if artifacts[0].Kind != "claude_settings" || artifacts[0].SourceLocator != ".claude/settings.json#$.theme" || artifacts[0].SourceDigest == artifacts[0].ContentDigest || artifacts[0].Sensitivity != "private" || artifacts[0].Trust != "user_authored" || artifacts[0].ContainsExecutable || artifacts[0].Evidence != "known_claude_settings" {
		t.Fatalf("transport metadata = %+v", artifacts[0])
	}
	artifacts[0].Content[0] = 'X'
	if got := string(first.Artifacts()[0].Content); got != `"dark"` {
		t.Fatalf("version content was mutable: %q", got)
	}
}

func TestCompilerRejectsUnsafeSelectedPathsWithoutExecutingContent(t *testing.T) {
	root := t.TempDir()
	outside := filepath.Join(t.TempDir(), "outside")
	writeFile(t, outside, []byte("secret"), 0o600)
	if err := os.Symlink(outside, filepath.Join(root, "escape")); err != nil {
		t.Fatal(err)
	}

	if _, err := profile.Compile(root, []profile.Selector{{Path: "escape", Selector: "$"}}); err == nil {
		t.Fatal("expected escaping symlink to be rejected")
	}
	if _, err := profile.Compile(root, []profile.Selector{{Path: "../outside", Selector: "$"}}); err == nil {
		t.Fatal("expected traversal to be rejected")
	}
	if _, err := profile.Compile(root, []profile.Selector{{Path: "id_ed25519", Selector: "$"}}); err == nil {
		t.Fatal("expected private-key path to be rejected before reading")
	}
}

func TestCompilerBlocksSelectedCredentialContent(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "settings.env"), []byte("API_TOKEN=not-portable-secret\n"), 0o600)
	if _, err := profile.Compile(root, []profile.Selector{{Path: "settings.env", Selector: "$"}}); err == nil {
		t.Fatal("expected selected credential content to be blocked")
	}
}

func TestCompilerRejectsDuplicateCanonicalSelectors(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "AGENTS.md"), []byte("Use Go.\n"), 0o644)
	_, err := profile.Compile(root, []profile.Selector{{Path: "AGENTS.md", Selector: "$"}, {Path: "./AGENTS.md", Selector: "$"}})
	if err == nil {
		t.Fatal("expected duplicate canonical selector to be rejected")
	}
}

func TestCompilerRejectsExplicitUnknownPath(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "unknown.txt"), []byte("not portable\n"), 0o644)
	if _, err := profile.Compile(root, []profile.Selector{{Path: "unknown.txt", Selector: "$"}}); err == nil {
		t.Fatal("expected explicit unknown path to remain excluded")
	}
}

func TestCompilerPreservesLargeJSONIntegerSelectedValue(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, ".claude"), 0o700); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(root, ".claude", "settings.json"), []byte(`{"identifier":9007199254740993}`), 0o600)
	version, err := profile.Compile(root, []profile.Selector{{Path: ".claude/settings.json", Selector: "$.identifier"}})
	if err != nil {
		t.Fatalf("Compile(): %v", err)
	}
	if got := string(version.Artifacts()[0].Content); got != "9007199254740993" {
		t.Fatalf("selected integer = %s", got)
	}
}

func TestCompilerRejectsMultipleRootJSONValues(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, ".claude"), 0o700); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(root, ".claude", "settings.json"), []byte(`{"identifier":1} {"identifier":2}`), 0o600)
	if _, err := profile.Compile(root, []profile.Selector{{Path: ".claude/settings.json", Selector: "$.identifier"}}); err == nil {
		t.Fatal("Compile() accepted multiple root JSON values")
	}
}

func writeFile(t *testing.T, path string, content []byte, mode os.FileMode) {
	t.Helper()
	if err := os.WriteFile(path, content, mode); err != nil {
		t.Fatal(err)
	}
}
