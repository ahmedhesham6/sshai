package main

import (
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/ahmedhesham6/sshai/libs/profile"
	"github.com/ahmedhesham6/sshai/libs/projectseed"
)

// RunPlan prints a deterministic, non-applying component and Project Seed
// creation plan. It performs no authentication, upload, or registration.
func RunPlan(ctx context.Context, repositoryRoot, profileRoot string, selectors []profile.Selector, output io.Writer) error {
	candidates, err := profile.Scan(profileRoot)
	if err != nil {
		return fmt.Errorf("scan Capsule candidates: %w", err)
	}
	selected, err := profile.Select(profileRoot, selectors)
	if err != nil {
		return fmt.Errorf("resolve Capsule selections: %w", err)
	}
	seed, err := projectseed.Package(ctx, repositoryRoot)
	if err != nil {
		return fmt.Errorf("package Project Seed: %w", err)
	}

	items := captureItems(candidates, selected)
	var plan strings.Builder
	fmt.Fprintf(&plan, "project_seed %s\nprofile_components:\n", seed.Digest())
	plan.WriteString(captureGroupsString(items))
	plan.WriteString("project_seed_evidence:\n")
	metadata := seed.Metadata()
	if metadata.BundleDigest != "" {
		fmt.Fprintf(&plan, "  source=project_seed path=%q evidence=unpushed_commits digest=%s\n", ".git", metadata.BundleDigest)
	}
	if len(seed.Patch()) > 0 {
		fmt.Fprintf(&plan, "  source=project_seed path=%q evidence=tracked_changes digest=%s\n", "tracked_changes.patch", metadata.PatchDigest)
	}
	for _, entry := range seed.Manifest() {
		fmt.Fprintf(&plan, "  source=project_seed path=%q evidence=untracked digest=%s\n", entry.Path, entry.ContentDigest)
	}
	if _, err := io.WriteString(output, plan.String()); err != nil {
		return fmt.Errorf("write plan: %w", err)
	}
	return nil
}
