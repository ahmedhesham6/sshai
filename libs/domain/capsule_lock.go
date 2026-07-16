package domain

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
)

type LockedCapsule struct {
	Ref    string
	Digest string
}

// ResolvedComponentSource records the ordered source layers for a resolved
// Component. Multiple sources mean the guest must merge those verified layers
// at materialization time; the order is digest-covered and is precedence.
type ResolvedComponentSource struct {
	CapsuleDigest   string
	ComponentDigest string
}

type ResolvedComponent struct {
	ID              string
	Type            ComponentType
	CapsuleDigest   string
	ComponentDigest string
	Scope           ComponentScope
	TrustClass      TrustClass
	Requirements    ComponentRequirements
	Provenance      map[string]string
	Sources         []ResolvedComponentSource
}

type CapsuleLockSnapshot struct {
	ID                   string
	EnvironmentID        string
	ProfileVersionID     string
	ProjectCapsuleDigest string
	Capsules             []LockedCapsule
	ResolvedComponents   map[string]ResolvedComponent
	Digest               string
	CreatedAt            time.Time
}

type CapsuleLock struct{ snapshot CapsuleLockSnapshot }

type canonicalCapsuleLock struct {
	ProfileVersionID     string                       `json:"profileVersionId"`
	ProjectCapsuleDigest string                       `json:"projectCapsuleDigest"`
	Capsules             []canonicalLockedCapsule     `json:"capsules"`
	ResolvedComponents   []canonicalResolvedComponent `json:"resolvedComponents"`
}

type canonicalLockedCapsule struct {
	Ref    string `json:"ref"`
	Digest string `json:"digest"`
}

type canonicalResolvedComponent struct {
	ID              string                    `json:"id"`
	Type            ComponentType             `json:"type,omitempty"`
	CapsuleDigest   string                    `json:"capsuleDigest"`
	ComponentDigest string                    `json:"componentDigest"`
	Scope           ComponentScope            `json:"scope"`
	TrustClass      TrustClass                `json:"trustClass"`
	Requirements    ComponentRequirements     `json:"requirements,omitempty"`
	Provenance      map[string]string         `json:"provenance,omitempty"`
	Sources         []ResolvedComponentSource `json:"sources,omitempty"`
}

// ComputeCapsuleLockDigest computes the content address over the lock's
// Profile Version, project Capsule, ordered Capsules, and sorted Components.
func ComputeCapsuleLockDigest(snapshot CapsuleLockSnapshot) string {
	componentIDs := make([]string, 0, len(snapshot.ResolvedComponents))
	for id := range snapshot.ResolvedComponents {
		componentIDs = append(componentIDs, id)
	}
	sort.Strings(componentIDs)
	components := make([]canonicalResolvedComponent, 0, len(componentIDs))
	for _, id := range componentIDs {
		component := snapshot.ResolvedComponents[id]
		components = append(components, canonicalResolvedComponent{
			ID: id, CapsuleDigest: component.CapsuleDigest, ComponentDigest: component.ComponentDigest,
			Type: component.Type, Scope: component.Scope, TrustClass: component.TrustClass,
			Requirements: component.Requirements, Provenance: cloneStringMap(component.Provenance), Sources: cloneResolvedComponentSources(component.Sources),
		})
	}
	capsules := make([]canonicalLockedCapsule, len(snapshot.Capsules))
	for index, capsule := range snapshot.Capsules {
		capsules[index] = canonicalLockedCapsule{Ref: capsule.Ref, Digest: capsule.Digest}
	}
	canonicalJSON, _ := json.Marshal(canonicalCapsuleLock{
		ProfileVersionID: snapshot.ProfileVersionID, ProjectCapsuleDigest: snapshot.ProjectCapsuleDigest,
		Capsules: capsules, ResolvedComponents: components,
	})
	digest := sha256.Sum256(canonicalJSON)
	return fmt.Sprintf("sha256:%x", digest)
}

func CreateCapsuleLock(snapshot CapsuleLockSnapshot) (CapsuleLock, error) {
	for _, field := range []struct{ name, value string }{
		{name: "ID", value: snapshot.ID},
		{name: "Environment ID", value: snapshot.EnvironmentID},
		{name: "Profile Version ID", value: snapshot.ProfileVersionID},
	} {
		if strings.TrimSpace(field.value) == "" {
			return CapsuleLock{}, fmt.Errorf("create Capsule Lock: %s is required", field.name)
		}
	}
	if !contentDigestPattern.MatchString(snapshot.ProjectCapsuleDigest) {
		return CapsuleLock{}, errors.New("create Capsule Lock: project Capsule digest is invalid")
	}
	if snapshot.Digest != "" && !contentDigestPattern.MatchString(snapshot.Digest) {
		return CapsuleLock{}, errors.New("create Capsule Lock: digest is invalid")
	}
	if len(snapshot.Capsules) == 0 {
		return CapsuleLock{}, errors.New("create Capsule Lock: at least one locked Capsule is required")
	}
	listedDigests := make(map[string]struct{}, len(snapshot.Capsules))
	for index, capsule := range snapshot.Capsules {
		if strings.TrimSpace(capsule.Ref) == "" || strings.TrimSpace(capsule.Ref) != capsule.Ref || !registryCapsuleRefPattern.MatchString(capsule.Ref) {
			return CapsuleLock{}, fmt.Errorf("create Capsule Lock: Capsule %d reference is invalid", index)
		}
		if !contentDigestPattern.MatchString(capsule.Digest) {
			return CapsuleLock{}, fmt.Errorf("create Capsule Lock: Capsule %d digest is invalid", index)
		}
		listedDigests[capsule.Digest] = struct{}{}
	}
	for key, component := range snapshot.ResolvedComponents {
		if strings.TrimSpace(key) == "" || component.ID != key || strings.TrimSpace(component.ID) == "" {
			return CapsuleLock{}, fmt.Errorf("create Capsule Lock: resolved Component %q has inconsistent ID", key)
		}
		if _, listed := listedDigests[component.CapsuleDigest]; !listed && component.CapsuleDigest != snapshot.ProjectCapsuleDigest {
			return CapsuleLock{}, fmt.Errorf("create Capsule Lock: resolved Component %q references an unlisted Capsule digest", key)
		}
		if !contentDigestPattern.MatchString(component.ComponentDigest) {
			return CapsuleLock{}, fmt.Errorf("create Capsule Lock: resolved Component %q digest is invalid", key)
		}
		for sourceIndex, source := range component.Sources {
			if _, listed := listedDigests[source.CapsuleDigest]; !listed && source.CapsuleDigest != snapshot.ProjectCapsuleDigest {
				return CapsuleLock{}, fmt.Errorf("create Capsule Lock: resolved Component %q source %d references an unlisted Capsule digest", key, sourceIndex)
			}
			if !contentDigestPattern.MatchString(source.ComponentDigest) {
				return CapsuleLock{}, fmt.Errorf("create Capsule Lock: resolved Component %q source %d digest is invalid", key, sourceIndex)
			}
		}
		if component.Type != "" && !component.Type.Valid() {
			return CapsuleLock{}, fmt.Errorf("create Capsule Lock: resolved Component %q type is invalid", key)
		}
		if !component.Scope.Valid() {
			return CapsuleLock{}, fmt.Errorf("create Capsule Lock: resolved Component %q scope is invalid", key)
		}
		if !component.TrustClass.Valid() {
			return CapsuleLock{}, fmt.Errorf("create Capsule Lock: resolved Component %q trust class is invalid", key)
		}
	}
	if snapshot.CreatedAt.IsZero() || snapshot.CreatedAt.Location() != time.UTC {
		return CapsuleLock{}, errors.New("create Capsule Lock: creation time must be UTC")
	}
	computedDigest := ComputeCapsuleLockDigest(snapshot)
	if snapshot.Digest == "" {
		snapshot.Digest = computedDigest
	} else if snapshot.Digest != computedDigest {
		return CapsuleLock{}, errors.New("create Capsule Lock: digest does not match lock contents")
	}
	snapshot.CreatedAt = snapshot.CreatedAt.Round(0)
	snapshot.Capsules = append([]LockedCapsule(nil), snapshot.Capsules...)
	snapshot.ResolvedComponents = cloneResolvedComponents(snapshot.ResolvedComponents)
	return CapsuleLock{snapshot: snapshot}, nil
}

func NewCapsuleLock(snapshot CapsuleLockSnapshot) (CapsuleLock, error) {
	return CreateCapsuleLock(snapshot)
}

func (lock CapsuleLock) Snapshot() CapsuleLockSnapshot {
	snapshot := lock.snapshot
	snapshot.Capsules = append([]LockedCapsule(nil), snapshot.Capsules...)
	snapshot.ResolvedComponents = cloneResolvedComponents(snapshot.ResolvedComponents)
	return snapshot
}

func cloneResolvedComponents(components map[string]ResolvedComponent) map[string]ResolvedComponent {
	if components == nil {
		return nil
	}
	returnMap := make(map[string]ResolvedComponent, len(components))
	for key, component := range components {
		component.Requirements.Commands = append([]string(nil), component.Requirements.Commands...)
		component.Requirements.Secrets = append([]string(nil), component.Requirements.Secrets...)
		component.Provenance = cloneStringMap(component.Provenance)
		component.Sources = cloneResolvedComponentSources(component.Sources)
		returnMap[key] = component
	}
	return returnMap
}

func cloneResolvedComponentSources(sources []ResolvedComponentSource) []ResolvedComponentSource {
	if sources == nil {
		return nil
	}
	return append([]ResolvedComponentSource(nil), sources...)
}

func cloneStringMap(values map[string]string) map[string]string {
	if values == nil {
		return nil
	}
	clone := make(map[string]string, len(values))
	for key, value := range values {
		clone[key] = value
	}
	return clone
}

type ResolutionClassification string

const (
	AutoSafe       ResolutionClassification = "auto_safe"
	RequiresReview ResolutionClassification = "requires_review"

	ResolutionAutoSafe        = AutoSafe
	ResolutionRequiresReview  = RequiresReview
	CompositionAutoSafe       = AutoSafe
	CompositionRequiresReview = RequiresReview
)

type CompositionClassification = ResolutionClassification

func (classification ResolutionClassification) NeverAutoSafe() bool {
	return classification == RequiresReview
}

func IsNeverAutoSafe(classification ResolutionClassification) bool {
	return classification.NeverAutoSafe()
}

var ErrComponentConflict = errors.New("component conflict")

type ComponentConflictError struct {
	IDs       []string
	Conflicts []ComponentConflict
}

type ComponentConflict struct {
	ID       string
	Capsules []string
	Digests  []string
}

func (err *ComponentConflictError) Error() string {
	if len(err.Conflicts) == 0 {
		return fmt.Sprintf("conflicting Component IDs: %s", strings.Join(err.IDs, ", "))
	}
	parts := make([]string, 0, len(err.Conflicts))
	for _, conflict := range err.Conflicts {
		parts = append(parts, fmt.Sprintf("%s (capsules: %s)", conflict.ID, strings.Join(conflict.Capsules, ", ")))
	}
	return fmt.Sprintf("conflicting Component IDs: %s", strings.Join(parts, "; "))
}

func (err *ComponentConflictError) Unwrap() error { return ErrComponentConflict }

func ResolveComponents(orderedCapsuleComponents [][]Component) (map[string]Component, ResolutionClassification, error) {
	sets := make([]CapsuleComponentSet, len(orderedCapsuleComponents))
	for index, components := range orderedCapsuleComponents {
		sets[index] = CapsuleComponentSet{Ref: fmt.Sprintf("capsule[%d]", index), Components: components}
	}
	result, err := ResolveCapsuleComposition(sets, nil)
	if err != nil {
		return nil, result.Classification, err
	}
	return result.Components, result.Classification, nil
}

func componentRequiresReview(component Component) bool {
	return component.TrustClass == TrustPermission ||
		component.Type == ComponentIntegration ||
		component.Type == ComponentPermissionPolicy ||
		component.Type == ComponentHook ||
		component.Type == ComponentExtension ||
		len(component.Requirements.Secrets) > 0
}

func cloneComponent(component Component) Component {
	component.Requirements.Commands = append([]string(nil), component.Requirements.Commands...)
	component.Requirements.Secrets = append([]string(nil), component.Requirements.Secrets...)
	component.Content = append([]byte(nil), component.Content...)
	component.Provenance = cloneStringMap(component.Provenance)
	return component
}

func sameStringSet(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	leftCopy := append([]string(nil), left...)
	rightCopy := append([]string(nil), right...)
	sort.Strings(leftCopy)
	sort.Strings(rightCopy)
	for index := range leftCopy {
		if leftCopy[index] != rightCopy[index] {
			return false
		}
	}
	return true
}
