package guest

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"github.com/ahmedhesham6/sshai/libs/adapters"
	"github.com/ahmedhesham6/sshai/libs/capsule"
	capsuleoci "github.com/ahmedhesham6/sshai/libs/capsule/oci"
	"github.com/ahmedhesham6/sshai/libs/domain"
	"github.com/ahmedhesham6/sshai/libs/profile"
	orasoci "oras.land/oras-go/v2/content/oci"
)

// ErrCapsuleContentInvalid identifies deterministic disagreement between an
// immutable Capsule and the Capsule Lock that selected it.
var ErrCapsuleContentInvalid = errors.New("Capsule content does not match the Lock")

func invalidCapsuleContent(message string, arguments ...any) error {
	return fmt.Errorf("%w: %s", ErrCapsuleContentInvalid, fmt.Sprintf(message, arguments...))
}

// MaterializeCapsuleLock pulls or loads each Capsule referenced by the Lock,
// verifies the resolved Component and its layer contents, lets the Claude
// adapter translate those Components, then delegates all mutations to the
// generic three-way engine.
func MaterializeCapsuleLock(ctx context.Context, batch CapsuleLockMaterializationBatch) ([]ProfileMaterializationResult, error) {
	if err := validateCapsuleLockMaterializationBatch(batch); err != nil {
		return nil, err
	}
	installed := make(map[string]InstalledMaterialization, len(batch.Installed))
	for _, record := range batch.Installed {
		if record.ComponentID == "" {
			return nil, errors.New("materialize Capsule Lock: installed Component ID is required")
		}
		if _, duplicate := installed[record.ComponentID]; duplicate {
			return nil, fmt.Errorf("materialize Capsule Lock: duplicate installed Component ID %q", record.ComponentID)
		}
		installed[record.ComponentID] = record
	}
	cache, err := openCapsuleCache(batch.CacheRoot)
	if err != nil {
		return nil, fmt.Errorf("materialize Capsule Lock: %w", err)
	}

	snapshot := batch.Lock.Snapshot()
	capsules := make(map[string]capsule.Capsule)
	for _, capsuleDigest := range lockCapsuleDigests(snapshot) {
		value, err := loadOrPullCapsule(ctx, cache, capsuleDigest, batch)
		if err != nil {
			return nil, fmt.Errorf("materialize Capsule Lock: load Capsule %s: %w", capsuleDigest, err)
		}
		capsules[capsuleDigest] = value
	}
	adapterID := batch.AdapterID
	if adapterID == "" {
		adapterID = "claude"
	}
	adapter, err := adapters.For(adapterID)
	if err != nil {
		return nil, invalidCapsuleContent("materialize Capsule Lock: select adapter: %v", err)
	}

	items := make([]ProfileMaterialization, 0, len(snapshot.ResolvedComponents))
	componentIDs := make([]string, 0, len(snapshot.ResolvedComponents))
	for componentID := range snapshot.ResolvedComponents {
		componentIDs = append(componentIDs, componentID)
	}
	sort.Strings(componentIDs)
	for _, componentID := range componentIDs {
		locked := snapshot.ResolvedComponents[componentID]
		component, files, err := resolvedComponentContent(capsules, locked)
		if err != nil {
			return nil, fmt.Errorf("materialize Capsule Lock: Component %q: %w", componentID, err)
		}
		item, err := adapter.Translate(snapshot, locked.CapsuleDigest, component, files, installed[componentID], installed[componentID].ComponentID != "", batch)
		if err != nil {
			return nil, invalidCapsuleContent("materialize Capsule Lock: translate Component %q: %v", componentID, err)
		}
		items = append(items, item)
	}
	removedIDs := make([]string, 0)
	for componentID := range installed {
		if _, stillDesired := snapshot.ResolvedComponents[componentID]; !stillDesired {
			removedIDs = append(removedIDs, componentID)
		}
	}
	sort.Strings(removedIDs)
	for _, componentID := range removedIDs {
		record := installed[componentID]
		if record.Target == "" {
			return nil, fmt.Errorf("materialize Capsule Lock: installed Component %q has no native target", componentID)
		}
		mode := record.Mode
		if mode == "" {
			mode = MaterializationManaged
		}
		selector := record.Selector
		if selector == "" {
			selector = "$"
		}
		items = append(items, ProfileMaterialization{
			ID: componentID, LockID: snapshot.ID, LockDigest: snapshot.Digest, CapsuleDigest: record.CapsuleDigest, ComponentID: componentID,
			ComponentDigest: record.ComponentDigest, AdapterID: record.AdapterID, AdapterVersion: record.AdapterVersion,
			TargetAgentVersion: record.TargetAgentVersion, Scope: record.Scope,
			NonSecretOverridesDigest: record.NonSecretOverridesDigest, SecretVersionIdentifiers: append([]string(nil), record.SecretVersionIdentifiers...),
			EffectiveCacheKey: record.EffectiveCacheKey, Mode: mode, Root: record.Root, Target: record.Target, Selector: selector,
			Directory: record.Directory, FilePaths: append([]string(nil), record.FilePaths...),
			LastAppliedDigest: record.LastAppliedDigest, ObservedDigest: record.ObservedDigest,
		})
	}

	return ApplyProfileMaterializations(ProfileMaterializationBatch{
		HomeRoot: batch.HomeRoot, WorkspaceRoot: batch.WorkspaceRoot, Intent: batch.Intent,
		Items: items, Approvals: batch.Approvals, Metrics: batch.Metrics,
	})
}

func validateCapsuleLockMaterializationBatch(batch CapsuleLockMaterializationBatch) error {
	snapshot := batch.Lock.Snapshot()
	if snapshot.ID == "" || snapshot.Digest == "" {
		return errors.New("materialize Capsule Lock: a valid Capsule Lock is required")
	}
	if batch.Intent != profile.IntentReconcile && batch.Intent != profile.IntentPrune {
		return fmt.Errorf("materialize Capsule Lock: unsupported intent %q", batch.Intent)
	}
	if batch.CacheRoot == "" || !filepath.IsAbs(batch.CacheRoot) || filepath.Clean(batch.CacheRoot) != batch.CacheRoot {
		return errors.New("materialize Capsule Lock: cache State Component root must be an absolute clean path")
	}
	if batch.Intent == profile.IntentReconcile && (batch.HomeRoot == "" || batch.WorkspaceRoot == "") {
		return errors.New("materialize Capsule Lock: home and workspace State Component roots are required")
	}
	return nil
}

func lockCapsuleDigests(snapshot domain.CapsuleLockSnapshot) []string {
	seen := make(map[string]struct{})
	for _, component := range snapshot.ResolvedComponents {
		seen[component.CapsuleDigest] = struct{}{}
		for _, source := range component.Sources {
			seen[source.CapsuleDigest] = struct{}{}
		}
	}
	if len(seen) == 0 && snapshot.ProjectCapsuleDigest != "" {
		seen[snapshot.ProjectCapsuleDigest] = struct{}{}
	}
	result := make([]string, 0, len(seen))
	for digest := range seen {
		result = append(result, digest)
	}
	sort.Strings(result)
	return result
}

func openCapsuleCache(cacheRoot string) (*orasoci.Store, error) {
	info, err := os.Lstat(cacheRoot)
	if errors.Is(err, os.ErrNotExist) {
		if err := os.MkdirAll(cacheRoot, 0o700); err != nil {
			return nil, err
		}
		info, err = os.Lstat(cacheRoot)
	}
	if err != nil {
		return nil, err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return nil, errors.New("cache State Component root is not a safe directory")
	}
	layoutRoot := filepath.Join(cacheRoot, "capsule-oci")
	if err := os.MkdirAll(layoutRoot, 0o700); err != nil {
		return nil, err
	}
	layoutInfo, err := os.Lstat(layoutRoot)
	if err != nil {
		return nil, err
	}
	if !layoutInfo.IsDir() || layoutInfo.Mode()&os.ModeSymlink != 0 {
		return nil, errors.New("Capsule blob cache is not a safe directory")
	}
	return orasoci.New(layoutRoot)
}

func loadOrPullCapsule(ctx context.Context, cache *orasoci.Store, capsuleDigest string, batch CapsuleLockMaterializationBatch) (capsule.Capsule, error) {
	if _, err := cache.Resolve(ctx, capsuleDigest); err == nil {
		value, err := capsuleoci.Parse(ctx, cache, capsuleDigest)
		if err != nil {
			return capsule.Capsule{}, invalidCapsuleContent("cached Capsule failed verification: %v", err)
		}
		if value.Digest != capsuleDigest {
			return capsule.Capsule{}, invalidCapsuleContent("cached Capsule digest does not match the Lock")
		}
		return value, nil
	}
	if batch.Grants == nil {
		return capsule.Capsule{}, errors.New("Capsule is not cached and a GrantProvider is required")
	}
	client, err := capsuleoci.NewClient(batch.OwnerID, batch.Grants)
	if err != nil {
		return capsule.Capsule{}, err
	}
	value, err := client.Pull(ctx, capsuleDigest, cache, nil)
	if err != nil {
		if errors.Is(err, capsuleoci.ErrContentInvalid) {
			return capsule.Capsule{}, invalidCapsuleContent("pulled Capsule failed verification: %v", err)
		}
		return capsule.Capsule{}, err
	}
	if value.Digest != capsuleDigest {
		return capsule.Capsule{}, invalidCapsuleContent("pulled Capsule digest does not match the Lock")
	}
	if batch.Metrics != nil {
		batch.Metrics.AddCounter(domain.MetricCapsulePullsTotal, 1)
	}
	return value, nil
}

func resolvedComponentContent(capsules map[string]capsule.Capsule, locked domain.ResolvedComponent) (capsule.Component, []capsuleFile, error) {
	sources := append([]domain.ResolvedComponentSource(nil), locked.Sources...)
	if len(sources) == 0 {
		sources = []domain.ResolvedComponentSource{{CapsuleDigest: locked.CapsuleDigest, ComponentDigest: locked.ComponentDigest}}
	}
	components := make([]capsule.Component, 0, len(sources))
	contents := make([][]byte, 0, len(sources))
	var firstFiles []capsuleFile
	for index, source := range sources {
		value, present := capsules[source.CapsuleDigest]
		if !present {
			return capsule.Component{}, nil, invalidCapsuleContent("source Capsule %q is absent from the Lock", source.CapsuleDigest)
		}
		sourceLock := locked
		sourceLock.CapsuleDigest = source.CapsuleDigest
		sourceLock.ComponentDigest = source.ComponentDigest
		component, files, err := resolvedSourceComponentContent(value, sourceLock)
		if err != nil {
			return capsule.Component{}, nil, fmt.Errorf("source %d: %w", index, err)
		}
		if index == 0 {
			firstFiles = files
		} else if len(files) != len(firstFiles) {
			return capsule.Component{}, nil, invalidCapsuleContent("merged Component source file shapes do not match")
		}
		for fileIndex, file := range files {
			if file.Path != firstFiles[fileIndex].Path {
				return capsule.Component{}, nil, invalidCapsuleContent("merged Component source paths do not match")
			}
		}
		components = append(components, component)
		if len(sources) > 1 {
			if component.Type != capsule.ComponentTypeConfig || len(files) != 1 {
				return capsule.Component{}, nil, invalidCapsuleContent("only single-file config Components can be merged")
			}
			contents = append(contents, files[0].Content)
		}
	}

	component := components[0]
	if len(components) > 1 {
		merged, err := domain.MergeConfigContents(component.MediaType, contents...)
		if err != nil {
			return capsule.Component{}, nil, invalidCapsuleContent("merge source config: %v", err)
		}
		if domain.ContentDigest(merged) != locked.ComponentDigest {
			return capsule.Component{}, nil, invalidCapsuleContent("merged Component digest does not match the Lock")
		}
		component.Digest = locked.ComponentDigest
		component.SizeBytes = int64(len(merged))
		firstFiles[0].Content = merged
		firstFiles[0].Mode = os.FileMode(0o644)
	} else if component.Digest != locked.ComponentDigest {
		return capsule.Component{}, nil, invalidCapsuleContent("Component digest does not match the Lock")
	}
	effectiveScope := capsule.ScopeUser
	for _, source := range components {
		if source.Scope == capsule.ScopeProject {
			effectiveScope = capsule.ScopeProject
		}
	}
	if effectiveScope != capsule.Scope(locked.Scope) {
		return capsule.Component{}, nil, invalidCapsuleContent("Component effective scope does not match the Lock")
	}
	component.Scope = effectiveScope
	component.TrustClass = capsule.TrustClass(locked.TrustClass)
	return component, firstFiles, nil
}

func resolvedSourceComponentContent(value capsule.Capsule, locked domain.ResolvedComponent) (capsule.Component, []capsuleFile, error) {
	if value.Digest != locked.CapsuleDigest {
		return capsule.Component{}, nil, invalidCapsuleContent("Capsule digest does not match the Lock")
	}
	index := -1
	for i, component := range value.Manifest.Components {
		if component.ID == locked.ID {
			index = i
			break
		}
	}
	if index < 0 || index >= len(value.Layers) {
		return capsule.Component{}, nil, invalidCapsuleContent("Component is absent from the pulled Capsule")
	}
	component := value.Manifest.Components[index]
	if component.Digest != locked.ComponentDigest || (locked.Type != "" && component.Type != capsule.ComponentType(locked.Type)) || capsule.TrustClass(component.TrustClass) != capsule.TrustClass(locked.TrustClass) {
		return capsule.Component{}, nil, invalidCapsuleContent("Component metadata does not match the Lock")
	}
	layer := value.Layers[index]
	if layer.ComponentID != component.ID || layer.Digest != component.Digest || materializationContentDigest(layer.Bytes) != layer.Digest {
		return capsule.Component{}, nil, invalidCapsuleContent("Component layer digest verification failed")
	}
	files, err := extractCapsuleLayer(layer)
	if err != nil {
		return capsule.Component{}, nil, invalidCapsuleContent("extract Component layer: %v", err)
	}
	return component, files, nil
}

func extractCapsuleLayer(layer capsule.Layer) ([]capsuleFile, error) {
	reader, err := gzip.NewReader(bytes.NewReader(layer.Bytes))
	if err != nil {
		return nil, fmt.Errorf("open verified Component layer: %w", err)
	}
	defer reader.Close()
	tarReader := tar.NewReader(reader)
	indexed := make(map[string]capsule.FileIndexEntry, len(layer.Index))
	for _, entry := range layer.Index {
		if entry.Path == capsule.IndexPath {
			return nil, errors.New("Component layer index cannot index its reserved index.json path")
		}
		if _, duplicate := indexed[entry.Path]; duplicate {
			return nil, fmt.Errorf("Component layer index contains duplicate path %q", entry.Path)
		}
		indexed[entry.Path] = entry
	}
	seen := make(map[string]struct{}, len(indexed))
	seenLayerPaths := make(map[string]struct{}, len(indexed)+1)
	files := make([]capsuleFile, 0, len(indexed))
	for {
		header, err := tarReader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("read Component layer: %w", err)
		}
		if _, duplicate := seenLayerPaths[header.Name]; duplicate {
			return nil, fmt.Errorf("Component layer contains duplicate path %q", header.Name)
		}
		seenLayerPaths[header.Name] = struct{}{}
		if header.Name == capsule.IndexPath {
			if header.Typeflag != tar.TypeReg {
				return nil, errors.New("Component layer index.json is not a regular file")
			}
			indexContent, readErr := io.ReadAll(tarReader)
			if readErr != nil {
				return nil, fmt.Errorf("read Component layer index.json: %w", readErr)
			}
			canonical, canonicalErr := (capsule.Layer{Index: layer.Index}).CanonicalIndexJSON()
			if canonicalErr != nil || !bytes.Equal(canonical, indexContent) {
				return nil, errors.New("Component layer index.json is not canonical")
			}
			continue
		}
		clean := path.Clean(header.Name)
		if clean != header.Name || path.IsAbs(header.Name) || clean == ".." || strings.HasPrefix(clean, "../") {
			return nil, fmt.Errorf("Component layer path %q escapes its root", header.Name)
		}
		if header.Typeflag == tar.TypeDir {
			continue
		}
		if header.Typeflag != tar.TypeReg {
			return nil, fmt.Errorf("Component layer entry %q is not a regular file", header.Name)
		}
		entry, ok := indexed[header.Name]
		if !ok {
			return nil, fmt.Errorf("Component layer file %q is missing from its index", header.Name)
		}
		content, err := io.ReadAll(tarReader)
		if err != nil {
			return nil, err
		}
		if materializationContentDigest(content) != entry.Digest {
			return nil, fmt.Errorf("Component file %q digest does not match its index", header.Name)
		}
		mode := os.FileMode(header.Mode).Perm()
		if mode != 0o644 && mode != 0o755 {
			return nil, fmt.Errorf("Component file %q has unsafe mode %o", header.Name, mode)
		}
		if uint32(mode) != entry.Mode {
			return nil, fmt.Errorf("Component file %q mode does not match its index", header.Name)
		}
		seen[header.Name] = struct{}{}
		files = append(files, capsuleFile{Path: header.Name, Content: content, Mode: mode})
	}
	if len(seen) != len(indexed) {
		return nil, errors.New("Component layer index contains a missing file")
	}
	sort.Slice(files, func(i, j int) bool { return files[i].Path < files[j].Path })
	return files, nil
}
