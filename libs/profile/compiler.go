// Package profile compiles explicitly selected local configuration into an
// immutable, content-addressed Profile Version. Compilation only reads data;
// selected content is never executed.
package profile

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// Selector identifies one explicitly allowed path and the value within it.
// "$" selects the complete file. "$.name" selectors address JSON object fields.
type Selector struct {
	Path     string
	Selector string
}

// Artifact is selected Profile content. Content is returned as a copy.
type Artifact struct {
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
	Mode               uint32
	Content            []byte
}

// Version is an immutable compilation result.
type Version struct {
	digest    string
	artifacts []Artifact
}

// Digest returns the content address of the complete Profile Version.
func (v Version) Digest() string { return v.digest }

// Artifacts returns a deep copy sorted by path and selector.
func (v Version) Artifacts() []Artifact { return cloneArtifacts(v.artifacts) }

// Compile reads only explicitly selected files beneath root and produces a
// deterministic Profile Version independent of selector input order.
func Compile(root string, selectors []Selector) (Version, error) {
	resolvedRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return Version{}, fmt.Errorf("resolve profile root: %w", err)
	}
	resolvedRoot, err = filepath.Abs(resolvedRoot)
	if err != nil {
		return Version{}, fmt.Errorf("make profile root absolute: %w", err)
	}

	ordered := append([]Selector(nil), selectors...)
	for i := range ordered {
		ordered[i].Path = filepath.ToSlash(filepath.Clean(ordered[i].Path))
	}
	sort.Slice(ordered, func(i, j int) bool {
		if ordered[i].Path == ordered[j].Path {
			return ordered[i].Selector < ordered[j].Selector
		}
		return ordered[i].Path < ordered[j].Path
	})

	artifacts := make([]Artifact, 0, len(ordered))
	for i, selector := range ordered {
		if i > 0 && selector == ordered[i-1] {
			return Version{}, fmt.Errorf("duplicate selector %q for %q", selector.Selector, selector.Path)
		}
		artifact, err := compileArtifact(resolvedRoot, selector)
		if err != nil {
			return Version{}, err
		}
		artifacts = append(artifacts, artifact)
	}

	encoded, err := json.Marshal(artifacts)
	if err != nil {
		return Version{}, fmt.Errorf("encode profile artifacts: %w", err)
	}
	digest := sha256.Sum256(append([]byte("sshai-profile-v2\x00"), encoded...))
	return Version{digest: "sha256:" + hex.EncodeToString(digest[:]), artifacts: cloneArtifacts(artifacts)}, nil
}

func compileArtifact(root string, selector Selector) (Artifact, error) {
	metadata := classifyKnownPath(selector.Path)
	if !metadata.readContent || metadata.disposition == "excluded" {
		return Artifact{}, fmt.Errorf("selected path %q is not a portable Profile candidate", selector.Path)
	}
	path, err := safeSelectedPath(root, selector.Path)
	if err != nil {
		return Artifact{}, err
	}
	info, err := os.Stat(path)
	if err != nil {
		return Artifact{}, fmt.Errorf("stat selected path %q: %w", selector.Path, err)
	}
	if !info.Mode().IsRegular() {
		return Artifact{}, fmt.Errorf("selected path %q is not a regular file", selector.Path)
	}
	content, err := os.ReadFile(path)
	if err != nil {
		return Artifact{}, fmt.Errorf("read selected path %q: %w", selector.Path, err)
	}
	sourceDigest := contentDigest(content)
	if containsCredential(content) {
		return Artifact{}, fmt.Errorf("selected source %q contains a credential-like value", selector.Path)
	}
	content, err = applySelector(content, selector.Selector)
	if err != nil {
		return Artifact{}, fmt.Errorf("select %q from %q: %w", selector.Selector, selector.Path, err)
	}
	mode := info.Mode().Perm()
	containsExecutable := metadata.containsExecutable
	evidence := metadata.evidence
	if mode&0o111 != 0 && !metadata.containsExecutable {
		evidence += "+executable_mode"
	}
	return Artifact{
		Kind:               metadata.kind,
		Path:               filepath.ToSlash(filepath.Clean(selector.Path)),
		Selector:           selector.Selector,
		SourceLocator:      filepath.ToSlash(filepath.Clean(selector.Path)) + "#" + selector.Selector,
		SourceDigest:       sourceDigest,
		ContentDigest:      contentDigest(content),
		Sensitivity:        metadata.sensitivity,
		Trust:              metadata.trust,
		ContainsExecutable: containsExecutable,
		Evidence:           evidence,
		Mode:               uint32(mode),
		Content:            append([]byte(nil), content...),
	}, nil
}

func safeSelectedPath(root, selected string) (string, error) {
	clean := filepath.Clean(selected)
	if selected == "" || filepath.IsAbs(selected) || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("selected path %q escapes profile root", selected)
	}
	if isPrivateKeyPath(clean) {
		return "", fmt.Errorf("selected path %q is a private-key path", selected)
	}
	resolved, err := filepath.EvalSymlinks(filepath.Join(root, clean))
	if err != nil {
		return "", fmt.Errorf("resolve selected path %q: %w", selected, err)
	}
	relative, err := filepath.Rel(root, resolved)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("selected path %q escapes profile root", selected)
	}
	return resolved, nil
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

func cloneArtifacts(source []Artifact) []Artifact {
	clone := make([]Artifact, len(source))
	for i, artifact := range source {
		clone[i] = artifact
		clone[i].Content = append([]byte(nil), artifact.Content...)
	}
	return clone
}

var credentialPatterns = []*regexp.Regexp{
	regexp.MustCompile(`-----BEGIN (?:[A-Z0-9 ]+ )?PRIVATE KEY-----`),
	regexp.MustCompile(`AKIA[0-9A-Z]{16}`),
	regexp.MustCompile(`(?i)(?:token|secret|password|api[_-]?key)\s*[:=]\s*[^\s]{8,}`),
}

func containsCredential(content []byte) bool {
	for _, pattern := range credentialPatterns {
		if pattern.Match(content) {
			return true
		}
	}
	return false
}
