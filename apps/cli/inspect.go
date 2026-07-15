package main

import (
	"context"
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/ahmedhesham6/sshai/libs/profile"
	"github.com/ahmedhesham6/sshai/libs/projectseed"
)

// RunInspect reports local classification and repository evidence without
// compiling, packaging, publishing, or displaying file/key contents.
func RunInspect(ctx context.Context, repositoryRoot, profileRoot, sshDirectory string, output io.Writer) error {
	inspection, err := projectseed.Inspect(ctx, repositoryRoot)
	if err != nil {
		return fmt.Errorf("inspect repository: %w", err)
	}
	candidates, err := profile.Scan(profileRoot)
	if err != nil {
		return fmt.Errorf("inspect Profile candidates: %w", err)
	}
	keys, err := discoverEd25519Keys(sshDirectory)
	if err != nil {
		return fmt.Errorf("inspect SSH keys: %w", err)
	}

	tracked := append([]string(nil), inspection.TrackedChanges...)
	untracked := append([]string(nil), inspection.UntrackedFiles...)
	sort.Strings(tracked)
	sort.Strings(untracked)
	var report strings.Builder
	report.WriteString("project:\n")
	fmt.Fprintf(&report, "  revision=%s\n  base_revision=%s\n", inspection.Revision, inspection.BaseRevision)
	for _, path := range tracked {
		fmt.Fprintf(&report, "  tracked_change path=%q\n", path)
	}
	for _, path := range untracked {
		fmt.Fprintf(&report, "  untracked path=%q\n", path)
	}
	report.WriteString("profile_candidates:\n")
	for _, candidate := range candidates {
		writeCandidateItem(&report, candidate, candidate.Evidence)
	}
	report.WriteString("ssh_keys:\n")
	for _, key := range keys {
		fmt.Fprintf(&report, "  label=%q fingerprint=%s private_key_path=%q\n", key.Label, key.Fingerprint, key.PrivateKeyPath)
	}
	if _, err := io.WriteString(output, report.String()); err != nil {
		return fmt.Errorf("write inspection: %w", err)
	}
	return nil
}
