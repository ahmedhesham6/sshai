package profile

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/ahmedhesham6/sshai/libs/domain"
)

// Candidate is local-only classification metadata. It never contains source
// content and can be displayed before an explicit Selector is accepted.
type Candidate struct {
	Kind               string
	Path               string
	Selector           string
	SourceLocator      string
	SourceDigest       string
	ContentDigest      string
	Sensitivity        string
	Trust              string
	ContainsExecutable bool
	Evidence           string
	Disposition        string
}

type classification struct {
	kind, sensitivity, trust, evidence, disposition string
	containsExecutable                              bool
	readContent                                     bool
}

// Scan classifies files only at known Profile roots. Unknown files inside
// those roots are reported as excluded without reading their contents.
func Scan(root string) ([]Candidate, error) {
	resolvedRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return nil, fmt.Errorf("resolve profile root: %w", err)
	}
	resolvedRoot, err = filepath.Abs(resolvedRoot)
	if err != nil {
		return nil, fmt.Errorf("make profile root absolute: %w", err)
	}
	paths := make([]string, 0)
	for _, path := range []string{"AGENTS.md", "CLAUDE.md", ".bashrc", ".zshrc", ".gitconfig"} {
		if _, err := os.Lstat(filepath.Join(resolvedRoot, path)); err == nil {
			paths = append(paths, path)
		} else if !os.IsNotExist(err) {
			return nil, fmt.Errorf("inspect candidate %q: %w", path, err)
		}
	}
	for _, knownRoot := range []string{".codex", ".claude", ".ssh"} {
		path := filepath.Join(resolvedRoot, knownRoot)
		info, err := os.Lstat(path)
		if os.IsNotExist(err) {
			continue
		} else if err != nil {
			return nil, fmt.Errorf("inspect known root %q: %w", knownRoot, err)
		}
		if info.Mode()&os.ModeSymlink != 0 {
			path, err = filepath.EvalSymlinks(path)
			if err != nil {
				return nil, fmt.Errorf("resolve known root %q: %w", knownRoot, err)
			}
			relative, relErr := filepath.Rel(resolvedRoot, path)
			if relErr != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
				return nil, fmt.Errorf("known root %q escapes profile root", knownRoot)
			}
			info, err = os.Stat(path)
			if err != nil {
				return nil, fmt.Errorf("stat known root %q: %w", knownRoot, err)
			}
		}
		if !info.IsDir() {
			return nil, fmt.Errorf("known root %q is not a directory", knownRoot)
		}
		err = filepath.WalkDir(path, func(candidatePath string, entry fs.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			if entry.IsDir() {
				return nil
			}
			relative, err := filepath.Rel(path, candidatePath)
			if err != nil {
				return err
			}
			paths = append(paths, filepath.ToSlash(filepath.Join(knownRoot, relative)))
			return nil
		})
		if err != nil {
			return nil, fmt.Errorf("scan known root %q: %w", knownRoot, err)
		}
	}
	sort.Strings(paths)
	candidates := make([]Candidate, 0, len(paths))
	for _, path := range paths {
		candidate, err := classifyCandidate(resolvedRoot, path)
		if err != nil {
			return nil, err
		}
		candidates = append(candidates, candidate)
	}
	return candidates, nil
}

func classifyCandidate(root, path string) (Candidate, error) {
	metadata := classifyKnownPath(path)
	candidate := Candidate{
		Kind: metadata.kind, Path: path, Selector: "$", SourceLocator: path + "#$",
		Sensitivity: metadata.sensitivity, Trust: metadata.trust,
		ContainsExecutable: metadata.containsExecutable, Evidence: metadata.evidence, Disposition: metadata.disposition,
	}
	if !metadata.readContent {
		return candidate, nil
	}
	resolved, err := safeSelectedPath(root, path)
	if err != nil {
		return Candidate{}, fmt.Errorf("classify candidate %q: %w", path, err)
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return Candidate{}, fmt.Errorf("stat candidate %q: %w", path, err)
	}
	if !info.Mode().IsRegular() {
		return Candidate{}, fmt.Errorf("candidate %q is not a regular file", path)
	}
	content, err := os.ReadFile(resolved)
	if err != nil {
		return Candidate{}, fmt.Errorf("read candidate %q: %w", path, err)
	}
	if containsCredential(content) {
		candidate.Sensitivity = "credential"
		candidate.Disposition = "excluded"
		candidate.Evidence = "credential_content_detected"
		return candidate, nil
	}
	if info.Mode().Perm()&0o111 != 0 {
		candidate.Disposition = "review"
		candidate.Evidence += "+executable_mode"
	}
	candidate.SourceDigest = contentDigest(content)
	candidate.ContentDigest = candidate.SourceDigest
	return candidate, nil
}

func classifyKnownPath(path string) classification {
	normalized := filepath.ToSlash(filepath.Clean(filepath.FromSlash(path)))
	switch normalized {
	case "AGENTS.md", "CLAUDE.md", ".claude/CLAUDE.md":
		return portableClassification(string(domain.ArtifactAgentInstruction), "user_authored", "known_agent_instruction")
	case ".codex/config.toml":
		return portableClassification(string(domain.ArtifactCodexSettings), "user_authored", "known_codex_settings")
	case ".claude/settings.json":
		return portableClassification(string(domain.ArtifactClaudeSettings), "user_authored", "known_claude_settings")
	case ".bashrc", ".zshrc":
		return classification{kind: string(domain.ArtifactShellPreferences), sensitivity: "private", trust: "user_authored", evidence: "known_shell_preferences", disposition: "review", containsExecutable: true, readContent: true}
	case ".gitconfig":
		return portableClassification(string(domain.ArtifactGitPreferences), "user_authored", "known_git_preferences")
	case ".codex/auth.json", ".claude/.credentials.json":
		return classification{kind: "auth_cache", sensitivity: "credential", trust: "unknown", evidence: "known_auth_cache", disposition: "excluded"}
	}
	parts := strings.Split(normalized, "/")
	if len(parts) == 4 && (parts[0] == ".codex" || parts[0] == ".claude") && parts[1] == "skills" && parts[3] == "SKILL.md" {
		return portableClassification(string(domain.ArtifactSkillInstruction), "third_party", "open_agent_skill_manifest")
	}
	if len(parts) >= 5 && (parts[0] == ".codex" || parts[0] == ".claude") && parts[1] == "skills" && parts[3] == "scripts" {
		return classification{kind: string(domain.ArtifactSkillExecutable), sensitivity: "private", trust: "third_party", evidence: "open_agent_skill_script", disposition: "review", containsExecutable: true, readContent: true}
	}
	if strings.HasPrefix(normalized, ".ssh/") && isPrivateKeyPath(normalized) {
		return classification{kind: "private_key", sensitivity: "credential", trust: "unknown", evidence: "private_key_path", disposition: "excluded"}
	}
	return classification{kind: "unknown", sensitivity: "unknown", trust: "unknown", evidence: "unknown_file_in_known_root", disposition: "excluded"}
}

func portableClassification(kind, trust, evidence string) classification {
	return classification{kind: kind, sensitivity: "private", trust: trust, evidence: evidence, disposition: "safe", readContent: true}
}

func contentDigest(content []byte) string {
	digest := sha256.Sum256(content)
	return "sha256:" + hex.EncodeToString(digest[:])
}
