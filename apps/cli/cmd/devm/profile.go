package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/ahmedhesham6/sshai/libs/contracts"
)

const maximumProfilePages = 1000

func (application cli) runProfile(ctx context.Context, arguments []string) error {
	if len(arguments) == 0 {
		return profileUsage()
	}
	switch arguments[0] {
	case "list":
		return application.runProfileList(ctx, arguments[1:])
	case "show":
		return application.runProfileShow(ctx, arguments[1:])
	case "create":
		return application.runProfileCreate(ctx, arguments[1:])
	case "fork":
		return application.runProfileFork(ctx, arguments[1:])
	case "select":
		return application.runProfileSelect(ctx, arguments[1:])
	case "add":
		return application.runProfileAdd(ctx, arguments[1:])
	case "remove":
		return application.runProfileRemove(ctx, arguments[1:])
	case "refresh":
		return application.runProfileRefresh(ctx, arguments[1:])
	case "publish":
		return application.runProfilePublish(ctx, arguments[1:])
	case "apply":
		return application.runProfileApply(ctx, arguments[1:])
	default:
		return profileUsage()
	}
}

func profileUsage() error {
	return errors.New("usage: devm profile <list|show|create|fork|select|add|remove|refresh|publish|apply>")
}

func (application cli) profileStore() (localStateStore, error) {
	if application.configDirectory == nil {
		return localStateStore{}, errors.New("configure Profile command: local state directory is unavailable")
	}
	directory, err := application.configDirectory()
	if err != nil {
		return localStateStore{}, errors.New("resolve local state directory for Profile command")
	}
	return newLocalStateStore(directory), nil
}

func (application cli) runProfileList(ctx context.Context, arguments []string) error {
	flags := flag.NewFlagSet("profile list", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	jsonOutput := flags.Bool("json", false, "emit JSON")
	if err := flags.Parse(arguments); err != nil || flags.NArg() != 0 {
		return errors.New("usage: devm profile list [--json]")
	}
	client, err := application.lifecycleClient(ctx)
	if err != nil {
		return err
	}
	profiles, err := listAllProfiles(ctx, client)
	if err != nil {
		return err
	}
	output := writerOrDiscard(application.output)
	if *jsonOutput {
		encoder := json.NewEncoder(output)
		encoder.SetEscapeHTML(true)
		if err := encoder.Encode(profiles); err != nil {
			return errors.New("write Profile list JSON")
		}
		return nil
	}
	if _, err := fmt.Fprintln(output, "NAME\tID\tHEAD VERSION"); err != nil {
		return errors.New("write Profile list")
	}
	for _, profile := range profiles {
		head := "-"
		if profile.HeadVersionId != nil && *profile.HeadVersionId != "" {
			head = *profile.HeadVersionId
		}
		if _, err := fmt.Fprintf(output, "%s\t%s\t%s\n", terminalSafe(profile.Name), profile.Id, head); err != nil {
			return errors.New("write Profile list")
		}
	}
	return nil
}

func listAllProfiles(ctx context.Context, client lifecycleClient) ([]contracts.ProfileSummary, error) {
	pageSize := contracts.PageSize(100)
	var cursor *contracts.Cursor
	seen := make(map[string]struct{})
	var profiles []contracts.ProfileSummary
	for range maximumProfilePages {
		requestContext, cancel := context.WithTimeout(ctx, lifecycleRequestTimeout)
		response, err := client.api.ListProfilesWithResponse(requestContext,
			&contracts.ListProfilesParams{Cursor: cursor, PageSize: &pageSize}, client.editor())
		cancel()
		if err != nil {
			return nil, lifecycleUnavailable(ctx, "list Profiles", err)
		}
		if response.StatusCode() != http.StatusOK || response.JSON200 == nil {
			return nil, profileResponseError("list Profiles", response.StatusCode(), response.JSONDefault)
		}
		profiles = append(profiles, response.JSON200.Items...)
		if response.JSON200.NextCursor == nil || *response.JSON200.NextCursor == "" {
			sort.SliceStable(profiles, func(i, j int) bool { return profiles[i].Name < profiles[j].Name })
			return profiles, nil
		}
		next := *response.JSON200.NextCursor
		if _, exists := seen[next]; exists {
			return nil, errors.New("list Profiles: control plane repeated a pagination cursor")
		}
		seen[next] = struct{}{}
		value := contracts.Cursor(next)
		cursor = &value
	}
	return nil, errors.New("list Profiles: pagination limit exceeded")
}

func (application cli) runProfileShow(ctx context.Context, arguments []string) error {
	flags := flag.NewFlagSet("profile show", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	jsonOutput := flags.Bool("json", false, "emit JSON")
	if err := flags.Parse(arguments); err != nil || flags.NArg() != 1 {
		return errors.New("usage: devm profile show [--json] <name>")
	}
	client, err := application.lifecycleClient(ctx)
	if err != nil {
		return err
	}
	profile, err := resolveProfile(ctx, client, flags.Arg(0))
	if err != nil {
		return err
	}
	var version *contracts.ProfileVersion
	if profile.HeadVersionId != nil && *profile.HeadVersionId != "" {
		version, err = getProfileVersion(ctx, client, *profile.HeadVersionId)
		if err != nil {
			return fmt.Errorf("show Profile: %w", err)
		}
		if version.ProfileId != profile.Id {
			return errors.New("show Profile: control plane returned a head from another Profile")
		}
	}
	result := struct {
		Profile contracts.ProfileSummary  `json:"profile"`
		Head    *contracts.ProfileVersion `json:"headVersion"`
	}{Profile: profile, Head: version}
	output := writerOrDiscard(application.output)
	if *jsonOutput {
		encoder := json.NewEncoder(output)
		encoder.SetEscapeHTML(true)
		if err := encoder.Encode(result); err != nil {
			return errors.New("write Profile JSON")
		}
		return nil
	}
	head := "-"
	if version != nil {
		head = version.Id
	}
	if _, err := fmt.Fprintf(output, "Profile\t%s\nID\t%s\nHead version\t%s\nCAPSULE REF\tFRESHNESS\tEXCLUSIONS\n", terminalSafe(profile.Name), profile.Id, head); err != nil {
		return errors.New("write Profile")
	}
	if version != nil {
		for _, ref := range version.CapsuleRefs {
			if _, err := fmt.Fprintf(output, "%s\t%s\t%s\n", ref.Ref, ref.FreshnessPolicy, strings.Join(terminalSafeStrings(stringSlice(ref.Exclusions)), ",")); err != nil {
				return errors.New("write Profile")
			}
		}
	}
	return nil
}

func resolveProfile(ctx context.Context, client lifecycleClient, identifier string) (contracts.ProfileSummary, error) {
	profiles, err := listAllProfiles(ctx, client)
	if err != nil {
		return contracts.ProfileSummary{}, err
	}
	for _, profile := range profiles {
		if identifier == profile.Id {
			return resolvedProfile(ctx, client, profile.Id)
		}
	}
	var match *contracts.ProfileSummary
	for index := range profiles {
		profile := &profiles[index]
		if identifier != profile.Name && identifier != profile.Slug {
			continue
		}
		if match != nil && match.Id != profile.Id {
			return contracts.ProfileSummary{}, fmt.Errorf("Profile %q is ambiguous; use its Profile ID", identifier)
		}
		match = profile
	}
	if match != nil {
		return resolvedProfile(ctx, client, match.Id)
	}
	return contracts.ProfileSummary{}, fmt.Errorf("Profile %q was not found", identifier)
}

func resolvedProfile(ctx context.Context, client lifecycleClient, profileID string) (contracts.ProfileSummary, error) {
	resolved, err := getProfile(ctx, client, profileID)
	if err != nil {
		return contracts.ProfileSummary{}, err
	}
	return *resolved, nil
}

func getProfile(ctx context.Context, client lifecycleClient, profileID string) (*contracts.ProfileSummary, error) {
	requestContext, cancel := context.WithTimeout(ctx, lifecycleRequestTimeout)
	response, err := client.api.GetProfileWithResponse(requestContext, profileID, client.editor())
	cancel()
	if err != nil {
		return nil, lifecycleUnavailable(ctx, "get Profile", err)
	}
	if response.StatusCode() != http.StatusOK || response.JSON200 == nil || response.JSON200.Id != profileID {
		return nil, profileResponseError("get Profile", response.StatusCode(), response.JSONDefault)
	}
	return response.JSON200, nil
}

func getProfileVersion(ctx context.Context, client lifecycleClient, versionID string) (*contracts.ProfileVersion, error) {
	requestContext, cancel := context.WithTimeout(ctx, lifecycleRequestTimeout)
	response, err := client.api.GetProfileVersionWithResponse(requestContext, versionID, client.editor())
	cancel()
	if err != nil {
		return nil, lifecycleUnavailable(ctx, "get Profile Version", err)
	}
	if response.StatusCode() != http.StatusOK || response.JSON200 == nil || response.JSON200.Id != versionID {
		return nil, profileResponseError("get Profile Version", response.StatusCode(), response.JSONDefault)
	}
	return response.JSON200, nil
}

func (application cli) runProfileCreate(ctx context.Context, arguments []string) error {
	if len(arguments) != 1 || strings.TrimSpace(arguments[0]) == "" {
		return errors.New("usage: devm profile create <name>")
	}
	client, err := application.lifecycleClient(ctx)
	if err != nil {
		return err
	}
	created, err := createRemoteProfile(ctx, client, arguments[0], deterministicKey("profile-create", arguments[0]))
	if err != nil {
		return err
	}
	store, err := application.profileStore()
	if err != nil {
		return err
	}
	record := authoringProfileFromContracts(created.Id, created.Name, "", nil)
	if err := store.SaveAuthoringProfile(ctx, record); err != nil {
		return fmt.Errorf("save authoring Profile: %w", err)
	}
	if _, err := fmt.Fprintf(writerOrDiscard(application.output), "Created Profile %s (%s); it is now selected for authoring.\n", terminalSafe(created.Name), created.Id); err != nil {
		return errors.New("write Profile creation result")
	}
	return nil
}

func (application cli) runProfileFork(ctx context.Context, arguments []string) error {
	if len(arguments) != 2 || strings.TrimSpace(arguments[0]) == "" || strings.TrimSpace(arguments[1]) == "" {
		return errors.New("usage: devm profile fork <source-version> <name> | devm profile fork --local <name>")
	}
	client, err := application.lifecycleClient(ctx)
	if err != nil {
		return err
	}
	var refs []contracts.CapsuleRef
	var forkIdentity string
	if arguments[0] == "--local" {
		store, storeErr := application.profileStore()
		if storeErr != nil {
			return storeErr
		}
		local, readErr := store.ReadSelectedAuthoringProfile()
		if readErr != nil {
			return readErr
		}
		if len(local.CapsuleRefs) == 0 {
			return errors.New("fork Profile: the selected local draft has no Capsule Refs")
		}
		refs = local.contractRefs()
		encodedRefs, _ := json.Marshal(refs)
		forkIdentity = "local\x00" + local.ProfileID + "\x00" + string(encodedRefs)
	} else {
		source, sourceErr := getProfileVersion(ctx, client, arguments[0])
		if sourceErr != nil {
			return fmt.Errorf("fork Profile: %w", sourceErr)
		}
		refs = source.CapsuleRefs
		forkIdentity = "version\x00" + source.Id
	}
	return application.forkAuthoringProfileWithClient(ctx, client, arguments[1], forkIdentity, refs)
}

func createRemoteProfile(ctx context.Context, client lifecycleClient, name, idempotencyKey string) (*contracts.ProfileSummary, error) {
	requestContext, cancel := context.WithTimeout(ctx, lifecycleRequestTimeout)
	response, err := client.api.CreateProfileWithResponse(requestContext,
		&contracts.CreateProfileParams{IdempotencyKey: idempotencyKey},
		contracts.CreateProfileJSONRequestBody{Name: name}, client.editor())
	cancel()
	if err != nil {
		return nil, lifecycleUnavailable(ctx, "create Profile", err)
	}
	if response.StatusCode() != http.StatusCreated || response.JSON201 == nil || !localProfileIDPattern.MatchString(response.JSON201.Id) {
		return nil, profileResponseError("create Profile", response.StatusCode(), response.JSONDefault)
	}
	if response.JSON201.Name != name {
		return nil, errors.New("create Profile: control plane returned a different Profile identity")
	}
	return response.JSON201, nil
}

func (application cli) forkAuthoringProfileWithClient(ctx context.Context, client lifecycleClient, name, forkIdentity string, refs []contracts.CapsuleRef) error {
	if len(refs) == 0 {
		return errors.New("fork Profile: source Profile Version has no Capsule Refs")
	}
	if err := validateProfilePublicationForOwner(ctx, client, refs); err != nil {
		return fmt.Errorf("fork Profile: %w", err)
	}
	created, err := createRemoteProfile(ctx, client, name, deterministicKey("profile-fork-create", name+"\x00"+forkIdentity))
	if err != nil {
		return err
	}
	body := contracts.PublishProfileVersionJSONRequestBody{ExpectedHeadVersionId: nil, CapsuleRefs: refs}
	encoded, _ := json.Marshal(body)
	requestContext, cancel := context.WithTimeout(ctx, lifecycleRequestTimeout)
	response, requestErr := client.api.PublishProfileVersionWithResponse(requestContext, created.Id,
		&contracts.PublishProfileVersionParams{IdempotencyKey: deterministicKey("profile-publish", created.Id+"\x00"+string(encoded))}, body, client.editor())
	cancel()
	if requestErr != nil {
		return lifecycleUnavailable(ctx, "fork Profile", requestErr)
	}
	if response.StatusCode() != http.StatusCreated || response.JSON201 == nil || response.JSON201.ProfileId != created.Id || response.JSON201.Id == "" {
		return profileResponseError("fork Profile", response.StatusCode(), response.JSONDefault)
	}
	store, err := application.profileStore()
	if err != nil {
		return err
	}
	record := authoringProfileFromContracts(created.Id, created.Name, response.JSON201.Id, refs)
	if err := store.SaveAuthoringProfile(ctx, record); err != nil {
		return fmt.Errorf("save forked authoring Profile: %w", err)
	}
	if _, err := fmt.Fprintf(writerOrDiscard(application.output), "Forked Profile %s (%s) at Profile Version %s; it is now selected for authoring.\n", terminalSafe(created.Name), created.Id, response.JSON201.Id); err != nil {
		return errors.New("write Profile fork result")
	}
	return nil
}

func (application cli) runProfileSelect(ctx context.Context, arguments []string) error {
	if len(arguments) != 1 || strings.TrimSpace(arguments[0]) == "" {
		return errors.New("usage: devm profile select <profile-id>")
	}
	store, err := application.profileStore()
	if err != nil {
		return err
	}
	if err := store.SelectAuthoringProfile(ctx, arguments[0]); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(writerOrDiscard(application.output), "Selected authoring Profile %s.\n", arguments[0]); err != nil {
		return errors.New("write Profile selection result")
	}
	return nil
}

func (application cli) runProfileAdd(ctx context.Context, arguments []string) error {
	ref, policy, exclusions, err := parseProfileAddArguments(arguments)
	if err != nil {
		return err
	}
	candidate := localCapsuleRef{Ref: ref, FreshnessPolicy: policy, Exclusions: exclusions}
	if err := validateLocalCapsuleRef(candidate); err != nil {
		return fmt.Errorf("add Capsule Ref: %w", err)
	}
	store, err := application.profileStore()
	if err != nil {
		return err
	}
	err = store.UpdateSelectedAuthoringProfile(ctx, func(profile *localAuthoringProfile) error {
		for _, existing := range profile.CapsuleRefs {
			if existing.Ref == ref {
				return fmt.Errorf("add Capsule Ref: %s is already present", ref)
			}
		}
		profile.CapsuleRefs = append(profile.CapsuleRefs, candidate)
		return nil
	})
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(writerOrDiscard(application.output), "Added Capsule Ref %s to the selected authoring Profile.\n", ref)
	if err != nil {
		return errors.New("write Capsule Ref result")
	}
	return nil
}

func parseProfileAddArguments(arguments []string) (string, string, []string, error) {
	policy := string(contracts.Track)
	var ref string
	var exclusions []string
	for index := 0; index < len(arguments); index++ {
		switch arguments[index] {
		case "--freshness":
			index++
			if index >= len(arguments) {
				return "", "", nil, errors.New("usage: devm profile add <capsule-ref> [--freshness track|review|pin] [--exclude COMPONENT]")
			}
			policy = arguments[index]
		case "--exclude":
			index++
			if index >= len(arguments) || strings.TrimSpace(arguments[index]) == "" {
				return "", "", nil, errors.New("usage: devm profile add <capsule-ref> [--freshness track|review|pin] [--exclude COMPONENT]")
			}
			exclusions = append(exclusions, arguments[index])
		default:
			if strings.HasPrefix(arguments[index], "-") || ref != "" {
				return "", "", nil, errors.New("usage: devm profile add <capsule-ref> [--freshness track|review|pin] [--exclude COMPONENT]")
			}
			ref = arguments[index]
		}
	}
	if ref == "" {
		return "", "", nil, errors.New("usage: devm profile add <capsule-ref> [--freshness track|review|pin] [--exclude COMPONENT]")
	}
	return ref, policy, exclusions, nil
}

func (application cli) runProfileRemove(ctx context.Context, arguments []string) error {
	if len(arguments) != 1 {
		return errors.New("usage: devm profile remove <capsule-ref>")
	}
	ref := arguments[0]
	if _, err := contracts.ParseOwnedCapsuleRef(ref); err != nil {
		return fmt.Errorf("remove Capsule Ref: %w", err)
	}
	store, err := application.profileStore()
	if err != nil {
		return err
	}
	err = store.UpdateSelectedAuthoringProfile(ctx, func(profile *localAuthoringProfile) error {
		for index, existing := range profile.CapsuleRefs {
			if existing.Ref == ref {
				profile.CapsuleRefs = append(profile.CapsuleRefs[:index], profile.CapsuleRefs[index+1:]...)
				return nil
			}
		}
		return fmt.Errorf("remove Capsule Ref: %s is not present", ref)
	})
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(writerOrDiscard(application.output), "Removed Capsule Ref %s from the selected authoring Profile.\n", ref)
	if err != nil {
		return errors.New("write Capsule Ref result")
	}
	return nil
}

func (application cli) runProfileRefresh(ctx context.Context, arguments []string) error {
	if len(arguments) != 0 {
		return errors.New("usage: devm profile refresh")
	}
	store, err := application.profileStore()
	if err != nil {
		return err
	}
	local, err := store.ReadSelectedAuthoringProfile()
	if err != nil {
		return err
	}
	client, err := application.lifecycleClient(ctx)
	if err != nil {
		return err
	}
	var baseRefs []contracts.CapsuleRef
	if local.LastObservedHeadVersionID != "" {
		base, baseErr := getProfileVersion(ctx, client, local.LastObservedHeadVersionID)
		if baseErr != nil {
			return fmt.Errorf("refresh Profile: load last observed head: %w", baseErr)
		}
		if base.ProfileId != local.ProfileID {
			return errors.New("refresh Profile: last observed head belongs to another Profile")
		}
		baseRefs = base.CapsuleRefs
	}
	remote, err := getProfile(ctx, client, local.ProfileID)
	if err != nil {
		return fmt.Errorf("refresh Profile: %w", err)
	}
	remoteHead := ""
	var remoteRefs []contracts.CapsuleRef
	if remote.HeadVersionId != nil && *remote.HeadVersionId != "" {
		remoteHead = *remote.HeadVersionId
		version, versionErr := getProfileVersion(ctx, client, remoteHead)
		if versionErr != nil {
			return fmt.Errorf("refresh Profile: load remote head: %w", versionErr)
		}
		if version.ProfileId != local.ProfileID {
			return errors.New("refresh Profile: control plane returned a head from another Profile")
		}
		remoteRefs = version.CapsuleRefs
	}
	baseLocalRefs := localCapsuleRefsFromContracts(baseRefs)
	remoteLocalRefs := localCapsuleRefsFromContracts(remoteRefs)
	pending := false
	var pendingRefs []contracts.CapsuleRef
	if err := store.UpdateAuthoringProfile(ctx, local.ProfileID, func(current *localAuthoringProfile) error {
		if current.LastObservedHeadVersionID != local.LastObservedHeadVersionID {
			return errLocalStateConflict
		}
		pending = !reflect.DeepEqual(current.CapsuleRefs, baseLocalRefs)
		if !pending {
			current.CapsuleRefs = append([]localCapsuleRef(nil), remoteLocalRefs...)
		}
		current.LastObservedHeadVersionID = remoteHead
		pendingRefs = current.contractRefs()
		return nil
	}); err != nil {
		return fmt.Errorf("refresh Profile: %w", err)
	}
	oldHead := local.LastObservedHeadVersionID
	if oldHead == "" {
		oldHead = "<none>"
	}
	newHead := remoteHead
	if newHead == "" {
		newHead = "<none>"
	}
	message := fmt.Sprintf("Refreshed authoring Profile %s from head %s to %s. Remote Capsule Ref changes: %s.", local.ProfileID, oldHead, newHead, capsuleRefChanges(baseRefs, remoteRefs))
	if pending {
		message += " Unpublished local edits remain pending: " + describeCapsuleRefs(pendingRefs) + "."
	} else {
		message += " The local Capsule Ref list now matches the remote head."
	}
	if _, err := fmt.Fprintln(writerOrDiscard(application.output), message); err != nil {
		return errors.New("write Profile refresh result")
	}
	return nil
}

func (application cli) runProfilePublish(ctx context.Context, arguments []string) error {
	if len(arguments) != 0 {
		return errors.New("usage: devm profile publish")
	}
	store, err := application.profileStore()
	if err != nil {
		return err
	}
	profile, err := store.ReadSelectedAuthoringProfile()
	if err != nil {
		return err
	}
	if len(profile.CapsuleRefs) == 0 {
		return errors.New("publish Profile: add at least one Capsule Ref first")
	}
	client, err := application.lifecycleClient(ctx)
	if err != nil {
		return err
	}
	if err := validateProfilePublicationForOwner(ctx, client, profile.contractRefs()); err != nil {
		return fmt.Errorf("publish Profile: %w", err)
	}
	var expected *string
	if profile.LastObservedHeadVersionID != "" {
		value := profile.LastObservedHeadVersionID
		expected = &value
	}
	body := contracts.PublishProfileVersionJSONRequestBody{ExpectedHeadVersionId: expected, CapsuleRefs: profile.contractRefs()}
	encoded, _ := json.Marshal(body)
	requestContext, cancel := context.WithTimeout(ctx, lifecycleRequestTimeout)
	response, requestErr := client.api.PublishProfileVersionWithResponse(requestContext, profile.ProfileID,
		&contracts.PublishProfileVersionParams{IdempotencyKey: deterministicKey("profile-publish", profile.ProfileID+"\x00"+string(encoded))}, body, client.editor())
	cancel()
	if requestErr != nil {
		return lifecycleUnavailable(ctx, "publish Profile", requestErr)
	}
	if response.StatusCode() == http.StatusConflict && response.JSONDefault != nil && response.JSONDefault.Error.Code == "STALE_PROFILE_HEAD" {
		return application.staleProfileHeadError(ctx, client, profile)
	}
	if response.StatusCode() != http.StatusCreated || response.JSON201 == nil || response.JSON201.ProfileId != profile.ProfileID || response.JSON201.Id == "" {
		return profileResponseError("publish Profile", response.StatusCode(), response.JSONDefault)
	}
	version := response.JSON201
	pendingLocalEdits := false
	if err := store.UpdateAuthoringProfile(ctx, profile.ProfileID, func(current *localAuthoringProfile) error {
		if current.LastObservedHeadVersionID == version.Id {
			pendingLocalEdits = !reflect.DeepEqual(current.CapsuleRefs, profile.CapsuleRefs)
			return nil
		}
		if current.LastObservedHeadVersionID != profile.LastObservedHeadVersionID {
			return errLocalStateConflict
		}
		pendingLocalEdits = !reflect.DeepEqual(current.CapsuleRefs, profile.CapsuleRefs)
		current.LastObservedHeadVersionID = version.Id
		return nil
	}); err != nil {
		return fmt.Errorf("save published Profile head: %w", err)
	}
	suffix := ""
	if pendingLocalEdits {
		suffix = " Concurrent local Capsule Ref edits remain pending."
	}
	_, err = fmt.Fprintf(writerOrDiscard(application.output), "Published Profile Version %s (version %d).%s\n", version.Id, version.Version, suffix)
	if err != nil {
		return errors.New("write Profile publication result")
	}
	return nil
}

func (application cli) staleProfileHeadError(ctx context.Context, client lifecycleClient, local localAuthoringProfile) error {
	current, err := getProfile(ctx, client, local.ProfileID)
	if err != nil || current.HeadVersionId == nil || *current.HeadVersionId == "" {
		return fmt.Errorf("publish Profile: the Profile head changed; run `devm profile refresh` to advance this draft, or `devm profile fork --local <new-name>` to publish it separately; use `devm profile select %s` to return to this record", local.ProfileID)
	}
	currentVersion, err := getProfileVersion(ctx, client, *current.HeadVersionId)
	if err != nil || currentVersion.ProfileId != local.ProfileID {
		return fmt.Errorf("publish Profile: the Profile head changed; run `devm profile refresh` to advance this draft, or `devm profile fork --local <new-name>` to publish it separately; use `devm profile select %s` to return to this record", local.ProfileID)
	}
	var baseRefs []contracts.CapsuleRef
	if local.LastObservedHeadVersionID != "" {
		base, baseErr := getProfileVersion(ctx, client, local.LastObservedHeadVersionID)
		if baseErr != nil || base.ProfileId != local.ProfileID {
			return fmt.Errorf("publish Profile: stale head (last observed %q, observed remote %q); run `devm profile refresh` to inspect the remote changes and advance this draft, or `devm profile fork --local <new-name>` to publish it separately; use `devm profile select %s` to return to this record", local.LastObservedHeadVersionID, currentVersion.Id, local.ProfileID)
		}
		baseRefs = base.CapsuleRefs
	}
	remoteChanges := capsuleRefChanges(baseRefs, currentVersion.CapsuleRefs)
	localChanges := capsuleRefChanges(baseRefs, local.contractRefs())
	return fmt.Errorf("publish Profile: stale head (last observed %q, observed remote %q); intervening remote Capsule Ref changes: %s; unpublished local Capsule Ref changes: %s. Run `devm profile refresh` to advance the selected draft while keeping unpublished edits pending, or `devm profile fork --local <new-name>` to publish this local draft as a new Profile; use `devm profile select %s` to return to this record", local.LastObservedHeadVersionID, currentVersion.Id, remoteChanges, localChanges, local.ProfileID)
}

func capsuleRefChanges(oldRefs, newRefs []contracts.CapsuleRef) string {
	if reflect.DeepEqual(oldRefs, newRefs) {
		return "none"
	}
	oldSet, newSet := make(map[string]contracts.CapsuleRef), make(map[string]contracts.CapsuleRef)
	for _, ref := range oldRefs {
		oldSet[ref.Ref] = ref
	}
	for _, ref := range newRefs {
		newSet[ref.Ref] = ref
	}
	var changes []string
	for _, ref := range newRefs {
		old, exists := oldSet[ref.Ref]
		if !exists {
			changes = append(changes, "+"+ref.Ref)
		} else if old.FreshnessPolicy != ref.FreshnessPolicy || !reflect.DeepEqual(stringSlice(old.Exclusions), stringSlice(ref.Exclusions)) {
			changes = append(changes, "~"+ref.Ref+" (freshness/exclusions changed)")
		}
	}
	for _, ref := range oldRefs {
		if _, exists := newSet[ref.Ref]; !exists {
			changes = append(changes, "-"+ref.Ref)
		}
	}
	if len(oldRefs) == len(newRefs) && len(oldSet) == len(newSet) {
		for index := range oldRefs {
			if oldRefs[index].Ref != newRefs[index].Ref {
				changes = append(changes, "order changed")
				break
			}
		}
	}
	if len(changes) == 0 {
		return "none"
	}
	return strings.Join(changes, ", ")
}

func describeCapsuleRefs(refs []contracts.CapsuleRef) string {
	if len(refs) == 0 {
		return "<empty>"
	}
	descriptions := make([]string, len(refs))
	for index, ref := range refs {
		descriptions[index] = fmt.Sprintf("%s[%s; exclusions=%s]", ref.Ref, ref.FreshnessPolicy, strings.Join(terminalSafeStrings(stringSlice(ref.Exclusions)), ","))
	}
	return strings.Join(descriptions, " -> ")
}

func (application cli) runProfileApply(ctx context.Context, arguments []string) error {
	versionID, environmentID, noWait, err := parseProfileApplyArguments(arguments)
	if err != nil {
		return err
	}
	environmentID, err = application.resolveEnvironmentID(ctx, environmentID)
	if err != nil {
		return err
	}
	client, err := application.lifecycleClient(ctx)
	if err != nil {
		return err
	}
	requestContext, cancel := context.WithTimeout(ctx, lifecycleRequestTimeout)
	response, requestErr := client.api.ApplyEnvironmentProfileWithResponse(requestContext, environmentID,
		&contracts.ApplyEnvironmentProfileParams{IdempotencyKey: deterministicKey("profile-apply", environmentID+"\x00"+versionID)},
		contracts.ApplyEnvironmentProfileJSONRequestBody{ProfileVersionId: versionID}, client.editor())
	cancel()
	if requestErr != nil {
		return lifecycleUnavailable(ctx, "apply Profile", requestErr)
	}
	if response.StatusCode() != http.StatusAccepted || response.JSON202 == nil || response.JSON202.Operation.Id == "" || response.JSON202.Environment.Id != environmentID {
		if response.StatusCode() == http.StatusNotImplemented {
			return errors.New("profile apply is not yet available on this control plane")
		}
		return profileResponseError("apply Profile", response.StatusCode(), response.JSONDefault)
	}
	operation := response.JSON202.Operation
	if _, err := fmt.Fprintf(writerOrDiscard(application.output), "Profile apply requested: %s (%s).\n", operation.Id, operation.Status); err != nil {
		return errors.New("write Profile apply result")
	}
	if noWait {
		return nil
	}
	return application.pollProfileApplyOperation(ctx, client, operation)
}

func parseProfileApplyArguments(arguments []string) (string, string, bool, error) {
	var versionID, environmentID string
	var noWait bool
	for index := 0; index < len(arguments); index++ {
		switch arguments[index] {
		case "--environment":
			index++
			if index >= len(arguments) || strings.TrimSpace(arguments[index]) == "" {
				return "", "", false, errors.New("usage: devm profile apply <version> [--environment ID] [--no-wait]")
			}
			environmentID = arguments[index]
		case "--no-wait":
			noWait = true
		default:
			if strings.HasPrefix(arguments[index], "-") || versionID != "" {
				return "", "", false, errors.New("usage: devm profile apply <version> [--environment ID] [--no-wait]")
			}
			versionID = arguments[index]
		}
	}
	if versionID == "" {
		return "", "", false, errors.New("usage: devm profile apply <version> [--environment ID] [--no-wait]")
	}
	return versionID, environmentID, noWait, nil
}

func (application cli) pollProfileApplyOperation(ctx context.Context, client lifecycleClient, operation contracts.Operation) error {
	interval := application.stopPollInterval
	if interval <= 0 {
		interval = time.Second
	}
	timeout := application.stopWaitTimeout
	if timeout <= 0 {
		timeout = 60 * time.Second
	}
	waitContext, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	wait := application.wait
	if wait == nil {
		wait = waitForContext
	}
	for !operationTerminal(operation.Status) {
		if err := wait(waitContext, interval); err != nil {
			if errors.Is(err, context.DeadlineExceeded) {
				return fmt.Errorf("apply Profile: Operation %s is still %s after %s; check `devm status`", operation.Id, operation.Status, timeout)
			}
			return err
		}
		current, err := getLifecycleOperation(waitContext, client, operation.Id)
		if err != nil {
			return fmt.Errorf("poll Profile apply Operation: %w", err)
		}
		operation = *current
	}
	if _, err := fmt.Fprintf(writerOrDiscard(application.output), "Profile apply Operation %s: %s.\n", operation.Id, operation.Status); err != nil {
		return errors.New("write Profile apply result")
	}
	if operation.Status != contracts.OperationStatusSucceeded {
		return fmt.Errorf("apply Profile: Operation %s ended %s", operation.Id, operation.Status)
	}
	return nil
}

func stringSlice(value *[]string) []string {
	if value == nil {
		return nil
	}
	return *value
}

func terminalSafe(value string) string {
	if strings.IndexFunc(value, unicode.IsControl) >= 0 {
		return strconv.QuoteToASCII(value)
	}
	return value
}

func terminalSafeStrings(values []string) []string {
	result := make([]string, len(values))
	for index, value := range values {
		result[index] = terminalSafe(value)
	}
	return result
}

func validateProfilePublicationForOwner(ctx context.Context, client lifecycleClient, refs []contracts.CapsuleRef) error {
	if len(refs) == 0 {
		return errors.New("at least one Capsule Ref is required")
	}
	seen := make(map[string]struct{}, len(refs))
	parsedOwners := make([]string, len(refs))
	for index, ref := range refs {
		local := localCapsuleRefsFromContracts([]contracts.CapsuleRef{ref})[0]
		if err := validateLocalCapsuleRef(local); err != nil {
			return fmt.Errorf("Capsule Ref %d is invalid: %w", index+1, err)
		}
		if _, duplicate := seen[ref.Ref]; duplicate {
			return fmt.Errorf("Capsule Ref %d duplicates an earlier ref", index+1)
		}
		seen[ref.Ref] = struct{}{}
		parsed, _ := contracts.ParseOwnedCapsuleRef(ref.Ref)
		parsedOwners[index] = parsed.OwnerID
	}
	requestContext, cancel := context.WithTimeout(ctx, lifecycleRequestTimeout)
	response, err := client.api.GetCurrentUserWithResponse(requestContext, client.editor())
	cancel()
	if err != nil {
		return lifecycleUnavailable(ctx, "validate Profile publication owner", err)
	}
	if response.StatusCode() != http.StatusOK || response.JSON200 == nil || response.JSON200.Id == "" {
		return profileResponseError("validate Profile publication owner", response.StatusCode(), response.JSONDefault)
	}
	for index, ownerID := range parsedOwners {
		if ownerID != response.JSON200.Id {
			return fmt.Errorf("Capsule Ref %d belongs to a different owner; only Capsule Refs owned by the authenticated user can be published", index+1)
		}
	}
	return nil
}

func profileResponseError(action string, status int, response *contracts.Error) error {
	code := ""
	if response != nil {
		code = response.Error.Code
	}
	switch code {
	case "PROFILE_CONFLICT":
		return fmt.Errorf("%s: a Profile with this name already exists", action)
	case "PROFILE_INCOMPATIBLE":
		return fmt.Errorf("%s: the Profile is incompatible; verify that every Capsule Ref belongs to the authenticated user", action)
	case "PROFILE_FORK_UNSUPPORTED":
		return fmt.Errorf("%s: server-side Profile forks are not supported by this control plane", action)
	case "PROFILE_NOT_FOUND":
		return fmt.Errorf("%s: the Profile was not found or is not owned by the authenticated user", action)
	case "PROFILE_VERSION_NOT_FOUND":
		return fmt.Errorf("%s: the Profile Version was not found or is not owned by the authenticated user", action)
	case "IDEMPOTENCY_CONFLICT":
		return fmt.Errorf("%s: a previous request used the same retry identity with different input", action)
	case "AUTHORIZATION_FAILED":
		return errors.New("not authenticated: run `devm login`")
	case "CREDITS_POLICY_BLOCKED":
		return fmt.Errorf("%s: the operation is blocked by the current credit policy", action)
	case "COMMAND_UNAVAILABLE", "INTERNAL_ERROR":
		return fmt.Errorf("%s: the control plane is temporarily unavailable; retry later", action)
	}
	switch status {
	case http.StatusTooManyRequests, http.StatusInternalServerError, http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		return fmt.Errorf("%s: the control plane is temporarily unavailable (HTTP %d); retry later", action, status)
	default:
		return fmt.Errorf("%s: control plane rejected the request (HTTP %d)", action, status)
	}
}
