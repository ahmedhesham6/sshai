package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"reflect"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/ahmedhesham6/sshai/libs/contracts"
	"github.com/pelletier/go-toml/v2"
)

var localProfileIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)

type localAuthoringProfile struct {
	Version                   int               `toml:"version"`
	ProfileID                 string            `toml:"profile_id"`
	Name                      string            `toml:"name"`
	LastObservedHeadVersionID string            `toml:"last_observed_head_version_id"`
	CapsuleRefs               []localCapsuleRef `toml:"capsule_refs"`
}

type localCapsuleRef struct {
	Ref             string   `toml:"ref"`
	FreshnessPolicy string   `toml:"freshness_policy"`
	Exclusions      []string `toml:"exclusions,omitempty"`
}

type localProfileSelection struct {
	Version   int    `toml:"version"`
	ProfileID string `toml:"profile_id"`
}

func authoringProfileFromContracts(profileID, name, head string, refs []contracts.CapsuleRef) localAuthoringProfile {
	result := localAuthoringProfile{Version: localStateVersion, ProfileID: profileID, Name: name, LastObservedHeadVersionID: head}
	result.CapsuleRefs = localCapsuleRefsFromContracts(refs)
	return result
}

func localCapsuleRefsFromContracts(refs []contracts.CapsuleRef) []localCapsuleRef {
	result := make([]localCapsuleRef, len(refs))
	for index, ref := range refs {
		result[index] = localCapsuleRef{Ref: ref.Ref, FreshnessPolicy: string(ref.FreshnessPolicy)}
		if ref.Exclusions != nil {
			result[index].Exclusions = append([]string(nil), (*ref.Exclusions)...)
		}
	}
	return result
}

func (profile localAuthoringProfile) contractRefs() []contracts.CapsuleRef {
	refs := make([]contracts.CapsuleRef, len(profile.CapsuleRefs))
	for index, ref := range profile.CapsuleRefs {
		refs[index] = contracts.CapsuleRef{Ref: ref.Ref, FreshnessPolicy: contracts.CapsuleRefFreshnessPolicy(ref.FreshnessPolicy)}
		if ref.Exclusions != nil {
			exclusions := append([]string(nil), ref.Exclusions...)
			refs[index].Exclusions = &exclusions
		}
	}
	return refs
}

func (store localStateStore) SaveAuthoringProfile(ctx context.Context, profile localAuthoringProfile) error {
	content, err := encodeLocalAuthoringProfile(profile)
	if err != nil {
		return err
	}
	profiles, lock, err := store.openProfilesForUpdate(ctx)
	if err != nil {
		return err
	}
	defer profiles.Close()
	defer lock.Close()
	existing, found, err := readAuthoringProfileFrom(profiles, profile.ProfileID)
	if err != nil {
		return err
	}
	if found {
		if existing.Name != profile.Name {
			return errors.New("save authoring Profile: existing record has a different identity")
		}
		if reflect.DeepEqual(existing, profile) {
			if err := profiles.writePrivate(profile.ProfileID+".toml", content); err != nil {
				return fmt.Errorf("write authoring Profile: %w", err)
			}
		}
		return writeProfileSelection(profiles, profile.ProfileID)
	}
	return writeAuthoringProfileAndSelection(profiles, profile, content)
}

func (store localStateStore) ReadAuthoringProfile(profileID string) (localAuthoringProfile, bool, error) {
	if !localProfileIDPattern.MatchString(profileID) {
		return localAuthoringProfile{}, false, errors.New("read authoring Profile: Profile ID is invalid")
	}
	state, err := openAnchoredDirectory(store.directory, false, 0)
	if errors.Is(err, os.ErrNotExist) {
		return localAuthoringProfile{}, false, nil
	}
	if err != nil {
		return localAuthoringProfile{}, false, fmt.Errorf("open local state: %w", err)
	}
	defer state.Close()
	if err := requirePrivateDirectory(state, "local state"); err != nil {
		return localAuthoringProfile{}, false, err
	}
	profiles, err := openAnchoredChild(state, "profiles", false)
	if errors.Is(err, os.ErrNotExist) {
		return localAuthoringProfile{}, false, nil
	}
	if err != nil {
		return localAuthoringProfile{}, false, err
	}
	defer profiles.Close()
	return readAuthoringProfileFrom(profiles, profileID)
}

func (store localStateStore) ReadSelectedAuthoringProfile() (localAuthoringProfile, error) {
	state, err := openAnchoredDirectory(store.directory, false, 0)
	if errors.Is(err, os.ErrNotExist) {
		return localAuthoringProfile{}, errors.New("no authoring Profile selected; run `devm profile create` or `devm profile fork`")
	}
	if err != nil {
		return localAuthoringProfile{}, fmt.Errorf("open local state: %w", err)
	}
	defer state.Close()
	if err := requirePrivateDirectory(state, "local state"); err != nil {
		return localAuthoringProfile{}, err
	}
	profiles, err := openAnchoredChild(state, "profiles", false)
	if errors.Is(err, os.ErrNotExist) {
		return localAuthoringProfile{}, errors.New("no authoring Profile selected; run `devm profile create` or `devm profile fork`")
	}
	if err != nil {
		return localAuthoringProfile{}, err
	}
	defer profiles.Close()
	selectionContent, info, err := profiles.readRegular("selection.toml", maxLocalStateFileSize)
	if errors.Is(err, os.ErrNotExist) {
		return localAuthoringProfile{}, errors.New("no authoring Profile selected; run `devm profile create` or `devm profile fork`")
	}
	if err != nil || info.Mode().Perm() != 0o600 {
		return localAuthoringProfile{}, errors.New("selected authoring Profile is unsafe")
	}
	var selection localProfileSelection
	decoder := toml.NewDecoder(bytes.NewReader(selectionContent))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&selection) != nil || selection.Version != localStateVersion || !localProfileIDPattern.MatchString(selection.ProfileID) {
		return localAuthoringProfile{}, errors.New("selected authoring Profile is malformed")
	}
	profile, found, err := readAuthoringProfileFrom(profiles, selection.ProfileID)
	if err != nil {
		return localAuthoringProfile{}, err
	}
	if !found {
		return localAuthoringProfile{}, errors.New("selected authoring Profile record is missing")
	}
	return profile, nil
}

func (store localStateStore) UpdateSelectedAuthoringProfile(ctx context.Context, update func(*localAuthoringProfile) error) error {
	if update == nil {
		return errors.New("update authoring Profile: update is required")
	}
	profiles, lock, err := store.openProfilesForUpdate(ctx)
	if err != nil {
		return err
	}
	defer profiles.Close()
	defer lock.Close()
	selection, err := readProfileSelectionFrom(profiles)
	if err != nil {
		return err
	}
	return updateAuthoringProfileFrom(profiles, selection.ProfileID, update)
}

func (store localStateStore) UpdateAuthoringProfile(ctx context.Context, profileID string, update func(*localAuthoringProfile) error) error {
	if update == nil {
		return errors.New("update authoring Profile: update is required")
	}
	if !localProfileIDPattern.MatchString(profileID) {
		return errors.New("update authoring Profile: Profile ID is invalid")
	}
	profiles, lock, err := store.openProfilesForUpdate(ctx)
	if err != nil {
		return err
	}
	defer profiles.Close()
	defer lock.Close()
	return updateAuthoringProfileFrom(profiles, profileID, update)
}

func (store localStateStore) SelectAuthoringProfile(ctx context.Context, profileID string) error {
	if !localProfileIDPattern.MatchString(profileID) {
		return errors.New("select authoring Profile: Profile ID is invalid")
	}
	profiles, lock, err := store.openProfilesForUpdate(ctx)
	if err != nil {
		return err
	}
	defer profiles.Close()
	defer lock.Close()
	if _, found, err := readAuthoringProfileFrom(profiles, profileID); err != nil {
		return err
	} else if !found {
		return fmt.Errorf("select authoring Profile: local record %q was not found", profileID)
	}
	return writeProfileSelection(profiles, profileID)
}

func updateAuthoringProfileFrom(profiles *anchoredDirectory, profileID string, update func(*localAuthoringProfile) error) error {
	profile, found, err := readAuthoringProfileFrom(profiles, profileID)
	if err != nil {
		return err
	}
	if !found {
		return errors.New("authoring Profile record is missing")
	}
	if err := update(&profile); err != nil {
		return err
	}
	content, err := encodeLocalAuthoringProfile(profile)
	if err != nil {
		return err
	}
	return profiles.writePrivate(profile.ProfileID+".toml", content)
}

func (store localStateStore) openProfilesForUpdate(ctx context.Context) (*anchoredDirectory, ioCloser, error) {
	state, err := openOwnedDirectory(store.directory)
	if err != nil {
		return nil, nil, fmt.Errorf("open local state: %w", err)
	}
	profiles, err := state.ownedChild("profiles")
	state.Close()
	if err != nil {
		return nil, nil, fmt.Errorf("open authoring Profiles: %w", err)
	}
	lock, err := acquirePrivateFileLock(ctx, profiles, "profiles.lock")
	if err != nil {
		profiles.Close()
		return nil, nil, fmt.Errorf("lock authoring Profiles: %w", err)
	}
	return profiles, lock, nil
}

type ioCloser interface{ Close() error }

func writeAuthoringProfileAndSelection(profiles *anchoredDirectory, profile localAuthoringProfile, content []byte) error {
	if err := profiles.writePrivate(profile.ProfileID+".toml", content); err != nil {
		return fmt.Errorf("write authoring Profile: %w", err)
	}
	return writeProfileSelection(profiles, profile.ProfileID)
}

func writeProfileSelection(profiles *anchoredDirectory, profileID string) error {
	selection, err := toml.Marshal(localProfileSelection{Version: localStateVersion, ProfileID: profileID})
	if err != nil {
		return errors.New("encode authoring Profile selection")
	}
	if err := profiles.writePrivate("selection.toml", selection); err != nil {
		return fmt.Errorf("select authoring Profile: %w", err)
	}
	return nil
}

func readProfileSelectionFrom(profiles *anchoredDirectory) (localProfileSelection, error) {
	content, info, err := profiles.readRegular("selection.toml", maxLocalStateFileSize)
	if errors.Is(err, os.ErrNotExist) {
		return localProfileSelection{}, errors.New("no authoring Profile selected; run `devm profile create` or `devm profile fork`")
	}
	if err != nil || info.Mode().Perm() != 0o600 {
		return localProfileSelection{}, errors.New("selected authoring Profile is unsafe")
	}
	var selection localProfileSelection
	decoder := toml.NewDecoder(bytes.NewReader(content))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&selection) != nil || selection.Version != localStateVersion || !localProfileIDPattern.MatchString(selection.ProfileID) {
		return localProfileSelection{}, errors.New("selected authoring Profile is malformed")
	}
	return selection, nil
}

func readAuthoringProfileFrom(profiles *anchoredDirectory, profileID string) (localAuthoringProfile, bool, error) {
	content, info, err := profiles.readRegular(profileID+".toml", maxLocalStateFileSize)
	if errors.Is(err, os.ErrNotExist) {
		return localAuthoringProfile{}, false, nil
	}
	if err != nil || info.Mode().Perm() != 0o600 {
		return localAuthoringProfile{}, false, errors.New("authoring Profile record is unsafe")
	}
	var profile localAuthoringProfile
	decoder := toml.NewDecoder(bytes.NewReader(content))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&profile) != nil {
		return localAuthoringProfile{}, false, errors.New("authoring Profile record is malformed")
	}
	if profile.ProfileID != profileID {
		return localAuthoringProfile{}, false, errors.New("authoring Profile record does not match its filename")
	}
	if err := validateLocalAuthoringProfile(profile); err != nil {
		return localAuthoringProfile{}, false, err
	}
	return profile, true, nil
}

func validateLocalAuthoringProfile(profile localAuthoringProfile) error {
	if profile.Version != localStateVersion || !localProfileIDPattern.MatchString(profile.ProfileID) || strings.TrimSpace(profile.Name) == "" {
		return errors.New("authoring Profile record is malformed")
	}
	seen := make(map[string]struct{}, len(profile.CapsuleRefs))
	for _, ref := range profile.CapsuleRefs {
		if err := validateLocalCapsuleRef(ref); err != nil {
			return err
		}
		if _, exists := seen[ref.Ref]; exists {
			return errors.New("authoring Profile contains a duplicate Capsule Ref")
		}
		seen[ref.Ref] = struct{}{}
	}
	return nil
}

func validateLocalCapsuleRef(ref localCapsuleRef) error {
	if _, err := contracts.ParseOwnedCapsuleRef(ref.Ref); err != nil {
		return fmt.Errorf("authoring Profile contains invalid Capsule Ref: %w", err)
	}
	if !contracts.CapsuleRefFreshnessPolicy(ref.FreshnessPolicy).Valid() {
		return errors.New("authoring Profile contains an invalid freshness policy")
	}
	seenExclusions := make(map[string]struct{}, len(ref.Exclusions))
	for _, exclusion := range ref.Exclusions {
		if strings.TrimSpace(exclusion) == "" || exclusion != strings.TrimSpace(exclusion) || utf8.RuneCountInString(exclusion) > 256 {
			return errors.New("authoring Profile contains an invalid component exclusion (must be 1-256 characters without surrounding whitespace)")
		}
		if _, duplicate := seenExclusions[exclusion]; duplicate {
			return errors.New("authoring Profile contains a duplicate component exclusion")
		}
		seenExclusions[exclusion] = struct{}{}
	}
	return nil
}

func encodeLocalAuthoringProfile(profile localAuthoringProfile) ([]byte, error) {
	if err := validateLocalAuthoringProfile(profile); err != nil {
		return nil, err
	}
	content, err := toml.Marshal(profile)
	if err != nil {
		return nil, errors.New("encode authoring Profile")
	}
	if len(content) > maxLocalStateFileSize {
		return nil, errors.New("authoring Profile record exceeds the local-state size limit")
	}
	return content, nil
}
