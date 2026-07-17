package db

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/ahmedhesham6/sshai/libs/db/internal/dbsql"
	"github.com/jackc/pgx/v5"
)

// EnvironmentCreatePin is the read-only projection an EnvironmentCreate
// Operation ID resolves to: the Environment it targets, that Environment's
// owner, and the Profile Version pinned at Environment creation. It carries
// no side effects — unlike RecordEnvironmentCreateInvocation, which requires
// a Restate invocation ID and mutates the Operation record, this looks up
// the mapping the pinned Profile Version resolver needs without recording
// anything.
type EnvironmentCreatePin struct {
	OwnerUserID            string
	EnvironmentID          string
	PinnedProfileVersionID string
}

// LoadEnvironmentCreatePin loads the Environment and Profile Version pin
// targeted by an environment.create Operation. An unknown Operation ID (one
// that is not a recorded environment.create Operation) reports
// ErrReferenceNotOwned, matching the convention used by
// LoadProfileResolveState and LoadProfileVersion for unrecognized
// references.
func (store *Store) LoadEnvironmentCreatePin(ctx context.Context, operationID string) (EnvironmentCreatePin, error) {
	if strings.TrimSpace(operationID) == "" {
		return EnvironmentCreatePin{}, errors.New("load Environment create pin: Operation ID is required")
	}
	row, err := store.queries.GetEnvironmentCreatePin(ctx, operationID)
	if errors.Is(err, pgx.ErrNoRows) {
		return EnvironmentCreatePin{}, ErrReferenceNotOwned
	}
	if err != nil {
		return EnvironmentCreatePin{}, fmt.Errorf("load Environment create pin: %w", err)
	}
	return environmentCreatePinFromRow(row), nil
}

func environmentCreatePinFromRow(row dbsql.GetEnvironmentCreatePinRow) EnvironmentCreatePin {
	return EnvironmentCreatePin{
		OwnerUserID: row.OwnerUserID, EnvironmentID: row.EnvironmentID, PinnedProfileVersionID: row.PinnedProfileVersionID,
	}
}
