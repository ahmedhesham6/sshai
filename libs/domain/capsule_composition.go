package domain

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/pelletier/go-toml/v2"
)

// CapsuleComponentSet is one ordered Capsule contribution to composition.
// The project Capsule is passed separately and is applied after profile
// Capsules, matching the Environment's project authority.
type CapsuleComponentSet struct {
	Ref        string
	Digest     string
	Exclusions []string
	Components []Component
}

// CapsuleComponentSource is a compatibility alias for callers that describe
// one source as a source rather than a set.
type CapsuleComponentSource = CapsuleComponentSet

// ComponentCompositionResult is the pure result of ordered composition.
type ComponentCompositionResult struct {
	Components              map[string]Component
	ResolvedComponents      map[string]Component
	Classification          ResolutionClassification
	PermissionPolicyDigest  string
	ComponentCapsuleDigests map[string]string
	ComponentSources        map[string][]ResolvedComponentSource
}

type componentContribution struct {
	source    CapsuleComponentSet
	component Component
	label     string
}

// ResolveCapsuleComposition resolves ordered profile Capsules and then the
// optional Environment project Capsule. Exclusions are applied at this
// boundary so callers cannot accidentally compose excluded Components.
func ResolveCapsuleComposition(capsules []CapsuleComponentSet, project *CapsuleComponentSet) (ComponentCompositionResult, error) {
	sources := append([]CapsuleComponentSet(nil), capsules...)
	if project != nil {
		sources = append(sources, *project)
	}
	contributions := make(map[string][]componentContribution)
	classification := AutoSafe
	permissionContributions := make([]componentContribution, 0)
	for sourceIndex, source := range sources {
		excluded := make(map[string]struct{}, len(source.Exclusions))
		for _, id := range source.Exclusions {
			excluded[id] = struct{}{}
		}
		label := capsuleSourceLabel(source, sourceIndex)
		for _, component := range source.Components {
			if _, omit := excluded[component.ID]; omit {
				continue
			}
			if err := component.Validate(); err != nil {
				return ComponentCompositionResult{Classification: classification}, fmt.Errorf("resolve Component %q: %w", component.ID, err)
			}
			if componentRequiresReview(component) {
				classification = RequiresReview
			}
			contribution := componentContribution{source: source, component: cloneComponent(component), label: label}
			contributions[component.ID] = append(contributions[component.ID], contribution)
			if component.Type == ComponentPermissionPolicy || component.TrustClass == TrustPermission {
				permissionContributions = append(permissionContributions, contribution)
			}
		}
	}

	result := ComponentCompositionResult{
		Components:              make(map[string]Component, len(contributions)),
		Classification:          classification,
		ComponentCapsuleDigests: make(map[string]string, len(contributions)),
		ComponentSources:        make(map[string][]ResolvedComponentSource, len(contributions)),
	}
	conflicts := make([]ComponentConflict, 0)
	ids := make([]string, 0, len(contributions))
	for id := range contributions {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		parts := contributions[id]
		resolved, sourceDigest, sources, conflict := composeContributions(parts)
		if conflict != nil {
			conflicts = append(conflicts, *conflict)
			continue
		}
		result.Components[id] = resolved
		result.ComponentCapsuleDigests[id] = sourceDigest
		result.ComponentSources[id] = sources
	}
	if len(permissionContributions) > 0 {
		result.PermissionPolicyDigest = effectivePermissionPolicyDigest(permissionContributions)
		result.Classification = RequiresReview
	}
	result.ResolvedComponents = result.Components
	if len(conflicts) > 0 {
		conflictIDs := make([]string, len(conflicts))
		for index, conflict := range conflicts {
			conflictIDs[index] = conflict.ID
		}
		return result, &ComponentConflictError{IDs: conflictIDs, Conflicts: conflicts}
	}
	return result, nil
}

func capsuleSourceLabel(source CapsuleComponentSet, index int) string {
	if strings.TrimSpace(source.Ref) != "" {
		return source.Ref
	}
	if strings.TrimSpace(source.Digest) != "" {
		return source.Digest
	}
	return fmt.Sprintf("capsule[%d]", index)
}

func composeContributions(parts []componentContribution) (Component, string, []ResolvedComponentSource, *ComponentConflict) {
	resolved := cloneComponent(parts[0].component)
	sourceDigest := parts[0].source.Digest
	sources := make([]ResolvedComponentSource, 0, len(parts))
	if len(resolved.Content) > 0 && configFormat(resolved.MediaType, resolved.Content) != "" {
		if document, _, err := parseConfigDocument(resolved.MediaType, resolved.Content); err == nil {
			resolved.Provenance = make(map[string]string)
			flattenConfigProvenance(document, "", resolved.Provenance, parts[0].label)
		}
	}
	conflict := &ComponentConflict{ID: resolved.ID}
	for _, part := range parts {
		conflict.Capsules = append(conflict.Capsules, part.label)
		conflict.Digests = append(conflict.Digests, part.component.Digest)
		sources = append(sources, ResolvedComponentSource{CapsuleDigest: part.source.Digest, ComponentDigest: part.component.Digest})
	}
	for index := 1; index < len(parts); index++ {
		next := parts[index].component
		if resolved.Digest == next.Digest {
			if !sameStringSet(resolved.Requirements.Secrets, next.Requirements.Secrets) {
				// The layer is identical but its declared requirement metadata
				// disagrees, so it must not enter an automatic path.
				continue
			}
			continue
		}
		if mergeableConfig(resolved, next) {
			merged, err := mergeConfigComponents(resolved, next, parts[index].label)
			if err != nil {
				return Component{}, "", nil, &ComponentConflict{ID: resolved.ID, Capsules: conflict.Capsules, Digests: conflict.Digests}
			}
			resolved = merged
			sourceDigest = parts[index].source.Digest
			continue
		}
		return Component{}, "", nil, conflict
	}
	return resolved, sourceDigest, sources, nil
}

func mergeableConfig(left, right Component) bool {
	if left.Type != ComponentConfig || right.Type != ComponentConfig || len(left.Content) == 0 || len(right.Content) == 0 {
		return false
	}
	leftFormat := configFormat(left.MediaType, left.Content)
	rightFormat := configFormat(right.MediaType, right.Content)
	return leftFormat != "" && leftFormat == rightFormat
}

func mergeConfigComponents(left, right Component, source string) (Component, error) {
	leftDocument, format, err := parseConfigDocument(left.MediaType, left.Content)
	if err != nil {
		return Component{}, err
	}
	rightDocument, rightFormat, err := parseConfigDocument(right.MediaType, right.Content)
	if err != nil {
		return Component{}, err
	}
	if format != rightFormat {
		return Component{}, fmt.Errorf("config formats %q and %q do not match", format, rightFormat)
	}
	provenance := cloneStringMap(left.Provenance)
	if provenance == nil {
		provenance = make(map[string]string)
		flattenConfigProvenance(leftDocument, "", provenance, "")
	}
	mergeConfigMaps(leftDocument, rightDocument, "", provenance, source)
	encoded, err := marshalConfigDocument(leftDocument, format)
	if err != nil {
		return Component{}, err
	}
	merged := cloneComponent(left)
	merged.Content = encoded
	merged.SizeBytes = int64(len(encoded))
	merged.Digest = digestBytes(encoded)
	merged.Scope = moreRestrictiveScope(left.Scope, right.Scope)
	merged.Provenance = provenance
	return merged, nil
}

func moreRestrictiveScope(left, right ComponentScope) ComponentScope {
	if left == ScopeProject || right == ScopeProject {
		return ScopeProject
	}
	return ScopeUser
}

// MergeConfigContents applies the deterministic config merge used by domain
// composition. Guests use it to rebuild a merged Component from its verified
// source layers before materialization.
func MergeConfigContents(mediaType string, contents ...[]byte) ([]byte, error) {
	if len(contents) == 0 {
		return nil, fmt.Errorf("merge config contents: at least one source is required")
	}
	document, format, err := parseConfigDocument(mediaType, contents[0])
	if err != nil {
		return nil, err
	}
	for index, content := range contents[1:] {
		sourceDocument, sourceFormat, parseErr := parseConfigDocument(mediaType, content)
		if parseErr != nil {
			return nil, fmt.Errorf("merge config source %d: %w", index+1, parseErr)
		}
		if sourceFormat != format {
			return nil, fmt.Errorf("config formats %q and %q do not match", format, sourceFormat)
		}
		mergeConfigMaps(document, sourceDocument, "", make(map[string]string), "")
	}
	return marshalConfigDocument(document, format)
}

// ContentDigest returns the digest used for synthesized merged config
// content.
func ContentDigest(content []byte) string { return digestBytes(content) }

func configFormat(mediaType string, content []byte) string {
	lower := strings.ToLower(mediaType)
	switch {
	case strings.Contains(lower, "toml"):
		return "toml"
	case strings.Contains(lower, "json"):
		return "json"
	default:
		trimmed := strings.TrimSpace(string(content))
		if strings.HasPrefix(trimmed, "{") {
			return "json"
		}
		return ""
	}
}

func parseConfigDocument(mediaType string, content []byte) (map[string]any, string, error) {
	format := configFormat(mediaType, content)
	switch format {
	case "json":
		var document map[string]any
		if err := json.Unmarshal(content, &document); err != nil {
			return nil, "", fmt.Errorf("parse JSON config: %w", err)
		}
		if document == nil {
			return nil, "", fmt.Errorf("parse JSON config: object is required")
		}
		return document, format, nil
	case "toml":
		document := make(map[string]any)
		if err := toml.Unmarshal(content, &document); err != nil {
			return nil, "", fmt.Errorf("parse TOML config: %w", err)
		}
		return document, format, nil
	default:
		return nil, "", fmt.Errorf("config media type %q is not mergeable JSON/TOML", mediaType)
	}
}

func mergeConfigMaps(destination, source map[string]any, prefix string, provenance map[string]string, sourceLabel string) {
	keys := make([]string, 0, len(source))
	for key := range source {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		path := key
		if prefix != "" {
			path = prefix + "." + key
		}
		sourceValue := source[key]
		sourceMap, sourceIsMap := sourceValue.(map[string]any)
		destinationMap, destinationIsMap := destination[key].(map[string]any)
		if sourceIsMap && destinationIsMap {
			mergeConfigMaps(destinationMap, sourceMap, path, provenance, sourceLabel)
			continue
		}
		destination[key] = cloneConfigValue(sourceValue)
		clearConfigProvenance(provenance, path)
		if sourceIsMap {
			flattenConfigProvenance(sourceMap, path, provenance, sourceLabel)
		} else {
			provenance[path] = sourceLabel
		}
	}
}

func flattenConfigProvenance(document map[string]any, prefix string, provenance map[string]string, source string) {
	keys := make([]string, 0, len(document))
	for key := range document {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		path := key
		if prefix != "" {
			path = prefix + "." + key
		}
		if nested, ok := document[key].(map[string]any); ok {
			flattenConfigProvenance(nested, path, provenance, source)
			continue
		}
		if source != "" {
			provenance[path] = source
		}
	}
}

func clearConfigProvenance(provenance map[string]string, prefix string) {
	for key := range provenance {
		if key == prefix || strings.HasPrefix(key, prefix+".") {
			delete(provenance, key)
		}
	}
}

func cloneConfigValue(value any) any {
	switch typed := value.(type) {
	case map[string]any:
		clone := make(map[string]any, len(typed))
		for key, value := range typed {
			clone[key] = cloneConfigValue(value)
		}
		return clone
	case []any:
		clone := make([]any, len(typed))
		for index, value := range typed {
			clone[index] = cloneConfigValue(value)
		}
		return clone
	default:
		return typed
	}
}

func marshalConfigDocument(document map[string]any, format string) ([]byte, error) {
	if format == "json" {
		return json.Marshal(document)
	}
	return toml.Marshal(document)
}

func digestBytes(value []byte) string {
	digest := sha256.Sum256(value)
	return fmt.Sprintf("sha256:%x", digest)
}

func effectivePermissionPolicyDigest(parts []componentContribution) string {
	sort.SliceStable(parts, func(i, j int) bool {
		if parts[i].component.ID == parts[j].component.ID {
			return parts[i].component.Digest < parts[j].component.Digest
		}
		return parts[i].component.ID < parts[j].component.ID
	})
	type policyPart struct {
		ID      string
		Digest  string
		Capsule string
	}
	canonical := make([]policyPart, len(parts))
	for index, part := range parts {
		canonical[index] = policyPart{ID: part.component.ID, Digest: part.component.Digest, Capsule: part.label}
	}
	encoded, _ := json.Marshal(canonical)
	return digestBytes(encoded)
}
