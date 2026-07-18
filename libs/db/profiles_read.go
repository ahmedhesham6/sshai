package db

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/ahmedhesham6/sshai/libs/db/internal/dbsql"
	"github.com/ahmedhesham6/sshai/libs/domain"
	"github.com/jackc/pgx/v5"
)

// ProfileDetail is the read-only projection an owner-scoped Profile query
// resolves to: the Profile aggregate alongside the ID of its current head
// Profile Version, if one has been published yet.
type ProfileDetail struct {
	Profile       domain.Profile
	HeadVersionID *string
}

// GetOwnedProfile loads a single Profile owned by ownerID. An absent or
// foreign Profile reports ErrReferenceNotOwned, matching the convention used
// by every other owner-scoped Get.
func (store *Store) GetOwnedProfile(ctx context.Context, ownerID, profileID string) (ProfileDetail, error) {
	if strings.TrimSpace(ownerID) == "" || strings.TrimSpace(profileID) == "" {
		return ProfileDetail{}, errors.New("get owned Profile: canonical owner and Profile IDs are required")
	}
	row, err := store.queries.GetOwnedProfile(ctx, dbsql.GetOwnedProfileParams{ProfileID: profileID, OwnerUserID: ownerID})
	if errors.Is(err, pgx.ErrNoRows) {
		return ProfileDetail{}, ErrReferenceNotOwned
	}
	if err != nil {
		return ProfileDetail{}, fmt.Errorf("get owned Profile: %w", err)
	}
	profile, err := restoreProfile(row.ID, row.OwnerUserID, row.Name, row.Slug, row.CreatedAt, row.ArchivedAt)
	if err != nil {
		return ProfileDetail{}, err
	}
	headVersionID, err := store.profileHeadVersionID(ctx, row.ID)
	if err != nil {
		return ProfileDetail{}, err
	}
	return ProfileDetail{Profile: profile, HeadVersionID: headVersionID}, nil
}

// ListOwnedProfiles loads every Profile owned by ownerID, ordered by creation
// time then ID.
func (store *Store) ListOwnedProfiles(ctx context.Context, ownerID string) ([]ProfileDetail, error) {
	if strings.TrimSpace(ownerID) == "" {
		return nil, errors.New("list owned Profiles: canonical owner User ID is required")
	}
	rows, err := store.queries.ListOwnedProfiles(ctx, ownerID)
	if err != nil {
		return nil, fmt.Errorf("list owned Profiles: %w", err)
	}
	details := make([]ProfileDetail, len(rows))
	for index, row := range rows {
		profile, err := restoreProfile(row.ID, row.OwnerUserID, row.Name, row.Slug, row.CreatedAt, row.ArchivedAt)
		if err != nil {
			return nil, err
		}
		headVersionID, err := store.profileHeadVersionID(ctx, row.ID)
		if err != nil {
			return nil, err
		}
		details[index] = ProfileDetail{Profile: profile, HeadVersionID: headVersionID}
	}
	return details, nil
}

func (store *Store) profileHeadVersionID(ctx context.Context, profileID string) (*string, error) {
	id, err := store.queries.GetProfileHeadVersionID(ctx, profileID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("load Profile head Version ID: %w", err)
	}
	return &id, nil
}

// GetOwnedProfileVersion loads an immutable Profile Version, scoped to the
// Profile owner. An absent or foreign Profile Version reports
// ErrReferenceNotOwned.
func (store *Store) GetOwnedProfileVersion(ctx context.Context, ownerID, profileVersionID string) (domain.ProfileVersion, error) {
	if strings.TrimSpace(ownerID) == "" || strings.TrimSpace(profileVersionID) == "" {
		return domain.ProfileVersion{}, errors.New("get owned Profile Version: canonical owner and Version IDs are required")
	}
	row, err := store.queries.GetOwnedProfileVersion(ctx, dbsql.GetOwnedProfileVersionParams{
		ProfileVersionID: profileVersionID, OwnerUserID: ownerID,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.ProfileVersion{}, ErrReferenceNotOwned
	}
	if err != nil {
		return domain.ProfileVersion{}, fmt.Errorf("get owned Profile Version: %w", err)
	}
	return restoreProfileVersion(ctx, store.queries, row)
}
