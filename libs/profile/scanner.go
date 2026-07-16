package profile

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/ahmedhesham6/sshai/libs/domain"
)

// Candidate is local-only classification metadata. It never contains source
// content and can be displayed before an explicit Selector is accepted.
type Candidate struct {
	// Component is the typed capture mapping. Its layer digest and media type
	// are populated only after explicit selection and capsule compilation.
	Component          domain.Component
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
	for _, path := range []string{
		"AGENTS.md", "CLAUDE.md", ".bashrc", ".zshrc", ".gitconfig",
		".mcp.json", "mcp.json", "opencode.json", "opencode.jsonc", ".opencode.json",
	} {
		if _, err := os.Lstat(filepath.Join(resolvedRoot, path)); err == nil {
			paths = append(paths, path)
		} else if !os.IsNotExist(err) {
			return nil, fmt.Errorf("inspect candidate %q: %w", path, err)
		}
	}
	for _, knownRoot := range []string{
		".codex", ".claude", ".ssh", ".opencode", ".config/opencode", ".config/codex",
		".config/claude", ".local/share/opencode", ".local/share/codex", ".local/share/claude",
		"Library/Application Support/opencode",
	} {
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
	identity, err := resolveCandidateIdentity(root, path)
	if err != nil {
		return Candidate{}, fmt.Errorf("resolve candidate %q: %w", path, err)
	}
	metadataPath := path
	if identity.symlink {
		metadataPath = identity.relative
	}
	metadata := classifyKnownPath(metadataPath)
	evidence := metadata.evidence
	if identity.symlink && metadata.disposition == "excluded" {
		evidence = fmt.Sprintf("symlink_target_excluded:%s->%s:%s", path, identity.relative, metadata.evidence)
	}
	candidate := Candidate{
		Kind: metadata.kind, Path: path, Selector: "$", SourceLocator: path + "#$",
		Sensitivity: metadata.sensitivity, Trust: metadata.trust,
		ContainsExecutable: metadata.containsExecutable, Evidence: evidence, Disposition: metadata.disposition,
	}
	if !metadata.readContent {
		candidate.Component = captureComponent(path, candidate.Selector, metadata.kind, metadata.containsExecutable, nil)
		return candidate, nil
	}
	info, err := os.Stat(identity.resolved)
	if err != nil {
		return Candidate{}, fmt.Errorf("stat candidate %q: %w", path, err)
	}
	if !info.Mode().IsRegular() {
		return Candidate{}, fmt.Errorf("candidate %q is not a regular file", path)
	}
	content, err := os.ReadFile(identity.resolved)
	if err != nil {
		return Candidate{}, fmt.Errorf("read candidate %q: %w", path, err)
	}
	secrets, blocked, foundMCP, err := extractMCPRequirements(metadataPath, content)
	if err != nil {
		return Candidate{}, fmt.Errorf("inspect MCP configuration %q: %w", path, err)
	}
	credentialInspection := inspectCredentialContent(metadataPath, content)
	secrets = deduplicateStrings(append(secrets, credentialInspection.references...))
	mcpCommand := foundMCP && containsMCPCommand(metadataPath, content)
	if mcpCommand {
		candidate.ContainsExecutable = true
		candidate.Evidence += "+executable_command"
	}
	if info.Mode().Perm()&0o111 != 0 {
		if foundMCP {
			candidate.ContainsExecutable = true
		}
		candidate.Evidence += "+executable_mode"
		if !foundMCP {
			candidate.Disposition = "review"
		}
	}
	if foundMCP && candidate.ContainsExecutable {
		candidate.Disposition = "requires_authorization"
		candidate.Evidence += "+executable_content"
	} else if foundMCP && len(secrets) > 0 {
		candidate.Disposition = "requires_authorization"
		candidate.Evidence += "+mcp_secret_references"
	}
	executableEvidence := strings.TrimPrefix(candidate.Evidence, metadata.evidence)
	if blocked || len(credentialInspection.hits) > 0 {
		candidate.Sensitivity = "credential"
		candidate.Disposition = "excluded"
		if blocked {
			candidate.Evidence = "mcp_env_value_detected" + executableEvidence
		} else {
			candidate.Evidence = "credential_content_detected:" + strings.Join(credentialInspection.hits, ",") + executableEvidence
		}
		candidate.Component = captureComponent(path, candidate.Selector, metadata.kind, candidate.ContainsExecutable, secrets)
		if foundMCP {
			candidate.Component.Type = domain.ComponentIntegration
			candidate.Component.ID = componentID(candidate.Component.Type, path, candidate.Selector)
		}
		return candidate, nil
	}
	candidate.SourceDigest = contentDigest(content)
	candidate.ContentDigest = candidate.SourceDigest
	candidate.Component = captureComponent(path, candidate.Selector, metadata.kind, candidate.ContainsExecutable, secrets)
	if foundMCP {
		candidate.Component.Type = domain.ComponentIntegration
		candidate.Component.ID = componentID(candidate.Component.Type, path, candidate.Selector)
	}
	return candidate, nil
}

func classifyKnownPath(path string) classification {
	normalized := filepath.ToSlash(filepath.Clean(filepath.FromSlash(path)))
	switch normalized {
	case "AGENTS.md", "CLAUDE.md", ".claude/CLAUDE.md":
		return portableClassification("agent_instruction", "user_authored", "known_agent_instruction")
	case ".codex/config.toml":
		return portableClassification("codex_settings", "user_authored", "known_codex_settings")
	case ".claude/settings.json":
		return portableClassification("claude_settings", "user_authored", "known_claude_settings")
	case ".bashrc", ".zshrc":
		return classification{kind: "shell_preferences", sensitivity: "private", trust: "user_authored", evidence: "known_shell_preferences", disposition: "review", containsExecutable: true, readContent: true}
	case ".gitconfig":
		return portableClassification("git_preferences", "user_authored", "known_git_preferences")
	case ".mcp.json", "mcp.json", "opencode.json", "opencode.jsonc", ".opencode.json", ".claude/mcp.json", ".claude/.mcp.json", ".codex/mcp.json", ".codex/.mcp.json":
		return portableClassification("mcp_reference", "user_authored", "known_mcp_reference")
	}
	if isOpenCodeConfigPath(normalized) {
		return portableClassification("mcp_reference", "user_authored", "known_mcp_reference")
	}
	if isAuthCachePath(normalized) {
		return classification{kind: "auth_cache", sensitivity: "credential", trust: "unknown", evidence: "known_auth_cache", disposition: "excluded"}
	}
	if isAgentSessionHistoryPath(normalized) {
		return classification{kind: "agent_session_history", sensitivity: "private", trust: "unknown", evidence: "agent_session_history", disposition: "excluded"}
	}
	parts := strings.Split(normalized, "/")
	if len(parts) == 4 && (parts[0] == ".codex" || parts[0] == ".claude") && parts[1] == "skills" && parts[3] == "SKILL.md" {
		return portableClassification("agent_skill_instruction", "third_party", "open_agent_skill_manifest")
	}
	if len(parts) >= 5 && (parts[0] == ".codex" || parts[0] == ".claude") && parts[1] == "skills" && parts[3] == "scripts" {
		return classification{kind: "agent_skill_executable", sensitivity: "private", trust: "third_party", evidence: "open_agent_skill_script", disposition: "review", containsExecutable: true, readContent: true}
	}
	if strings.HasPrefix(normalized, ".ssh/") && isPrivateKeyPath(normalized) {
		return classification{kind: "private_key", sensitivity: "credential", trust: "unknown", evidence: "private_key_path", disposition: "excluded"}
	}
	return classification{kind: "unknown", sensitivity: "unknown", trust: "unknown", evidence: "unknown_file_in_known_root", disposition: "excluded"}
}

func captureComponent(path, selector, kind string, executable bool, secrets []string) domain.Component {
	componentType := domain.ComponentConfig
	trustClass := domain.TrustDeclarative
	switch kind {
	case "agent_skill_instruction", "agent_skill_executable":
		componentType = domain.ComponentSkill
		if executable {
			trustClass = domain.TrustExecutable
		}
	case "shell_preferences":
		if executable {
			trustClass = domain.TrustExecutable
		}
	case "mcp_reference":
		if executable {
			trustClass = domain.TrustExecutable
		}
	}
	return domain.Component{
		ID:         componentID(componentType, path, selector),
		Type:       componentType,
		Scope:      domain.ScopeUser,
		TrustClass: trustClass,
		Requirements: domain.ComponentRequirements{
			Secrets: append([]string(nil), secrets...),
		},
	}
}

func componentID(componentType domain.ComponentType, path, selector string) string {
	part := filepath.ToSlash(filepath.Clean(path))
	if selector != "" && selector != "$" {
		part += "#" + selector
	}
	part = strings.Map(func(r rune) rune {
		if r == ' ' || r == '\t' || r == '\r' || r == '\n' {
			return '-'
		}
		return r
	}, part)
	return string(componentType) + ":" + part
}

func isAuthCachePath(path string) bool {
	base := strings.ToLower(filepath.Base(path))
	if base != "auth.json" && base != "mcp-auth.json" {
		return false
	}
	for _, prefix := range []string{
		".codex/", ".claude/", ".opencode/", ".config/opencode/", ".config/codex/", ".config/claude/",
		".local/share/opencode/", ".local/share/codex/", ".local/share/claude/", "library/application support/opencode/",
	} {
		if strings.HasPrefix(strings.ToLower(path), prefix) {
			return true
		}
	}
	return false
}

func isOpenCodeConfigPath(path string) bool {
	lowerPath := strings.ToLower(filepath.ToSlash(path))
	for _, prefix := range []string{".opencode/", ".config/opencode/", ".local/share/opencode/", "library/application support/opencode/"} {
		if !strings.HasPrefix(lowerPath, prefix) {
			continue
		}
		base := strings.ToLower(filepath.Base(lowerPath))
		return base == "opencode.json" || base == "opencode.jsonc" || base == "mcp.json" || base == ".mcp.json"
	}
	return false
}

func isAgentSessionHistoryPath(path string) bool {
	path = strings.ToLower(filepath.ToSlash(path))
	for _, prefix := range []string{
		".codex/sessions/", ".config/codex/sessions/", ".local/share/codex/sessions/",
		".claude/projects/", ".config/claude/projects/", ".local/share/claude/projects/", ".claude/history", ".claude/transcripts/",
		".opencode/storage/", ".config/opencode/storage/", ".local/share/opencode/storage/",
		"library/application support/opencode/storage/",
	} {
		if strings.HasPrefix(path, prefix) {
			return true
		}
	}
	return false
}

var (
	tomlMCPSectionPattern = regexp.MustCompile(`(?i)^\s*\[([^]]*mcp[^]]*\.env)\]\s*$`)
	tomlEnvEntryPattern   = regexp.MustCompile(`^\s*([A-Za-z_][A-Za-z0-9_]*)\s*=\s*(.*)$`)
	tomlInlineEnvPattern  = regexp.MustCompile(`(?i)\benv\s*=\s*\{([^}]*)\}`)
	jsonReferencePattern  = regexp.MustCompile(`^(?:\$[A-Za-z_][A-Za-z0-9_]*|\$\{[A-Za-z_][A-Za-z0-9_]*\}|\{env:[A-Za-z_][A-Za-z0-9_]*\}|(?:secret|ref)://\S+)$`)
)

func extractMCPRequirements(path string, content []byte) ([]string, bool, bool, error) {
	ext := filepath.Ext(path)
	if ext == ".toml" {
		secrets, blocked, found := extractTOMLMCPRequirements(content)
		return secrets, blocked, found, nil
	}
	if ext != ".json" && ext != ".jsonc" {
		return nil, false, false, nil
	}

	var value any
	decoder := json.NewDecoder(strings.NewReader(stripJSONComments(string(content))))
	decoder.UseNumber()
	if err := decoder.Decode(&value); err != nil {
		return nil, false, false, fmt.Errorf("decode JSON: %w", err)
	}
	secrets := make([]string, 0)
	blocked := false
	found := strings.Contains(strings.ToLower(filepath.ToSlash(path)), "mcp") || strings.Contains(strings.ToLower(filepath.Base(path)), "opencode")
	var walk func(any, bool)
	walk = func(current any, inMCP bool) {
		switch typed := current.(type) {
		case map[string]any:
			for key, child := range typed {
				lowerKey := strings.ToLower(key)
				if lowerKey == "mcp" || lowerKey == "mcpservers" || lowerKey == "mcp_servers" {
					found = true
				}
				childInMCP := inMCP || lowerKey == "mcp" || lowerKey == "mcpservers" || lowerKey == "mcp_servers"
				if childInMCP && (lowerKey == "env" || lowerKey == "environment") {
					found = true
					extractJSONEnv(child, &secrets, &blocked)
					continue
				}
				walk(child, childInMCP)
			}
		case []any:
			for _, child := range typed {
				walk(child, inMCP)
			}
		}
	}
	walk(value, found)
	sort.Strings(secrets)
	return deduplicateStrings(secrets), blocked, found, nil
}

func extractJSONEnv(value any, secrets *[]string, blocked *bool) {
	object, ok := value.(map[string]any)
	if !ok {
		*blocked = true
		return
	}
	for name, raw := range object {
		*secrets = append(*secrets, name)
		if raw == nil {
			continue
		}
		text, ok := raw.(string)
		if !ok || (strings.TrimSpace(text) != "" && !jsonReferencePattern.MatchString(strings.TrimSpace(text))) {
			*blocked = true
		}
	}
}

func containsMCPCommand(path string, content []byte) bool {
	ext := strings.ToLower(filepath.Ext(path))
	if ext == ".json" || ext == ".jsonc" {
		var value any
		decoder := json.NewDecoder(strings.NewReader(stripJSONComments(string(content))))
		if err := decoder.Decode(&value); err != nil {
			return false
		}
		found := strings.Contains(strings.ToLower(filepath.ToSlash(path)), "mcp") || strings.Contains(strings.ToLower(filepath.Base(path)), "opencode")
		var walk func(any, bool) bool
		walk = func(current any, inMCP bool) bool {
			switch typed := current.(type) {
			case map[string]any:
				for key, child := range typed {
					lowerKey := strings.ToLower(key)
					childInMCP := inMCP || lowerKey == "mcp" || lowerKey == "mcpservers" || lowerKey == "mcp_servers"
					if childInMCP && lowerKey == "command" {
						if command, ok := child.(string); ok && strings.TrimSpace(command) != "" {
							return true
						}
					}
					if walk(child, childInMCP) {
						return true
					}
				}
			case []any:
				for _, child := range typed {
					if walk(child, inMCP) {
						return true
					}
				}
			}
			return false
		}
		return walk(value, found)
	}
	if ext != ".toml" {
		return false
	}
	inMCPSection := false
	for _, rawLine := range strings.Split(string(content), "\n") {
		line := strings.TrimSpace(strings.SplitN(rawLine, "#", 2)[0])
		if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
			inMCPSection = strings.Contains(strings.ToLower(line), "mcp")
			continue
		}
		if !inMCPSection {
			continue
		}
		if match := tomlEnvEntryPattern.FindStringSubmatch(line); match != nil && strings.EqualFold(match[1], "command") && strings.TrimSpace(match[2]) != "" {
			return true
		}
	}
	return false
}

func extractTOMLMCPRequirements(content []byte) ([]string, bool, bool) {
	secrets := make([]string, 0)
	blocked := false
	found := false
	inEnvSection := false
	scanner := bufio.NewScanner(strings.NewReader(string(content)))
	for scanner.Scan() {
		line := strings.TrimSpace(strings.SplitN(scanner.Text(), "#", 2)[0])
		if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
			inEnvSection = tomlMCPSectionPattern.MatchString(line)
			found = found || strings.Contains(strings.ToLower(line), "mcp")
			continue
		}
		if match := tomlInlineEnvPattern.FindStringSubmatch(line); match != nil {
			found = true
			for _, entry := range strings.Split(match[1], ",") {
				if parsed := tomlEnvEntryPattern.FindStringSubmatch(strings.TrimSpace(entry)); parsed != nil {
					tomlSecretValue(parsed[1], parsed[2], &secrets, &blocked)
				}
			}
			continue
		}
		if !inEnvSection {
			continue
		}
		if match := tomlEnvEntryPattern.FindStringSubmatch(line); match != nil {
			tomlSecretValue(match[1], match[2], &secrets, &blocked)
		}
	}
	sort.Strings(secrets)
	return deduplicateStrings(secrets), blocked, found
}

func tomlSecretValue(name, raw string, secrets *[]string, blocked *bool) {
	*secrets = append(*secrets, name)
	value := strings.TrimSpace(raw)
	if len(value) >= 2 && value[0] == '"' && value[len(value)-1] == '"' {
		value = value[1 : len(value)-1]
	}
	if value != "" && !jsonReferencePattern.MatchString(value) {
		*blocked = true
	}
}

func deduplicateStrings(values []string) []string {
	if len(values) < 2 {
		return values
	}
	unique := values[:1]
	for _, value := range values[1:] {
		if value != unique[len(unique)-1] {
			unique = append(unique, value)
		}
	}
	return unique
}

func stripJSONComments(content string) string {
	var builder strings.Builder
	inString := false
	escaped := false
	blockComment := false
	lineComment := false
	for index := 0; index < len(content); index++ {
		character := content[index]
		if lineComment {
			if character == '\n' {
				lineComment = false
				builder.WriteByte(character)
			}
			continue
		}
		if blockComment {
			if character == '*' && index+1 < len(content) && content[index+1] == '/' {
				blockComment = false
				index++
			}
			continue
		}
		if !inString && character == '/' && index+1 < len(content) {
			switch content[index+1] {
			case '/':
				lineComment = true
				index++
				continue
			case '*':
				blockComment = true
				index++
				continue
			}
		}
		builder.WriteByte(character)
		if character == '"' && !escaped {
			inString = !inString
		}
		if character == '\\' && !escaped {
			escaped = true
		} else {
			escaped = false
		}
	}
	return builder.String()
}

func portableClassification(kind, trust, evidence string) classification {
	return classification{kind: kind, sensitivity: "private", trust: trust, evidence: evidence, disposition: "safe", readContent: true}
}

func contentDigest(content []byte) string {
	digest := sha256.Sum256(content)
	return "sha256:" + hex.EncodeToString(digest[:])
}
