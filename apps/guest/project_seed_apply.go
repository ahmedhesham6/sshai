package guest

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"strings"
)

const projectSeedMarker = "sshai-project-seed"

var gitObjectID = regexp.MustCompile(`^(?:[0-9a-f]{40}|[0-9a-f]{64})$`)
var sha256Digest = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)

// ProjectSeedArtifact is content accompanied by its required SHA-256 digest.
type ProjectSeedArtifact struct {
	SHA256  string
	Content []byte
}

// ProjectSeedApplicationInput describes a Project Seed. The constructor
// validates and copies all content before any workspace mutation can begin.
type ProjectSeedApplicationInput struct {
	RepositoryURL string
	BaseRevision  string
	GitBundle     ProjectSeedArtifact
	TrackedPatch  ProjectSeedArtifact
	UntrackedTar  ProjectSeedArtifact
	Manifest      ProjectSeedArtifact
}

// ProjectSeedApplication owns validation and atomic workspace initialization.
type ProjectSeedApplication struct {
	repositoryURL string
	baseRevision  string
	fingerprint   string
	bundle        []byte
	patch         []byte
	files         []projectSeedFile
}

type projectSeedManifestEntry struct {
	Path          string
	Mode          uint32
	Size          int64
	Executable    bool
	ContentDigest string
}

type projectSeedFile struct {
	entry   projectSeedManifestEntry
	content []byte
}

func NewProjectSeedApplication(input ProjectSeedApplicationInput) (*ProjectSeedApplication, error) {
	if err := validateRepositoryURL(input.RepositoryURL); err != nil {
		return nil, err
	}
	if !gitObjectID.MatchString(input.BaseRevision) {
		return nil, errors.New("Project Seed base revision must be an exact Git object ID")
	}
	if err := validateArtifact("manifest", input.Manifest, true); err != nil {
		return nil, err
	}
	for _, candidate := range []struct {
		name     string
		artifact ProjectSeedArtifact
	}{
		{name: "Git bundle", artifact: input.GitBundle},
		{name: "tracked patch", artifact: input.TrackedPatch},
		{name: "untracked tar", artifact: input.UntrackedTar},
	} {
		if err := validateArtifact(candidate.name, candidate.artifact, false); err != nil {
			return nil, err
		}
	}
	files, err := validateUntracked(input.Manifest.Content, input.UntrackedTar)
	if err != nil {
		return nil, err
	}
	fingerprint := artifactDigest([]byte(strings.Join([]string{
		input.RepositoryURL, input.BaseRevision, input.GitBundle.SHA256,
		input.TrackedPatch.SHA256, input.UntrackedTar.SHA256, input.Manifest.SHA256,
	}, "\x00")))
	return &ProjectSeedApplication{
		repositoryURL: input.RepositoryURL,
		baseRevision:  input.BaseRevision,
		fingerprint:   fingerprint,
		bundle:        append([]byte(nil), input.GitBundle.Content...),
		patch:         append([]byte(nil), input.TrackedPatch.Content...),
		files:         files,
	}, nil
}

func (application *ProjectSeedApplication) Apply(ctx context.Context, workspace string) error {
	if application == nil {
		return errors.New("Project Seed application is not initialized")
	}
	if replayed, err := application.prepareWorkspace(workspace); replayed || err != nil {
		return err
	}
	parent := filepath.Dir(workspace)
	if err := os.MkdirAll(parent, 0o755); err != nil {
		return fmt.Errorf("create workspace parent: %w", err)
	}
	stagingRoot, err := os.MkdirTemp(parent, ".sshai-project-seed-*")
	if err != nil {
		return fmt.Errorf("create Project Seed staging directory: %w", err)
	}
	defer os.RemoveAll(stagingRoot)
	staging := filepath.Join(stagingRoot, "workspace")
	if err := runSeedGit(ctx, parent, "clone", "--no-checkout", "--no-local", "--", application.repositoryURL, staging); err != nil {
		return err
	}
	if err := runSeedGit(ctx, staging, "checkout", "--detach", application.baseRevision); err != nil {
		return err
	}
	if len(application.bundle) > 0 {
		if err := application.applyBundle(ctx, staging); err != nil {
			return err
		}
	}
	if len(application.patch) > 0 {
		if err := application.applyPatch(ctx, staging); err != nil {
			return err
		}
	}
	if err := applyUntracked(staging, application.files); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(staging, ".git", projectSeedMarker), []byte(application.fingerprint+"\n"), 0o444); err != nil {
		return fmt.Errorf("record Project Seed application: %w", err)
	}
	if err := publishProjectSeedWorkspace(staging, workspace); err != nil {
		if replayed, replayErr := application.replayed(workspace); replayed || replayErr != nil {
			return replayErr
		}
		return fmt.Errorf("publish Project Seed workspace: %w", err)
	}
	return nil
}

// prepareWorkspace preserves the remote-authority guard while allowing the
// empty directory created by persistent-state bootstrap to be initialized.
// Removing an empty directory is race-safe: os.Remove fails if another actor
// publishes content between ReadDir and Remove, and that failure is surfaced.
func (application *ProjectSeedApplication) prepareWorkspace(workspace string) (bool, error) {
	replayed, err := application.replayed(workspace)
	if replayed || err == nil {
		return replayed, err
	}
	if !errors.Is(err, ErrProjectSeedWorkspaceDiverged) {
		return false, err
	}
	info, statErr := os.Lstat(workspace)
	if statErr != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return false, err
	}
	entries, readErr := os.ReadDir(workspace)
	if readErr != nil || len(entries) != 0 {
		return false, err
	}
	if removeErr := os.Remove(workspace); removeErr != nil {
		return false, fmt.Errorf("remove empty bootstrap workspace: %w", removeErr)
	}
	return false, nil
}

func (application *ProjectSeedApplication) applyBundle(ctx context.Context, workspace string) error {
	bundlePath := filepath.Join(workspace, ".git", "sshai-project-seed.bundle")
	if err := os.WriteFile(bundlePath, application.bundle, 0o600); err != nil {
		return fmt.Errorf("stage Project Seed Git bundle: %w", err)
	}
	defer os.Remove(bundlePath)
	if err := runSeedGit(ctx, workspace, "bundle", "verify", bundlePath); err != nil {
		return fmt.Errorf("verify Project Seed Git bundle: %w", err)
	}
	heads, err := runSeedGitOutput(ctx, workspace, "bundle", "list-heads", bundlePath)
	if err != nil {
		return fmt.Errorf("inspect Project Seed Git bundle refs: %w", err)
	}
	refspecs, bundledHead, err := projectSeedBundleRefspecs(heads)
	if err != nil {
		return err
	}
	fetchArguments := append([]string{"fetch", "--force", "--no-tags", bundlePath}, refspecs...)
	if err := runSeedGit(ctx, workspace, fetchArguments...); err != nil {
		return fmt.Errorf("import Project Seed Git bundle: %w", err)
	}
	if err := runSeedGit(ctx, workspace, "merge-base", "--is-ancestor", application.baseRevision, bundledHead); err != nil {
		return errors.New("Project Seed Git bundle does not descend from the canonical base revision")
	}
	if err := runSeedGit(ctx, workspace, "checkout", "--detach", bundledHead); err != nil {
		return fmt.Errorf("check out Project Seed Git bundle: %w", err)
	}
	return nil
}

func projectSeedBundleRefspecs(output string) ([]string, string, error) {
	var bundledHead string
	refspecs := make([]string, 0)
	for _, line := range strings.Split(strings.TrimSpace(output), "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 || !gitObjectID.MatchString(fields[0]) {
			return nil, "", errors.New("Project Seed Git bundle has an invalid ref listing")
		}
		source := fields[1]
		if source == "HEAD" {
			bundledHead = fields[0]
			refspecs = append(refspecs, "HEAD:refs/sshai/project-seed/head")
			continue
		}
		if !strings.HasPrefix(source, "refs/") {
			return nil, "", fmt.Errorf("Project Seed Git bundle ref %q is unsafe", source)
		}
		refspecs = append(refspecs, source+":refs/sshai/project-seed/"+strings.TrimPrefix(source, "refs/"))
	}
	if bundledHead == "" {
		return nil, "", errors.New("Project Seed Git bundle does not declare HEAD")
	}
	return refspecs, bundledHead, nil
}

func (application *ProjectSeedApplication) applyPatch(ctx context.Context, workspace string) error {
	patchPath := filepath.Join(workspace, ".git", "sshai-project-seed.patch")
	if err := os.WriteFile(patchPath, application.patch, 0o600); err != nil {
		return fmt.Errorf("stage Project Seed tracked patch: %w", err)
	}
	defer os.Remove(patchPath)
	if err := runSeedGit(ctx, workspace, "apply", "--binary", "--whitespace=nowarn", "--", patchPath); err != nil {
		return fmt.Errorf("apply Project Seed tracked patch: %w", err)
	}
	return nil
}

func validateUntracked(manifestJSON []byte, archive ProjectSeedArtifact) ([]projectSeedFile, error) {
	decoder := json.NewDecoder(bytes.NewReader(manifestJSON))
	decoder.DisallowUnknownFields()
	var manifest []projectSeedManifestEntry
	if err := decoder.Decode(&manifest); err != nil {
		return nil, fmt.Errorf("decode Project Seed manifest: %w", err)
	}
	if manifest == nil {
		return nil, errors.New("Project Seed manifest must be a JSON array")
	}
	if err := requireJSONEnd(decoder); err != nil {
		return nil, err
	}
	declared := make(map[string]projectSeedManifestEntry, len(manifest))
	for _, entry := range manifest {
		if err := validateManifestEntry(entry); err != nil {
			return nil, err
		}
		if _, duplicate := declared[entry.Path]; duplicate {
			return nil, fmt.Errorf("Project Seed manifest declares %q more than once", entry.Path)
		}
		declared[entry.Path] = entry
	}
	if archive.SHA256 == "" {
		if len(declared) != 0 {
			return nil, errors.New("Project Seed manifest requires an untracked tar")
		}
		return nil, nil
	}
	return validateUntrackedTar(archive.Content, declared)
}

func requireJSONEnd(decoder *json.Decoder) error {
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return errors.New("Project Seed manifest must contain one JSON document")
	}
	return nil
}

func validateManifestEntry(entry projectSeedManifestEntry) error {
	clean := path.Clean(entry.Path)
	if entry.Path == "" || clean != entry.Path || path.IsAbs(entry.Path) || clean == ".." || strings.HasPrefix(clean, "../") || strings.ContainsAny(entry.Path, "\\\x00") {
		return fmt.Errorf("Project Seed manifest path %q is unsafe", entry.Path)
	}
	if clean == ".git" || strings.HasPrefix(clean, ".git/") {
		return fmt.Errorf("Project Seed manifest path %q targets Git metadata", entry.Path)
	}
	if entry.Mode > 0o777 || entry.Size < 0 || entry.Executable != (entry.Mode&0o111 != 0) {
		return fmt.Errorf("Project Seed manifest metadata for %q is invalid", entry.Path)
	}
	if !validSHA256(entry.ContentDigest) {
		return fmt.Errorf("Project Seed manifest digest for %q is invalid", entry.Path)
	}
	return nil
}

func validateUntrackedTar(content []byte, declared map[string]projectSeedManifestEntry) ([]projectSeedFile, error) {
	reader := tar.NewReader(bytes.NewReader(content))
	files := make([]projectSeedFile, 0, len(declared))
	seen := make(map[string]struct{}, len(declared))
	for {
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("read Project Seed untracked tar: %w", err)
		}
		entry, ok := declared[header.Name]
		if !ok {
			return nil, fmt.Errorf("Project Seed untracked tar contains undeclared path %q", header.Name)
		}
		if _, duplicate := seen[header.Name]; duplicate {
			return nil, fmt.Errorf("Project Seed untracked tar contains duplicate path %q", header.Name)
		}
		if header.Typeflag != tar.TypeReg && header.Typeflag != tar.TypeRegA {
			return nil, fmt.Errorf("Project Seed untracked path %q is not a regular file", header.Name)
		}
		if header.Size != entry.Size || uint32(header.Mode&0o777) != entry.Mode {
			return nil, fmt.Errorf("Project Seed untracked metadata for %q does not match its manifest", header.Name)
		}
		fileContent, err := io.ReadAll(reader)
		if err != nil {
			return nil, fmt.Errorf("read Project Seed untracked path %q: %w", header.Name, err)
		}
		if int64(len(fileContent)) != entry.Size || artifactDigest(fileContent) != entry.ContentDigest {
			return nil, fmt.Errorf("Project Seed untracked content for %q does not match its manifest", header.Name)
		}
		seen[header.Name] = struct{}{}
		files = append(files, projectSeedFile{entry: entry, content: append([]byte(nil), fileContent...)})
	}
	if len(seen) != len(declared) {
		return nil, errors.New("Project Seed untracked tar is missing a manifest-declared file")
	}
	return files, nil
}

func applyUntracked(workspace string, files []projectSeedFile) error {
	for _, file := range files {
		target := filepath.Join(workspace, filepath.FromSlash(file.entry.Path))
		if err := makeSafeParents(workspace, filepath.Dir(target)); err != nil {
			return err
		}
		handle, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL, os.FileMode(file.entry.Mode))
		if err != nil {
			return fmt.Errorf("create Project Seed untracked path %q: %w", file.entry.Path, err)
		}
		if _, err := handle.Write(file.content); err != nil {
			handle.Close()
			return fmt.Errorf("write Project Seed untracked path %q: %w", file.entry.Path, err)
		}
		if err := handle.Close(); err != nil {
			return fmt.Errorf("close Project Seed untracked path %q: %w", file.entry.Path, err)
		}
		if err := os.Chmod(target, os.FileMode(file.entry.Mode)); err != nil {
			return fmt.Errorf("set Project Seed untracked mode for %q: %w", file.entry.Path, err)
		}
	}
	return nil
}

func makeSafeParents(workspace, directory string) error {
	relative, err := filepath.Rel(workspace, directory)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return errors.New("Project Seed untracked parent escapes the workspace")
	}
	current := workspace
	for _, part := range strings.Split(relative, string(filepath.Separator)) {
		if part == "." || part == "" {
			continue
		}
		current = filepath.Join(current, part)
		info, err := os.Lstat(current)
		if errors.Is(err, os.ErrNotExist) {
			if err := os.Mkdir(current, 0o755); err != nil {
				return fmt.Errorf("create Project Seed untracked directory: %w", err)
			}
			continue
		}
		if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("Project Seed untracked parent %q is not a safe directory", current)
		}
	}
	return nil
}

func (application *ProjectSeedApplication) replayed(workspace string) (bool, error) {
	_, err := os.Lstat(workspace)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("inspect workspace: %w", err)
	}
	marker, err := os.ReadFile(filepath.Join(workspace, ".git", projectSeedMarker))
	if err == nil && strings.TrimSpace(string(marker)) == application.fingerprint {
		return true, nil
	}
	return false, ErrProjectSeedWorkspaceDiverged
}

// ErrProjectSeedWorkspaceDiverged means the durable workspace contains state
// that was not initialized by this exact Project Seed.
var ErrProjectSeedWorkspaceDiverged = errors.New("workspace diverges from this Project Seed")

func validateRepositoryURL(raw string) error {
	repository, err := url.Parse(raw)
	if err != nil || repository.Scheme == "" || repository.User != nil || repository.Fragment != "" || repository.RawQuery != "" {
		return errors.New("Project Seed repository URL must be absolute and credential-free")
	}
	if repository.Scheme == "file" {
		if repository.Path == "" || !filepath.IsAbs(repository.Path) {
			return errors.New("Project Seed file repository URL must have an absolute path")
		}
		return nil
	}
	if repository.Host == "" || (repository.Scheme != "https" && repository.Scheme != "ssh") {
		return errors.New("Project Seed repository URL uses an unsupported transport")
	}
	return nil
}

func validateArtifact(name string, artifact ProjectSeedArtifact, required bool) error {
	if artifact.SHA256 == "" && len(artifact.Content) == 0 && !required {
		return nil
	}
	if artifact.SHA256 == "" || (len(artifact.Content) == 0 && required) {
		return fmt.Errorf("Project Seed %s content and SHA-256 are required", name)
	}
	if artifact.SHA256 != artifactDigest(artifact.Content) {
		return fmt.Errorf("Project Seed %s SHA-256 mismatch", name)
	}
	return nil
}

func artifactDigest(content []byte) string {
	sum := sha256.Sum256(content)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func validSHA256(value string) bool {
	return sha256Digest.MatchString(value)
}

func runSeedGit(ctx context.Context, directory string, arguments ...string) error {
	_, err := runSeedGitOutput(ctx, directory, arguments...)
	return err
}

func runSeedGitOutput(ctx context.Context, directory string, arguments ...string) (string, error) {
	safe := []string{
		"-c", "core.hooksPath=/dev/null",
		"-c", "core.fsmonitor=false",
		"-c", "core.attributesFile=/dev/null",
		"-c", "diff.external=",
		"-c", "core.pager=cat",
		"-c", "protocol.ext.allow=never",
	}
	command := exec.CommandContext(ctx, "git", append(safe, arguments...)...)
	command.Dir = directory
	command.Env = seedGitEnvironment()
	output, err := command.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("git %s: %w: %s", arguments[0], err, strings.TrimSpace(string(output)))
	}
	return string(output), nil
}

func seedGitEnvironment() []string {
	environment := make([]string, 0, len(os.Environ())+5)
	for _, entry := range os.Environ() {
		if !strings.HasPrefix(entry, "GIT_") && !strings.HasPrefix(entry, "SSH_ASKPASS=") {
			environment = append(environment, entry)
		}
	}
	return append(environment,
		"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1", "GIT_PAGER=cat",
		"GIT_TERMINAL_PROMPT=0", "GIT_ASKPASS=/bin/false", "SSH_ASKPASS=/bin/false",
	)
}
