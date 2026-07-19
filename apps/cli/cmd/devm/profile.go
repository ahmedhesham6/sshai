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
	"strings"
	"time"

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
	case "add":
		return application.runProfileAdd(ctx, arguments[1:])
	case "remove":
		return application.runProfileRemove(ctx, arguments[1:])
	case "publish":
		return application.runProfilePublish(ctx, arguments[1:])
	case "apply":
		return application.runProfileApply(ctx, arguments[1:])
	default:
		return profileUsage()
	}
}

func profileUsage() error {
	return errors.New("usage: devm profile <list|show|create|fork|add|remove|publish|apply>")
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
		if _, err := fmt.Fprintf(output, "%s\t%s\t%s\n", profile.Name, profile.Id, head); err != nil {
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
			return nil, fmt.Errorf("list Profiles: control plane returned HTTP %d", response.StatusCode())
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
	if _, err := fmt.Fprintf(output, "Profile\t%s\nID\t%s\nHead version\t%s\nCAPSULE REF\tFRESHNESS\tEXCLUSIONS\n", profile.Name, profile.Id, head); err != nil {
		return errors.New("write Profile")
	}
	if version != nil {
		for _, ref := range version.CapsuleRefs {
			if _, err := fmt.Fprintf(output, "%s\t%s\t%s\n", ref.Ref, ref.FreshnessPolicy, strings.Join(stringSlice(ref.Exclusions), ",")); err != nil {
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
		if identifier == profile.Name || identifier == profile.Id || identifier == profile.Slug {
			requestContext, cancel := context.WithTimeout(ctx, lifecycleRequestTimeout)
			response, requestErr := client.api.GetProfileWithResponse(requestContext, profile.Id, client.editor())
			cancel()
			if requestErr != nil {
				return contracts.ProfileSummary{}, lifecycleUnavailable(ctx, "get Profile", requestErr)
			}
			if response.StatusCode() != http.StatusOK || response.JSON200 == nil || response.JSON200.Id != profile.Id {
				return contracts.ProfileSummary{}, fmt.Errorf("get Profile: control plane returned HTTP %d", response.StatusCode())
			}
			return *response.JSON200, nil
		}
	}
	return contracts.ProfileSummary{}, fmt.Errorf("Profile %q was not found", identifier)
}

func getProfileVersion(ctx context.Context, client lifecycleClient, versionID string) (*contracts.ProfileVersion, error) {
	requestContext, cancel := context.WithTimeout(ctx, lifecycleRequestTimeout)
	response, err := client.api.GetProfileVersionWithResponse(requestContext, versionID, client.editor())
	cancel()
	if err != nil {
		return nil, lifecycleUnavailable(ctx, "get Profile Version", err)
	}
	if response.StatusCode() != http.StatusOK || response.JSON200 == nil || response.JSON200.Id != versionID {
		return nil, fmt.Errorf("get Profile Version: control plane returned HTTP %d", response.StatusCode())
	}
	return response.JSON200, nil
}

func (application cli) runProfileCreate(ctx context.Context, arguments []string) error {
	if len(arguments) != 1 || strings.TrimSpace(arguments[0]) == "" {
		return errors.New("usage: devm profile create <name>")
	}
	return application.createAuthoringProfile(ctx, arguments[0], nil, nil)
}

func (application cli) runProfileFork(ctx context.Context, arguments []string) error {
	if len(arguments) != 2 || strings.TrimSpace(arguments[0]) == "" || strings.TrimSpace(arguments[1]) == "" {
		return errors.New("usage: devm profile fork <source-version> <name>")
	}
	client, err := application.lifecycleClient(ctx)
	if err != nil {
		return err
	}
	source, err := getProfileVersion(ctx, client, arguments[0])
	if err != nil {
		return fmt.Errorf("fork Profile: %w", err)
	}
	return application.createAuthoringProfileWithClient(ctx, client, arguments[1], &source.Id, source.CapsuleRefs)
}

func (application cli) createAuthoringProfile(ctx context.Context, name string, forkedFrom *string, refs []contracts.CapsuleRef) error {
	client, err := application.lifecycleClient(ctx)
	if err != nil {
		return err
	}
	return application.createAuthoringProfileWithClient(ctx, client, name, forkedFrom, refs)
}

func (application cli) createAuthoringProfileWithClient(ctx context.Context, client lifecycleClient, name string, forkedFrom *string, refs []contracts.CapsuleRef) error {
	keyMaterial := name
	if forkedFrom != nil {
		keyMaterial += "\x00" + *forkedFrom
	}
	requestContext, cancel := context.WithTimeout(ctx, lifecycleRequestTimeout)
	response, err := client.api.CreateProfileWithResponse(requestContext,
		&contracts.CreateProfileParams{IdempotencyKey: deterministicKey("profile-create", keyMaterial)},
		contracts.CreateProfileJSONRequestBody{Name: name, ForkedFromVersionId: forkedFrom}, client.editor())
	cancel()
	if err != nil {
		return lifecycleUnavailable(ctx, "create Profile", err)
	}
	if response.StatusCode() != http.StatusCreated || response.JSON201 == nil || !localProfileIDPattern.MatchString(response.JSON201.Id) {
		return fmt.Errorf("create Profile: control plane returned HTTP %d", response.StatusCode())
	}
	head := ""
	if response.JSON201.HeadVersionId != nil {
		head = *response.JSON201.HeadVersionId
	}
	store, err := application.profileStore()
	if err != nil {
		return err
	}
	record := authoringProfileFromContracts(response.JSON201.Id, response.JSON201.Name, head, refs)
	if err := store.SaveAuthoringProfile(ctx, record); err != nil {
		return fmt.Errorf("save authoring Profile: %w", err)
	}
	verb := "Created"
	if forkedFrom != nil {
		verb = "Forked"
	}
	_, err = fmt.Fprintf(writerOrDiscard(application.output), "%s Profile %s (%s); it is now selected for authoring.\n", verb, response.JSON201.Name, response.JSON201.Id)
	if err != nil {
		return errors.New("write Profile creation result")
	}
	return nil
}

func (application cli) runProfileAdd(ctx context.Context, arguments []string) error {
	ref, policy, exclusions, err := parseProfileAddArguments(arguments)
	if err != nil {
		return err
	}
	if _, err := contracts.ParseOwnedCapsuleRef(ref); err != nil {
		return fmt.Errorf("add Capsule Ref: %w", err)
	}
	if !contracts.CapsuleRefFreshnessPolicy(policy).Valid() {
		return errors.New("add Capsule Ref: freshness must be track, review, or pin")
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
		profile.CapsuleRefs = append(profile.CapsuleRefs, localCapsuleRef{Ref: ref, FreshnessPolicy: policy, Exclusions: exclusions})
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
		return fmt.Errorf("publish Profile: control plane returned HTTP %d", response.StatusCode())
	}
	version := response.JSON201
	if err := store.UpdateSelectedAuthoringProfile(ctx, func(current *localAuthoringProfile) error {
		if current.ProfileID != profile.ProfileID || current.LastObservedHeadVersionID != profile.LastObservedHeadVersionID ||
			!reflect.DeepEqual(current.CapsuleRefs, profile.CapsuleRefs) {
			return errLocalStateConflict
		}
		current.LastObservedHeadVersionID = version.Id
		return nil
	}); err != nil {
		return fmt.Errorf("save published Profile head: %w", err)
	}
	_, err = fmt.Fprintf(writerOrDiscard(application.output), "Published Profile Version %s (version %d).\n", version.Id, version.Version)
	if err != nil {
		return errors.New("write Profile publication result")
	}
	return nil
}

func (application cli) staleProfileHeadError(ctx context.Context, client lifecycleClient, local localAuthoringProfile) error {
	current, err := resolveProfile(ctx, client, local.ProfileID)
	if err != nil || current.HeadVersionId == nil || *current.HeadVersionId == "" {
		return errors.New("publish Profile: the Profile head changed; refresh the authoring Profile or fork the current head before publishing again (automatic merging is not supported)")
	}
	currentVersion, err := getProfileVersion(ctx, client, *current.HeadVersionId)
	if err != nil {
		return errors.New("publish Profile: the Profile head changed; refresh the authoring Profile or fork the current head before publishing again (automatic merging is not supported)")
	}
	changes := capsuleRefChanges(local.contractRefs(), currentVersion.CapsuleRefs)
	return fmt.Errorf("publish Profile: stale head (last observed %q, current %q); intervening Capsule Ref changes: %s. Refresh the authoring Profile from current head %s, or fork it with `devm profile fork %s <new-name>`; automatic merging is not supported", local.LastObservedHeadVersionID, currentVersion.Id, changes, currentVersion.Id, currentVersion.Id)
}

func capsuleRefChanges(oldRefs, newRefs []contracts.CapsuleRef) string {
	oldSet, newSet := make(map[string]struct{}), make(map[string]struct{})
	for _, ref := range oldRefs {
		oldSet[ref.Ref] = struct{}{}
	}
	for _, ref := range newRefs {
		newSet[ref.Ref] = struct{}{}
	}
	var changes []string
	for _, ref := range newRefs {
		if _, exists := oldSet[ref.Ref]; !exists {
			changes = append(changes, "+"+ref.Ref)
		}
	}
	for _, ref := range oldRefs {
		if _, exists := newSet[ref.Ref]; !exists {
			changes = append(changes, "-"+ref.Ref)
		}
	}
	if len(changes) == 0 {
		return "metadata or ordering changed"
	}
	return strings.Join(changes, ", ")
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
		return fmt.Errorf("apply Profile: control plane returned HTTP %d", response.StatusCode())
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
