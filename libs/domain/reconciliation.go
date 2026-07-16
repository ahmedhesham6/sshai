package domain

import "sort"

type InstalledComponentState struct {
	ComponentID     string
	LockDigest      string
	CapsuleDigest   string
	ComponentDigest string
}

type ComponentDivergenceClass string

const (
	DivergenceMissing  ComponentDivergenceClass = "missing"
	DivergenceExtra    ComponentDivergenceClass = "extra"
	DivergenceChanged  ComponentDivergenceClass = "changed"
	DivergenceConflict ComponentDivergenceClass = "conflicting"
)

type ComponentDivergence struct {
	ComponentID    string
	Class          ComponentDivergenceClass
	ExpectedDigest string
	ObservedDigest string
	ExpectedLock   string
	ObservedLock   string
}

// ReconcileCapsuleLockComponents compares only the lock-derived component
// identity. It reports every component-level divergence without mutating
// installed state.
func ReconcileCapsuleLockComponents(lock CapsuleLockSnapshot, installed []InstalledComponentState) []ComponentDivergence {
	installedByID := make(map[string]InstalledComponentState, len(installed))
	for _, component := range installed {
		installedByID[component.ComponentID] = component
	}
	ids := make([]string, 0, len(lock.ResolvedComponents)+len(installedByID))
	for id := range lock.ResolvedComponents {
		ids = append(ids, id)
	}
	for id := range installedByID {
		if _, present := lock.ResolvedComponents[id]; !present {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	divergences := make([]ComponentDivergence, 0)
	seen := make(map[string]struct{}, len(ids))
	for _, id := range ids {
		if _, done := seen[id]; done {
			continue
		}
		seen[id] = struct{}{}
		expected, desired := lock.ResolvedComponents[id]
		observed, present := installedByID[id]
		switch {
		case !desired:
			divergences = append(divergences, ComponentDivergence{ComponentID: id, Class: DivergenceExtra, ObservedDigest: observed.ComponentDigest, ObservedLock: observed.LockDigest})
		case !present:
			divergences = append(divergences, ComponentDivergence{ComponentID: id, Class: DivergenceMissing, ExpectedDigest: expected.ComponentDigest, ExpectedLock: lock.Digest})
		case observed.LockDigest == "" || observed.CapsuleDigest == "" || observed.ComponentDigest == "":
			divergences = append(divergences, ComponentDivergence{ComponentID: id, Class: DivergenceMissing, ExpectedDigest: expected.ComponentDigest, ObservedDigest: observed.ComponentDigest, ExpectedLock: lock.Digest, ObservedLock: observed.LockDigest})
		case observed.LockDigest != "" && observed.LockDigest != lock.Digest:
			divergences = append(divergences, ComponentDivergence{ComponentID: id, Class: DivergenceConflict, ExpectedDigest: expected.ComponentDigest, ObservedDigest: observed.ComponentDigest, ExpectedLock: lock.Digest, ObservedLock: observed.LockDigest})
		case observed.CapsuleDigest != "" && observed.CapsuleDigest != expected.CapsuleDigest:
			divergences = append(divergences, ComponentDivergence{ComponentID: id, Class: DivergenceChanged, ExpectedDigest: expected.ComponentDigest, ObservedDigest: observed.ComponentDigest, ExpectedLock: lock.Digest, ObservedLock: observed.LockDigest})
		case observed.ComponentDigest != expected.ComponentDigest:
			divergences = append(divergences, ComponentDivergence{ComponentID: id, Class: DivergenceChanged, ExpectedDigest: expected.ComponentDigest, ObservedDigest: observed.ComponentDigest, ExpectedLock: lock.Digest, ObservedLock: observed.LockDigest})
		}
	}
	return divergences
}
