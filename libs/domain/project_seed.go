package domain

import (
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"time"
)

var contentDigestPattern = regexp.MustCompile(`^sha256:[a-f0-9]{64}$`)

type ProjectSeedSnapshot struct {
	ID                    string
	OwnerUserID           string
	RepositoryURL         string
	BaseRevision          string
	Digest                string
	GitBundleDigest       string
	TrackedPatchDigest    string
	UntrackedBundleDigest string
	ManifestDigest        string
	CreatedAt             time.Time
}

type ProjectSeed struct {
	snapshot ProjectSeedSnapshot
}

func RegisterProjectSeed(snapshot ProjectSeedSnapshot) (ProjectSeed, error) {
	if strings.TrimSpace(snapshot.ID) == "" || strings.TrimSpace(snapshot.OwnerUserID) == "" || strings.TrimSpace(snapshot.BaseRevision) == "" {
		return ProjectSeed{}, errors.New("register Project Seed: ID, owner User ID, and base revision are required")
	}
	repositoryURL, err := url.Parse(snapshot.RepositoryURL)
	if err != nil || (repositoryURL.Scheme != "https" && repositoryURL.Scheme != "ssh") || repositoryURL.Host == "" ||
		repositoryURL.User != nil || repositoryURL.RawQuery != "" || repositoryURL.Fragment != "" {
		return ProjectSeed{}, errors.New("register Project Seed: repository URL must be an absolute credential-free URL")
	}
	digests := [...]struct{ name, value string }{
		{name: "Project Seed", value: snapshot.Digest},
		{name: "manifest", value: snapshot.ManifestDigest},
		{name: "Git bundle", value: snapshot.GitBundleDigest},
		{name: "tracked patch", value: snapshot.TrackedPatchDigest},
		{name: "untracked bundle", value: snapshot.UntrackedBundleDigest},
	}
	for _, digest := range digests {
		if digest.value != "" && !contentDigestPattern.MatchString(digest.value) {
			return ProjectSeed{}, fmt.Errorf("register Project Seed: %s digest is invalid", digest.name)
		}
	}
	if snapshot.Digest == "" || snapshot.ManifestDigest == "" {
		return ProjectSeed{}, errors.New("register Project Seed: content and manifest digests are required")
	}
	if snapshot.CreatedAt.IsZero() {
		return ProjectSeed{}, errors.New("register Project Seed: creation time is required")
	}
	snapshot.CreatedAt = snapshot.CreatedAt.Round(0).UTC()
	return ProjectSeed{snapshot: snapshot}, nil
}

func (seed ProjectSeed) Snapshot() ProjectSeedSnapshot { return seed.snapshot }
