package db_test

import (
	"context"
	"errors"
	"testing"
	"time"

	dbstore "github.com/ahmedhesham6/sshai/libs/db"
)

func TestLoadEnvironmentCreatePinReturnsOwnerEnvironmentAndPinnedVersion(t *testing.T) {
	ctx := context.Background()
	store, pool := openTestStoreAndPool(t, ctx)
	createdAt := time.Date(2026, time.July, 17, 12, 0, 0, 0, time.UTC)
	insertCreationPrerequisites(t, ctx, pool)

	creation := newEnvironmentCreation(t, "environment-1", "policy-1", "operation-1", []byte(`{"name":"workspace"}`), createdAt)
	if _, err := store.ReserveEnvironmentCreation(ctx, creation); err != nil {
		t.Fatalf("reserve Environment creation: %v", err)
	}

	pin, err := store.LoadEnvironmentCreatePin(ctx, "operation-1")
	if err != nil {
		t.Fatalf("LoadEnvironmentCreatePin() error = %v", err)
	}
	if pin.OwnerUserID != "user-1" || pin.EnvironmentID != "environment-1" || pin.PinnedProfileVersionID != "profile-version-1" {
		t.Fatalf("LoadEnvironmentCreatePin() = %#v, want owner:user-1 environment:environment-1 pinnedProfileVersion:profile-version-1", pin)
	}
}

func TestLoadEnvironmentCreatePinRejectsUnknownOperation(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t, ctx)

	if _, err := store.LoadEnvironmentCreatePin(ctx, "operation-missing"); !errors.Is(err, dbstore.ErrReferenceNotOwned) {
		t.Fatalf("LoadEnvironmentCreatePin() error = %v, want ErrReferenceNotOwned", err)
	}
}

func TestLoadEnvironmentCreatePinRejectsBlankOperationID(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t, ctx)

	if _, err := store.LoadEnvironmentCreatePin(ctx, "   "); err == nil {
		t.Fatal("LoadEnvironmentCreatePin() error = nil, want error for blank Operation ID")
	}
}

func TestLoadEnvironmentCreatePinIgnoresOperationsOfOtherTypes(t *testing.T) {
	ctx := context.Background()
	store, pool := openTestStoreAndPool(t, ctx)
	now := time.Date(2026, time.July, 17, 16, 0, 0, 0, time.UTC)
	seedEnvironmentMaterializationPrerequisites(t, ctx, pool)

	if err := store.RecordProfileResolveInvocation(ctx, "operation-resolve", "invocation-resolve", "environment-1", "version-1", nil, now); err != nil {
		t.Fatalf("record Profile resolve invocation: %v", err)
	}

	if _, err := store.LoadEnvironmentCreatePin(ctx, "operation-resolve"); !errors.Is(err, dbstore.ErrReferenceNotOwned) {
		t.Fatalf("LoadEnvironmentCreatePin() error = %v, want ErrReferenceNotOwned for a non environment.create Operation", err)
	}
}
