package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/ahmedhesham6/sshai/libs/contracts"
	"github.com/ahmedhesham6/sshai/libs/projectseed"
)

func (application cli) ensureLifecycleSSHKeyIDs(ctx context.Context, client lifecycleClient) ([]string, error) {
	if err := application.ensureSSHSetup(ctx); err != nil {
		return nil, err
	}
	sshDirectory, err := application.sshDirectory()
	if err != nil {
		return nil, errors.New("select SSH key: resolve SSH directory; run `devm ssh setup`")
	}
	local, err := discoverEd25519Keys(sshDirectory)
	if err != nil {
		return nil, fmt.Errorf("select SSH key: %w", err)
	}
	selected, _, err := chooseLocalSSHKey(local, "")
	if err != nil || selected.PublicKey == "" {
		return nil, errors.New("select SSH key: no usable key found after setup; run `devm ssh setup`")
	}
	registered, err := listSetupSSHKeys(ctx, client.api, client.token)
	if err != nil {
		return nil, err
	}
	for _, key := range registered {
		if key.PublicKey == selected.PublicKey && key.Fingerprint == selected.Fingerprint && key.Id != "" {
			return []string{key.Id}, nil
		}
	}
	return nil, errors.New("select SSH key: setup did not return the registered key; rerun `devm ssh setup`")
}

func ensureLifecycleProfileVersion(ctx context.Context, client lifecycleClient) (string, error) {
	profiles, err := listLifecycleProfiles(ctx, client)
	if err != nil {
		return "", err
	}
	for _, profile := range profiles {
		if profile.HeadVersionId != nil && strings.TrimSpace(*profile.HeadVersionId) != "" {
			return *profile.HeadVersionId, nil
		}
	}
	var profileID string
	if len(profiles) > 0 {
		profileID = profiles[0].Id
	} else {
		requestContext, cancel := context.WithTimeout(ctx, lifecycleRequestTimeout)
		response, requestErr := client.api.CreateProfileWithResponse(requestContext,
			&contracts.CreateProfileParams{IdempotencyKey: contracts.IdempotencyKey(deterministicKey("profile-create", "default"))},
			contracts.CreateProfileJSONRequestBody{Name: "Default"}, client.editor())
		cancel()
		if requestErr != nil {
			return "", lifecycleUnavailable(ctx, "create Profile", requestErr)
		}
		if response.StatusCode() != http.StatusCreated || response.JSON201 == nil || response.JSON201.Id == "" {
			return "", fmt.Errorf("create Profile: control plane returned HTTP %d", response.StatusCode())
		}
		profileID = response.JSON201.Id
	}
	if profileID == "" {
		return "", errors.New("create Profile Version: Profile ID is unavailable; rerun `devm`")
	}
	requestContext, cancel := context.WithTimeout(ctx, lifecycleRequestTimeout)
	response, requestErr := client.api.PublishProfileVersionWithResponse(requestContext, profileID,
		&contracts.PublishProfileVersionParams{IdempotencyKey: contracts.IdempotencyKey(deterministicKey("profile-version-create", profileID))},
		contracts.PublishProfileVersionJSONRequestBody{CapsuleRefs: []contracts.CapsuleRef{}}, client.editor())
	cancel()
	if requestErr != nil {
		return "", lifecycleUnavailable(ctx, "create Profile Version", requestErr)
	}
	if response.StatusCode() != http.StatusCreated || response.JSON201 == nil || response.JSON201.Id == "" {
		return "", fmt.Errorf("create Profile Version: control plane returned HTTP %d; rerun `devm`", response.StatusCode())
	}
	return response.JSON201.Id, nil
}

func listLifecycleProfiles(ctx context.Context, client lifecycleClient) ([]contracts.ProfileSummary, error) {
	pageSize := contracts.PageSize(setupPageSize)
	return paginateSetup(ctx, "list Profiles", func(requestContext context.Context, cursor *contracts.Cursor) (setupPage[contracts.ProfileSummary], error) {
		response, err := client.api.ListProfilesWithResponse(requestContext, &contracts.ListProfilesParams{Cursor: cursor, PageSize: &pageSize}, client.editor())
		if err != nil {
			return setupPage[contracts.ProfileSummary]{}, err
		}
		page := setupPage[contracts.ProfileSummary]{status: response.StatusCode()}
		if response.JSON200 != nil {
			page.items, page.next, page.valid = response.JSON200.Items, response.JSON200.NextCursor, true
		}
		return page, nil
	})
}

type lifecycleUpload struct {
	kind   contracts.CreateUploadIntentJSONBodyKind
	digest string
	data   []byte
}

func (application cli) ensureLifecycleProjectSeed(ctx context.Context, client lifecycleClient, root, identity string, store localStateStore) (string, error) {
	seed, err := projectseed.Package(ctx, root)
	if err != nil {
		return "", fmt.Errorf("create Project Seed: %w", err)
	}
	manifest, err := json.Marshal(seed.Manifest())
	if err != nil {
		return "", errors.New("create Project Seed: encode manifest")
	}
	metadata := seed.Metadata()
	uploads := []lifecycleUpload{
		{contracts.SeedManifest, metadata.ManifestDigest, manifest},
		{contracts.TrackedPatch, metadata.PatchDigest, seed.Patch()},
		{contracts.UntrackedBundle, metadata.ArchiveDigest, seed.Archive()},
	}
	if metadata.BundleDigest != "" {
		uploads = append(uploads, lifecycleUpload{contracts.GitBundle, metadata.BundleDigest, seed.Bundle()})
	}
	for _, upload := range uploads {
		if err := application.uploadLifecycleSeedPart(ctx, client, upload); err != nil {
			return "", err
		}
	}
	repositoryURL, err := repositoryURLWithoutHTTPUserinfo(metadata.RepositoryURL)
	if err != nil {
		return "", fmt.Errorf("create Project Seed: repository URL: %w", err)
	}
	var bundleDigest, patchDigest, archiveDigest *string
	if metadata.BundleDigest != "" {
		bundleDigest = &metadata.BundleDigest
	}
	patchDigest, archiveDigest = &metadata.PatchDigest, &metadata.ArchiveDigest
	body := contracts.CreateProjectSeedJSONRequestBody{
		RepositoryUrl: repositoryURL, BaseRevision: metadata.BaseRevision, Digest: seed.Digest(),
		GitBundleDigest: bundleDigest, TrackedPatchDigest: patchDigest,
		UntrackedBundleDigest: archiveDigest, ManifestDigest: metadata.ManifestDigest,
	}
	requestContext, cancel := context.WithTimeout(ctx, lifecycleRequestTimeout)
	response, requestErr := client.api.CreateProjectSeedWithResponse(requestContext,
		&contracts.CreateProjectSeedParams{IdempotencyKey: contracts.IdempotencyKey(deterministicKey("project-seed-create", identity+"\x00"+seed.Digest()))}, body, client.editor())
	cancel()
	if requestErr != nil {
		return "", lifecycleUnavailable(ctx, "create Project Seed", requestErr)
	}
	if response.StatusCode() != http.StatusCreated || response.JSON201 == nil || response.JSON201.Id == "" || response.JSON201.Digest != seed.Digest() {
		return "", fmt.Errorf("create Project Seed: control plane returned HTTP %d", response.StatusCode())
	}
	if err := store.SetProjectSeed(ctx, identity, response.JSON201.Id); err != nil {
		return "", fmt.Errorf("save Project Seed binding: %w", err)
	}
	return response.JSON201.Id, nil
}

func (application cli) uploadLifecycleSeedPart(ctx context.Context, client lifecycleClient, upload lifecycleUpload) error {
	requestContext, cancel := context.WithTimeout(ctx, lifecycleRequestTimeout)
	response, requestErr := client.api.CreateUploadIntentWithResponse(requestContext,
		&contracts.CreateUploadIntentParams{IdempotencyKey: contracts.IdempotencyKey(deterministicKey("project-seed-upload", string(upload.kind)+"\x00"+upload.digest))},
		contracts.CreateUploadIntentJSONRequestBody{Kind: upload.kind, Digest: upload.digest, SizeBytes: int64(len(upload.data))}, client.editor())
	cancel()
	if requestErr != nil {
		return lifecycleUnavailable(ctx, "create Project Seed upload", requestErr)
	}
	if response.StatusCode() != http.StatusCreated || response.JSON201 == nil || response.JSON201.Url == "" {
		return fmt.Errorf("create Project Seed upload: control plane returned HTTP %d", response.StatusCode())
	}
	uploadURL, err := url.Parse(response.JSON201.Url)
	if err != nil || !strings.EqualFold(uploadURL.Scheme, "https") || uploadURL.Host == "" || uploadURL.User != nil || uploadURL.Fragment != "" {
		return errors.New("create Project Seed upload: control plane returned an unsafe upload URL")
	}
	uploadContext, uploadCancel := context.WithTimeout(ctx, lifecycleRequestTimeout)
	defer uploadCancel()
	request, err := http.NewRequestWithContext(uploadContext, http.MethodPut, uploadURL.String(), bytes.NewReader(upload.data))
	if err != nil {
		return errors.New("create Project Seed upload: build upload request")
	}
	request.ContentLength = int64(len(upload.data))
	for name, value := range response.JSON201.RequiredHeaders {
		if strings.EqualFold(name, "Content-Length") {
			length, parseErr := strconv.ParseInt(value, 10, 64)
			if parseErr != nil || length != request.ContentLength {
				return errors.New("create Project Seed upload: invalid required Content-Length")
			}
			continue
		}
		request.Header.Set(name, value)
	}
	httpClient := cloneProxyHTTPClient(application.httpClient)
	result, err := httpClient.Do(request)
	if err != nil {
		return lifecycleUnavailable(ctx, "upload Project Seed content", err)
	}
	defer result.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(result.Body, 64<<10))
	if result.StatusCode < 200 || result.StatusCode >= 300 {
		return fmt.Errorf("upload Project Seed content: storage returned HTTP %d", result.StatusCode)
	}
	return nil
}

func repositoryURLWithoutHTTPUserinfo(raw string) (string, error) {
	if !strings.Contains(raw, "://") {
		return raw, nil
	}
	parsed, err := url.Parse(raw)
	if err != nil {
		return "", err
	}
	switch strings.ToLower(parsed.Scheme) {
	case "http", "https":
		parsed.User = nil
	case "ssh", "git+ssh":
		if parsed.User != nil {
			parsed.User = url.User(parsed.User.Username())
		}
	}
	parsed.RawQuery = ""
	parsed.ForceQuery = false
	parsed.Fragment = ""
	return parsed.String(), nil
}

func createLifecycleEnvironment(ctx context.Context, client lifecycleClient, identity string, body contracts.CreateEnvironmentJSONRequestBody) (contracts.EnvironmentOperation, error) {
	response, err := requestLifecycleEnvironmentCreate(ctx, client, identity, body)
	if err != nil {
		return contracts.EnvironmentOperation{}, err
	}
	if response.StatusCode() == http.StatusConflict && response.JSONDefault != nil && response.JSONDefault.Error.Code == "ENVIRONMENT_NAME_CONFLICT" {
		body.Name = conflictEnvironmentName(body.Name, identity)
		response, err = requestLifecycleEnvironmentCreate(ctx, client, identity, body)
		if err != nil {
			return contracts.EnvironmentOperation{}, err
		}
	}
	if response.StatusCode() != http.StatusAccepted || response.JSON202 == nil || !sshIdentifierPattern.MatchString(response.JSON202.Environment.Id) || response.JSON202.Operation.Id == "" {
		return contracts.EnvironmentOperation{}, fmt.Errorf("create Environment: control plane returned HTTP %d", response.StatusCode())
	}
	if response.JSON202.Operation.EnvironmentId != response.JSON202.Environment.Id || response.JSON202.Operation.Type != "environment.create" {
		return contracts.EnvironmentOperation{}, errors.New("create Environment: control plane returned a mismatched Operation")
	}
	return *response.JSON202, nil
}

func requestLifecycleEnvironmentCreate(ctx context.Context, client lifecycleClient, identity string, body contracts.CreateEnvironmentJSONRequestBody) (*contracts.CreateEnvironmentResponse, error) {
	requestContext, cancel := context.WithTimeout(ctx, lifecycleRequestTimeout)
	response, requestErr := client.api.CreateEnvironmentWithResponse(requestContext,
		&contracts.CreateEnvironmentParams{IdempotencyKey: contracts.IdempotencyKey(deterministicKey("environment-create", identity+"\x00"+body.Name))}, body, client.editor())
	cancel()
	if requestErr != nil {
		return nil, lifecycleUnavailable(ctx, "create Environment", requestErr)
	}
	return response, nil
}

func conflictEnvironmentName(base, identity string) string {
	suffix := deterministicKey("repo", identity)
	suffix = suffix[len(suffix)-8:]
	base = strings.TrimRight(base, "-")
	if len(base) > 55 {
		base = strings.TrimRight(base[:55], "-")
	}
	return base + "-" + suffix
}

func (application cli) resumeEnvironmentCreate(ctx context.Context, client lifecycleClient, environment contracts.Environment) (contracts.Environment, error) {
	if environment.ActiveOperationId == nil || *environment.ActiveOperationId == "" {
		return contracts.Environment{}, errors.New("resume Environment create: creating Environment has no active Operation; run `devm status`")
	}
	operation, err := getLifecycleOperation(ctx, client, *environment.ActiveOperationId)
	if err != nil {
		return contracts.Environment{}, fmt.Errorf("resume Environment create: %w", err)
	}
	return application.pollEnvironmentCreate(ctx, client, *operation)
}

func (application cli) pollEnvironmentCreate(ctx context.Context, client lifecycleClient, operation contracts.Operation) (contracts.Environment, error) {
	if operation.Id == "" || operation.EnvironmentId == "" || operation.Type != "environment.create" {
		return contracts.Environment{}, errors.New("create Environment: control plane returned an invalid Operation")
	}
	interval := application.createPollInterval
	if interval <= 0 {
		interval = time.Second
	}
	timeout := application.createWaitTimeout
	if timeout <= 0 {
		timeout = 10 * time.Minute
	}
	waitContext, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	wait := application.wait
	if wait == nil {
		wait = waitForContext
	}
	last := ""
	for {
		progress := string(operation.Status)
		for _, step := range operation.Steps {
			if step.Status == contracts.Running || step.Status == contracts.Blocked || step.Status == contracts.Failed {
				progress += ": " + step.Summary
				break
			}
		}
		if progress != last {
			_, _ = fmt.Fprintf(writerOrDiscard(application.errorOutput), "Environment create %s: %s\n", operation.Id, progress)
			last = progress
		}
		if operationTerminal(operation.Status) {
			if operation.Status != contracts.OperationStatusSucceeded {
				return contracts.Environment{}, fmt.Errorf("create Environment: Operation %s ended %s; binding retained for safe resume", operation.Id, operation.Status)
			}
			environment, err := getLifecycleEnvironment(waitContext, client, operation.EnvironmentId)
			if err == nil && environment.Lifecycle == contracts.Active {
				return environment, nil
			}
		}
		if err := wait(waitContext, interval); err != nil {
			if errors.Is(err, context.DeadlineExceeded) {
				return contracts.Environment{}, fmt.Errorf("create Environment: Operation %s did not produce an active Environment after %s; rerun `devm` to resume", operation.Id, timeout)
			}
			return contracts.Environment{}, err
		}
		current, err := getLifecycleOperation(waitContext, client, operation.Id)
		if err != nil {
			return contracts.Environment{}, fmt.Errorf("poll create Operation: %w", err)
		}
		if current.EnvironmentId != operation.EnvironmentId || current.Type != "environment.create" {
			return contracts.Environment{}, errors.New("poll create Operation: control plane returned a mismatched Operation")
		}
		operation = *current
	}
}
