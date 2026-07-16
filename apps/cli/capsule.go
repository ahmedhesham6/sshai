package main

import (
	"context"
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/ahmedhesham6/sshai/libs/profile"
)

// RunCapsuleCapture scans local candidates and prints an explicit component
// selection plan. It does not package, upload, authenticate, or prompt.
func RunCapsuleCapture(ctx context.Context, profileRoot string, selectors []profile.Selector, output io.Writer) error {
	_ = ctx
	candidates, err := profile.Scan(profileRoot)
	if err != nil {
		return fmt.Errorf("scan Capsule candidates: %w", err)
	}
	selected, err := profile.Select(profileRoot, selectors)
	if err != nil {
		return fmt.Errorf("resolve Capsule selections: %w", err)
	}
	items := captureItems(candidates, selected)
	return writeCaptureGroups(items, output)
}

// RunCapsuleBuild compiles explicit selections into a local Capsule and
// prints only its manifest and Component layer digests.
func RunCapsuleBuild(ctx context.Context, profileRoot string, selectors []profile.Selector, output io.Writer) error {
	_ = ctx
	capsule, err := profile.Compile(profileRoot, selectors)
	if err != nil {
		return fmt.Errorf("build Capsule: %w", err)
	}
	var report strings.Builder
	fmt.Fprintf(&report, "manifest_digest %s\n", capsule.Digest)
	for _, component := range capsule.Manifest.Components {
		fmt.Fprintf(&report, "component id=%q digest=%s size_bytes=%d type=%s scope=%s trust=%s\n", component.ID, component.Digest, component.SizeBytes, component.Type, component.Scope, component.TrustClass)
	}
	if _, err := io.WriteString(output, report.String()); err != nil {
		return fmt.Errorf("write Capsule build: %w", err)
	}
	return nil
}

type captureItem struct {
	candidate profile.Candidate
	selected  bool
}

func captureItems(candidates, selected []profile.Candidate) []captureItem {
	selectedByPath := make(map[string][]profile.Candidate, len(selected))
	for _, candidate := range selected {
		selectedByPath[candidate.Path] = append(selectedByPath[candidate.Path], candidate)
	}
	items := make([]captureItem, 0, len(candidates)+len(selected))
	for _, candidate := range candidates {
		selectedItems := selectedByPath[candidate.Path]
		if len(selectedItems) == 0 {
			items = append(items, captureItem{candidate: candidate})
			continue
		}
		for _, selectedCandidate := range selectedItems {
			items = append(items, captureItem{candidate: selectedCandidate, selected: true})
		}
		delete(selectedByPath, candidate.Path)
	}
	for _, remaining := range selectedByPath {
		for _, candidate := range remaining {
			items = append(items, captureItem{candidate: candidate, selected: true})
		}
	}
	sort.Slice(items, func(i, j int) bool {
		return items[i].candidate.Component.ID < items[j].candidate.Component.ID
	})
	return items
}

func writeCaptureGroups(items []captureItem, output io.Writer) error {
	var report strings.Builder
	report.WriteString(captureGroupsString(items))
	if _, err := io.WriteString(output, report.String()); err != nil {
		return fmt.Errorf("write Capsule capture: %w", err)
	}
	return nil
}

func captureGroupsString(items []captureItem) string {
	var report strings.Builder
	groups := []string{"safe", "review", "requires_authorization", "excluded", "conflict"}
	for _, group := range groups {
		fmt.Fprintf(&report, "%s:\n", group)
		for _, item := range items {
			if candidateGroup(item.candidate) != group {
				continue
			}
			evidence := item.candidate.Evidence
			if !item.selected && item.candidate.Disposition != "excluded" {
				evidence = "not_selected+" + evidence
			}
			writeComponentItem(&report, item.candidate, evidence, item.selected)
		}
	}
	return report.String()
}

func candidateGroup(candidate profile.Candidate) string {
	switch candidate.Disposition {
	case "safe", "review", "requires_authorization", "excluded":
		return candidate.Disposition
	default:
		return "conflict"
	}
}

func writeComponentItem(output *strings.Builder, candidate profile.Candidate, evidence string, selected bool) {
	component := candidate.Component
	fmt.Fprintf(output, "  component=%s type=%s scope=%s trust=%s path=%q selector=%q selected=%t evidence=%s sensitivity=%s contains_executable=%t", component.ID, component.Type, component.Scope, component.TrustClass, candidate.Path, candidate.Selector, selected, evidence, candidate.Sensitivity, candidate.ContainsExecutable)
	if len(component.Requirements.Secrets) > 0 {
		fmt.Fprintf(output, " requirements_secrets=%q", strings.Join(component.Requirements.Secrets, ","))
	}
	if candidate.SourceDigest != "" {
		fmt.Fprintf(output, " source_digest=%s content_digest=%s", candidate.SourceDigest, candidate.ContentDigest)
	}
	output.WriteByte('\n')
}
