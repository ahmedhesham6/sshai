package profile

import (
	"fmt"
	"path/filepath"
	"sort"
)

// Select resolves explicit selectors to capture candidates without staging or
// packaging a Capsule. It is the shared seam for capture and planning output.
func Select(root string, selectors []Selector) ([]Candidate, error) {
	candidates, err := Scan(root)
	if err != nil {
		return nil, err
	}
	byPath := make(map[string]Candidate, len(candidates))
	for _, candidate := range candidates {
		byPath[candidate.Path] = candidate
	}
	ordered := append([]Selector(nil), selectors...)
	for index := range ordered {
		ordered[index].Path = filepath.ToSlash(filepath.Clean(ordered[index].Path))
	}
	sort.Slice(ordered, func(i, j int) bool {
		if ordered[i].Path == ordered[j].Path {
			return ordered[i].Selector < ordered[j].Selector
		}
		return ordered[i].Path < ordered[j].Path
	})
	selected := make([]Candidate, 0, len(ordered))
	seen := make(map[Selector]struct{}, len(ordered))
	for _, selector := range ordered {
		if selector.Path == "" || selector.Selector == "" {
			return nil, fmt.Errorf("select candidate: path and selector are required")
		}
		if _, exists := seen[selector]; exists {
			return nil, fmt.Errorf("select candidate: duplicate selector %q for %q", selector.Selector, selector.Path)
		}
		seen[selector] = struct{}{}
		candidate, ok := byPath[selector.Path]
		if !ok {
			return nil, fmt.Errorf("select candidate: path %q is not a scanned candidate", selector.Path)
		}
		if candidate.Disposition == "excluded" {
			return nil, fmt.Errorf("select candidate: path %q is excluded", selector.Path)
		}
		compiled, err := compileSelection(root, selector)
		if err != nil {
			return nil, err
		}
		candidate.Selector = selector.Selector
		candidate.SourceLocator = selector.Path + "#" + selector.Selector
		candidate.SourceDigest = compiled.sourceDigest
		candidate.ContentDigest = contentDigest(compiled.content)
		candidate.Component = compiled.component
		selected = append(selected, candidate)
	}
	return selected, nil
}
