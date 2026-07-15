// Package projectseed inspects a local Git repository and packages its dirty
// initial state without executing repository content.
package projectseed

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

// Inspection describes repository state relevant to a Project Seed.
type Inspection struct {
	RepositoryURL  string
	Revision       string
	BaseRevision   string
	hasUpstream    bool
	TrackedChanges []string
	UntrackedFiles []string
}

// Entry describes one nonignored untracked file in a Project Seed archive.
type Entry struct {
	Path          string
	Mode          uint32
	Size          int64
	Executable    bool
	ContentDigest string
}

// Metadata is transport-ready content-address metadata for a Project Seed.
type Metadata struct {
	RepositoryURL  string
	Revision       string
	BaseRevision   string
	BundleDigest   string
	PatchDigest    string
	ArchiveDigest  string
	ManifestDigest string
}

// Seed is an immutable package of an initial local Git state.
type Seed struct {
	digest   string
	metadata Metadata
	bundle   []byte
	patch    []byte
	archive  []byte
	manifest []Entry
}

func (s Seed) Digest() string     { return s.digest }
func (s Seed) Metadata() Metadata { return s.metadata }
func (s Seed) Bundle() []byte     { return append([]byte(nil), s.bundle...) }
func (s Seed) Patch() []byte      { return append([]byte(nil), s.patch...) }
func (s Seed) Archive() []byte    { return append([]byte(nil), s.archive...) }
func (s Seed) Manifest() []Entry  { return append([]Entry(nil), s.manifest...) }

// Inspect reads repository metadata and file names only. Git hooks, external
// diff commands, text conversion, pagers, and filesystem monitors are disabled.
func Inspect(ctx context.Context, root string) (Inspection, error) {
	repositoryRoot, err := repositoryRoot(ctx, root)
	if err != nil {
		return Inspection{}, err
	}
	repository, ok := gitOptional(ctx, repositoryRoot, "remote", "get-url", "origin")
	if !ok || strings.TrimSpace(repository) == "" {
		return Inspection{}, fmt.Errorf("repository has no canonical origin URL")
	}
	revision, err := git(ctx, repositoryRoot, "rev-parse", "HEAD")
	if err != nil {
		return Inspection{}, err
	}
	base, hasUpstream := gitOptional(ctx, repositoryRoot, "rev-parse", "@{upstream}")
	if !hasUpstream {
		base = revision
	}
	tracked, err := gitPaths(ctx, repositoryRoot, "diff", "--name-only", "--no-ext-diff", "--no-textconv", "-z", "HEAD", "--")
	if err != nil {
		return Inspection{}, err
	}
	untracked, err := gitPaths(ctx, repositoryRoot, "ls-files", "--others", "--exclude-standard", "-z", "--")
	if err != nil {
		return Inspection{}, err
	}
	return Inspection{
		RepositoryURL:  strings.TrimSpace(repository),
		Revision:       strings.TrimSpace(revision),
		BaseRevision:   strings.TrimSpace(base),
		hasUpstream:    hasUpstream,
		TrackedChanges: tracked,
		UntrackedFiles: untracked,
	}, nil
}

// Package creates a deterministic, content-addressed Project Seed containing
// unpushed commits, all tracked changes, and nonignored untracked regular files.
func Package(ctx context.Context, root string) (Seed, error) {
	inspection, err := Inspect(ctx, root)
	if err != nil {
		return Seed{}, err
	}
	repositoryRoot, err := repositoryRoot(ctx, root)
	if err != nil {
		return Seed{}, err
	}

	bundle, err := createBundle(ctx, repositoryRoot, inspection.BaseRevision, inspection.Revision, inspection.hasUpstream)
	if err != nil {
		return Seed{}, err
	}
	patch, err := gitBytes(ctx, repositoryRoot, "diff", "--binary", "--full-index", "--no-ext-diff", "--no-textconv", "HEAD", "--")
	if err != nil {
		return Seed{}, err
	}
	if containsSecret(patch) {
		return Seed{}, fmt.Errorf("tracked changes contain a credential-like value")
	}
	archive, manifest, err := archiveUntracked(repositoryRoot, inspection.UntrackedFiles)
	if err != nil {
		return Seed{}, err
	}
	manifestJSON, err := json.Marshal(manifest)
	if err != nil {
		return Seed{}, fmt.Errorf("encode Project Seed manifest: %w", err)
	}

	metadata := Metadata{
		RepositoryURL:  inspection.RepositoryURL,
		Revision:       inspection.Revision,
		BaseRevision:   inspection.BaseRevision,
		BundleDigest:   optionalDigest(bundle),
		PatchDigest:    digest(patch),
		ArchiveDigest:  digest(archive),
		ManifestDigest: digest(manifestJSON),
	}
	metadataJSON, err := json.Marshal(metadata)
	if err != nil {
		return Seed{}, fmt.Errorf("encode Project Seed metadata: %w", err)
	}
	return Seed{
		digest:   digest(append([]byte("sshai-project-seed-v1\x00"), metadataJSON...)),
		metadata: metadata,
		bundle:   append([]byte(nil), bundle...),
		patch:    append([]byte(nil), patch...),
		archive:  append([]byte(nil), archive...),
		manifest: append([]Entry(nil), manifest...),
	}, nil
}

func repositoryRoot(ctx context.Context, root string) (string, error) {
	absolute, err := filepath.Abs(root)
	if err != nil {
		return "", fmt.Errorf("make repository path absolute: %w", err)
	}
	output, err := git(ctx, absolute, "rev-parse", "--show-toplevel")
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(output), nil
}

func createBundle(ctx context.Context, root, base, revision string, hasUpstream bool) ([]byte, error) {
	if hasUpstream && base == revision {
		return nil, nil
	}
	file, err := os.CreateTemp("", "sshai-project-seed-*.bundle")
	if err != nil {
		return nil, fmt.Errorf("create temporary Git bundle: %w", err)
	}
	path := file.Name()
	if err := file.Close(); err != nil {
		return nil, fmt.Errorf("close temporary Git bundle: %w", err)
	}
	defer os.Remove(path)
	arguments := []string{"bundle", "create", path, "HEAD"}
	if hasUpstream {
		arguments = append(arguments, "^"+base)
	}
	if _, err := git(ctx, root, arguments...); err != nil {
		return nil, err
	}
	bundle, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read temporary Git bundle: %w", err)
	}
	return bundle, nil
}

func archiveUntracked(root string, paths []string) ([]byte, []Entry, error) {
	ordered := append([]string(nil), paths...)
	sort.Strings(ordered)
	var archive bytes.Buffer
	writer := tar.NewWriter(&archive)
	manifest := make([]Entry, 0, len(ordered))
	for _, path := range ordered {
		if err := validateGitPath(path); err != nil {
			return nil, nil, err
		}
		fullPath := filepath.Join(root, filepath.FromSlash(path))
		info, err := os.Lstat(fullPath)
		if err != nil {
			return nil, nil, fmt.Errorf("stat untracked path %q: %w", path, err)
		}
		if !info.Mode().IsRegular() {
			return nil, nil, fmt.Errorf("untracked path %q is not a regular file", path)
		}
		content, err := os.ReadFile(fullPath)
		if err != nil {
			return nil, nil, fmt.Errorf("read untracked path %q: %w", path, err)
		}
		if containsSecret(content) {
			return nil, nil, fmt.Errorf("untracked path %q contains a credential-like value", path)
		}
		mode := info.Mode().Perm()
		entry := Entry{Path: path, Mode: uint32(mode), Size: int64(len(content)), Executable: mode&0o111 != 0, ContentDigest: digest(content)}
		manifest = append(manifest, entry)
		header := &tar.Header{Name: path, Mode: int64(mode), Size: int64(len(content)), ModTime: time.Unix(0, 0), AccessTime: time.Time{}, ChangeTime: time.Time{}, Format: tar.FormatPAX}
		if err := writer.WriteHeader(header); err != nil {
			return nil, nil, fmt.Errorf("write archive header for %q: %w", path, err)
		}
		if _, err := writer.Write(content); err != nil {
			return nil, nil, fmt.Errorf("write archive content for %q: %w", path, err)
		}
	}
	if err := writer.Close(); err != nil {
		return nil, nil, fmt.Errorf("close untracked archive: %w", err)
	}
	return archive.Bytes(), manifest, nil
}

func validateGitPath(path string) error {
	clean := filepath.ToSlash(filepath.Clean(filepath.FromSlash(path)))
	if path == "" || filepath.IsAbs(path) || clean != path || clean == ".." || strings.HasPrefix(clean, "../") {
		return fmt.Errorf("unsafe Git path %q", path)
	}
	return nil
}

func gitPaths(ctx context.Context, root string, args ...string) ([]string, error) {
	output, err := gitBytes(ctx, root, args...)
	if err != nil {
		return nil, err
	}
	parts := bytes.Split(output, []byte{0})
	paths := make([]string, 0, len(parts))
	for _, part := range parts {
		if len(part) > 0 {
			paths = append(paths, string(part))
		}
	}
	sort.Strings(paths)
	return paths, nil
}

func git(ctx context.Context, root string, args ...string) (string, error) {
	output, err := gitBytes(ctx, root, args...)
	return string(output), err
}

func gitOptional(ctx context.Context, root string, args ...string) (string, bool) {
	output, err := git(ctx, root, args...)
	return output, err == nil
}

func gitBytes(ctx context.Context, root string, args ...string) ([]byte, error) {
	safeArgs := []string{"-c", "core.hooksPath=/dev/null", "-c", "core.fsmonitor=false", "-c", "diff.external=", "-c", "core.pager=cat"}
	command := exec.CommandContext(ctx, "git", append(safeArgs, args...)...)
	command.Dir = root
	command.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1", "GIT_PAGER=cat")
	output, err := command.Output()
	if err != nil {
		var exitError *exec.ExitError
		if errors.As(err, &exitError) {
			return nil, fmt.Errorf("git %s: %s", args[0], strings.TrimSpace(string(exitError.Stderr)))
		}
		return nil, fmt.Errorf("git %s: %w", args[0], err)
	}
	return output, nil
}

func digest(content []byte) string {
	sum := sha256.Sum256(content)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func optionalDigest(content []byte) string {
	if len(content) == 0 {
		return ""
	}
	return digest(content)
}

var secretPatterns = []*regexp.Regexp{
	regexp.MustCompile(`-----BEGIN (?:[A-Z0-9 ]+ )?PRIVATE KEY-----`),
	regexp.MustCompile(`AKIA[0-9A-Z]{16}`),
	regexp.MustCompile(`(?i)(?:token|secret|password|api[_-]?key)\s*[:=]\s*[^\s]{8,}`),
}

func containsSecret(content []byte) bool {
	for _, pattern := range secretPatterns {
		if pattern.Match(content) {
			return true
		}
	}
	return false
}
