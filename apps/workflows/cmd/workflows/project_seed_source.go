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
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"
)

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
	var aggregateSize int64
	read := func(kind domain.UploadKind, digest string) (guest.ProjectSeedArtifact, error) {
		if digest == "" {
			return guest.ProjectSeedArtifact{}, nil
		}
		content, readErr := source.objects.ReadProjectSeedObject(ctx, ownerUserID, kind, digest)
		if readErr != nil {
			return guest.ProjectSeedArtifact{}, readErr
		}
		if int64(len(content)) > application.ProjectSeedTransportMaximumRawBytes-aggregateSize {
			return guest.ProjectSeedArtifact{}, permanentGuestTransportError{err: fmt.Errorf("load Project Seed: aggregate content exceeds %d bytes", application.ProjectSeedTransportMaximumRawBytes)}
		}
		aggregateSize += int64(len(content))
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
		wrapped := fmt.Errorf("read Project Seed object %s: %w", kind, err)
		if isMissingProjectSeedObject(err) {
			return nil, permanentGuestTransportError{err: wrapped}
		}
		return nil, transientGuestTransportError{err: wrapped}
	}
	if response == nil || response.Body == nil {
		return nil, permanentGuestTransportError{err: fmt.Errorf("read Project Seed object %s: object response has no body", kind)}
	}
	content, err := io.ReadAll(io.LimitReader(response.Body, application.ProjectSeedTransportMaximumRawBytes+1))
	closeErr := response.Body.Close()
	if err != nil {
		return nil, transientGuestTransportError{err: fmt.Errorf("read Project Seed object %s body: %w", kind, err)}
	}
	if int64(len(content)) > application.ProjectSeedTransportMaximumRawBytes {
		return nil, permanentGuestTransportError{err: fmt.Errorf("read Project Seed object %s: object exceeds maximum size", kind)}
	}
	if closeErr != nil {
		return nil, transientGuestTransportError{err: fmt.Errorf("close Project Seed object %s: %w", kind, closeErr)}
	}
	return content, nil
}

func isMissingProjectSeedObject(err error) bool {
	var noSuchKey *s3types.NoSuchKey
	if errors.As(err, &noSuchKey) {
		return true
	}
	var apiError smithy.APIError
	if !errors.As(err, &apiError) {
		return false
	}
	switch apiError.ErrorCode() {
	case "NoSuchKey", "NoSuchVersion", "NotFound":
		return true
	default:
		return false
	}
}

type permanentGuestTransportError struct{ err error }

func (err permanentGuestTransportError) Error() string { return err.err.Error() }
func (err permanentGuestTransportError) Unwrap() error { return err.err }
func (permanentGuestTransportError) Transient() bool   { return false }

type transientGuestTransportError struct{ err error }

func (err transientGuestTransportError) Error() string { return err.err.Error() }
func (err transientGuestTransportError) Unwrap() error { return err.err }
func (transientGuestTransportError) Transient() bool   { return true }
