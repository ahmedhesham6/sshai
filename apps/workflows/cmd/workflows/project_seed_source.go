package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/ahmedhesham6/sshai/apps/guest"
	"github.com/ahmedhesham6/sshai/libs/application"
	dbstore "github.com/ahmedhesham6/sshai/libs/db"
	"github.com/ahmedhesham6/sshai/libs/domain"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

const maximumProjectSeedObjectBytes = 1 << 30

type projectSeedMetadataSource interface {
	LoadEnvironmentProjectSeed(context.Context, string, string, string) (domain.ProjectSeedSnapshot, error)
}

type projectSeedObjectSource interface {
	ReadProjectSeedObject(context.Context, string, domain.UploadKind, string) ([]byte, error)
}

type storedProjectSeedSource struct {
	metadata projectSeedMetadataSource
	objects  projectSeedObjectSource
}

func (source storedProjectSeedSource) LoadProjectSeedApplication(ctx context.Context, ownerUserID, environmentID, projectSeedID string) (guest.ProjectSeedApplicationInput, error) {
	if source.metadata == nil || source.objects == nil {
		return guest.ProjectSeedApplicationInput{}, permanentGuestTransportError{err: errors.New("load Project Seed application: source is not configured")}
	}
	snapshot, err := source.metadata.LoadEnvironmentProjectSeed(ctx, ownerUserID, environmentID, projectSeedID)
	if errors.Is(err, dbstore.ErrReferenceNotOwned) {
		return guest.ProjectSeedApplicationInput{}, permanentGuestTransportError{err: err}
	}
	if err != nil {
		return guest.ProjectSeedApplicationInput{}, err
	}
	if snapshot.OwnerUserID != ownerUserID || snapshot.ID != projectSeedID {
		return guest.ProjectSeedApplicationInput{}, permanentGuestTransportError{err: errors.New("load Project Seed application: persisted identity diverged")}
	}
	read := func(kind domain.UploadKind, digest string) (guest.ProjectSeedArtifact, error) {
		if digest == "" {
			return guest.ProjectSeedArtifact{}, nil
		}
		content, readErr := source.objects.ReadProjectSeedObject(ctx, ownerUserID, kind, digest)
		if readErr != nil {
			return guest.ProjectSeedArtifact{}, readErr
		}
		return guest.ProjectSeedArtifact{SHA256: digest, Content: content}, nil
	}
	manifest, err := read(domain.UploadSeedManifest, snapshot.ManifestDigest)
	if err != nil {
		return guest.ProjectSeedApplicationInput{}, fmt.Errorf("load Project Seed manifest: %w", err)
	}
	gitBundle, err := read(domain.UploadGitBundle, snapshot.GitBundleDigest)
	if err != nil {
		return guest.ProjectSeedApplicationInput{}, fmt.Errorf("load Project Seed Git bundle: %w", err)
	}
	trackedPatch, err := read(domain.UploadTrackedPatch, snapshot.TrackedPatchDigest)
	if err != nil {
		return guest.ProjectSeedApplicationInput{}, fmt.Errorf("load Project Seed tracked patch: %w", err)
	}
	untrackedTar, err := read(domain.UploadUntrackedBundle, snapshot.UntrackedBundleDigest)
	if err != nil {
		return guest.ProjectSeedApplicationInput{}, fmt.Errorf("load Project Seed untracked bundle: %w", err)
	}
	return guest.ProjectSeedApplicationInput{
		RepositoryURL: snapshot.RepositoryURL, BaseRevision: snapshot.BaseRevision,
		GitBundle: gitBundle, TrackedPatch: trackedPatch, UntrackedTar: untrackedTar, Manifest: manifest,
	}, nil
}

type s3ProjectSeedObjectClient interface {
	GetObject(context.Context, *s3.GetObjectInput, ...func(*s3.Options)) (*s3.GetObjectOutput, error)
}

type s3ProjectSeedObjectSource struct {
	client s3ProjectSeedObjectClient
	bucket string
}

func (source s3ProjectSeedObjectSource) ReadProjectSeedObject(ctx context.Context, ownerUserID string, kind domain.UploadKind, digest string) ([]byte, error) {
	if source.client == nil || strings.TrimSpace(source.bucket) == "" {
		return nil, permanentGuestTransportError{err: errors.New("read Project Seed object: S3 source is not configured")}
	}
	key := application.FinalizedUploadObjectKey(ownerUserID, kind, digest)
	response, err := source.client.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String(source.bucket), Key: aws.String(key)})
	if err != nil {
		return nil, fmt.Errorf("read Project Seed object %s: %w", kind, err)
	}
	content, err := io.ReadAll(io.LimitReader(response.Body, maximumProjectSeedObjectBytes+1))
	closeErr := response.Body.Close()
	if err != nil {
		return nil, fmt.Errorf("read Project Seed object %s body: %w", kind, err)
	}
	if len(content) > maximumProjectSeedObjectBytes {
		return nil, permanentGuestTransportError{err: fmt.Errorf("read Project Seed object %s: object exceeds maximum size", kind)}
	}
	if closeErr != nil {
		return nil, fmt.Errorf("close Project Seed object %s: %w", kind, closeErr)
	}
	return content, nil
}

type permanentGuestTransportError struct{ err error }

func (err permanentGuestTransportError) Error() string { return err.err.Error() }
func (err permanentGuestTransportError) Unwrap() error { return err.err }
func (permanentGuestTransportError) Transient() bool   { return false }
