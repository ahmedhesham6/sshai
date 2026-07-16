package capsule

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"time"
)

// Builder creates deterministic Capsule layers. SourceDateEpoch is used for
// tar entry mtimes; a zero value follows the SOURCE_DATE_EPOCH default.
type Builder struct {
	// SourceDateEpoch pins the modification time written to every tar header.
	SourceDateEpoch int64
}

// NewBuilder creates a Builder configured with sourceDateEpoch. Use zero to
// match the SOURCE_DATE_EPOCH convention for an unspecified value.
func NewBuilder(sourceDateEpoch int64) Builder {
	return Builder{SourceDateEpoch: sourceDateEpoch}
}

type treeEntry struct {
	Path    string
	Info    fs.FileInfo
	IsDir   bool
	Content []byte
}

var readFile = os.ReadFile

// BuildLayer packages one component directory tree as a deterministic tar+gzip
// layer with the media type for componentType.
func (builder Builder) BuildLayer(root string, componentType ComponentType) (Layer, error) {
	if builder.SourceDateEpoch < 0 {
		return Layer{}, fmt.Errorf("build capsule layer: source date epoch cannot be negative: %d", builder.SourceDateEpoch)
	}
	if componentType == "" {
		return Layer{}, fmt.Errorf("build capsule layer: component type is required")
	}
	if !componentType.valid() {
		return Layer{}, fmt.Errorf("build capsule layer: component type %q is invalid", componentType)
	}

	resolvedRoot, err := resolveRoot(root)
	if err != nil {
		return Layer{}, fmt.Errorf("build capsule layer: resolve component root: %w", err)
	}

	entries, index, err := collectTree(resolvedRoot)
	if err != nil {
		return Layer{}, fmt.Errorf("build capsule layer: %w", err)
	}
	if len(index) == 0 {
		return Layer{}, fmt.Errorf("build capsule layer: component is empty")
	}

	layer := Layer{MediaType: LayerMediaType(componentType)}
	layer.Index = index
	indexJSON, err := layer.CanonicalIndexJSON()
	if err != nil {
		return Layer{}, fmt.Errorf("build capsule layer: encode file index: %w", err)
	}

	var compressed bytes.Buffer
	gzipWriter := gzip.NewWriter(&compressed)
	gzipWriter.Header.Name = ""
	gzipWriter.Header.Comment = ""
	gzipWriter.Header.ModTime = time.Time{}
	gzipWriter.Header.Extra = nil
	gzipWriter.Header.OS = 255
	tarWriter := tar.NewWriter(gzipWriter)
	entryTime := time.Unix(builder.SourceDateEpoch, 0).UTC()

	for _, entry := range entries {
		header := &tar.Header{
			Name:       entry.Path,
			Mode:       normalizedMode(entry.Info, entry.IsDir),
			Uid:        0,
			Gid:        0,
			Uname:      "",
			Gname:      "",
			ModTime:    entryTime,
			AccessTime: time.Time{},
			ChangeTime: time.Time{},
			Format:     tar.FormatUSTAR,
		}
		if entry.IsDir {
			header.Typeflag = tar.TypeDir
		} else {
			header.Typeflag = tar.TypeReg
			content := entry.Content
			header.Size = int64(len(content))
			if writeErr := writeTarEntry(tarWriter, header, content); writeErr != nil {
				return Layer{}, fmt.Errorf("build capsule layer: write %q: %w", entry.Path, writeErr)
			}
			continue
		}
		if writeErr := tarWriter.WriteHeader(header); writeErr != nil {
			return Layer{}, fmt.Errorf("build capsule layer: write %q: %w", entry.Path, writeErr)
		}
	}

	indexHeader := &tar.Header{
		Name:       IndexPath,
		Mode:       0o644,
		Uid:        0,
		Gid:        0,
		Uname:      "",
		Gname:      "",
		Size:       int64(len(indexJSON)),
		ModTime:    entryTime,
		AccessTime: time.Time{},
		ChangeTime: time.Time{},
		Typeflag:   tar.TypeReg,
		Format:     tar.FormatUSTAR,
	}
	if err := writeTarEntry(tarWriter, indexHeader, indexJSON); err != nil {
		return Layer{}, fmt.Errorf("build capsule layer: write %q: %w", IndexPath, err)
	}
	if err := tarWriter.Close(); err != nil {
		return Layer{}, fmt.Errorf("build capsule layer: close tar: %w", err)
	}
	if err := gzipWriter.Close(); err != nil {
		return Layer{}, fmt.Errorf("build capsule layer: close gzip: %w", err)
	}

	layer.Bytes = append([]byte(nil), compressed.Bytes()...)
	layer.SizeBytes = int64(len(layer.Bytes))
	layer.Digest = digestBytes(layer.Bytes)
	return layer, nil
}

// Build creates one deterministic layer per manifest Component, fills each
// descriptor's layer digest and size, and computes the capsule digest from the
// resulting canonical manifest.
func (builder Builder) Build(manifest Manifest, componentDirs map[string]string) (Capsule, error) {
	if err := manifest.validate(false); err != nil {
		return Capsule{}, fmt.Errorf("build capsule: %w", err)
	}
	if len(componentDirs) != len(manifest.Components) {
		return Capsule{}, fmt.Errorf("build capsule: component directories do not match manifest components")
	}

	builtManifest := manifest
	builtManifest.Components = append([]Component(nil), manifest.Components...)
	layers := make([]Layer, len(builtManifest.Components))
	usedDirectories := make(map[string]string, len(componentDirs))
	for index := range builtManifest.Components {
		component := &builtManifest.Components[index]
		root, ok := componentDirs[component.ID]
		if !ok {
			return Capsule{}, fmt.Errorf("build capsule: component directory for %q is missing", component.ID)
		}
		resolvedRoot, err := resolveRoot(root)
		if err != nil {
			return Capsule{}, fmt.Errorf("build capsule: component %q: resolve component root: %w", component.ID, err)
		}
		if previousID, exists := usedDirectories[resolvedRoot]; exists {
			return Capsule{}, fmt.Errorf("build capsule: component directory %q is used by components %q and %q", resolvedRoot, previousID, component.ID)
		}
		usedDirectories[resolvedRoot] = component.ID
		layer, err := builder.BuildLayer(resolvedRoot, component.Type)
		if err != nil {
			return Capsule{}, fmt.Errorf("build capsule: component %q: %w", component.ID, err)
		}
		layer.ComponentID = component.ID
		component.MediaType = layer.MediaType
		component.Digest = layer.Digest
		component.SizeBytes = layer.SizeBytes
		layers[index] = layer
	}

	digest, err := ComputeCapsuleDigest(builtManifest)
	if err != nil {
		return Capsule{}, fmt.Errorf("build capsule: compute digest: %w", err)
	}
	return Capsule{
		Manifest: builtManifest,
		Layers:   layers,
		Digest:   digest,
	}, nil
}

func resolveRoot(root string) (string, error) {
	if strings.TrimSpace(root) == "" {
		return "", fmt.Errorf("component root is required")
	}
	absolute, err := filepath.Abs(root)
	if err != nil {
		return "", err
	}
	resolved, err := filepath.EvalSymlinks(absolute)
	if err != nil {
		return "", err
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return "", err
	}
	if !info.IsDir() {
		return "", fmt.Errorf("component root is not a directory")
	}
	return resolved, nil
}

func collectTree(root string) ([]treeEntry, []FileIndexEntry, error) {
	var entries []treeEntry
	var index []FileIndexEntry
	err := filepath.WalkDir(root, func(path string, dirEntry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if path == root {
			return nil
		}
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		relative = filepath.ToSlash(relative)
		if err := validateUSTARPath(relative); err != nil {
			return err
		}
		if relative == IndexPath {
			return fmt.Errorf("path %q is reserved for the layer index", relative)
		}

		if dirEntry.Type()&os.ModeSymlink != 0 {
			resolved, err := filepath.EvalSymlinks(path)
			if err != nil {
				return fmt.Errorf("resolve symlink %q: %w", relative, err)
			}
			relativeTarget, err := filepath.Rel(root, resolved)
			if err != nil {
				return fmt.Errorf("inspect symlink %q: %w", relative, err)
			}
			if relativeTarget == ".." || strings.HasPrefix(relativeTarget, ".."+string(filepath.Separator)) {
				return fmt.Errorf("symlink %q escapes component root", relative)
			}
			return fmt.Errorf("symlink %q is not supported", relative)
		}

		info, err := dirEntry.Info()
		if err != nil {
			return fmt.Errorf("stat %q: %w", relative, err)
		}
		if dirEntry.IsDir() {
			entries = append(entries, treeEntry{Path: relative, Info: info, IsDir: true})
			return nil
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("path %q is not a regular file or directory", relative)
		}
		if nlink, ok := fileLinkCount(info); ok && nlink > 1 {
			return fmt.Errorf("path %q has %d hard links; hard links are not supported", relative, nlink)
		}
		content, err := readFile(path)
		if err != nil {
			return fmt.Errorf("read %q: %w", relative, err)
		}
		index = append(index, FileIndexEntry{
			Path:   relative,
			Digest: digestBytes(content),
			Mode:   uint32(normalizedMode(info, false)),
		})
		entries = append(entries, treeEntry{Path: relative, Info: info, Content: content})
		return nil
	})
	if err != nil {
		return nil, nil, err
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Path < entries[j].Path })
	sort.Slice(index, func(i, j int) bool { return index[i].Path < index[j].Path })
	return entries, index, nil
}

func validateUSTARPath(path string) error {
	for _, character := range path {
		if character == 0 {
			return fmt.Errorf("path %q contains NUL; USTAR names must not contain NUL", path)
		}
		if character > 0x7f {
			return fmt.Errorf("path %q contains non-ASCII characters; USTAR names must be ASCII", path)
		}
	}
	if len(path) <= 100 {
		return nil
	}

	length := len(path)
	if length > 155+1 {
		length = 155 + 1
	} else if path[length-1] == '/' {
		length--
	}
	separator := strings.LastIndex(path[:length], "/")
	nameLength := len(path) - separator - 1
	prefixLength := separator
	if separator <= 0 || nameLength > 100 || nameLength == 0 || prefixLength > 155 {
		return fmt.Errorf("path %q exceeds USTAR limits: name field <= 100 bytes and prefix field <= 155 bytes", path)
	}
	return nil
}

func fileLinkCount(info fs.FileInfo) (uint64, bool) {
	value := reflect.ValueOf(info.Sys())
	for value.IsValid() && (value.Kind() == reflect.Pointer || value.Kind() == reflect.Interface) {
		if value.IsNil() {
			return 0, false
		}
		value = value.Elem()
	}
	if !value.IsValid() || value.Kind() != reflect.Struct {
		return 0, false
	}
	field := value.FieldByName("Nlink")
	if !field.IsValid() {
		return 0, false
	}
	switch field.Kind() {
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return field.Uint(), true
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		if field.Int() < 0 {
			return 0, false
		}
		return uint64(field.Int()), true
	default:
		return 0, false
	}
}

func normalizedMode(info fs.FileInfo, directory bool) int64 {
	if directory || info.Mode().Perm()&0o111 != 0 {
		return 0o755
	}
	return 0o644
}

func writeTarEntry(writer *tar.Writer, header *tar.Header, content []byte) error {
	if err := writer.WriteHeader(header); err != nil {
		return err
	}
	_, err := writer.Write(content)
	return err
}
