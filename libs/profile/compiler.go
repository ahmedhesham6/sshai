// Package profile compiles explicitly selected local configuration into an
// immutable Capsule. Compilation only reads data; selected content is never
// executed.
package profile

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"unicode"

	"github.com/ahmedhesham6/sshai/libs/capsule"
	"github.com/ahmedhesham6/sshai/libs/domain"
)

// Selector identifies one explicitly allowed path and the value within it.
// "$" selects the complete file. "$.name" selectors address JSON object fields.
type Selector struct {
	Path     string
	Selector string
}

// Compile reads only explicitly selected files beneath root and produces a
// deterministic Capsule independent of selector input order.
func Compile(root string, selectors []Selector) (capsule.Capsule, error) {
	return CompileNamed(root, "captured-profile", selectors)
}

// CompileNamed is Compile with a caller-supplied Capsule name.
func CompileNamed(root, name string, selectors []Selector) (capsule.Capsule, error) {
	resolvedRoot, err := resolveProfileRoot(root)
	if err != nil {
		return capsule.Capsule{}, err
	}
	if strings.TrimSpace(name) == "" {
		return capsule.Capsule{}, errors.New("compile Capsule: name is required")
	}

	ordered := append([]Selector(nil), selectors...)
	for index := range ordered {
		ordered[index].Path = filepath.ToSlash(filepath.Clean(ordered[index].Path))
	}
	sort.Slice(ordered, func(i, j int) bool {
		if ordered[i].Path == ordered[j].Path {
			return ordered[i].Selector < ordered[j].Selector
		}
		return ordered[i].Path < ordered[j].Path
	})

	stageRoot, err := os.MkdirTemp("", "sshai-capsule-")
	if err != nil {
		return capsule.Capsule{}, fmt.Errorf("compile Capsule: create staging root: %w", err)
	}
	defer os.RemoveAll(stageRoot)

	components := make([]capsule.Component, 0, len(ordered))
	componentDirs := make(map[string]string, len(ordered))
	seen := make(map[Selector]struct{}, len(ordered))
	for index, selector := range ordered {
		if selector.Path == "" || selector.Selector == "" {
			return capsule.Capsule{}, fmt.Errorf("compile Capsule: path and selector are required")
		}
		if _, exists := seen[selector]; exists {
			return capsule.Capsule{}, fmt.Errorf("duplicate selector %q for %q", selector.Selector, selector.Path)
		}
		seen[selector] = struct{}{}

		selected, err := compileSelection(resolvedRoot, selector)
		if err != nil {
			return capsule.Capsule{}, err
		}
		component := capsuleComponent(selected.component)
		componentDir := filepath.Join(stageRoot, fmt.Sprintf("%04d", index))
		if err := stageSelection(componentDir, selected.path, selected.content, selected.mode); err != nil {
			return capsule.Capsule{}, fmt.Errorf("compile Capsule: stage %q: %w", selector.Path, err)
		}
		components = append(components, component)
		componentDirs[component.ID] = componentDir
	}

	builder := capsule.NewBuilder(0)
	result, err := builder.Build(capsule.Manifest{
		SchemaVersion: capsule.SchemaVersion,
		Name:          name,
		Components:    components,
		Requirements:  capsule.Requirements{},
	}, componentDirs)
	if err != nil {
		return capsule.Capsule{}, fmt.Errorf("compile Capsule: %w", err)
	}
	for _, component := range result.Manifest.Components {
		if err := validateCompiledComponent(component); err != nil {
			return capsule.Capsule{}, fmt.Errorf("compile Capsule: component %q: %w", component.ID, err)
		}
	}
	return result, nil
}

type compiledSelection struct {
	component    domain.Component
	path         string
	sourceDigest string
	content      []byte
	mode         os.FileMode
}

func resolveProfileRoot(root string) (string, error) {
	resolvedRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return "", fmt.Errorf("resolve profile root: %w", err)
	}
	resolvedRoot, err = filepath.Abs(resolvedRoot)
	if err != nil {
		return "", fmt.Errorf("make profile root absolute: %w", err)
	}
	return resolvedRoot, nil
}

func compileSelection(root string, selector Selector) (compiledSelection, error) {
	identity, err := resolveCandidateIdentity(root, selector.Path)
	if err != nil {
		return compiledSelection{}, err
	}
	metadataPath := selector.Path
	if identity.symlink {
		metadataPath = identity.relative
	}
	metadata := classifyKnownPath(metadataPath)
	if !metadata.readContent || metadata.disposition == "excluded" {
		if identity.symlink {
			return compiledSelection{}, fmt.Errorf("selected path %q resolves to excluded target %q (%s)", selector.Path, identity.relative, metadata.evidence)
		}
		return compiledSelection{}, fmt.Errorf("selected path %q is not a portable Capsule candidate", selector.Path)
	}
	info, err := os.Stat(identity.resolved)
	if err != nil {
		return compiledSelection{}, fmt.Errorf("stat selected path %q: %w", selector.Path, err)
	}
	if !info.Mode().IsRegular() {
		return compiledSelection{}, fmt.Errorf("selected path %q is not a regular file", selector.Path)
	}
	source, err := os.ReadFile(identity.resolved)
	if err != nil {
		return compiledSelection{}, fmt.Errorf("read selected path %q: %w", selector.Path, err)
	}
	selectionSource := source
	if strings.EqualFold(filepath.Ext(metadataPath), ".jsonc") && selector.Selector != "$" {
		selectionSource = []byte(stripJSONComments(string(source)))
	}
	content, err := applySelector(selectionSource, selector.Selector)
	if err != nil {
		return compiledSelection{}, fmt.Errorf("select %q from %q: %w", selector.Selector, selector.Path, err)
	}
	secrets, blocked, foundMCP, err := extractMCPRequirements(metadataPath, content)
	if err != nil {
		return compiledSelection{}, fmt.Errorf("inspect MCP configuration %q: %w", selector.Path, err)
	}
	credentialInspection := inspectCredentialContent(metadataPath, content)
	inspectSelectedCredentialKey(selector.Selector, content, &credentialInspection)
	if blocked || len(credentialInspection.hits) > 0 {
		evidence := append([]string(nil), credentialInspection.hits...)
		if blocked {
			evidence = append(evidence, "mcp_env_value_detected")
		}
		return compiledSelection{}, fmt.Errorf("selected source %q contains credential-like data (%s)", selector.Path, strings.Join(evidence, ","))
	}
	secrets = deduplicateStrings(append(secrets, credentialInspection.references...))
	componentKind := metadata.kind
	containsExecutable := metadata.containsExecutable
	if foundMCP && (containsMCPCommand(metadataPath, content) || info.Mode().Perm()&0o111 != 0) {
		containsExecutable = true
	}
	component := captureComponent(selector.Path, selector.Selector, componentKind, containsExecutable, secrets)
	if foundMCP {
		component.Type = domain.ComponentIntegration
		component.ID = componentID(component.Type, selector.Path, selector.Selector)
	}
	return compiledSelection{
		component:    component,
		path:         filepath.ToSlash(filepath.Clean(selector.Path)),
		sourceDigest: contentDigest(source),
		content:      content,
		mode:         info.Mode().Perm(),
	}, nil
}

func stageSelection(root, sourcePath string, content []byte, mode os.FileMode) error {
	target := filepath.Join(root, filepath.FromSlash(sourcePath))
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(target, content, mode); err != nil {
		return err
	}
	return nil
}

func capsuleComponent(component domain.Component) capsule.Component {
	commands := append([]string(nil), component.Requirements.Commands...)
	secrets := append([]string(nil), component.Requirements.Secrets...)
	return capsule.Component{
		ID:         component.ID,
		Type:       capsule.ComponentType(component.Type),
		Scope:      capsule.Scope(component.Scope),
		TrustClass: capsule.TrustClass(component.TrustClass),
		Requirements: capsule.Requirements{
			Commands: commands,
			Secrets:  secrets,
		},
	}
}

func validateCompiledComponent(component capsule.Component) error {
	domainComponent := domain.Component{
		ID: component.ID, Type: domain.ComponentType(component.Type), MediaType: component.MediaType,
		Digest: component.Digest, SizeBytes: component.SizeBytes, Scope: domain.ComponentScope(component.Scope),
		TrustClass: domain.TrustClass(component.TrustClass),
		Requirements: domain.ComponentRequirements{
			Commands: append([]string(nil), component.Requirements.Commands...),
			Secrets:  append([]string(nil), component.Requirements.Secrets...),
		},
	}
	return domainComponent.Validate()
}

type candidateIdentity struct {
	resolved string
	relative string
	symlink  bool
}

func resolveCandidateIdentity(root, selected string) (candidateIdentity, error) {
	clean := filepath.Clean(selected)
	if selected == "" || filepath.IsAbs(selected) || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return candidateIdentity{}, fmt.Errorf("selected path %q escapes profile root", selected)
	}
	candidatePath := filepath.Join(root, clean)
	linkInfo, err := os.Lstat(candidatePath)
	if err != nil {
		return candidateIdentity{}, fmt.Errorf("inspect selected path %q: %w", selected, err)
	}
	resolved, err := filepath.EvalSymlinks(candidatePath)
	if err != nil {
		return candidateIdentity{}, fmt.Errorf("resolve selected path %q: %w", selected, err)
	}
	resolved, err = filepath.Abs(resolved)
	if err != nil {
		return candidateIdentity{}, fmt.Errorf("make selected path %q absolute: %w", selected, err)
	}
	relative, err := filepath.Rel(root, resolved)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return candidateIdentity{}, fmt.Errorf("selected path %q escapes profile root", selected)
	}
	return candidateIdentity{resolved: resolved, relative: filepath.ToSlash(relative), symlink: linkInfo.Mode()&os.ModeSymlink != 0}, nil
}

func safeSelectedPath(root, selected string) (string, error) {
	identity, err := resolveCandidateIdentity(root, selected)
	if err != nil {
		return "", err
	}
	if isPrivateKeyPath(identity.relative) {
		return "", fmt.Errorf("selected path %q is a private-key path", selected)
	}
	return identity.resolved, nil
}

func isPrivateKeyPath(path string) bool {
	name := strings.ToLower(filepath.Base(path))
	return name == "id_rsa" || name == "id_dsa" || name == "id_ecdsa" || name == "id_ed25519" || strings.HasSuffix(name, ".pem") || strings.HasSuffix(name, ".key")
}

func applySelector(content []byte, selector string) ([]byte, error) {
	if selector == "$" {
		return append([]byte(nil), content...), nil
	}
	if !strings.HasPrefix(selector, "$.") {
		return nil, errors.New("selector must be $ or a JSON object path beginning with $.")
	}
	var value any
	decoder := json.NewDecoder(bytes.NewReader(content))
	decoder.UseNumber()
	if err := decoder.Decode(&value); err != nil {
		return nil, fmt.Errorf("decode JSON: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return nil, errors.New("decode JSON: multiple values are not allowed")
	}
	for _, field := range strings.Split(strings.TrimPrefix(selector, "$."), ".") {
		object, ok := value.(map[string]any)
		if !ok || field == "" {
			return nil, fmt.Errorf("field %q does not identify an object value", field)
		}
		value, ok = object[field]
		if !ok {
			return nil, fmt.Errorf("field %q is absent", field)
		}
	}
	selected, err := json.Marshal(value)
	if err != nil {
		return nil, fmt.Errorf("encode selected JSON: %w", err)
	}
	return selected, nil
}

var credentialPatterns = []*regexp.Regexp{
	regexp.MustCompile(`-----BEGIN (?:[A-Z0-9 ]+ )?PRIVATE KEY-----`),
	regexp.MustCompile(`AKIA[0-9A-Z]{16}`),
	regexp.MustCompile(`(?i)(?:token|secret|password|api[_-]?key)\s*[:=]\s*[^\s]{8,}`),
}

func containsCredential(content []byte) bool {
	return len(inspectCredentialContent("", content).hits) > 0
}

type credentialInspection struct {
	hits       []string
	references []string
}

func inspectCredentialContent(path string, content []byte) credentialInspection {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".json", ".jsonc":
		if inspection, ok := inspectJSONCredentialContent(content); ok {
			return inspection
		}
	case ".toml":
		return inspectTOMLCredentialContent(content)
	}
	return inspectTextCredentialContent(content)
}

func inspectJSONCredentialContent(content []byte) (credentialInspection, bool) {
	var value any
	decoder := json.NewDecoder(strings.NewReader(stripJSONComments(string(content))))
	decoder.UseNumber()
	if err := decoder.Decode(&value); err != nil {
		return credentialInspection{}, false
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return credentialInspection{}, false
	}

	inspection := credentialInspection{}
	var walk func(any, string)
	walk = func(current any, key string) {
		switch typed := current.(type) {
		case map[string]any:
			for childKey, child := range typed {
				walk(child, childKey)
			}
		case []any:
			for _, child := range typed {
				walk(child, key)
			}
		case string:
			inspectStructuredString(&inspection, key, typed)
		}
	}
	walk(value, "")
	finalizeCredentialInspection(&inspection)
	return inspection, true
}

func inspectTOMLCredentialContent(content []byte) credentialInspection {
	inspection := credentialInspection{}
	for _, rawLine := range strings.Split(string(content), "\n") {
		line := stripTOMLComment(rawLine)
		separator := strings.IndexByte(line, '=')
		if separator < 0 {
			continue
		}
		key := strings.TrimSpace(line[:separator])
		if dot := strings.LastIndexByte(key, '.'); dot >= 0 {
			key = strings.TrimSpace(key[dot+1:])
		}
		key = strings.Trim(key, " \t\"'")
		values := tomlStringValues(strings.TrimSpace(line[separator+1:]))
		for _, value := range values {
			inspectStructuredString(&inspection, key, value)
		}
	}
	finalizeCredentialInspection(&inspection)
	return inspection
}

func inspectTextCredentialContent(content []byte) credentialInspection {
	inspection := credentialInspection{}
	for _, pattern := range credentialPatterns {
		for _, location := range pattern.FindAllIndex(content, -1) {
			match := string(content[location[0]:location[1]])
			if credentialMatchIsReference(match) {
				continue
			}
			inspection.hits = append(inspection.hits, "credential_pattern_detected")
		}
	}
	finalizeCredentialInspection(&inspection)
	return inspection
}

func inspectStructuredString(inspection *credentialInspection, key, value string) {
	value = strings.TrimSpace(value)
	if value == "" || isCredentialReferenceValue(value) {
		if isCredentialKey(key) && value != "" {
			inspection.references = append(inspection.references, key)
		}
		return
	}
	if isCredentialKey(key) {
		inspection.hits = append(inspection.hits, "credential_key="+key)
		return
	}
	if isHighEntropyString(value) {
		inspection.hits = append(inspection.hits, "high_entropy_key="+key)
	}
}

func inspectSelectedCredentialKey(selector string, content []byte, inspection *credentialInspection) {
	if !strings.HasPrefix(selector, "$.") {
		return
	}
	fields := strings.Split(strings.TrimPrefix(selector, "$."), ".")
	if len(fields) == 0 || fields[len(fields)-1] == "" {
		return
	}
	var value any
	decoder := json.NewDecoder(bytes.NewReader(content))
	if err := decoder.Decode(&value); err != nil {
		return
	}
	text, ok := value.(string)
	if !ok {
		return
	}
	inspectStructuredString(inspection, fields[len(fields)-1], text)
	finalizeCredentialInspection(inspection)
}

func isCredentialKey(key string) bool {
	normalized := strings.NewReplacer("-", "_", " ", "_").Replace(strings.TrimSpace(key))
	var normalizedKey strings.Builder
	var previous rune
	for _, character := range normalized {
		if unicode.IsUpper(character) && normalizedKey.Len() > 0 && previous != '_' {
			normalizedKey.WriteByte('_')
		}
		previous = unicode.ToLower(character)
		normalizedKey.WriteRune(previous)
	}
	normalized = normalizedKey.String()
	if normalized == "apikey" || normalized == "api_key" || normalized == "token" || normalized == "secret" || normalized == "password" || normalized == "credential" || normalized == "authorization" || normalized == "key" {
		return true
	}
	for _, part := range strings.FieldsFunc(normalized, func(r rune) bool { return r == '_' || r == '.' }) {
		if part == "token" || part == "secret" || part == "password" || part == "credential" || part == "authorization" {
			return true
		}
	}
	return strings.HasSuffix(normalized, "_key")
}

func isCredentialReferenceValue(value string) bool {
	value = strings.TrimSpace(value)
	return jsonReferencePattern.MatchString(value) || strings.HasPrefix(value, "ref:") || strings.HasPrefix(value, "secret://")
}

func isHighEntropyString(value string) bool {
	if len(value) < 20 || strings.IndexFunc(value, unicode.IsSpace) >= 0 {
		return false
	}
	counts := make(map[rune]int)
	for _, character := range value {
		counts[character]++
	}
	if len(counts) < 6 {
		return false
	}
	entropy := 0.0
	length := float64(len([]rune(value)))
	for _, count := range counts {
		probability := float64(count) / length
		entropy -= probability * math.Log2(probability)
	}
	return entropy >= 3.5
}

func tomlStringValues(raw string) []string {
	raw = strings.TrimSpace(raw)
	if strings.HasPrefix(raw, "[") {
		values := make([]string, 0)
		for index := 0; index < len(raw); index++ {
			if raw[index] != '"' && raw[index] != '\'' {
				continue
			}
			quote := raw[index]
			start := index + 1
			index++
			for index < len(raw) && raw[index] != quote {
				if raw[index] == '\\' {
					index++
				}
				index++
			}
			values = append(values, raw[start:index])
		}
		return values
	}
	if len(raw) >= 2 && ((raw[0] == '"' && raw[len(raw)-1] == '"') || (raw[0] == '\'' && raw[len(raw)-1] == '\'')) {
		if raw[0] == '"' {
			if value, err := strconv.Unquote(raw); err == nil {
				return []string{value}
			}
		}
		return []string{raw[1 : len(raw)-1]}
	}
	return nil
}

func stripTOMLComment(line string) string {
	inString := byte(0)
	escaped := false
	for index := 0; index < len(line); index++ {
		character := line[index]
		if inString != 0 {
			if character == inString && !escaped {
				inString = 0
			}
			if character == '\\' && !escaped {
				escaped = true
			} else {
				escaped = false
			}
			continue
		}
		if character == '"' || character == '\'' {
			inString = character
			continue
		}
		if character == '#' {
			return line[:index]
		}
	}
	return line
}

func finalizeCredentialInspection(inspection *credentialInspection) {
	sort.Strings(inspection.hits)
	inspection.hits = deduplicateStrings(inspection.hits)
	sort.Strings(inspection.references)
	inspection.references = deduplicateStrings(inspection.references)
}

func credentialMatchIsReference(match string) bool {
	separator := strings.IndexAny(match, ":=")
	if separator == -1 {
		return false
	}
	value := strings.Trim(strings.TrimSpace(match[separator+1:]), "\"'")
	return strings.HasPrefix(value, "$") || strings.HasPrefix(value, "{env:") || strings.HasPrefix(value, "ref:") || strings.HasPrefix(value, "secret://")
}
