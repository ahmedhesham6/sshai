package guest

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

type DurabilityClass string

const (
	DurabilityDurable    DurabilityClass = "durable"
	DurabilityDisposable DurabilityClass = "disposable"
)

type PersistentVolumeIdentity struct {
	DeviceID string `json:"device_id"`
	VolumeID string `json:"volume_id"`
}

type PersistentMount struct {
	Mounted    bool
	MountPoint string
	DeviceID   string
	VolumeID   string
	Writable   bool
}

type PersistentMountInspector interface {
	InspectPersistentMount(context.Context, string) (PersistentMount, error)
}

type PersistentStateRequest struct {
	Root             string
	ExpectedDeviceID string
	ExpectedVolumeID string
}

type PersistentStateLayout struct {
	Workspace string
	Home      string
	Services  string
	Cache     string
	Platform  string
}

type StateComponentMetadata struct {
	SchemaVersion int             `json:"schema_version"`
	Name          string          `json:"name"`
	Durability    DurabilityClass `json:"durability"`
}

func BootstrapPersistentState(ctx context.Context, request PersistentStateRequest, inspector PersistentMountInspector) (PersistentStateLayout, error) {
	if inspector == nil {
		return PersistentStateLayout{}, errors.New("bootstrap persistent state: mount inspector is required")
	}
	rootPath, err := validatePersistentRoot(request.Root)
	if err != nil {
		return PersistentStateLayout{}, fmt.Errorf("bootstrap persistent state: %w", err)
	}
	expected := PersistentVolumeIdentity{DeviceID: strings.TrimSpace(request.ExpectedDeviceID), VolumeID: strings.TrimSpace(request.ExpectedVolumeID)}
	if expected.DeviceID == "" || expected.VolumeID == "" {
		return PersistentStateLayout{}, errors.New("bootstrap persistent state: expected device and volume identities are required")
	}
	mount, err := inspector.InspectPersistentMount(ctx, rootPath)
	if err != nil {
		return PersistentStateLayout{}, fmt.Errorf("bootstrap persistent state: inspect mount: %w", err)
	}
	if !mount.Mounted || !mount.Writable || filepath.Clean(mount.MountPoint) != rootPath || mount.DeviceID != expected.DeviceID || mount.VolumeID != expected.VolumeID {
		return PersistentStateLayout{}, errors.New("bootstrap persistent state: persistent mount evidence does not match the expected writable provider volume")
	}
	root, err := os.OpenRoot(rootPath)
	if err != nil {
		return PersistentStateLayout{}, fmt.Errorf("bootstrap persistent state: open rooted filesystem: %w", err)
	}
	defer root.Close()
	if err := validateStateRootEntries(root); err != nil {
		return PersistentStateLayout{}, fmt.Errorf("bootstrap persistent state: %w", err)
	}
	for _, directory := range []string{"workspace", "home", "services", "cache", "platform", filepath.Join("platform", "components")} {
		if err := ensureSecureDirectory(root, directory); err != nil {
			return PersistentStateLayout{}, fmt.Errorf("bootstrap persistent state: %w", err)
		}
	}
	if err := verifyRootWritable(root); err != nil {
		return PersistentStateLayout{}, fmt.Errorf("bootstrap persistent state: verify writable root: %w", err)
	}
	if err := ensureVolumeIdentity(root, expected); err != nil {
		return PersistentStateLayout{}, fmt.Errorf("bootstrap persistent state: %w", err)
	}
	components := []StateComponentMetadata{
		{SchemaVersion: 1, Name: "workspace", Durability: DurabilityDurable},
		{SchemaVersion: 1, Name: "home", Durability: DurabilityDurable},
		{SchemaVersion: 1, Name: "services", Durability: DurabilityDurable},
		{SchemaVersion: 1, Name: "cache", Durability: DurabilityDisposable},
	}
	for _, metadata := range components {
		content, err := json.Marshal(metadata)
		if err != nil {
			return PersistentStateLayout{}, fmt.Errorf("bootstrap persistent state: encode %s metadata: %w", metadata.Name, err)
		}
		if err := ensureAtomicFile(root, filepath.Join("platform", "components", metadata.Name+".json"), append(content, '\n')); err != nil {
			return PersistentStateLayout{}, fmt.Errorf("bootstrap persistent state: persist %s metadata: %w", metadata.Name, err)
		}
	}
	return PersistentStateLayout{
		Workspace: filepath.Join(rootPath, "workspace"), Home: filepath.Join(rootPath, "home"),
		Services: filepath.Join(rootPath, "services"), Cache: filepath.Join(rootPath, "cache"), Platform: filepath.Join(rootPath, "platform"),
	}, nil
}

func validatePersistentRoot(rootPath string) (string, error) {
	return validateRootPath(rootPath, true)
}

func validateSystemRoot(rootPath string) (string, error) {
	return validateRootPath(rootPath, false)
}

func validateRootPath(rootPath string, private bool) (string, error) {
	if rootPath == "" || !filepath.IsAbs(rootPath) || filepath.Clean(rootPath) != rootPath {
		return "", errors.New("persistent root must be an absolute clean path")
	}
	resolved, err := filepath.EvalSymlinks(rootPath)
	if err != nil {
		return "", fmt.Errorf("resolve persistent root: %w", err)
	}
	if resolved != rootPath {
		return "", errors.New("persistent root may not contain symbolic links")
	}
	info, err := os.Lstat(rootPath)
	if err != nil {
		return "", fmt.Errorf("inspect persistent root: %w", err)
	}
	if !info.IsDir() {
		return "", errors.New("root must be a real directory")
	}
	permissions := info.Mode().Perm()
	if private && permissions != 0o700 {
		return "", errors.New("persistent root must be a private directory")
	}
	if !private && (permissions&0o022 != 0 || permissions&0o200 == 0) {
		return "", errors.New("system root must be owner-writable and not group/world-writable")
	}
	return rootPath, nil
}

func validateStateRootEntries(root *os.Root) error {
	entries, err := fs.ReadDir(root.FS(), ".")
	if err != nil {
		return fmt.Errorf("read persistent root: %w", err)
	}
	allowed := map[string]struct{}{"workspace": {}, "home": {}, "services": {}, "cache": {}, "platform": {}}
	for _, entry := range entries {
		if _, ok := allowed[entry.Name()]; !ok {
			return fmt.Errorf("unexpected persistent root entry %q", entry.Name())
		}
	}
	return nil
}

func ensureSecureDirectory(root *os.Root, name string) error {
	info, err := root.Lstat(name)
	if errors.Is(err, fs.ErrNotExist) {
		if err := root.Mkdir(name, 0o700); err != nil {
			return fmt.Errorf("create state directory %q: %w", name, err)
		}
		return nil
	}
	if err != nil {
		return fmt.Errorf("inspect state directory %q: %w", name, err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() || info.Mode().Perm() != 0o700 {
		return fmt.Errorf("state directory %q is not a private real directory", name)
	}
	return nil
}

func ensureVolumeIdentity(root *os.Root, expected PersistentVolumeIdentity) error {
	const name = "platform/volume-identity.json"
	content, err := readSecureFile(root, name)
	if err == nil {
		var observed PersistentVolumeIdentity
		if json.Unmarshal(content, &observed) != nil || observed != expected {
			return errors.New("persisted provider volume identity drifted")
		}
		return nil
	}
	if !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("read provider volume identity: %w", err)
	}
	content, err = json.Marshal(expected)
	if err != nil {
		return fmt.Errorf("encode provider volume identity: %w", err)
	}
	return writeAtomicFile(root, name, append(content, '\n'))
}

func ensureAtomicFile(root *os.Root, name string, content []byte) error {
	existing, err := readSecureFile(root, name)
	if err == nil {
		if !bytes.Equal(existing, content) {
			return fmt.Errorf("durability metadata %q drifted", name)
		}
		return nil
	}
	if !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return writeAtomicFile(root, name, content)
}

func validateSecureFile(root *os.Root, name string) error {
	return validateRegularFile(root, name, 0o600)
}

func validateRegularFile(root *os.Root, name string, mode fs.FileMode) error {
	info, err := root.Lstat(name)
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() || info.Mode().Perm() != mode.Perm() {
		return fmt.Errorf("state file %q is not a private regular file", name)
	}
	return nil
}

func readSecureFile(root *os.Root, name string) ([]byte, error) {
	if err := validateSecureFile(root, name); err != nil {
		return nil, err
	}
	return root.ReadFile(name)
}

func writeAtomicFile(root *os.Root, name string, content []byte) error {
	token := make([]byte, 8)
	if _, err := io.ReadFull(rand.Reader, token); err != nil {
		return fmt.Errorf("create atomic file token: %w", err)
	}
	temporary := name + ".tmp-" + hex.EncodeToString(token)
	file, err := root.OpenFile(temporary, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	remove := true
	defer func() {
		if remove {
			_ = root.Remove(temporary)
		}
	}()
	if _, err := file.Write(content); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	if err := root.Link(temporary, name); err != nil {
		return err
	}
	if err := root.Remove(temporary); err != nil {
		return err
	}
	remove = false
	return syncRootDirectory(root, filepath.Dir(name))
}

func verifyRootWritable(root *os.Root) error {
	token := make([]byte, 8)
	if _, err := io.ReadFull(rand.Reader, token); err != nil {
		return err
	}
	name := filepath.Join("platform", ".writable-"+hex.EncodeToString(token))
	file, err := root.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		_ = root.Remove(name)
		return err
	}
	if err := file.Close(); err != nil {
		_ = root.Remove(name)
		return err
	}
	if err := root.Remove(name); err != nil {
		return err
	}
	return syncRootDirectory(root, "platform")
}

func syncRootDirectory(root *os.Root, name string) error {
	directory, err := root.Open(name)
	if err != nil {
		return err
	}
	defer directory.Close()
	return directory.Sync()
}
