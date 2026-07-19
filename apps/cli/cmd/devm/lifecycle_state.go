package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/pelletier/go-toml/v2"
)

const (
	maxLocalStateFileSize = 256 << 10
	localStateVersion     = 1
)

// localConfig contains only user-owned defaults and references. Authentication
// credentials remain isolated under auth/ and are never represented here.
type localConfig struct {
	Version                 int      `toml:"version"`
	DefaultRegion           string   `toml:"default_region"`
	RuntimePreset           string   `toml:"runtime_preset"`
	ProfileVersionID        string   `toml:"profile_version_id"`
	ProjectSeedID           string   `toml:"project_seed_id"`
	SSHKeyIDs               []string `toml:"ssh_key_ids"`
	AutoStopMode            string   `toml:"auto_stop_mode"`
	AutoStopGracePeriodSecs int      `toml:"auto_stop_grace_period_seconds"`
}

func defaultLocalConfig() localConfig {
	return localConfig{
		Version: localStateVersion, DefaultRegion: "eu-central-1", RuntimePreset: "cpu2-mem8",
		AutoStopMode: "when_fully_idle", AutoStopGracePeriodSecs: 300,
	}
}

type projectBinding struct {
	Version            int    `toml:"version"`
	RepositoryIdentity string `toml:"repository_identity"`
	EnvironmentID      string `toml:"environment_id"`
}

type localStateStore struct{ directory string }

func newLocalStateStore(directory string) localStateStore {
	return localStateStore{directory: directory}
}

func (store localStateStore) EnsureConfig(ctx context.Context) error {
	return store.UpdateConfig(ctx, func(*localConfig) error { return nil })
}

func (store localStateStore) ReadConfig() (localConfig, error) {
	directory, err := openAnchoredDirectory(store.directory, false, 0)
	if errors.Is(err, os.ErrNotExist) {
		return defaultLocalConfig(), nil
	}
	if err != nil {
		return localConfig{}, fmt.Errorf("open local state: %w", err)
	}
	defer directory.Close()
	if err := requirePrivateDirectory(directory, "local state"); err != nil {
		return localConfig{}, err
	}
	content, info, err := directory.readRegular("config.toml", maxLocalStateFileSize)
	if errors.Is(err, os.ErrNotExist) {
		return defaultLocalConfig(), nil
	}
	if err != nil {
		return localConfig{}, fmt.Errorf("read config.toml: %w", err)
	}
	if info.Mode().Perm() != 0o600 {
		return localConfig{}, errors.New("config.toml permissions must be 0600")
	}
	return decodeLocalConfig(content)
}

func (store localStateStore) UpdateConfig(ctx context.Context, update func(*localConfig) error) error {
	if update == nil {
		return errors.New("update config.toml: update is required")
	}
	directory, err := openOwnedDirectory(store.directory)
	if err != nil {
		return fmt.Errorf("open local state: %w", err)
	}
	defer directory.Close()
	lock, err := acquirePrivateFileLock(ctx, directory, "state.lock")
	if err != nil {
		return fmt.Errorf("lock local state: %w", err)
	}
	defer lock.Close()
	config := defaultLocalConfig()
	content, info, readErr := directory.readRegular("config.toml", maxLocalStateFileSize)
	if readErr == nil {
		if info.Mode().Perm() != 0o600 {
			return errors.New("config.toml permissions must be 0600")
		}
		config, err = decodeLocalConfig(content)
		if err != nil {
			return err
		}
	} else if !errors.Is(readErr, os.ErrNotExist) {
		return fmt.Errorf("read config.toml: %w", readErr)
	}
	if err := update(&config); err != nil {
		return err
	}
	if err := validateLocalConfig(config); err != nil {
		return err
	}
	encoded, err := toml.Marshal(config)
	if err != nil {
		return errors.New("encode config.toml")
	}
	return directory.writePrivate("config.toml", encoded)
}

func decodeLocalConfig(content []byte) (localConfig, error) {
	config := defaultLocalConfig()
	decoder := toml.NewDecoder(bytes.NewReader(content))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&config); err != nil {
		return localConfig{}, errors.New("config.toml is malformed")
	}
	if err := validateLocalConfig(config); err != nil {
		return localConfig{}, err
	}
	return config, nil
}

func validateLocalConfig(config localConfig) error {
	if config.Version != localStateVersion {
		return errors.New("config.toml has an unsupported version")
	}
	if strings.TrimSpace(config.DefaultRegion) == "" || strings.TrimSpace(config.RuntimePreset) == "" {
		return errors.New("config.toml requires default_region and runtime_preset")
	}
	if config.AutoStopMode == "" || config.AutoStopGracePeriodSecs < 0 || config.AutoStopGracePeriodSecs > 86400 {
		return errors.New("config.toml has an invalid Auto-stop Policy")
	}
	switch config.AutoStopMode {
	case "manual", "when_disconnected", "when_agents_finish", "when_fully_idle":
	default:
		return errors.New("config.toml has an unknown Auto-stop Policy mode")
	}
	seen := make(map[string]struct{}, len(config.SSHKeyIDs))
	for _, id := range config.SSHKeyIDs {
		if strings.TrimSpace(id) == "" {
			return errors.New("config.toml contains an empty SSH key ID")
		}
		if _, exists := seen[id]; exists {
			return errors.New("config.toml contains a duplicate SSH key ID")
		}
		seen[id] = struct{}{}
	}
	return nil
}

func (store localStateStore) ReadProject(identity string) (projectBinding, bool, error) {
	state, err := openAnchoredDirectory(store.directory, false, 0)
	if errors.Is(err, os.ErrNotExist) {
		return projectBinding{}, false, nil
	}
	if err != nil {
		return projectBinding{}, false, fmt.Errorf("open local state: %w", err)
	}
	defer state.Close()
	if err := requirePrivateDirectory(state, "local state"); err != nil {
		return projectBinding{}, false, err
	}
	projects, err := openAnchoredChild(state, "projects", false)
	if errors.Is(err, os.ErrNotExist) {
		return projectBinding{}, false, nil
	}
	if err != nil {
		return projectBinding{}, false, err
	}
	defer projects.Close()
	name := projectBindingName(identity)
	content, info, err := projects.readRegular(name, maxLocalStateFileSize)
	if errors.Is(err, os.ErrNotExist) {
		return projectBinding{}, false, nil
	}
	if err != nil {
		return projectBinding{}, false, fmt.Errorf("read Project Binding: %w", err)
	}
	if info.Mode().Perm() != 0o600 {
		return projectBinding{}, false, errors.New("Project Binding permissions must be 0600")
	}
	var binding projectBinding
	decoder := toml.NewDecoder(bytes.NewReader(content))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&binding); err != nil || binding.Version != localStateVersion || binding.RepositoryIdentity != identity || strings.TrimSpace(binding.EnvironmentID) == "" {
		return projectBinding{}, false, errors.New("Project Binding is malformed or does not match this repository")
	}
	return binding, true, nil
}

func (store localStateStore) BindProject(ctx context.Context, identity, environmentID string) error {
	if identity == "" || strings.TrimSpace(environmentID) == "" {
		return errors.New("write Project Binding: repository identity and Environment ID are required")
	}
	state, err := openOwnedDirectory(store.directory)
	if err != nil {
		return fmt.Errorf("open local state: %w", err)
	}
	defer state.Close()
	projects, err := state.ownedChild("projects")
	if err != nil {
		return fmt.Errorf("open Project Bindings: %w", err)
	}
	defer projects.Close()
	lock, err := acquirePrivateFileLock(ctx, projects, "bindings.lock")
	if err != nil {
		return fmt.Errorf("lock Project Bindings: %w", err)
	}
	defer lock.Close()
	if current, found, readErr := store.readProjectFrom(projects, identity); readErr != nil {
		return readErr
	} else if found {
		if current.EnvironmentID == environmentID {
			return nil
		}
		return errLocalStateConflict
	}
	binding := projectBinding{Version: localStateVersion, RepositoryIdentity: identity, EnvironmentID: environmentID}
	content, err := toml.Marshal(binding)
	if err != nil {
		return errors.New("encode Project Binding")
	}
	return projects.writePrivate(projectBindingName(identity), content)
}

func (store localStateStore) readProjectFrom(projects *anchoredDirectory, identity string) (projectBinding, bool, error) {
	content, info, err := projects.readRegular(projectBindingName(identity), maxLocalStateFileSize)
	if errors.Is(err, os.ErrNotExist) {
		return projectBinding{}, false, nil
	}
	if err != nil || info.Mode().Perm() != 0o600 {
		return projectBinding{}, false, errors.New("existing Project Binding is unsafe")
	}
	var binding projectBinding
	if toml.Unmarshal(content, &binding) != nil || binding.Version != localStateVersion || binding.RepositoryIdentity != identity || binding.EnvironmentID == "" {
		return projectBinding{}, false, errors.New("existing Project Binding is malformed")
	}
	return binding, true, nil
}

func requirePrivateDirectory(directory *anchoredDirectory, label string) error {
	info, err := directory.root.Stat(".")
	if err != nil || !info.IsDir() || info.Mode().Perm() != 0o700 {
		return fmt.Errorf("%s directory permissions must be 0700", label)
	}
	return nil
}

func openAnchoredChild(parent *anchoredDirectory, name string, create bool) (*anchoredDirectory, error) {
	if create {
		return parent.ownedChild(name)
	}
	info, err := parent.root.Lstat(name)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm() != 0o700 {
		return nil, errors.New("local-state child directory is unsafe")
	}
	root, err := parent.root.OpenRoot(name)
	if err != nil {
		return nil, err
	}
	opened, openErr := root.Stat(".")
	current, currentErr := parent.root.Lstat(name)
	if openErr != nil || currentErr != nil || !current.IsDir() || !os.SameFile(opened, current) {
		root.Close()
		return nil, errors.New("local-state child directory changed while opening")
	}
	return &anchoredDirectory{root: root}, nil
}

func projectBindingName(identity string) string {
	digest := sha256.Sum256([]byte(identity))
	return hex.EncodeToString(digest[:]) + ".toml"
}

type gitRunner func(context.Context, string, ...string) (string, error)

func runGit(ctx context.Context, directory string, arguments ...string) (string, error) {
	command := exec.CommandContext(ctx, "git", append([]string{"-C", directory}, arguments...)...)
	var output bytes.Buffer
	command.Stdout = &output
	command.Stderr = io.Discard
	if err := command.Run(); err != nil {
		return "", err
	}
	return strings.TrimSpace(output.String()), nil
}

// canonicalRepositoryIdentity prefers origin's fetch URL so separate local
// clones bind to one Environment. It normalizes URL scheme/host case, removes
// credentials, query/fragment, trailing slash and a terminal .git. Repositories
// without origin use the symlink-resolved absolute Git root prefixed by file://.
func canonicalRepositoryIdentity(ctx context.Context, directory string, git gitRunner) (identity, root string, err error) {
	if git == nil {
		git = runGit
	}
	root, err = git(ctx, directory, "rev-parse", "--show-toplevel")
	if err != nil || !filepath.IsAbs(root) {
		return "", "", errors.New("resolve repository: run devm from inside a Git repository")
	}
	root, err = filepath.EvalSymlinks(filepath.Clean(root))
	if err != nil {
		return "", "", errors.New("resolve repository: canonicalize Git root")
	}
	remote, remoteErr := git(ctx, root, "remote", "get-url", "origin")
	if remoteErr == nil && strings.TrimSpace(remote) != "" {
		identity, err = normalizeGitRemoteAt(remote, root)
		if err != nil {
			return "", "", fmt.Errorf("resolve repository: origin URL: %w", err)
		}
		return identity, root, nil
	}
	return "file://" + filepath.ToSlash(root), root, nil
}

func normalizeGitRemote(remote string) (string, error) {
	return normalizeGitRemoteAt(remote, "")
}

func normalizeGitRemoteAt(remote, repositoryRoot string) (string, error) {
	remote = strings.TrimSpace(remote)
	if remote == "" || strings.ContainsAny(remote, "\r\n\x00") {
		return "", errors.New("URL is invalid")
	}
	// Canonicalize the common SCP-style SSH form without treating a Windows
	// drive letter as a host separator.
	if !strings.Contains(remote, "://") {
		if at := strings.LastIndex(remote, "@"); at >= 0 {
			remote = remote[at+1:]
		}
		if separator := strings.IndexByte(remote, ':'); separator > 0 {
			host := strings.ToLower(remote[:separator])
			path := strings.TrimPrefix(remote[separator+1:], "/")
			return finishRemoteIdentity(host, path)
		}
		absolute, err := filepath.Abs(remote)
		if repositoryRoot != "" && !filepath.IsAbs(remote) {
			absolute, err = filepath.Abs(filepath.Join(repositoryRoot, remote))
		}
		if err != nil {
			return "", errors.New("local origin path is invalid")
		}
		resolved, err := filepath.EvalSymlinks(absolute)
		if err != nil {
			return "", errors.New("local origin path cannot be resolved")
		}
		return "file://" + filepath.ToSlash(filepath.Clean(resolved)), nil
	}
	parsed, err := url.Parse(remote)
	if err != nil {
		return "", errors.New("URL is invalid")
	}
	if strings.EqualFold(parsed.Scheme, "file") {
		if parsed.Host != "" && !strings.EqualFold(parsed.Host, "localhost") {
			return "", errors.New("file URL host is invalid")
		}
		path, err := url.PathUnescape(parsed.EscapedPath())
		if err != nil {
			return "", errors.New("file URL path is invalid")
		}
		resolved, err := filepath.EvalSymlinks(filepath.Clean(path))
		if err != nil || !filepath.IsAbs(resolved) {
			return "", errors.New("file URL path cannot be resolved")
		}
		return "file://" + filepath.ToSlash(resolved), nil
	}
	if parsed.Hostname() == "" {
		return "", errors.New("URL is invalid")
	}
	host := strings.ToLower(parsed.Hostname())
	if port := parsed.Port(); port != "" {
		host += ":" + port
	}
	return finishRemoteIdentity(host, parsed.EscapedPath())
}

func finishRemoteIdentity(host, path string) (string, error) {
	path, err := url.PathUnescape(path)
	if err != nil {
		return "", errors.New("URL path is invalid")
	}
	path = strings.TrimSuffix(strings.TrimRight(path, "/"), ".git")
	path = strings.TrimPrefix(path, "/")
	if host == "" || path == "" {
		return "", errors.New("URL repository path is invalid")
	}
	parts := strings.Split(path, "/")
	filtered := parts[:0]
	for _, part := range parts {
		if part == ".." {
			return "", errors.New("URL repository path is invalid")
		}
		if part != "" && part != "." {
			filtered = append(filtered, part)
		}
	}
	if len(filtered) == 0 {
		return "", errors.New("URL repository path is invalid")
	}
	return "git://" + host + "/" + strings.Join(filtered, "/"), nil
}
