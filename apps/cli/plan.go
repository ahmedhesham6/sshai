package main

import (
	"context"
	"fmt"
	"io"
	"path/filepath"
	"strings"

	"github.com/ahmedhesham6/sshai/libs/profile"
	"github.com/ahmedhesham6/sshai/libs/projectseed"
)

// RunPlan compiles local artifacts and prints a deterministic, non-applying
// creation plan. It performs no authentication, upload, or registration.
func RunPlan(ctx context.Context, repositoryRoot, profileRoot string, selectors []profile.Selector, output io.Writer) error {
	candidates, err := profile.Scan(profileRoot)
	if err != nil {
		return fmt.Errorf("scan Profile candidates: %w", err)
	}
	version, err := profile.Compile(profileRoot, selectors)
	if err != nil {
		return fmt.Errorf("compile Profile: %w", err)
	}
	seed, err := projectseed.Package(ctx, repositoryRoot)
	if err != nil {
		return fmt.Errorf("package Project Seed: %w", err)
	}

	var plan strings.Builder
	artifacts := version.Artifacts()
	patch := seed.Patch()
	fmt.Fprintf(&plan, "profile_version %s\nproject_seed %s\n", version.Digest(), seed.Digest())
	plan.WriteString("safe:\n")
	for _, artifact := range artifacts {
		if !artifact.ContainsExecutable {
			writeProfileItem(&plan, artifact)
		}
	}
	plan.WriteString("review:\n")
	for _, artifact := range artifacts {
		if artifact.ContainsExecutable {
			writeProfileItem(&plan, artifact)
		}
	}
	metadata := seed.Metadata()
	if metadata.BundleDigest != "" {
		fmt.Fprintf(&plan, "  source=project_seed path=%q evidence=unpushed_commits digest=%s\n", ".git", metadata.BundleDigest)
	}
	if len(patch) > 0 {
		fmt.Fprintf(&plan, "  source=project_seed path=%q evidence=tracked_changes digest=%s\n", "tracked_changes.patch", metadata.PatchDigest)
	}
	for _, entry := range seed.Manifest() {
		fmt.Fprintf(&plan, "  source=project_seed path=%q evidence=untracked digest=%s\n", entry.Path, entry.ContentDigest)
	}
	plan.WriteString("requires_authorization:\nexcluded:\n")
	selectedPaths := make(map[string]struct{}, len(selectors))
	for _, selector := range selectors {
		selectedPaths[filepath.ToSlash(filepath.Clean(selector.Path))] = struct{}{}
	}
	for _, candidate := range candidates {
		if _, ok := selectedPaths[candidate.Path]; ok {
			continue
		}
		evidence := candidate.Evidence
		if candidate.Disposition != "excluded" {
			evidence = "not_selected+" + evidence
		}
		writeCandidateItem(&plan, candidate, evidence)
	}
	plan.WriteString("conflict:\n")
	if _, err := io.WriteString(output, plan.String()); err != nil {
		return fmt.Errorf("write plan: %w", err)
	}
	return nil
}

func writeProfileItem(output *strings.Builder, artifact profile.Artifact) {
	fmt.Fprintf(output, "  source=profile type=%s path=%q selector=%q evidence=%s sensitivity=%s trust=%s executable=%t size_bytes=%d mode=%04o source_digest=%s content_digest=%s\n", artifact.Kind, artifact.Path, artifact.Selector, artifact.Evidence, artifact.Sensitivity, artifact.Trust, artifact.ContainsExecutable, len(artifact.Content), artifact.Mode, artifact.SourceDigest, artifact.ContentDigest)
}

func writeCandidateItem(output *strings.Builder, candidate profile.Candidate, evidence string) {
	fmt.Fprintf(output, "  source=profile type=%s path=%q selector=%q evidence=%s sensitivity=%s trust=%s executable=%t", candidate.Kind, candidate.Path, candidate.Selector, evidence, candidate.Sensitivity, candidate.Trust, candidate.ContainsExecutable)
	if candidate.SourceDigest != "" {
		fmt.Fprintf(output, " source_digest=%s content_digest=%s", candidate.SourceDigest, candidate.ContentDigest)
	}
	output.WriteByte('\n')
}
